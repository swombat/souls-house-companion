package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/volumes"
)

const (
	cmdSecret = "shcp_synthetic-poll-secret"
	cmdToken  = "shce_synthetic-enrollment-token"
)

type cmdHouse struct {
	mu     sync.Mutex
	hits   int
	id     string
	claims []string // "pending", "approved", "expired", "cancelled", "unknown"
	n      int
}

func (h *cmdHouse) handler(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hits++
	switch r.URL.Path {
	case "/api/v1/companion/pairings":
		var in map[string]any
		json.NewDecoder(r.Body).Decode(&in)
		h.id, _ = in["device_request_id"].(string)
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"id": "pub1", "code": "ABCD-EFGH", "poll_secret": cmdSecret,
			"expires_at": time.Now().Add(15 * time.Minute).UTC().Format(time.RFC3339), "poll_interval": 3})
	case "/api/v1/companion/pairings/pub1/claim":
		step := h.claims[min(h.n, len(h.claims)-1)]
		h.n++
		switch step {
		case "pending":
			w.WriteHeader(202)
			w.Write([]byte(`{"status":"pending"}`))
		case "approved":
			json.NewEncoder(w).Encode(map[string]any{"status": "approved", "enrollment_token": cmdToken,
				"device_request_id": h.id, "resident_ids": []string{"r1"}, "execute": true})
		case "unknown":
			w.WriteHeader(404)
		default:
			w.WriteHeader(410)
			w.Write([]byte(`{"status":"` + step + `"}`))
		}
	case "/api/v1/companion/enroll":
		json.NewEncoder(w).Encode(map[string]string{"device_id": "dev1", "device_credential": "shcd_synthetic-credential"})
	default:
		w.WriteHeader(404)
	}
}

type pairHarness struct {
	house *cmdHouse
	url   string
	dir   string
	out   *bytes.Buffer
	deps  pairDeps
}

func newPairHarness(t *testing.T, claims ...string) *pairHarness {
	h := &cmdHouse{claims: claims}
	s := httptest.NewTLSServer(http.HandlerFunc(h.handler))
	t.Cleanup(s.Close)
	c := client.New(nil)
	c.HTTP.Transport = s.Client().Transport
	clock := time.Now()
	out := &bytes.Buffer{}
	return &pairHarness{house: h, url: s.URL, dir: filepath.Join(t.TempDir(), "state"), out: out, deps: pairDeps{
		mounts: func() ([]volumes.Mount, error) {
			return []volumes.Mount{{Key: "k-data", Mountpoint: "/System/Volumes/Data"},
				{Key: "k-sealed", Mountpoint: "/System", Denied: "sealed"}}, nil
		},
		client: c, out: out, now: func() time.Time { return clock },
		sleep: func(ctx context.Context, d time.Duration) error { clock = clock.Add(d); return ctx.Err() },
	}}
}

func (p *pairHarness) run(ctx context.Context, specs ...string) error {
	return pairCommand(ctx, p.dir, p.url, "Paulina's MacBook Air", specs, p.deps)
}

func exitCode(err error) int {
	var e *exitError
	if errors.As(err, &e) {
		return e.code
	}
	if err != nil {
		return -1
	}
	return 0
}

func lines(t *testing.T, out *bytes.Buffer) []map[string]string {
	var all []map[string]string
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]string
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("stdout line %q is not JSON", l)
		}
		all = append(all, m)
	}
	return all
}

func noSecrets(t *testing.T, err error, out *bytes.Buffer) {
	t.Helper()
	text := out.String()
	if err != nil {
		text += err.Error()
	}
	for _, secret := range []string{cmdToken, cmdSecret, "shcd_", "shce_", "shcp_"} {
		if strings.Contains(text, secret) {
			t.Fatalf("output leaks %q: %s", secret, text)
		}
	}
}

