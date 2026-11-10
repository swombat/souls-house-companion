package desktop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeChrome speaks just enough DevTools protocol for browse, and records
// what it was asked to do.
type fakeChrome struct {
	mu        sync.Mutex
	srv       *httptest.Server
	pages     []string
	navigated []string
	created   []map[string]any
	launches  int
	sessions  int
	// Page state for the read and act verbs.
	loader    string
	actions   []string
	onResolve func()
}

func newFakeChrome(t *testing.T, pages ...string) *fakeChrome {
	fc := &fakeChrome{pages: pages, loader: "L1"}
	fc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/devtools/browser/") {
			http.NotFound(w, r)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		for {
			_, data, err := c.Read(ctx)
			if err != nil {
				return
			}
			var m struct {
				ID        int            `json:"id"`
				Method    string         `json:"method"`
				Params    map[string]any `json:"params"`
				SessionID string         `json:"sessionId"`
			}
			_ = json.Unmarshal(data, &m)
			// An event first: browse must skip it.
			_ = c.Write(ctx, websocket.MessageText, []byte(`{"method":"Target.targetInfoChanged","params":{}}`))
			result := map[string]any{}
			fc.mu.Lock()
			switch m.Method {
			case "Target.getTargets":
				infos := []map[string]any{{"targetId": "svc", "type": "service_worker"}}
				for _, p := range fc.pages {
					infos = append(infos, map[string]any{"targetId": p, "type": "page"})
				}
				result["targetInfos"] = infos
			case "Target.attachToTarget":
				fc.sessions++
				result["sessionId"] = "s-" + m.Params["targetId"].(string)
			case "Page.navigate":
				if m.SessionID == "" {
					fc.mu.Unlock()
					_ = c.Write(ctx, websocket.MessageText, []byte(`{"id":`+itoa(m.ID)+`,"error":{"message":"no session"}}`))
					continue
				}
				fc.navigated = append(fc.navigated, m.SessionID+" "+m.Params["url"].(string))
				result["frameId"] = "f"
			case "Target.createTarget":
				fc.created = append(fc.created, m.Params)
				result["targetId"] = "new"
			case "Page.getFrameTree":
				result["frameTree"] = map[string]any{"frame": map[string]any{"id": "F", "loaderId": fc.loader, "url": "https://example.org/page"}}
			case "Runtime.evaluate":
				result["result"] = map[string]any{"type": "string", "value": "Example page"}
			case "Accessibility.getFullAXTree":
				result["nodes"] = fakeAXTree
			case "DOM.resolveNode":
				hook := fc.onResolve
				id := m.Params["backendNodeId"].(float64)
				if hook != nil {
					fc.mu.Unlock()
					hook()
					fc.mu.Lock()
				}
				if id >= 100 {
					fc.mu.Unlock()
					_ = c.Write(ctx, websocket.MessageText, []byte(`{"id":`+itoa(m.ID)+`,"error":{"message":"No node with given id found"}}`))
					continue
				}
				result["object"] = map[string]any{"objectId": "obj-" + itoa(int(id))}
			case "Runtime.callFunctionOn":
				fc.actions = append(fc.actions, "call "+m.Params["objectId"].(string))
				result["result"] = map[string]any{"type": "boolean", "value": true}
			case "Input.insertText":
				fc.actions = append(fc.actions, "insert "+m.Params["text"].(string))
			case "Input.dispatchKeyEvent":
				text, _ := m.Params["text"].(string)
				fc.actions = append(fc.actions, "key "+m.Params["type"].(string)+" "+m.Params["key"].(string)+" "+itoa(len(text)))
			}
			fc.mu.Unlock()
			reply, _ := json.Marshal(map[string]any{"id": m.ID, "result": result})
			_ = c.Write(ctx, websocket.MessageText, reply)
		}
	}))
	t.Cleanup(fc.srv.Close)
	return fc
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func (fc *fakeChrome) port() string { return fc.srv.URL[strings.LastIndex(fc.srv.URL, ":")+1:] }

