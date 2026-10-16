package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestInputOneShotBoundedAndCredentialScoped(t *testing.T) {
	for _, tc := range []struct {
		name   string
		body   string
		status int
		wantOK bool
	}{
		{"exact", "abc", 200, true},
		{"short", "ab", 200, false},
		{"long", "abcd", 200, false},
		{"gone", "abc", 410, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "GET" || r.URL.Path != "/api/v1/companion/operations/op/input" ||
					r.Header.Get("Authorization") != "Bearer shcd_fixture" ||
					r.Header.Get("X-Companion-Epoch") != "epoch" || r.Header.Get("X-Companion-Revision") != "3" {
					t.Error("input authority envelope mismatch")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			c := New(nil)
			c.HTTP = server.Client()
			state := State{URL: server.URL, Credential: "shcd_fixture"}
			r := Request{ID: "op", Operation: "write", Epoch: "epoch", Revision: 3, Arguments: json.RawMessage(`{"input_size":3}`)}
			data, err := c.input(context.Background(), state, r)
			if (err == nil) != tc.wantOK || (tc.wantOK && string(data) != "abc") || calls.Load() != 1 {
				t.Fatalf("input result mismatch: ok=%v calls=%d", err == nil, calls.Load())
			}
		})
	}
}

func TestInputPauseAndMalformedMetadataNeverFetch(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	c := New(nil)
	c.HTTP = server.Client()
	state := State{URL: server.URL}
	for _, raw := range []string{`{}`, `{"input_size":-1}`, `{"input_size":8388609}`, `{"input_size":1.5}`} {
		if _, err := c.input(context.Background(), state, Request{Operation: "write", Arguments: json.RawMessage(raw)}); err == nil {
			t.Fatal("malformed size allowed")
		}
	}
	c.BeforeDelivery = func() error { return ErrPaused }
	if _, err := c.input(context.Background(), state, Request{Operation: "write", Arguments: json.RawMessage(`{"input_size":0}`)}); err == nil {
		t.Fatal("local pause allowed input")
	}
	if calls.Load() != 0 {
		t.Fatal("refused operation fetched input")
	}
}

func TestExecuteDeadlineDoesNotWidenFileDeadline(t *testing.T) {
	now := time.Now()
	if !validDeadline(Request{Operation: "execute", Deadline: now.Add(300 * time.Second)}, now) ||
		validDeadline(Request{Operation: "write", Deadline: now.Add(300 * time.Second)}, now) ||
		validDeadline(Request{Operation: "execute", Deadline: now.Add(302 * time.Second)}, now) {
		t.Fatal("operation deadline bounds incorrect")
	}
}
