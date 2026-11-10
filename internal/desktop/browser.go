package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/sys/unix"
)

// ResidentBrowser is the app name a local scope lists to let a resident use
// the companion's own browser: a separate Google Chrome instance with its own
// profile under the companion's state directory. It never holds the owner's
// cookies or windows, and its tabs are loaded over the DevTools protocol with
// background targets, so nothing is brought to the front and no keystroke is
// sent. Measured on the owner's Mac (10 Nov): Launch Services `open -g -n`
// and DevTools navigation kept the frontmost app unchanged, where opening a
// URL in the owner's Chrome brought Chrome forward every time. Starting the
// browser with a window also brought it forward, so it starts with none.
const ResidentBrowser = "souls.house browser"

const (
	BrowserDir               = "browser"
	ReasonBrowserUnavailable = "browser_unavailable"
	browserStartTimeout      = 20 * time.Second
	activePortFile           = "DevToolsActivePort"
)

var browserPathPattern = regexp.MustCompile(`^/devtools/browser/[A-Za-z0-9-]{1,64}$`)

// launchBrowser starts the resident Chrome on profile without activating it.
// --remote-debugging-port=0 lets Chrome choose a free port and write it to
// DevToolsActivePort in the profile, so the endpoint is read from a file only
// this user can write rather than guessed. The port listens on 127.0.0.1
// only; any local process of this user could reach it, as it could reach the
// profile directory itself.
func (s *Supervisor) launchBrowser(ctx context.Context, profile string) error {
	if s.BrowserLaunch != nil {
		return s.BrowserLaunch(ctx, profile)
	}
	opener := s.Opener
	if opener == "" {
		opener = "/usr/bin/open"
	}
	cmd := exec.CommandContext(ctx, opener, "-g", "-n", "-a", "Google Chrome", "--args",
		"--user-data-dir="+profile,
		"--remote-debugging-port=0",
		"--remote-debugging-address=127.0.0.1",
		"--no-first-run", "--no-default-browser-check",
		// A window at startup is brought to the front, -g or not
		// (measured); with none, the first open creates a background tab.
		"--no-startup-window")
	cmd.Env = []string{"HOME=" + os.Getenv("HOME"), "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LANG=en_US.UTF-8"}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd.Run()
}

// browserEndpoint reads the DevTools endpoint the resident browser published.
// Chrome writes the file 0644; it is trusted because the profile directory
// is checked private (0700, ours) and the file must be ours and regular.
func browserEndpoint(profile string) (string, error) {
	if err := privateDir(profile); err != nil {
		return "", err
	}
	f, err := os.OpenFile(filepath.Join(profile, activePortFile), os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Size() > 256 {
		return "", errors.New("DevToolsActivePort must be a small regular file")
	}
	if stat, ok := st.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Geteuid()) {
		return "", errors.New("DevToolsActivePort has the wrong owner")
	}
	data, err := io.ReadAll(io.LimitReader(f, 257))
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		return "", errors.New("malformed DevToolsActivePort")
	}
	port, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	path := strings.TrimSpace(lines[1])
	if err != nil || port < 1 || port > 65535 || !browserPathPattern.MatchString(path) {
		return "", errors.New("malformed DevToolsActivePort")
	}
	return "ws://127.0.0.1:" + strconv.Itoa(port) + path, nil
}

func dialBrowser(ctx context.Context, profile string) (*websocket.Conn, error) {
	endpoint, err := browserEndpoint(profile)
	if err != nil {
		return nil, err
	}
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dctx, endpoint, nil)
	if err != nil {
		return nil, err
	}
	conn.SetReadLimit(4 << 20)
	return conn, nil
}

// connectBrowser returns a DevTools connection to the resident browser,
// starting it first if it is not running (or its endpoint is stale).
func (s *Supervisor) connectBrowser(ctx context.Context) (*websocket.Conn, error) {
	profile := filepath.Join(s.StateDir, BrowserDir)
	if err := os.MkdirAll(profile, 0700); err != nil {
		return nil, err
	}
	if err := privateDir(profile); err != nil {
		return nil, err
	}
	if conn, err := dialBrowser(ctx, profile); err == nil {
		return conn, nil
	}
	// Not running, or a stale file from a browser that has gone: never
	// trust the old endpoint once a new browser is starting.
	_ = os.Remove(filepath.Join(profile, activePortFile))
	if err := s.launchBrowser(ctx, profile); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(browserStartTimeout)
	for {
		conn, err := dialBrowser(ctx, profile)
		if err == nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

type cdp struct {
	conn *websocket.Conn
	next int
}

type cdpError struct {
	Message string `json:"message"`
}

// call sends one DevTools command and waits for its reply, skipping events.
func (c *cdp) call(ctx context.Context, session, method string, params any, out any) error {
	c.next++
	msg := map[string]any{"id": c.next, "method": method, "params": params}
	if session != "" {
		msg["sessionId"] = session
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if err := c.conn.Write(ctx, websocket.MessageText, data); err != nil {
		return err
	}
	for {
		_, reply, err := c.conn.Read(ctx)
		if err != nil {
			return err
		}
		var r struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *cdpError       `json:"error"`
		}
		if json.Unmarshal(reply, &r) != nil || r.ID != c.next {
			continue
		}
		if r.Error != nil {
			return errors.New(method + ": " + r.Error.Message)
		}
		if out != nil {
			return json.Unmarshal(r.Result, out)
		}
		return nil
	}
}

// browse loads url in the resident browser. It reuses the first open tab
// (Page.navigate does not activate it); with no tab it creates a background
// one. The URL is not echoed in the result or the audit.
func (s *Supervisor) browse(ctx context.Context, step Step) StepResult {
	sr := StepResult{Command: step.Command, App: step.App, ExitCode: 1}
	fail := func(reason string) StepResult {
		sr.Output, _ = json.Marshal(map[string]string{"error": reason})
		return sr
	}
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
	how := "navigated"
	if page == "" {
		var created struct {
			TargetID string `json:"targetId"`
		}
		if err := c.call(ctx, "", "Target.createTarget", map[string]any{"url": step.url, "background": true}, &created); err != nil {
			return fail("navigation_failed")
		}
		how = "created"
	} else {
		var attached struct {
			SessionID string `json:"sessionId"`
		}
		if err := c.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": page, "flatten": true}, &attached); err != nil || attached.SessionID == "" {
			return fail(ReasonBrowserUnavailable)
		}
		var nav struct {
			ErrorText string `json:"errorText"`
		}
		err := c.call(ctx, attached.SessionID, "Page.navigate", map[string]any{"url": step.url}, &nav)
		_ = c.call(ctx, "", "Target.detachFromTarget", map[string]any{"sessionId": attached.SessionID}, nil)
		if err != nil || nav.ErrorText != "" {
			return fail("navigation_failed")
		}
	}
	sr.ExitCode = 0
	sr.Output, _ = json.Marshal(map[string]any{"ok": true, how: true})
	return sr
}
