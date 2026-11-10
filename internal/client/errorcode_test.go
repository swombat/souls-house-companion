package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/swombat/souls-house-companion/internal/policy"
)

type classifyingHandler struct {
	code func(error) string
}

func (classifyingHandler) Handle(context.Context, Request, policy.Policy) ([]byte, string, error) {
	return nil, "json", nil
}

func (h classifyingHandler) ErrorCode(err error) string { return h.code(err) }

// The header that reaches the house is only ever an allowlisted word, whatever
// the handler's classifier says, and a failed result never carries a body.
func TestResultSendsOnlyAllowlistedErrorCodes(t *testing.T) {
	type seen struct {
		header string
		body   int
	}
	got := make(chan seen, 1)
	// A header the transport refuses to send must fail the test, not hang it.
	receive := func() seen {
		select {
		case s := <-got:
			return s
		case <-time.After(5 * time.Second):
			return seen{header: "<no request reached the house>"}
		}
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- seen{r.Header.Get("X-Companion-Error"), len(body)}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	state := State{URL: server.URL, DeviceID: "device", Credential: "shcd_test"}
	failure := errors.New("Users/someone/Library/secret: no such file or directory")
	for answer, want := range map[string]string{
		"protected": "protected", "denied": "denied", "symlink": "symlink", "not_found": "not_found",
		"": "failed", "PROTECTED": "failed", "unavailable": "failed", "ok": "failed",
		"Users/someone/Library/secret": "failed", failure.Error(): "failed", "protected\r\nX-Evil: 1": "failed",
	} {
		answer := answer
		c := New(classifyingHandler{code: func(error) string { return answer }})
		tlsClient(c, server)
		c.result(context.Background(), state, Request{ID: "1", Epoch: "epoch", Revision: 1},
			[]byte("partial payload"), "json", failure)
		s := receive()
		if s.header != want || s.body != 0 {
			t.Errorf("classifier %q: header %q body %d, want %q and no body", answer, s.header, s.body, want)
		}
	}

	// Capacity refusals stay "unavailable" without asking the classifier, and a
	// handler with no classifier still sends only "failed".
	c := New(classifyingHandler{code: func(error) string { return "protected" }})
	tlsClient(c, server)
	c.result(context.Background(), state, Request{ID: "1", Epoch: "epoch"}, nil, "json", errUnavailable)
	if s := receive(); s.header != "unavailable" {
		t.Errorf("capacity: header %q", s.header)
	}
	plain := New(handlerFunc(func(context.Context, Request, policy.Policy) ([]byte, string, error) { return nil, "json", nil }))
	tlsClient(plain, server)
	plain.result(context.Background(), state, Request{ID: "1", Epoch: "epoch"}, nil, "json", failure)
	if s := receive(); s.header != "failed" {
		t.Errorf("no classifier: header %q", s.header)
	}
}
