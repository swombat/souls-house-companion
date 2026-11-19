package client

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/swombat/souls-house-companion/internal/policy"
	"github.com/swombat/souls-house-companion/internal/volumes"
)

const (
	testSecret = "shcp_synthetic-poll-secret"
	testToken  = "shce_synthetic-enrollment-token"
)

// fakeHouse scripts the claim responses and records every request body.
type fakeHouse struct {
	mu       sync.Mutex
	t        *testing.T
	claims   []func(w http.ResponseWriter, r *http.Request) // consumed in order; the last repeats
	created  map[string]any
	enrolled map[string]string
	nClaims  int
	nEnroll  int
	// enrollDrops: how many /enroll replies are lost after the house has
	// committed (the connection is cut instead of answered).
	enrollDrops int
	lifetime    time.Duration
	interval    int
}

func newFakeHouse(t *testing.T, claims ...func(http.ResponseWriter, *http.Request)) (*fakeHouse, *httptest.Server) {
	h := &fakeHouse{t: t, claims: claims, lifetime: 15 * time.Minute, interval: 3}
	s := httptest.NewTLSServer(http.HandlerFunc(h.serve))
	t.Cleanup(s.Close)
	return h, s
}

func (h *fakeHouse) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r.Header.Get("Authorization") != "" {
		h.t.Error("pairing is unauthenticated; no bearer header expected")
	}
	switch {
	case r.URL.Path == "/api/v1/companion/pairings":
		json.NewDecoder(r.Body).Decode(&h.created)
		w.WriteHeader(201)
		json.NewEncoder(w).Encode(map[string]any{"id": "pub1", "code": "ABCD-EFGH", "poll_secret": testSecret,
			"expires_at": time.Now().Add(h.lifetime).UTC().Format(time.RFC3339), "poll_interval": h.interval})
	case r.URL.Path == "/api/v1/companion/pairings/pub1/claim":
		var in map[string]string
		json.NewDecoder(r.Body).Decode(&in)
		if in["poll_secret"] != testSecret {
			w.WriteHeader(404)
			return
		}
		i := h.nClaims
		if i >= len(h.claims) {
			i = len(h.claims) - 1
		}
		h.nClaims++
		h.claims[i](w, r)
	case r.URL.Path == "/api/v1/companion/enroll":
		h.nEnroll++
		json.NewDecoder(r.Body).Decode(&h.enrolled)
		if h.nEnroll <= h.enrollDrops {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"device_id": "dev1", "device_credential": "shcd_synthetic"})
	default:
		w.WriteHeader(404)
	}
}

func pending(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(202)
	w.Write([]byte(`{"status":"pending"}`))
}

func gone(status string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(410)
		w.Write([]byte(`{"status":"` + status + `"}`))
	}
}

func approved(requestID func() string, extra map[string]any, residents ...string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		body := map[string]any{"status": "approved", "enrollment_token": testToken,
			"device_request_id": requestID(), "resident_ids": residents}
		for k, v := range extra {
			body[k] = v
		}
		json.NewEncoder(w).Encode(body)
	}
}

// fakeClock advances only when the code under test sleeps.
type fakeClock struct {
	now    time.Time
	sleeps []time.Duration
}

func (f *fakeClock) Now() time.Time { return f.now }
func (f *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.sleeps = append(f.sleeps, d)
	f.now = f.now.Add(d)
	return nil
}

func pairRequest(s *httptest.Server, clock *fakeClock, onCode func(string, time.Time)) PairRequest {
	return PairRequest{URL: s.URL, Name: "Paulina's MacBook Air", OnCode: onCode, Now: clock.Now, Sleep: clock.Sleep,
		Offers: []PairOffer{{"vol-a", "Main disk", policy.Read}, {"vol-b", "Backup", policy.Write}}}
}

func pairClient(s *httptest.Server) *Client {
	c := New(nil)
	tlsClient(c, s)
	return c
}