// launch plays Chrome starting up: it publishes the endpoint in the profile.
func (fc *fakeChrome) launch(_ context.Context, profile string) error {
	fc.mu.Lock()
	fc.launches++
	fc.mu.Unlock()
	return os.WriteFile(filepath.Join(profile, activePortFile), []byte(fc.port()+"\n/devtools/browser/abc-123\n"), 0644)
}

func browserFixture(t *testing.T, fc *fakeChrome) *fixture {
	f := newFixture(t)
	f.sup.BrowserLaunch = fc.launch
	if err := SaveScope(f.dir, Scope{Version: 1, Residents: []ResidentScope{{ID: "mira", Apps: []string{"Finder", ResidentBrowser}}}}); err != nil {
		t.Fatal(err)
	}
	f.on(15 * time.Minute)
	return f
}

const browseURL = `{"command":"open","app":"souls.house browser","url":"https://en.wikipedia.org/wiki/Physarum_polycephalum"}`

func TestResidentBrowserNavigatesItsOwnTabAndLaunchesOnce(t *testing.T) {
	fc := newFakeChrome(t, "tab1", "tab2")
	f := browserFixture(t, fc)
	for i := 0; i < 2; i++ {
		r := f.do(browseURL)
		if r.Status != "completed" || len(r.Steps) != 1 || r.Steps[0].ExitCode != 0 {
			t.Fatalf("browse %d: %+v", i, r)
		}
		if strings.Contains(string(r.Steps[0].Output), "wikipedia") {
			t.Fatalf("URL echoed in result: %s", r.Steps[0].Output)
		}
	}
	if fc.launches != 1 {
		t.Fatalf("launches = %d, want 1 (second call must reuse the running browser)", fc.launches)
	}
	want := "s-tab1 https://en.wikipedia.org/wiki/Physarum_polycephalum"
	if len(fc.navigated) != 2 || fc.navigated[0] != want {
		t.Fatalf("navigated = %q", fc.navigated)
	}
	if len(fc.created) != 0 {
		t.Fatalf("created a tab although one existed: %v", fc.created)
	}
	// No agent-desktop or opener process ran for it.
	if _, err := os.Stat(filepath.Join(f.root(), "tmp", "open-argv")); !os.IsNotExist(err) {
		t.Fatalf("opener ran for a browser step: %v", err)
	}
	audit, _ := os.ReadFile(filepath.Join(f.dir, AuditFile))
	if strings.Contains(string(audit), "wikipedia") || !strings.Contains(string(audit), ResidentBrowser) {
		t.Fatalf("audit: %s", audit)
	}
	if info, err := os.Stat(filepath.Join(f.dir, BrowserDir)); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("profile dir: %v %v", info, err)
	}
}

func TestResidentBrowserWithNoTabCreatesABackgroundOne(t *testing.T) {
	fc := newFakeChrome(t)
	f := browserFixture(t, fc)
	if r := f.do(browseURL); r.Status != "completed" || r.Steps[0].ExitCode != 0 {
		t.Fatalf("%+v", r)
	}
	if len(fc.created) != 1 || fc.created[0]["background"] != true || fc.created[0]["url"] != "https://en.wikipedia.org/wiki/Physarum_polycephalum" {
		t.Fatalf("created = %v", fc.created)
	}
}

func TestResidentBrowserRelaunchesOverAStaleEndpoint(t *testing.T) {
	fc := newFakeChrome(t, "tab1")
	f := browserFixture(t, fc)
	dead := httptest.NewServer(http.NotFoundHandler())
	deadPort := dead.URL[strings.LastIndex(dead.URL, ":")+1:]
	dead.Close()
	profile := filepath.Join(f.dir, BrowserDir)
	if err := os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, activePortFile), []byte(deadPort+"\n/devtools/browser/old\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if r := f.do(browseURL); r.Status != "completed" || r.Steps[0].ExitCode != 0 {
		t.Fatalf("%+v", r)
	}
	if fc.launches != 1 || len(fc.navigated) != 1 {
		t.Fatalf("launches=%d navigated=%v", fc.launches, fc.navigated)
	}
}

