package desktop

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func mustParse(t *testing.T, s string) Request {
	t.Helper()
	r, err := Parse(json.RawMessage(s))
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return r
}

func TestParseBuildsArgvFromTypedFields(t *testing.T) {
	r := mustParse(t, `{"command":"snapshot","app":"Finder","interactive":true,"max_depth":5}`)
	want := []string{"snapshot", "--app=Finder", "--interactive-only", "--max-depth=5"}
	if got := r.Steps[0].Argv(); !reflect.DeepEqual(got, want) {
		t.Fatalf("argv %q, want %q", got, want)
	}
	r = mustParse(t, `{"command":"click","app":"Finder","ref":"@s8f3k2p9:e1"}`)
	if got := r.Steps[0].Argv(); !reflect.DeepEqual(got, []string{"click", "--", "@s8f3k2p9:e1"}) {
		t.Fatalf("click argv %q", got)
	}
	r = mustParse(t, `{"command":"press","app":"Finder","combo":"cmd+shift+n"}`)
	if got := r.Steps[0].Argv(); !reflect.DeepEqual(got, []string{"press", "--app=Finder", "--", "cmd+shift+n"}) {
		t.Fatalf("press argv %q", got)
	}
	r = mustParse(t, `{"command":"find","app":"Finder","name":"-rf --headed"}`)
	for _, a := range r.Steps[0].Argv() {
		if a == "--headed" || a == "-rf" {
			t.Fatalf("value escaped into a flag: %q", r.Steps[0].Argv())
		}
	}
	if got := r.Steps[0].Argv(); got[len(got)-1] != "--limit=50" {
		t.Fatalf("find must be bounded by default: %q", got)
	}
}

func TestTypedTextNeverInArgv(t *testing.T) {
	r := mustParse(t, `{"command":"type","app":"Finder","ref":"@abc:e2","text":"secret words"}`)
	if strings.Contains(strings.Join(r.Steps[0].Argv(), " "), "secret") || string(r.Steps[0].stdin) != "secret words" {
		t.Fatalf("text must go over stdin only: %q", r.Steps[0].Argv())
	}
}

func TestParseRefusals(t *testing.T) {
	for _, s := range []string{
		`{"command":"launch","app":"Terminal"}`,
		`{"command":"snapshot"}`,                                      // app required
		`{"command":"snapshot","app":"Finder","headed":true}`,         // unknown key
		`{"command":"snapshot","app":"Finder","screenshot":"/tmp/x"}`, // output path
		`{"command":"snapshot","app":"Finder","surface":"dock"}`,      // global surface
		`{"command":"snapshot","app":"Finder","max_depth":99}`,
		`{"command":"snapshot","app":"Finder","wait_for":"button:OK","wait_timeout_ms":60000}`,
		`{"command":"click","app":"Finder","ref":"e1"}`,
		`{"command":"click","app":"Finder","ref":"@a:e1","argv":["--force"]}`,
		`{"command":"press","combo":"return"}`, // no app: frontmost escape
		`{"command":"press","app":"Finder","combo":"return --force"}`,
		`{"command":"find","app":"Finder","name":"a\nb"}`,
		`{"command":"find","app":"Finder","limit":0}`,
		`{"command":"type","app":"Finder","ref":"@a:e1","text":""}`,
		`{"command":"batch","steps":[{"command":"batch","steps":[]}]}`, // nested
		`{"command":"batch","steps":[]}`,
		`{"command":"batch","steps":[{"command":"snapshot","app":"Finder"}],"extra":1}`,
		`{"command":"snapshot","app":"Finder"} {"x":1}`,
		`{"command":"snapshot","app":"../../bin/sh"}`,
		`{"command":"snapshot","app":"Finder","timeout_seconds":999}`,
	} {
		if _, err := Parse(json.RawMessage(s)); err == nil {
			t.Errorf("accepted %s", s)
		}
	}
	var steps []string
	for i := 0; i < MaxBatchSteps+1; i++ {
		steps = append(steps, `{"command":"snapshot","app":"Finder"}`)
	}
	if _, err := Parse(json.RawMessage(`{"command":"batch","steps":[` + strings.Join(steps, ",") + `]}`)); err == nil {
		t.Error("accepted oversized batch")
	}
}

func TestFindSelectionsAreExclusive(t *testing.T) {
	r := mustParse(t, `{"command":"find","app":"Finder","name":"x","first":true}`)
	for _, a := range r.Steps[0].Argv() {
		if strings.HasPrefix(a, "--limit") {
			t.Fatalf("limit emitted with a selection: %q", r.Steps[0].Argv())
		}
	}
	for _, s := range []string{
		`{"command":"find","app":"Finder","first":true,"last":true}`,
		`{"command":"find","app":"Finder","count":true,"limit":5}`,
		`{"command":"find","app":"Finder","nth":2,"first":true}`,
		`{"command":"batch","steps":[{"command":"snapshot","app":"Finder"},{"command":"find","app":"Finder","nth":1,"limit":3}]}`,
	} {
		if _, err := Parse(json.RawMessage(s)); err == nil {
			t.Errorf("accepted %s", s)
		}
	}
}
