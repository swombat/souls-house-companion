package desktop

import (
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The grammar is allowlisted per verb. The companion builds argv itself from
// typed fields; callers never supply flags. Never emitted: --headed, --force,
// --session, --agent-id, --trace*, --screenshot, --debug, -v, any output path.

const (
	MaxBatchSteps = 16
	MaxTextBytes  = 10000
	MaxURLBytes   = 2048
	maxField      = 256
)

var (
	refPattern    = regexp.MustCompile(`^@[a-z0-9]{1,40}:e[0-9]{1,7}$`)
	windowPattern = regexp.MustCompile(`^w-[0-9]{1,12}$`)
	comboPattern  = regexp.MustCompile("^[a-z0-9]+(\\+[a-z0-9,./;'\\[\\]\\\\=`-]+)*$|^[,./;'\\[\\]\\\\=`-]$")
	// Printable ASCII URL characters only (RFC 3986, already percent-encoded).
	urlPattern = regexp.MustCompile(`^https?://[A-Za-z0-9._~:/?#\[\]@!$&'()*+,;=%-]+$`)
)

var (
	surfaces    = set("window", "menu", "menubar", "sheet", "popover", "alert")
	getProps    = set("text", "value", "title", "bounds", "role", "states")
	isProps     = set("visible", "enabled", "checked", "focused", "expanded", "selected")
	directions  = set("up", "down", "left", "right")
	verbs       = set("snapshot", "find", "get", "is", "click", "type", "press", "scroll", "open")
	allowedKeys = map[string]map[string]bool{
		"snapshot": set("command", "app", "interactive", "compact", "skeleton", "max_depth", "surface", "root", "window_id", "wait_for", "wait_timeout_ms"),
		"find":     set("command", "app", "role", "name", "value", "text", "description", "native_id", "exact", "first", "last", "count", "nth", "limit", "surface", "root", "window_id"),
		"get":      set("command", "app", "ref", "property"),
		"is":       set("command", "app", "ref", "property"),
		"click":    set("command", "app", "ref"),
		"type":     set("command", "app", "ref", "text"),
		"press":    set("command", "app", "combo"),
		"scroll":   set("command", "app", "ref", "direction", "amount"),
		"open":     set("command", "app", "url"),
	}
)

func set(items ...string) map[string]bool {
	m := make(map[string]bool, len(items))
	for _, i := range items {
		m[i] = true
	}
	return m
}

// Step is one validated agent-desktop call.
type Step struct {
	Command string
	App     string
	argv    []string
	stdin   []byte
	waitMS  int
	// opener: argv is for /usr/bin/open, not agent-desktop.
	opener bool
}

// Argv returns the arguments after the binary path.
func (s Step) Argv() []string { return append([]string(nil), s.argv...) }

type Request struct {
	Steps          []Step
	TimeoutSeconds int
}

var errInvalid = errors.New(ReasonInvalid)

// Parse validates the operation arguments. Unknown keys, nested batches,
// out-of-range values and control characters are refused as a whole.
func Parse(raw json.RawMessage) (Request, error) {
	var top map[string]json.RawMessage
	if len(raw) > 64<<10 || strictDecode(raw, &top) != nil {
		return Request{}, errInvalid
	}
	req := Request{TimeoutSeconds: 30}
	if t, ok := top["timeout_seconds"]; ok {
		n, err := intField(t, 1, 120)
		if err != nil {
			return Request{}, errInvalid
		}
		req.TimeoutSeconds = n
		delete(top, "timeout_seconds")
	}
	var command string
	if json.Unmarshal(top["command"], &command) != nil {
		return Request{}, errInvalid
	}
	if command == "batch" {
		if len(top) != 2 {
			return Request{}, errInvalid
		}
		var steps []map[string]json.RawMessage
		if strictDecode(top["steps"], &steps) != nil || len(steps) == 0 || len(steps) > MaxBatchSteps {
			return Request{}, errInvalid
		}
		for _, s := range steps {
			step, err := parseStep(s)
			if err != nil {
				return Request{}, err
			}
			req.Steps = append(req.Steps, step)
		}
		return req, nil
	}
	step, err := parseStep(top)
	if err != nil {
		return Request{}, err
	}
	req.Steps = []Step{step}
	return req, nil
}

func parseStep(f map[string]json.RawMessage) (Step, error) {
	var s Step
	if json.Unmarshal(f["command"], &s.Command) != nil || !verbs[s.Command] {
		return s, errInvalid
	}
	keys := allowedKeys[s.Command]
	for k := range f {
		if !keys[k] {
			return s, errInvalid
		}
	}
	app, err := str(f, "app", true)
	if err != nil || !appName.MatchString(app) {
		return s, errInvalid
	}
	s.App = app
	a := []string{s.Command}
	flag := func(name, value string) { a = append(a, "--"+name+"="+value) }
	boolFlag := func(key, name string) error {
		v, present, err := boolean(f, key)
		if err != nil {
			return err
		}
		if present && v {
			a = append(a, name)
		}
		return nil
	}
	refArg := func(key string, required bool) (string, error) {
		r, err := str(f, key, required)
		if err != nil || (r != "" && !refPattern.MatchString(r)) || (required && r == "") {
			return "", errInvalid
		}
		return r, nil
	}
	common := func(withWait bool) error {
		if v, err := enum(f, "surface", surfaces); err != nil {
			return err
		} else if v != "" {
			flag("surface", v)
		}
		if r, err := refArg("root", false); err != nil {
			return err
		} else if r != "" {
			flag("root", r)
		}
		if w, err := str(f, "window_id", false); err != nil || (w != "" && !windowPattern.MatchString(w)) {
			return errInvalid
		} else if w != "" {
			flag("window-id", w)
		}
		if !withWait {
			return nil
		}
		if w, err := str(f, "wait_for", false); err != nil {
			return err
		} else if w != "" {
			if !strings.Contains(w, ":") {
				return errInvalid
			}
			flag("wait-for", w)
		}
		if raw, ok := f["wait_timeout_ms"]; ok {
			n, err := intField(raw, 1, 30000)
			if err != nil {
				return err
			}
			flag("wait-timeout", strconv.Itoa(n))
			s.waitMS = n
		} else if _, ok := f["wait_for"]; ok {
			s.waitMS = 30000
		}
		return nil
	}
	switch s.Command {
	case "snapshot":
		flag("app", app)
		for _, b := range [][2]string{{"interactive", "--interactive-only"}, {"compact", "--compact"}, {"skeleton", "--skeleton"}} {
			if err := boolFlag(b[0], b[1]); err != nil {
				return s, err
			}
		}
		if raw, ok := f["max_depth"]; ok {
			n, err := intField(raw, 1, 30)
			if err != nil {
				return s, err
			}
			flag("max-depth", strconv.Itoa(n))
		}
		if err := common(true); err != nil {
			return s, err
		}
	case "find":
		flag("app", app)
		for _, k := range []string{"role", "name", "value", "text", "description", "native_id"} {
			v, err := str(f, k, false)
			if err != nil {
				return s, err
			}
			if v != "" {
				flag(strings.ReplaceAll(k, "_", "-"), v)
			}
		}
		if err := boolFlag("exact", "--exact"); err != nil {
			return s, err
		}
		// Selection modes are mutually exclusive upstream, and with --limit.
		selections := 0
		for _, b := range [][2]string{{"first", "--first"}, {"last", "--last"}, {"count", "--count"}} {
			v, present, err := boolean(f, b[0])
			if err != nil {
				return s, err
			}
			if present && v {
				selections++
				a = append(a, b[1])
			}
		}
		if raw, ok := f["nth"]; ok {
			n, err := intField(raw, 0, 999)
			if err != nil {
				return s, err
			}
			selections++
			flag("nth", strconv.Itoa(n))
		}
		_, hasLimit := f["limit"]
		if selections > 1 || (selections == 1 && hasLimit) {
			return s, errInvalid
		}
		if selections == 0 {
			limit := 50
			if hasLimit {
				n, err := intField(f["limit"], 1, 200)
				if err != nil {
					return s, err
				}
				limit = n
			}
			flag("limit", strconv.Itoa(limit)) // a list search is never unbounded
		}
		if err := common(false); err != nil {
			return s, err
		}
	case "get", "is":
		props := getProps
		if s.Command == "is" {
			props = isProps
		}
		r, err := refArg("ref", true)
		if err != nil {
			return s, err
		}
		p, err := enum(f, "property", props)
		if err != nil {
			return s, err
		}
		if p != "" {
			flag("property", p)
		}
		a = append(a, "--", r)
	case "click":
		r, err := refArg("ref", true)
		if err != nil {
			return s, err
		}
		a = append(a, "--", r)
	case "type":
		r, err := refArg("ref", true)
		if err != nil {
			return s, err
		}
		var text string
		if json.Unmarshal(f["text"], &text) != nil || text == "" || len(text) > MaxTextBytes || !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
			return s, errInvalid
		}
		s.stdin = []byte(text) // never in argv or the process table
		a = append(a, "--stdin", "--", r)
	case "press":
		c, err := str(f, "combo", true)
		if err != nil || len(c) > 32 || !comboPattern.MatchString(c) {
			return s, errInvalid
		}
		// --app sends keys to that process without focusing it: no keystroke
		// reaches whatever app happens to be frontmost.
		flag("app", app)
		a = append(a, "--", c)
	case "scroll":
		r, err := refArg("ref", true)
		if err != nil {
			return s, err
		}
		d, err := enum(f, "direction", directions)
		if err != nil {
			return s, err
		}
		if d != "" {
			flag("direction", d)
		}
		if raw, ok := f["amount"]; ok {
			n, err := intField(raw, 1, 50)
			if err != nil {
				return s, err
			}
			flag("amount", strconv.Itoa(n))
		}
		a = append(a, "--", r)
	case "open":
		var u string
		if json.Unmarshal(f["url"], &u) != nil || !validURL(u) {
			return s, errInvalid
		}
		// -g: Launch Services hands the URL to the app without bringing it
		// to the front, and no keystroke is involved, so nothing can land in
		// a field the owner has focused. The app opens it its own way (for a
		// browser, a new tab in its front window, which may be the owner's).
		s.opener = true
		s.argv = []string{"-g", "-a", app, u}
		return s, nil
	}
	s.argv = a
	return s, nil
}

