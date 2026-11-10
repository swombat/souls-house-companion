package desktop

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Verbs on the resident browser. Every one goes over the DevTools protocol to
// the browser's first tab, never through agent-desktop or the window server,
// so nothing is brought to the front and no key can reach another app.
//
//   - open: load a URL.
//   - snapshot: the tab's URL, title and accessibility tree, one entry per
//     element, with a ref where the element has a DOM node.
//   - click: click the element a ref names.
//   - type: focus the element a ref names and insert text into it.
//   - press: one key (browserKeys) to whatever has focus inside the tab.
//
// A ref is "@b<12 hex>:e<backend node id>". The prefix binds it to the
// resident, the on-period and the page load it was read from, so a ref from
// an earlier page, period or resident is refused as stale rather than resolved
// against whatever the node id names now.

const (
	browserMaxElements = 1500
	browserMaxName     = 300
)

// browserKeys are the keys press accepts for the resident browser, with
// their DevTools key event fields. No modifiers: no shortcut can reach the
// browser's own menus.
var browserKeys = map[string]struct {
	key, code, text string
	vk              int
}{
	"return":    {"Enter", "Enter", "\r", 13},
	"enter":     {"Enter", "Enter", "\r", 13},
	"tab":       {"Tab", "Tab", "", 9},
	"escape":    {"Escape", "Escape", "", 27},
	"backspace": {"Backspace", "Backspace", "", 8},
	"space":     {" ", "Space", " ", 32},
	"up":        {"ArrowUp", "ArrowUp", "", 38},
	"down":      {"ArrowDown", "ArrowDown", "", 40},
	"left":      {"ArrowLeft", "ArrowLeft", "", 37},
	"right":     {"ArrowRight", "ArrowRight", "", 39},
	"pageup":    {"PageUp", "PageUp", "", 33},
	"pagedown":  {"PageDown", "PageDown", "", 34},
	"home":      {"Home", "Home", "", 36},
	"end":       {"End", "End", "", 35},
}

// interactiveRoles is what snapshot keeps with interactive: true.
var interactiveRoles = set("link", "button", "textbox", "searchbox", "combobox", "checkbox", "radio",
	"menuitem", "menuitemcheckbox", "menuitemradio", "tab", "option", "switch", "slider", "spinbutton", "listbox")

// skippedRoles never appear in a snapshot: they repeat their parent's text.
var skippedRoles = set("InlineTextBox", "LineBreak", "none", "presentation")

func refPrefix(resident, generation, loaderID string) string {
	sum := sha256.Sum256([]byte(resident + "\x00" + generation + "\x00" + loaderID))
	return "b" + hex.EncodeToString(sum[:6])
}

// splitRef returns the prefix and backend node id of a resident-browser ref.
func splitRef(ref string) (string, int, bool) {
	if !refPattern.MatchString(ref) {
		return "", 0, false
	}
	i := strings.Index(ref, ":e")
	id, err := strconv.Atoi(ref[i+2:])
	if err != nil || id < 1 {
		return "", 0, false
	}
	return ref[1:i], id, true
}

func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "…"
}

var (
	errStaleRef     = errors.New("stale_ref")
	errNotFocusable = errors.New("not_focusable")
)

