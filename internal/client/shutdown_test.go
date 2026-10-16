package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/swombat/souls-house-companion/internal/policy"
)

// The returned channel injects a terminal frame (or closes the socket for nil).
// All authority, HTTP traffic and handlers are synthetic loopback fixtures.
func shutdownFixture(t *testing.T, h Handler, operations ...string) (*Client, State, chan []byte, *atomic.Int32) {
	t.Helper()
	terminal := make(chan []byte, 1)
	delivered := new(atomic.Int32)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v1/companion/session":
			_ = json.NewEncoder(w).Encode(map[string]any{"device_id": "device", "device_credential": "shcd_next",
				"cable_ticket": "synthetic-ticket", "revision": 1, "policy": policy.Policy{}})
		case strings.HasSuffix(r.URL.Path, "/result"):
			delivered.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusCreated)
		case r.URL.Path == "/cable":
			ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"actioncable-v1-json"}})
			if err != nil {
				return
			}
			defer ws.CloseNow()
			if _, _, err := ws.Read(r.Context()); err != nil {
				return
			}
			send := func(req Request) {
				raw, _ := json.Marshal(map[string]any{"message": req})
				_ = ws.Write(r.Context(), websocket.MessageText, raw)
			}
			send(Request{Type: "ready", DeviceID: "device", Epoch: "epoch"})
			for i, operation := range operations {
				send(Request{Type: "request", ID: string(rune('a' + i)), DeviceID: "device", Epoch: "epoch",
					Operation: operation, Deadline: time.Now().Add(20 * time.Second)})
			}
			// CloseRead lets cancellation unblock this fixture even when the
			// test does not inject a terminal frame.
			ctx := ws.CloseRead(r.Context())
			select {
			case raw := <-terminal:
				if raw != nil {
					_ = ws.Write(r.Context(), websocket.MessageText, raw)
					<-ctx.Done()
				}
			case <-ctx.Done():
			}
		default:
			t.Error("unexpected fixture route")
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	c := New(h)
	tlsClient(c, server)
	c.Save = func(State) error { return nil }
	return c, State{URL: server.URL, DeviceID: "device", Credential: "shcd_fixture"}, terminal, delivered
}

func awaitShutdown(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("fixture did not reach " + what)
	}
}