func str(f map[string]json.RawMessage, key string, required bool) (string, error) {
	raw, ok := f[key]
	if !ok {
		if required {
			return "", errInvalid
		}
		return "", nil
	}
	var v string
	if json.Unmarshal(raw, &v) != nil || v == "" || len(v) > maxField || !utf8.ValidString(v) {
		return "", errInvalid
	}
	for _, r := range v {
		if r < 0x20 || r == 0x7f {
			return "", errInvalid
		}
	}
	return v, nil
}

func enum(f map[string]json.RawMessage, key string, allowed map[string]bool) (string, error) {
	v, err := str(f, key, false)
	if err != nil || (v != "" && !allowed[v]) {
		return "", errInvalid
	}
	return v, nil
}

func boolean(f map[string]json.RawMessage, key string) (bool, bool, error) {
	raw, ok := f[key]
	if !ok {
		return false, false, nil
	}
	var v bool
	if json.Unmarshal(raw, &v) != nil {
		return false, true, errInvalid
	}
	return v, true, nil
}

func intField(raw json.RawMessage, lo, hi int) (int, error) {
	var n int
	if json.Unmarshal(raw, &n) != nil || n < lo || n > hi {
		return 0, errInvalid
	}
	return n, nil
}

// validURL accepts an absolute http(s) URL with a host and no userinfo, in
// printable ASCII. No other scheme is opened: file:, javascript: and app URL
// schemes would reach other handlers than the app in scope.
func validURL(v string) bool {
	if len(v) > MaxURLBytes || !urlPattern.MatchString(v) {
		return false
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Opaque != "" {
		return false
	}
	return u.Hostname() != ""
}