func TestPairCommandHappyPathSavesStateAndPrintsEvents(t *testing.T) {
	p := newPairHarness(t, "pending", "pending", "approved")
	err := p.run(context.Background(), "k-data=write")
	if err != nil {
		t.Fatal(err)
	}
	got := lines(t, p.out)
	if len(got) != 2 || got[0]["event"] != "code" || got[0]["code"] != "ABCD-EFGH" || got[0]["expires_at"] == "" ||
		got[1]["event"] != "paired" || got[1]["device_id"] != "dev1" {
		t.Fatalf("stdout %v", got)
	}
	noSecrets(t, err, p.out)
	state, err := client.Load(p.dir)
	if err != nil {
		t.Fatal(err)
	}
	r := state.Ceiling.Residents
	if state.DeviceID != "dev1" || len(r) != 1 || r[0].ID != "r1" || r[0].Execute ||
		len(r[0].Volumes) != 1 || r[0].Volumes[0].Volume != "k-data" || r[0].Volumes[0].Access != "write" {
		t.Fatalf("state %+v", state)
	}
	if info, _ := os.Stat(filepath.Join(p.dir, "state.json")); info.Mode().Perm() != 0600 {
		t.Fatal("state not private")
	}
}

func TestPairCommandEndsWithExitThreeAndNoState(t *testing.T) {
	for _, end := range []string{"expired", "cancelled"} {
		p := newPairHarness(t, "pending", end)
		err := p.run(context.Background(), "k-data")
		got := lines(t, p.out)
		if exitCode(err) != 3 || len(got) != 2 || got[1]["event"] != end {
			t.Fatalf("%s: err=%v stdout=%v", end, err, got)
		}
		if _, statErr := os.Lstat(filepath.Join(p.dir, "state.json")); !os.IsNotExist(statErr) {
			t.Fatalf("%s left state", end)
		}
		noSecrets(t, err, p.out)
	}
}

func TestPairCommandWrongSecretIsAnErrorWithoutAnEvent(t *testing.T) {
	p := newPairHarness(t, "unknown")
	err := p.run(context.Background(), "k-data")
	if exitCode(err) != 1 || !strings.HasPrefix(err.Error(), "companion: pair failed:") {
		t.Fatalf("err=%v", err)
	}
	if got := lines(t, p.out); len(got) != 1 || got[0]["event"] != "code" {
		t.Fatalf("stdout %v", got)
	}
	if _, statErr := os.Lstat(filepath.Join(p.dir, "state.json")); !os.IsNotExist(statErr) {
		t.Fatal("state written")
	}
	noSecrets(t, err, p.out)
}

func TestPairCommandRefusesBeforeAnyNetworkCall(t *testing.T) {
	for name, specs := range map[string][]string{
		"unknown": {"nope"}, "denied": {"k-sealed"}, "duplicate": {"k-data", "k-data=write"}, "none": nil,
	} {
		p := newPairHarness(t, "approved")
		err := p.run(context.Background(), specs...)
		if exitCode(err) != 1 || p.house.hits != 0 || p.out.Len() != 0 {
			t.Errorf("%s: err=%v hits=%d stdout=%q", name, err, p.house.hits, p.out)
		}
	}
	p := newPairHarness(t, "approved")
	p.deps.mounts = func() ([]volumes.Mount, error) { return nil, errors.New("discovery refused") }
	if err := p.run(context.Background(), "k-data"); exitCode(err) != 1 || p.house.hits != 0 {
		t.Fatalf("discovery failure: %v hits=%d", err, p.house.hits)
	}
}

func TestPairCommandRefusesWhenAlreadyEnrolled(t *testing.T) {
	p := newPairHarness(t, "approved")
	if err := client.Save(p.dir, client.State{URL: p.url, DeviceID: "existing"}); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(p.dir, "state.json"))
	err := p.run(context.Background(), "k-data")
	after, _ := os.ReadFile(filepath.Join(p.dir, "state.json"))
	if exitCode(err) != 1 || p.house.hits != 0 || !bytes.Equal(before, after) {
		t.Fatalf("err=%v hits=%d", err, p.house.hits)
	}
}

func TestPairCommandSignalExitsOneThirtyWithNothingWritten(t *testing.T) {
	p := newPairHarness(t, "pending")
	ctx, cancel := context.WithCancel(context.Background())
	p.deps.sleep = func(ctx context.Context, d time.Duration) error { cancel(); return ctx.Err() }
	err := p.run(ctx, "k-data")
	if exitCode(err) != 130 {
		t.Fatalf("exit %d: %v", exitCode(err), err)
	}
	if got := lines(t, p.out); len(got) != 1 || got[0]["event"] != "code" {
		t.Fatalf("stdout %v", got)
	}
	if _, statErr := os.Lstat(filepath.Join(p.dir, "state.json")); !os.IsNotExist(statErr) {
		t.Fatal("state written")
	}
}
