package service

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/policy"
)

func TestExecuteInputStrictAndBinaryStdin(t *testing.T) {
	input, stdin, err := parseExecuteInput([]byte(`{"argv":["printf","a b",""],"cwd":"/tmp","stdin_base64":"AP8="}`))
	if err != nil || len(input.Argv) != 3 || string(stdin) != "\x00\xff" {
		t.Fatal("valid argv/binary stdin refused")
	}
	for _, raw := range []string{
		`null`, `{}`, `{"argv":[""],"cwd":"/"}`, `{"argv":["x"],"cwd":"relative"}`,
		`{"argv":["x\u0000"],"cwd":"/"}`, `{"argv":["x"],"cwd":"/","shell":true}`,
		`{"argv":["x"],"cwd":"/","stdin_base64":"not-base64"}`,
		`{"argv":["x"],"cwd":"/"} {}`, string([]byte{0xff}),
		`{"argv":["` + strings.Repeat("x", (128<<10)+1) + `"],"cwd":"/"}`,
	} {
		if _, _, err := parseExecuteInput([]byte(raw)); err == nil {
			t.Fatal("malformed execution input accepted")
		}
	}
}

func TestInputAuthorityIntersectsIndependentGrants(t *testing.T) {
	read := policy.Policy{Residents: []policy.Resident{{ID: "actor", Volumes: []policy.Grant{{Volume: "volume", Access: policy.Read}}}}}
	write := policy.Policy{Residents: []policy.Resident{{ID: "actor", Volumes: []policy.Grant{{Volume: "volume", Access: policy.Write}}, Execute: true}}}
	h := &Handler{}
	r := client.Request{ResidentID: "actor", Operation: "write", Arguments: json.RawMessage(`{"volume":"volume"}`)}
	for _, pair := range [][2]policy.Policy{{read, write}, {write, read}, {read, read}} {
		r.Policy = pair[1]
		if h.AuthorizeInput(r, pair[0]) == nil {
			t.Fatal("one authority widened the other")
		}
	}
	r.Policy = write
	if h.AuthorizeInput(r, write) != nil {
		t.Fatal("dual write grant refused")
	}
	r.Operation = "execute"
	if h.AuthorizeInput(r, read) == nil || h.AuthorizeInput(r, write) != nil {
		t.Fatal("execute did not require explicit local authority")
	}
	r.ResidentID = "other"
	if h.AuthorizeInput(r, write) == nil {
		t.Fatal("another resident borrowed authority")
	}
}
