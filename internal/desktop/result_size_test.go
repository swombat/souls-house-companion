package desktop

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// longPage is an accessibility tree with n links whose names are long, like
// a Wikipedia article: far more than fits in one house result.
func longPage(n int) []map[string]any {
	root := map[string]any{"nodeId": "1", "role": map[string]any{"value": "RootWebArea"}, "name": map[string]any{"value": "Fungus"}, "backendDOMNodeId": 1, "frameId": "F"}
	nodes := []map[string]any{root}
	var kids []string
	for i := 0; i < n; i++ {
		id := strconv.Itoa(1000 + i)
		kids = append(kids, id)
		nodes = append(nodes, map[string]any{"nodeId": id, "parentId": "1", "role": map[string]any{"value": "link"},
			"name": map[string]any{"value": "Mycorrhizal association number " + id + " & <its> fungal partners"}, "backendDOMNodeId": 1000 + i, "frameId": "F"})
	}
	root["childIds"] = kids
	return nodes
}

// A long page used to produce a result the house refuses (over 64 KB), and
// the operation then sat until its deadline. The snapshot now stops at a
// byte budget, says truncated, and the whole result fits.
func TestResidentBrowserSnapshotOfALongPageFitsInOneResult(t *testing.T) {
	fc := newFakeChrome(t, "tab1")
	fc.ax = longPage(3000)
	f := browserFixture(t, fc)
	r := f.do(`{"command":"snapshot","app":"souls.house browser","interactive":true}`)
	if r.Status != "completed" || r.Steps[0].ExitCode != 0 {
		t.Fatalf("snapshot: %+v", r)
	}
	var out struct {
		Elements  []element `json:"elements"`
		Truncated bool      `json:"truncated"`
	}
	if err := json.Unmarshal(r.Steps[0].Output, &out); err != nil {
		t.Fatal(err)
	}
	if !out.Truncated || len(out.Elements) < 100 || len(out.Elements) >= browserMaxElements {
		t.Fatalf("truncated=%v elements=%d", out.Truncated, len(out.Elements))
	}
	data, err := r.Encode()
	if err != nil || len(data) > MaxResultBytes {
		t.Fatalf("encoded %d bytes (limit %d), err %v", len(data), MaxResultBytes, err)
	}
	if strings.Contains(string(data), ReasonResultTooLarge) {
		t.Fatalf("one snapshot should fit without dropping its output: %.200s", data)
	}
	// The page is still the page: the first links, in order.
	if out.Elements[0].Role != "link" || !strings.Contains(out.Elements[0].Name, "1000") {
		t.Fatalf("first element %+v", out.Elements[0])
	}
}

// Several big snapshots in one batch still exceed the limit. The result then
// keeps status and exit codes, drops the outputs, and says why.
func TestResultTooLargeKeepsExitCodesAndSaysWhy(t *testing.T) {
	fc := newFakeChrome(t, "tab1")
	fc.ax = longPage(3000)
	f := browserFixture(t, fc)
	step := `{"command":"snapshot","app":"souls.house browser","interactive":true}`
	r := f.do(`{"command":"batch","steps":[` + step + `,` + step + `,` + step + `]}`)
	if r.Status != "completed" || len(r.Steps) != 3 {
		t.Fatalf("batch: %+v", r.Status)
	}
	data, err := r.Encode()
	if err != nil || len(data) > MaxResultBytes {
		t.Fatalf("encoded %d bytes, err %v", len(data), err)
	}
	var got Result
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "completed" || got.Reason != ReasonResultTooLarge || len(got.Steps) != 3 {
		t.Fatalf("%+v", got)
	}
	for _, s := range got.Steps {
		if s.Output != nil || !s.Truncated || s.ExitCode != 0 || s.Command != "snapshot" {
			t.Fatalf("step %+v", s)
		}
	}

	// A reason already given (here a stop) is kept, not replaced.
	stopped := Result{Status: "stopped", Reason: ReasonStoppedMidCall + ":" + ReasonSwitchOff, Steps: r.Steps}
	data, _ = stopped.Encode()
	if err := json.Unmarshal(data, &got); err != nil || got.Reason != ReasonStoppedMidCall+":"+ReasonSwitchOff || len(data) > MaxResultBytes {
		t.Fatalf("stopped: %d bytes %+v", len(data), got.Reason)
	}

	// A result that fits is encoded unchanged.
	small := Result{Status: "completed", Steps: []StepResult{{Command: "open", App: "souls.house browser", Output: json.RawMessage(`{"ok":true}`)}}}
	data, _ = small.Encode()
	want, _ := json.Marshal(small)
	if string(data) != string(want) {
		t.Fatalf("small changed: %s", data)
	}
}
