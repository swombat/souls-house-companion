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
}

func newFakeChrome(t *testing.T, pages ...string) *fakeChrome {
	fc := &fakeChrome{pages: pages}
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

func TestResidentBrowserOnlyTakesOpen(t *testing.T) {
	for _, args := range []string{
		`{"command":"snapshot","app":"souls.house browser"}`,
		`{"command":"click","app":"souls.house browser","ref":"@a:e1"}`,
		`{"command":"press","app":"souls.house browser","combo":"return"}`,
		`{"command":"open","app":"souls.house browser","url":"file:///etc/passwd"}`,
	} {
		if _, err := Parse(json.RawMessage(args)); err == nil {
			t.Fatalf("accepted %s", args)
		}
	}
	r := mustParse(t, browseURL)
	if !r.Steps[0].browser || r.Steps[0].opener || len(r.Steps[0].argv) != 0 {
		t.Fatalf("%+v", r.Steps[0])
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