func TestResidentBrowserNeedsItsOwnScopeEntry(t *testing.T) {
	fc := newFakeChrome(t, "tab1")
	f := browserFixture(t, fc)
	if err := SaveScope(f.dir, Scope{Version: 1, Residents: []ResidentScope{{ID: "mira", Apps: []string{"Finder", "Google Chrome"}}}}); err != nil {
		t.Fatal(err)
	}
	if r := f.do(browseURL); r.Reason != ReasonAppNotAllowed {
		t.Fatalf("Chrome in scope implied the resident browser: %+v", r)
	}
	if fc.launches != 0 {
		t.Fatal("launched without scope")
	}
}

func TestResidentBrowserLaunchFailureIsReported(t *testing.T) {
	fc := newFakeChrome(t, "tab1")
	f := browserFixture(t, fc)
	f.sup.BrowserLaunch = func(context.Context, string) error { return os.ErrNotExist }
	r := f.do(browseURL)
	if r.Status != "completed" || len(r.Steps) != 1 || r.Steps[0].ExitCode == 0 || !strings.Contains(string(r.Steps[0].Output), ReasonBrowserUnavailable) {
		t.Fatalf("%+v", r)
	}
}

func TestResidentBrowserRefusesAnEndpointOutsideAPrivateProfile(t *testing.T) {
	fc := newFakeChrome(t, "tab1")
	f := browserFixture(t, fc)
	profile := filepath.Join(f.dir, BrowserDir)
	if err := os.MkdirAll(profile, 0700); err != nil {
		t.Fatal(err)
	}
	if err := fc.launch(context.Background(), profile); err != nil {
		t.Fatal(err)
	}
	if _, err := browserEndpoint(profile); err != nil {
		t.Fatalf("private profile: %v", err)
	}
	if err := os.Chmod(profile, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := browserEndpoint(profile); err == nil {
		t.Fatal("endpoint trusted in a group-readable profile")
	}
	for _, bad := range []string{"0\n/devtools/browser/x\n", "70000\n/devtools/browser/x\n", "9222\n/devtools/page/x\n", "9222\n", "9222\n/devtools/browser/x?y\n"} {
		_ = os.Chmod(profile, 0700)
		_ = os.WriteFile(filepath.Join(profile, activePortFile), []byte(bad), 0644)
		if _, err := browserEndpoint(profile); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestResidentBrowserGrammar(t *testing.T) {
	for _, args := range []string{
		`{"command":"find","app":"souls.house browser","name":"x"}`,
		`{"command":"get","app":"souls.house browser","ref":"@b0123456789ab:e1"}`,
		`{"command":"scroll","app":"souls.house browser","ref":"@b0123456789ab:e1"}`,
		`{"command":"snapshot","app":"souls.house browser","max_depth":3}`,
		`{"command":"snapshot","app":"souls.house browser","surface":"window"}`,
		`{"command":"click","app":"souls.house browser","ref":"@a:e1"}`,
		`{"command":"click","app":"souls.house browser","ref":"@b1:e0"}`,
		`{"command":"click","app":"souls.house browser"}`,
		`{"command":"type","app":"souls.house browser","ref":"@b1:e1","text":""}`,
		`{"command":"press","app":"souls.house browser","combo":"cmd+q"}`,
		`{"command":"press","app":"souls.house browser","combo":"cmd+l"}`,
		`{"command":"press","app":"souls.house browser","combo":"f5"}`,
		`{"command":"open","app":"souls.house browser","url":"file:///etc/passwd"}`,
		`{"command":"open","app":"souls.house browser","url":"javascript:alert(1)"}`,
	} {
		if _, err := Parse(json.RawMessage(args)); err == nil {
			t.Fatalf("accepted %s", args)
		}
	}
	for _, args := range []string{
		browseURL,
		`{"command":"snapshot","app":"souls.house browser"}`,
		`{"command":"snapshot","app":"souls.house browser","interactive":true}`,
		`{"command":"click","app":"souls.house browser","ref":"@b0123456789ab:e42"}`,
		`{"command":"type","app":"souls.house browser","ref":"@b0123456789ab:e42","text":"slime mould"}`,
		`{"command":"press","app":"souls.house browser","combo":"return"}`,
	} {
		r := mustParse(t, args)
		if !r.Steps[0].browser || r.Steps[0].opener || len(r.Steps[0].argv) != 0 {
			t.Fatalf("%s: %+v", args, r.Steps[0])
		}
	}
}

// Revocation while the browser is starting must stop the step before
// anything is loaded: switch-off, and switch-off-then-on (a new on-period).
func TestResidentBrowserStopsWhenControlIsRevokedDuringStart(t *testing.T) {
	cases := map[string]func(f *fixture) error{
		"switched off": func(f *fixture) error { return TurnOff(f.dir) },
		"new on-period": func(f *fixture) error {
			if err := TurnOff(f.dir); err != nil {
				return err
			}
			f.clock.Add(int64(time.Second))
			_, err := TurnOn(f.dir, f.sup.Now(), 15*time.Minute, false)
			return err
		},
		"scope edited": func(f *fixture) error {
			return SaveScope(f.dir, Scope{Version: 1, Residents: []ResidentScope{{ID: "mira", Apps: []string{"Finder"}}}})
		},
	}
	for name, revoke := range cases {
		for _, pages := range [][]string{{"tab1"}, nil} {
			t.Run(name, func(t *testing.T) {
				fc := newFakeChrome(t, pages...)
				f := browserFixture(t, fc)
				f.sup.BrowserLaunch = func(ctx context.Context, profile string) error {
					if err := revoke(f); err != nil {
						t.Fatal(err)
					}
					return fc.launch(ctx, profile)
				}
				r := f.do(browseURL)
				if r.Status != "stopped" || !strings.HasPrefix(r.Reason, ReasonStoppedMidCall+":") {
					t.Fatalf("%+v", r)
				}
				if len(fc.navigated) != 0 || len(fc.created) != 0 {
					t.Fatalf("loaded after revocation: navigated=%v created=%v", fc.navigated, fc.created)
				}
				audit, _ := os.ReadFile(filepath.Join(f.dir, AuditFile))
				if !strings.Contains(string(audit), ReasonStoppedMidCall) {
					t.Fatalf("audit: %s", audit)
				}
			})
		}
	}
}

// fakeAXTree: a heading, a paragraph whose text node repeats it, a link, an
// unnamed container, an ignored node and a search box.
var fakeAXTree = []map[string]any{
	{"nodeId": "1", "role": map[string]any{"value": "RootWebArea"}, "name": map[string]any{"value": "Example page"}, "childIds": []string{"2", "3", "5", "6", "7"}, "backendDOMNodeId": 1, "frameId": "F"},
	{"nodeId": "2", "parentId": "1", "role": map[string]any{"value": "heading"}, "name": map[string]any{"value": "Physarum"}, "backendDOMNodeId": 10, "frameId": "F"},
	{"nodeId": "3", "parentId": "1", "role": map[string]any{"value": "paragraph"}, "name": map[string]any{"value": ""}, "childIds": []string{"4"}, "backendDOMNodeId": 11, "frameId": "F"},
	{"nodeId": "4", "parentId": "3", "role": map[string]any{"value": "StaticText"}, "name": map[string]any{"value": "A slime mould that solves mazes."}, "backendDOMNodeId": 12, "frameId": "F"},
	{"nodeId": "5", "parentId": "1", "role": map[string]any{"value": "link"}, "name": map[string]any{"value": "Fairy ring"}, "childIds": []string{"8"}, "backendDOMNodeId": 13, "frameId": "F"},
	{"nodeId": "8", "parentId": "5", "role": map[string]any{"value": "StaticText"}, "name": map[string]any{"value": "Fairy ring"}, "backendDOMNodeId": 17, "frameId": "F"},
	{"nodeId": "6", "parentId": "1", "ignored": true, "role": map[string]any{"value": "generic"}, "name": map[string]any{"value": "hidden"}, "backendDOMNodeId": 14, "frameId": "F"},
	{"nodeId": "7", "parentId": "1", "role": map[string]any{"value": "searchbox"}, "name": map[string]any{"value": "Search"}, "value": map[string]any{"value": ""}, "backendDOMNodeId": 15, "frameId": "F"},
}

type snapshotOut struct {
	URL      string    `json:"url"`
	Title    string    `json:"title"`
	Elements []element `json:"elements"`
}

func snapshot(t *testing.T, f *fixture, interactive bool) snapshotOut {
	t.Helper()
	args := `{"command":"snapshot","app":"souls.house browser"}`
	if interactive {
		args = `{"command":"snapshot","app":"souls.house browser","interactive":true}`
	}
	r := f.do(args)
	if r.Status != "completed" || len(r.Steps) != 1 || r.Steps[0].ExitCode != 0 {
		t.Fatalf("snapshot: %+v", r)
	}
	var out snapshotOut
	if err := json.Unmarshal(r.Steps[0].Output, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func refFor(t *testing.T, out snapshotOut, name string) string {
	t.Helper()
	for _, e := range out.Elements {
		if e.Name == name && e.Ref != "" {
			return e.Ref
		}
	}
	t.Fatalf("no ref for %q in %+v", name, out.Elements)
	return ""
}

func TestResidentBrowserSnapshotReadsThePage(t *testing.T) {
	fc := newFakeChrome(t, "tab1")
	f := browserFixture(t, fc)
	out := snapshot(t, f, false)
	if out.URL != "https://example.org/page" || out.Title != "Example page" {
		t.Fatalf("%+v", out)
	}
	var got []string
	for _, e := range out.Elements {
		got = append(got, e.Role+":"+e.Name+":"+itoa(e.Depth))
		if e.Ref != "" && !strings.HasPrefix(e.Ref, "@b") {
			t.Fatalf("ref %q", e.Ref)
		}
	}
	want := []string{"RootWebArea:Example page:0", "heading:Physarum:1", "StaticText:A slime mould that solves mazes.:1", "link:Fairy ring:1", "searchbox:Search:1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("elements\n got %q\nwant %q", got, want)
	}
	inter := snapshot(t, f, true)
	if len(inter.Elements) != 2 || inter.Elements[0].Role != "link" || inter.Elements[1].Role != "searchbox" {
		t.Fatalf("interactive: %+v", inter.Elements)
	}
	if len(fc.actions) != 0 {
		t.Fatalf("snapshot acted: %v", fc.actions)
	}
}

func TestResidentBrowserClickTypeAndPress(t *testing.T) {
	fc := newFakeChrome(t, "tab1")
	f := browserFixture(t, fc)
	out := snapshot(t, f, true)
	link, box := refFor(t, out, "Fairy ring"), refFor(t, out, "Search")
	if r := f.do(`{"command":"click","app":"souls.house browser","ref":"` + link + `"}`); r.Status != "completed" || r.Steps[0].ExitCode != 0 {
		t.Fatalf("click: %+v", r)
	}
	r := f.do(`{"command":"type","app":"souls.house browser","ref":"` + box + `","text":"secret words"}`)
	if r.Status != "completed" || r.Steps[0].ExitCode != 0 || strings.Contains(string(r.Steps[0].Output), "secret") {
		t.Fatalf("type: %+v", r)
	}
	if r := f.do(`{"command":"press","app":"souls.house browser","combo":"return"}`); r.Status != "completed" || r.Steps[0].ExitCode != 0 {
		t.Fatalf("press: %+v", r)
	}
	want := []string{"call obj-13", "call obj-15", "insert secret words", "key keyDown Enter 1", "key keyUp Enter 0"}
	if strings.Join(fc.actions, "|") != strings.Join(want, "|") {
		t.Fatalf("actions\n got %q\nwant %q", fc.actions, want)
	}
	audit, _ := os.ReadFile(filepath.Join(f.dir, AuditFile))
	if strings.Contains(string(audit), "secret") || strings.Contains(string(audit), "Fairy") {
		t.Fatalf("audit leaks: %s", audit)
	}
}

// A ref is only good on the page load, on-period and resident it was read
// from: after a navigation, a new on-period, or from another resident it is
// stale, and nothing is clicked.
func TestResidentBrowserRefusesStaleRefs(t *testing.T) {
	cases := map[string]func(f *fixture, fc *fakeChrome) string{
		"new page load": func(f *fixture, fc *fakeChrome) string {
			fc.mu.Lock()
			fc.loader = "L2"
			fc.mu.Unlock()
			return "mira"
		},
		"new on-period": func(f *fixture, fc *fakeChrome) string {
			if err := TurnOff(f.dir); err != nil {
				t.Fatal(err)
			}
			f.clock.Add(int64(time.Second))
			if _, err := TurnOn(f.dir, f.sup.Now(), 15*time.Minute, false); err != nil {
				t.Fatal(err)
			}
			return "mira"
		},
		"forged prefix": func(f *fixture, fc *fakeChrome) string { return "forged" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			fc := newFakeChrome(t, "tab1")
			f := browserFixture(t, fc)
			link := refFor(t, snapshot(t, f, true), "Fairy ring")
			if change(f, fc) == "forged" {
				link = "@b000000000000" + link[strings.Index(link, ":"):]
			}
			r := f.do(`{"command":"click","app":"souls.house browser","ref":"` + link + `"}`)
			if r.Status != "completed" || r.Steps[0].ExitCode == 0 || !strings.Contains(string(r.Steps[0].Output), "stale_ref") {
				t.Fatalf("%+v %s", r, r.Steps[0].Output)
			}
			if len(fc.actions) != 0 {
				t.Fatalf("acted on a stale ref: %v", fc.actions)
			}
		})
	}
}

func TestResidentBrowserReadVerbsNeedATab(t *testing.T) {
	fc := newFakeChrome(t)
	f := browserFixture(t, fc)
	r := f.do(`{"command":"snapshot","app":"souls.house browser"}`)
	if r.Steps[0].ExitCode == 0 || !strings.Contains(string(r.Steps[0].Output), "no_page") || len(fc.created) != 0 {
		t.Fatalf("%+v", r)
	}
}

// Revocation between reading the ref and acting on it: nothing is clicked,
// typed or pressed.
func TestResidentBrowserStopsActingWhenRevoked(t *testing.T) {
	for _, verb := range []string{"click", "type"} {
		t.Run(verb, func(t *testing.T) {
			fc := newFakeChrome(t, "tab1")
			f := browserFixture(t, fc)
			box := refFor(t, snapshot(t, f, true), "Search")
			fc.mu.Lock()
			fc.onResolve = func() {
				if err := TurnOff(f.dir); err != nil {
					t.Error(err)
				}
			}
			fc.mu.Unlock()
			args := `{"command":"click","app":"souls.house browser","ref":"` + box + `"}`
			if verb == "type" {
				args = `{"command":"type","app":"souls.house browser","ref":"` + box + `","text":"x"}`
			}
			r := f.do(args)
			if r.Status != "stopped" || !strings.HasPrefix(r.Reason, ReasonStoppedMidCall+":") {
				t.Fatalf("%+v", r)
			}
			if len(fc.actions) != 0 {
				t.Fatalf("acted after revocation: %v", fc.actions)
			}
		})
	}
}