func TestPairHappyPathBuildsCeilingFromTheDevicesOwnOffer(t *testing.T) {
	var h *fakeHouse
	id := func() string { s, _ := h.created["device_request_id"].(string); return s }
	h, s := newFakeHouse(t, pending, pending, approved(id, map[string]any{
		"execute": true, "volumes": []any{map[string]string{"volume": "vol-evil", "access": "write"}},
		"residents": []any{map[string]any{"id": "rX", "execute": true}},
	}, "r1", "r2"))
	clock := &fakeClock{now: time.Now()}
	var codes []string
	state, err := pairClient(s).Pair(context.Background(), pairRequest(s, clock, func(c string, _ time.Time) { codes = append(codes, c) }))
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 1 || codes[0] != "ABCD-EFGH" {
		t.Fatalf("code shown %v", codes)
	}
	if h.created["name"] != "Paulina's MacBook Air" || !regexp.MustCompile(`\Apair-[0-9a-f]{24}\z`).MatchString(id()) {
		t.Fatalf("create body %v", h.created)
	}
	vols, _ := json.Marshal(h.created["volumes"])
	if string(vols) != `[{"access":"read","label":"Main disk","volume":"vol-a"},{"access":"write","label":"Backup","volume":"vol-b"}]` {
		t.Fatalf("offer sent %s", vols)
	}
	if h.nClaims != 3 || h.enrolled["enrollment_token"] != testToken || h.enrolled["device_request_id"] != id() {
		t.Fatalf("claims=%d enroll=%v", h.nClaims, h.enrolled)
	}
	want := policy.Policy{Residents: []policy.Resident{
		{ID: "r1", Volumes: []policy.Grant{{Volume: "vol-a", Access: policy.Read}, {Volume: "vol-b", Access: policy.Write}}},
		{ID: "r2", Volumes: []policy.Grant{{Volume: "vol-a", Access: policy.Read}, {Volume: "vol-b", Access: policy.Write}}},
	}}
	got, _ := json.Marshal(state.Ceiling)
	exp, _ := json.Marshal(want)
	if string(got) != string(exp) {
		t.Fatalf("ceiling %s, want %s", got, exp)
	}
	if state.DeviceID != "dev1" || state.URL != s.URL || state.Credential != "shcd_synthetic" {
		t.Fatalf("state %+v", state)
	}
	for _, d := range clock.sleeps {
		if d != 3*time.Second {
			t.Fatalf("poll slept %v, want the house's 3s", d)
		}
	}
}

func TestPairPollIntervalIsClamped(t *testing.T) {
	for interval, want := range map[int]time.Duration{0: 2 * time.Second, -5: 2 * time.Second, 1: 2 * time.Second, 5: 5 * time.Second, 9999: 30 * time.Second} {
		var h *fakeHouse
		id := func() string { s, _ := h.created["device_request_id"].(string); return s }
		h, s := newFakeHouse(t, pending, approved(id, nil, "r1"))
		h.interval = interval
		clock := &fakeClock{now: time.Now()}
		if _, err := pairClient(s).Pair(context.Background(), pairRequest(s, clock, nil)); err != nil {
			t.Fatal(err)
		}
		if clock.sleeps[0] != want {
			t.Errorf("interval %d slept %v, want %v", interval, clock.sleeps[0], want)
		}
	}
}

func TestPairEndsWithoutEnrollingOrSavingAnything(t *testing.T) {
	for name, tc := range map[string]struct {
		claim func(http.ResponseWriter, *http.Request)
		want  error
	}{
		"expired":   {gone("expired"), ErrPairExpired},
		"cancelled": {gone("cancelled"), ErrPairCancelled},
		"unknown":   {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }, ErrPairNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			h, s := newFakeHouse(t, pending, tc.claim)
			clock := &fakeClock{now: time.Now()}
			state, err := pairClient(s).Pair(context.Background(), pairRequest(s, clock, nil))
			if !errors.Is(err, tc.want) || h.nEnroll != 0 || state.Credential != "" {
				t.Fatalf("err=%v enrolls=%d", err, h.nEnroll)
			}
		})
	}
}

func TestPairWrongSecretIsUnknownAndStops(t *testing.T) {
	// The fake answers 404 to any secret but its own; prove a different one stops us.
	h, s := newFakeHouse(t, pending)
	h.claims = []func(http.ResponseWriter, *http.Request){func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }}
	clock := &fakeClock{now: time.Now()}
	if _, err := pairClient(s).Pair(context.Background(), pairRequest(s, clock, nil)); !errors.Is(err, ErrPairNotFound) || h.nClaims != 1 {
		t.Fatalf("err=%v claims=%d", err, h.nClaims)
	}
}