func TestRunOnceDrainsOnlyExecuteOnEveryConnectionExit(t *testing.T) {
	for _, exit := range []string{"cancel", "local-pause", "pause", "revoke", "disconnect", "reject", "malformed", "invalid-request", "socket-close"} {
		t.Run(exit, func(t *testing.T) {
			executeStarted, filesystemStarted := make(chan struct{}), make(chan struct{})
			cleanupStarted, releaseExecute, releaseFilesystem := make(chan struct{}), make(chan struct{}), make(chan struct{})
			defer close(releaseFilesystem)
			// Always release the execute fixture, including on assertion failure.
			defer func() {
				select {
				case <-releaseExecute:
				default:
					close(releaseExecute)
				}
			}()
			c, state, terminal, delivered := shutdownFixture(t, handlerFunc(func(ctx context.Context, r Request, _ policy.Policy) ([]byte, string, error) {
				if r.Operation == "execute" {
					close(executeStarted)
					<-ctx.Done()
					close(cleanupStarted)
					<-releaseExecute // Models independent runner cleanup.
				} else {
					close(filesystemStarted)
					<-releaseFilesystem // Models uninterruptible filesystem I/O.
				}
				return []byte("must not deliver"), "binary", nil
			}), "execute", "read")
			var denied atomic.Bool
			c.BeforeDelivery = func() error {
				if denied.Load() {
					return ErrPaused
				}
				return nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- c.RunOnce(ctx, &state) }()
			awaitShutdown(t, executeStarted, "execute")
			awaitShutdown(t, filesystemStarted, "filesystem")
			switch exit {
			case "cancel":
				cancel()
			case "local-pause":
				denied.Store(true)
			case "pause", "revoke":
				terminal <- []byte(`{"message":{"type":"` + exit + `"}}`)
			case "disconnect":
				terminal <- []byte(`{"type":"disconnect"}`)
			case "reject":
				terminal <- []byte(`{"type":"reject_subscription"}`)
			case "malformed":
				terminal <- []byte(`{`)
			case "invalid-request":
				terminal <- []byte(`{"message":{"type":"request"}}`)
			case "socket-close":
				terminal <- nil
			}
			awaitShutdown(t, cleanupStarted, "independent cleanup")
			select {
			case err := <-done:
				t.Fatalf("connection returned before execute cleanup: %v", err)
			case <-time.After(25 * time.Millisecond):
			}
			close(releaseExecute)
			select {
			case err := <-done:
				if !errors.Is(err, ErrTransport) || errors.Is(err, ErrExecuteShutdown) {
					t.Fatalf("unexpected drained connection result: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("execute cleanup waited for abandoned filesystem worker")
			}
			if delivered.Load() != 0 {
				t.Fatal("result delivered after cancellation")
			}
		})
	}
}

func TestExecuteShutdownTimeoutIsBoundedAndRetainsWorkerLimit(t *testing.T) {
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	defer close(release)
	c, state, _, delivered := shutdownFixture(t, handlerFunc(func(context.Context, Request, policy.Policy) ([]byte, string, error) {
		started <- struct{}{}
		<-release // Even a misbehaving execute handler cannot hang shutdown.
		return nil, "json", nil
	}), "execute", "execute", "execute", "execute")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	const grace = 80 * time.Millisecond
	go func() { done <- c.runOnce(ctx, &state, grace) }()
	for range 4 {
		awaitShutdown(t, started, "execute worker")
	}
	begin := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, ErrTransport) || !errors.Is(err, ErrExecuteShutdown) {
			t.Fatalf("missing typed incomplete shutdown: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not respect a single bounded execute grace")
	}
	if elapsed := time.Since(begin); elapsed < grace || elapsed > 500*time.Millisecond {
		t.Fatalf("shutdown grace applied incorrectly: %v", elapsed)
	}
	// A new connection must not bypass slots held by abandoned execute work.
	ctx, cancelReconnect := context.WithCancel(context.Background())
	defer cancelReconnect()
	go func() { done <- c.runOnce(ctx, &state, grace) }()
	deadline := time.Now().Add(3 * time.Second)
	for delivered.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if delivered.Load() != 4 || len(started) != 0 {
		t.Fatal("reconnect bypassed the bounded abandoned-worker pool")
	}
	cancelReconnect()
	select {
	case err := <-done:
		if errors.Is(err, ErrExecuteShutdown) {
			t.Fatal("new connection waited on an earlier connection's abandoned worker")
		}
	case <-time.After(time.Second):
		t.Fatal("reconnect did not cancel")
	}
}

func TestExecuteWorkerTrackingReleasesCompletedEntries(t *testing.T) {
	var workers executeWorkers
	for range 1000 {
		finish := workers.start()
		finish()
	}
	if len(workers.pending) != 0 || !workers.wait(time.Nanosecond) {
		t.Fatal("completed execute workers retained")
	}
}

// Like serve, the child uses NotifyContext and does not leave its RunOnce call
// until it returns. A real SIGTERM must not terminate independent cleanup.
func TestExecuteShutdownOnServiceSignal(t *testing.T) {
	if dir := os.Getenv("COMPANION_SHUTDOWN_SIGNAL_FIXTURE"); dir != "" {
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
		defer stop()
		c, state, _, _ := shutdownFixture(t, handlerFunc(func(ctx context.Context, _ Request, _ policy.Policy) ([]byte, string, error) {
			if err := os.WriteFile(filepath.Join(dir, "started"), nil, 0600); err != nil {
				t.Error(err)
			}
			<-ctx.Done()
			if err := os.WriteFile(filepath.Join(dir, "cancelled"), nil, 0600); err != nil {
				t.Error(err)
			}
			for {
				if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if err := os.WriteFile(filepath.Join(dir, "cleaned"), nil, 0600); err != nil {
				t.Error(err)
			}
			return nil, "json", nil
		}), "execute")
		if err := c.RunOnce(ctx, &state); errors.Is(err, ErrExecuteShutdown) {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(dir, "cleaned")); err != nil {
			t.Fatal("signal returned before cleanup finished")
		}
		return
	}
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(executable, "-test.run=^TestExecuteShutdownOnServiceSignal$")
	child.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "TMPDIR=" + dir, "COMPANION_SHUTDOWN_SIGNAL_FIXTURE=" + dir}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill() })
	done := make(chan error, 1)
	go func() { done <- child.Wait() }()
	waitFile := func(name string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
				return
			}
			select {
			case err := <-done:
				t.Fatalf("signal fixture exited before %s: %v", name, err)
			default:
			}
			if time.Now().After(deadline) {
				t.Fatal("signal fixture did not reach " + name)
			}
			time.Sleep(time.Millisecond)
		}
	}
	waitFile("started")
	if err := child.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitFile("cancelled")
	select {
	case err := <-done:
		t.Fatalf("process exited before execute cleanup: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("signal fixture failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("signal fixture did not exit after execute cleanup")
	}
	if _, err := os.Stat(filepath.Join(dir, "cleaned")); err != nil {
		t.Fatal("process exited without cleanup receipt")
	}
}