// browse runs one resident-browser step. The URL and typed text are not
// echoed in the result or the audit.
//
// Authority is watched for the whole step, as run does for a process: a
// switch-off, expiry, pause, scope edit or new on-period while the browser is
// starting cancels the step, and it is checked once more immediately before
// the one call that changes anything (load, click, insert, key), so nothing
// acts after revocation.
func (s *Supervisor) browse(parent context.Context, resident, generation string, step Step) (StepResult, string) {
	sr := StepResult{Command: step.Command, App: step.App, ExitCode: 1}
	ctx, cancel := context.WithTimeoutCause(parent, browserStartTimeout+stepSlack, errStepTimeout)
	defer cancel()
	stopReason := make(chan string, 1)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		t := time.NewTicker(pollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, reason := s.localAuthority(resident, []Step{step}, generation); reason != "" {
					stopReason <- reason
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-watchDone }()
	revoked := func() string {
		select {
		case r := <-stopReason:
			stopReason <- r
			return r
		default:
		}
		_, r := s.localAuthority(resident, []Step{step}, generation)
		return r
	}
	fail := func(reason string) (StepResult, string) {
		if r := revoked(); r != "" {
			return sr, ReasonStoppedMidCall + ":" + r
		}
		if errors.Is(context.Cause(ctx), errStepTimeout) || parent.Err() != nil {
			return sr, ReasonTimeout
		}
		sr.Output, _ = json.Marshal(map[string]string{"error": reason})
		return sr, ""
	}
	stopped := func(r string) (StepResult, string) { return sr, ReasonStoppedMidCall + ":" + r }

	conn, err := s.connectBrowser(ctx)
	if err != nil {
		return fail(ReasonBrowserUnavailable)
	}
	defer conn.CloseNow()
	c := &cdp{conn: conn}
	var targets struct {
		TargetInfos []struct {
			TargetID string `json:"targetId"`
			Type     string `json:"type"`
		} `json:"targetInfos"`
	}
	if err := c.call(ctx, "", "Target.getTargets", map[string]any{}, &targets); err != nil {
		return fail(ReasonBrowserUnavailable)
	}
	page := ""
	for _, t := range targets.TargetInfos {
		if t.Type == "page" {
			page = t.TargetID
			break
		}
	}
	if page == "" {
		if step.Command != "open" {
			return fail("no_page")
		}
		// Last authority check before the only call that loads anything.
		if r := revoked(); r != "" {
			return stopped(r)
		}
		var created struct {
			TargetID string `json:"targetId"`
		}
		if err := c.call(ctx, "", "Target.createTarget", map[string]any{"url": step.url, "background": true}, &created); err != nil {
			return fail("navigation_failed")
		}
		sr.ExitCode = 0
		sr.Output, _ = json.Marshal(map[string]any{"ok": true, "created": true})
		return sr, ""
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": page, "flatten": true}, &attached); err != nil || attached.SessionID == "" {
		return fail(ReasonBrowserUnavailable)
	}
	sess := attached.SessionID
	defer func() {
		dctx, dcancel := context.WithTimeout(context.Background(), time.Second)
		defer dcancel()
		_ = c.call(dctx, "", "Target.detachFromTarget", map[string]any{"sessionId": sess}, nil)
	}()

	if step.Command == "open" {
		// Last authority check before the only call that loads anything.
		if r := revoked(); r != "" {
			return stopped(r)
		}
		var nav struct {
			ErrorText string `json:"errorText"`
		}
		if err := c.call(ctx, sess, "Page.navigate", map[string]any{"url": step.url}, &nav); err != nil || nav.ErrorText != "" {
			return fail("navigation_failed")
		}
		sr.ExitCode = 0
		sr.Output, _ = json.Marshal(map[string]any{"ok": true, "navigated": true})
		return sr, ""
	}

	var tree struct {
		FrameTree struct {
			Frame struct {
				ID       string `json:"id"`
				LoaderID string `json:"loaderId"`
				URL      string `json:"url"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err := c.call(ctx, sess, "Page.getFrameTree", map[string]any{}, &tree); err != nil || tree.FrameTree.Frame.LoaderID == "" {
		return fail(ReasonBrowserUnavailable)
	}
	frame := tree.FrameTree.Frame
	prefix := refPrefix(resident, generation, frame.LoaderID)

	var out any
	switch step.Command {
	case "snapshot":
		out, err = snapshotTab(ctx, c, sess, frame.ID, frame.URL, prefix, step.interactive)
	case "click", "type":
		var objectID string
		if objectID, err = resolveRef(ctx, c, sess, prefix, step.ref); err != nil {
			break
		}
		// Last authority check before the call that acts.
		if r := revoked(); r != "" {
			return stopped(r)
		}
		if step.Command == "click" {
			err = callOn(ctx, c, sess, objectID, `function(){this.scrollIntoView({block:"center",inline:"center"});this.click();return true}`)
		} else {
			err = callOn(ctx, c, sess, objectID, `function(){this.scrollIntoView({block:"center"});this.focus();var a=document.activeElement;return a===this||this.contains(a)}`)
			if err == nil {
				err = c.call(ctx, sess, "Input.insertText", map[string]any{"text": string(step.stdin)}, nil)
			}
		}
		out = map[string]any{"ok": true}
	case "press":
		k := browserKeys[step.combo]
		if r := revoked(); r != "" {
			return stopped(r)
		}
		down := map[string]any{"type": "rawKeyDown", "key": k.key, "code": k.code, "windowsVirtualKeyCode": k.vk, "nativeVirtualKeyCode": k.vk}
		if k.text != "" {
			down["type"], down["text"] = "keyDown", k.text
		}
		if err = c.call(ctx, sess, "Input.dispatchKeyEvent", down, nil); err == nil {
			err = c.call(ctx, sess, "Input.dispatchKeyEvent", map[string]any{"type": "keyUp", "key": k.key, "code": k.code, "windowsVirtualKeyCode": k.vk, "nativeVirtualKeyCode": k.vk}, nil)
		}
		out = map[string]any{"ok": true}
	default:
		return fail(ReasonInvalid)
	}
	if err != nil {
		switch {
		case errors.Is(err, errStaleRef):
			return fail("stale_ref")
		case errors.Is(err, errNotFocusable):
			return fail("not_focusable")
		}
		return fail("action_failed")
	}
	data, err := json.Marshal(out)
	if err != nil || len(data) > OutputLimit {
		return fail("output_too_large")
	}
	sr.ExitCode = 0
	sr.Output = data
	return sr, ""
}

// resolveRef turns a ref read from this page load into a remote object.
func resolveRef(ctx context.Context, c *cdp, sess, prefix, ref string) (string, error) {
	p, id, ok := splitRef(ref)
	if !ok || p != prefix {
		return "", errStaleRef
	}
	var resolved struct {
		Object struct {
			ObjectID string `json:"objectId"`
		} `json:"object"`
	}
	if err := c.call(ctx, sess, "DOM.resolveNode", map[string]any{"backendNodeId": id}, &resolved); err != nil || resolved.Object.ObjectID == "" {
		return "", errStaleRef
	}
	return resolved.Object.ObjectID, nil
}

func callOn(ctx context.Context, c *cdp, sess, objectID, fn string) error {
	var r struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
		ExceptionDetails json.RawMessage `json:"exceptionDetails"`
	}
	if err := c.call(ctx, sess, "Runtime.callFunctionOn", map[string]any{"objectId": objectID, "functionDeclaration": fn, "returnByValue": true}, &r); err != nil {
		return err
	}
	if len(r.ExceptionDetails) > 0 {
		return errors.New("exception")
	}
	if r.Result.Value != true {
		return errNotFocusable
	}
	return nil
}

type axValue struct {
	Value any `json:"value"`
}

func (v *axValue) str() string {
	if v == nil {
		return ""
	}
	s, _ := v.Value.(string)
	return strings.TrimSpace(s)
}

type axNode struct {
	NodeID           string   `json:"nodeId"`
	Ignored          bool     `json:"ignored"`
	Role             *axValue `json:"role"`
	Name             *axValue `json:"name"`
	Value            *axValue `json:"value"`
	ParentID         string   `json:"parentId"`
	ChildIDs         []string `json:"childIds"`
	BackendDOMNodeID int      `json:"backendDOMNodeId"`
	FrameID          string   `json:"frameId"`
}

type element struct {
	Ref   string `json:"ref,omitempty"`
	Role  string `json:"role"`
	Name  string `json:"name,omitempty"`
	Value string `json:"value,omitempty"`
	Depth int    `json:"depth"`
}

// snapshotTab reads the main frame's accessibility tree in document order.
// Ignored and purely structural nodes are dropped, and their children lifted
// to the nearest kept ancestor's depth.
func snapshotTab(ctx context.Context, c *cdp, sess, frameID, url, prefix string, interactive bool) (any, error) {
	var title struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
	}
	if err := c.call(ctx, sess, "Runtime.evaluate", map[string]any{"expression": "document.title", "returnByValue": true}, &title); err != nil {
		return nil, err
	}
	var ax struct {
		Nodes []axNode `json:"nodes"`
	}
	if err := c.call(ctx, sess, "Accessibility.getFullAXTree", map[string]any{}, &ax); err != nil {
		return nil, err
	}
	byID := make(map[string]*axNode, len(ax.Nodes))
	for i := range ax.Nodes {
		byID[ax.Nodes[i].NodeID] = &ax.Nodes[i]
	}
	var roots []*axNode
	for i := range ax.Nodes {
		n := &ax.Nodes[i]
		if n.ParentID == "" || byID[n.ParentID] == nil {
			roots = append(roots, n)
		}
	}
	elems := []element{}
	truncated := false
	seen := make(map[string]bool, len(ax.Nodes))
	var walk func(n *axNode, depth int, parentName string)
	walk = func(n *axNode, depth int, parentName string) {
		if seen[n.NodeID] || truncated || depth > 64 {
			return
		}
		seen[n.NodeID] = true
		role, name := n.Role.str(), n.Name.str()
		keep := !n.Ignored && !skippedRoles[role] && (n.FrameID == "" || n.FrameID == frameID)
		if keep && role == "StaticText" && name == parentName {
			keep = false // the parent already says it
		}
		if keep && name == "" && !interactiveRoles[role] {
			keep = false // unnamed containers
		}
		if keep && interactive && !interactiveRoles[role] {
			keep = false
		}
		childDepth, childParent := depth, parentName
		if keep {
			if len(elems) >= browserMaxElements {
				truncated = true
				return
			}
			e := element{Role: role, Name: truncateUTF8(name, browserMaxName), Value: truncateUTF8(n.Value.str(), browserMaxName), Depth: depth}
			if n.BackendDOMNodeID > 0 && n.BackendDOMNodeID <= 9999999 {
				e.Ref = "@" + prefix + ":e" + strconv.Itoa(n.BackendDOMNodeID)
			}
			elems = append(elems, e)
			childDepth, childParent = depth+1, name
		} else if !n.Ignored && name != "" {
			childParent = name
		}
		for _, id := range n.ChildIDs {
			if child := byID[id]; child != nil {
				walk(child, childDepth, childParent)
			}
		}
	}
	for _, r := range roots {
		walk(r, 0, "")
	}
	t, _ := title.Result.Value.(string)
	return map[string]any{
		"url":       url,
		"title":     truncateUTF8(t, browserMaxName),
		"elements":  elems,
		"truncated": truncated,
	}, nil
}