func TestPairExpiresLocallyAfterLifetimePlusGrace(t *testing.T) {
	h, s := newFakeHouse(t, pending)
	h.lifetime = 60 * time.Second
	clock := &fakeClock{now: time.Now()}
	_, err := pairClient(s).Pair(context.Background(), pairRequest(s, clock, nil))
	if !errors.Is(err, ErrPairExpired) {
		t.Fatal(err)
	}
	var total time.Duration
	for _, d := range clock.sleeps {
		total += d
	}
	if total < 90*time.Second || total > 96*time.Second { // 60s lifetime + 30s grace, one interval of slack
		t.Fatalf("waited %v", total)
	}
}

func TestPairRetriesNetworkErrorsAndServerErrorsWhileLive(t *testing.T) {
	var h *fakeHouse
	id := func() string { s, _ := h.created["device_request_id"].(string); return s }
	drop := func(w http.ResponseWriter, _ *http.Request) {
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close()
	}
	boom := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }
	limited := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(429) }
	h, s := newFakeHouse(t, drop, drop, boom, limited, approved(id, nil, "r1"))
	clock := &fakeClock{now: time.Now()}
	state, err := pairClient(s).Pair(context.Background(), pairRequest(s, clock, nil))
	if err != nil || state.DeviceID != "dev1" {
		t.Fatal(err)
	}
	if len(clock.sleeps) != 5 || clock.sleeps[1] <= clock.sleeps[0] {
		t.Fatalf("no backoff: %v", clock.sleeps)
	}
	for _, d := range clock.sleeps {
		if d > 30*time.Second {
			t.Fatalf("backoff %v exceeds cap", d)
		}
	}
}

func TestPairCancellationWhileWaitingLeavesNothing(t *testing.T) {
	h, s := newFakeHouse(t, pending)
	ctx, cancel := context.WithCancel(context.Background())
	clock := &fakeClock{now: time.Now()}
	req := pairRequest(s, clock, nil)
	req.Sleep = func(ctx context.Context, d time.Duration) error {
		cancel() // the signal arrives mid-wait
		return clock.Sleep(ctx, d)
	}
	state, err := pairClient(s).Pair(ctx, req)
	if !errors.Is(err, context.Canceled) || h.nEnroll != 0 || state.Credential != "" {
		t.Fatalf("err=%v enrolls=%d", err, h.nEnroll)
	}
}

func TestPairRefusesAnApprovalThatDoesNotMatchOurRequest(t *testing.T) {
	for name, body := range map[string]string{
		"other request": `{"status":"approved","enrollment_token":"` + testToken + `","device_request_id":"pair-other","resident_ids":["r1"]}`,
		"odd token":     `{"status":"approved","enrollment_token":"nope","device_request_id":"%ID%","resident_ids":["r1"]}`,
		"dup resident":  `{"status":"approved","enrollment_token":"` + testToken + `","device_request_id":"%ID%","resident_ids":["r1","r1"]}`,
		"bad status":    `{"status":"pending","enrollment_token":"` + testToken + `","device_request_id":"%ID%","resident_ids":["r1"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var h *fakeHouse
			h, s := newFakeHouse(t, func(w http.ResponseWriter, _ *http.Request) {
				id, _ := h.created["device_request_id"].(string)
				w.Write([]byte(strings.ReplaceAll(body, "%ID%", id)))
			})
			clock := &fakeClock{now: time.Now()}
			if _, err := pairClient(s).Pair(context.Background(), pairRequest(s, clock, nil)); err == nil || h.nEnroll != 0 {
				t.Fatalf("accepted: err=%v enrolls=%d", err, h.nEnroll)
			}
		})
	}
}

func TestPairCreateFailuresAreClassifiedWithoutRemoteText(t *testing.T) {
	for _, status := range []int{422, 429, 500} {
		s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			w.Write([]byte(`{"error":"synthetic-remote-text"}`))
		}))
		clock := &fakeClock{now: time.Now()}
		_, err := pairClient(s).Pair(context.Background(), pairRequest(s, clock, nil))
		s.Close()
		if err == nil || strings.Contains(err.Error(), "synthetic-remote-text") {
			t.Fatalf("status %d: %v", status, err)
		}
	}
}

func TestPairRejectsBadRequestsBeforeAnyNetworkCall(t *testing.T) {
	calls := 0
	s := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls++ }))
	defer s.Close()
	c := pairClient(s)
	good := pairRequest(s, &fakeClock{now: time.Now()}, nil)
	for name, mutate := range map[string]func(*PairRequest){
		"http url":     func(r *PairRequest) { r.URL = "http://example.invalid" },
		"empty name":   func(r *PairRequest) { r.Name = " " },
		"control name": func(r *PairRequest) { r.Name = "bad\nname" },
		"long name":    func(r *PairRequest) { r.Name = strings.Repeat("x", 101) },
		"no offers":    func(r *PairRequest) { r.Offers = nil },
		"too many":     func(r *PairRequest) { r.Offers = make([]PairOffer, 17) },
	} {
		req := good
		mutate(&req)
		if _, err := c.Pair(context.Background(), req); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if calls != 0 {
		t.Fatalf("%d network calls", calls)
	}
}

func TestPairOffers(t *testing.T) {
	mounts := []volumes.Mount{
		{Key: "k-data", Mountpoint: "/System/Volumes/Data"},
		{Key: "k-root", Mountpoint: "/"},
		{Key: "k-backup", Mountpoint: "/Volumes/Backup"},
		{Key: "k-sealed", Mountpoint: "/System", Denied: "sealed"},
		{Key: "", Mountpoint: "/Volumes/NoKey", Denied: "x"},
	}
	offers, err := PairOffers(mounts, []string{"k-data", "k-backup=write", "k-root=read"})
	if err != nil {
		t.Fatal(err)
	}
	want := []PairOffer{{"k-data", "Main disk", policy.Read}, {"k-backup", "Backup", policy.Write}, {"k-root", "Main disk", policy.Read}}
	for i := range want {
		if offers[i] != want[i] {
			t.Errorf("offer %d = %+v, want %+v", i, offers[i], want[i])
		}
	}
	for name, specs := range map[string][]string{
		"unknown": {"nope"}, "denied": {"k-sealed"}, "denied write": {"k-sealed=write"}, "empty key": {""}, "empty": {},
		"duplicate": {"k-data", "k-data=write"}, "bare write": {"=write"}, "unknown access": {"k-data=admin"},
	} {
		if _, err := PairOffers(mounts, specs); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestPairLabels(t *testing.T) {
	for in, want := range map[string]string{
		"/": "Main disk", "/System/Volumes/Data": "Main disk", "/Volumes/Backup": "Backup",
		"/Volumes/Time Machine/": "Time Machine", "/mnt/data\n": "data",
	} {
		if got := pairLabel(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	if got := pairLabel("/Volumes/" + strings.Repeat("é", 80)); len(got) > 100 || !strings.HasPrefix(got, "é") {
		t.Errorf("long label %d bytes", len(got))
	}
}

func TestPairRecoversALostEnrollReplyWithTheSameTokenAndRequest(t *testing.T) {
	var h *fakeHouse
	id := func() string { s, _ := h.created["device_request_id"].(string); return s }
	h, s := newFakeHouse(t, approved(id, nil, "r1"))
	h.enrollDrops = 1
	clock := &fakeClock{now: time.Now()}
	state, err := pairClient(s).Pair(context.Background(), pairRequest(s, clock, nil))
	if err != nil {
		t.Fatal(err)
	}
	if h.nEnroll != 2 || h.enrolled["enrollment_token"] != testToken || h.enrolled["device_request_id"] != id() {
		t.Fatalf("enroll attempts %d, last %v", h.nEnroll, h.enrolled)
	}
	if state.DeviceID != "dev1" {
		t.Fatalf("state %+v", state)
	}
}

func TestPairThatNeverHearsBackFromEnrollIsUncertainNotNothing(t *testing.T) {
	var h *fakeHouse
	id := func() string { s, _ := h.created["device_request_id"].(string); return s }
	h, s := newFakeHouse(t, approved(id, nil, "r1"))
	h.enrollDrops = 100
	clock := &fakeClock{now: time.Now()}
	_, err := pairClient(s).Pair(context.Background(), pairRequest(s, clock, nil))
	if !errors.Is(err, ErrPairUncertain) {
		t.Fatalf("got %v, want ErrPairUncertain", err)
	}
	if h.nEnroll != pairEnrollAttempts {
		t.Fatalf("enroll attempts %d", h.nEnroll)
	}
}
