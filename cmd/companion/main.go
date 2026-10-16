package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/linuxfs"
	"github.com/swombat/souls-house-companion/internal/policy"
	"github.com/swombat/souls-house-companion/internal/service"
	"github.com/swombat/souls-house-companion/internal/volumes"
)

var version = "development"

func main() {
	if err := run(os.Args[1:]); err != nil {
		// Error values from network/operation payloads are never interpolated.
		fmt.Fprintln(os.Stderr, "companion: command failed; check configuration, authority and service status")
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("command required")
	}
	if args[0] == "version" {
		fmt.Println(version)
		return nil
	}
	policyAction := ""
	if args[0] == "policy" {
		if len(args) < 2 || (args[1] != "show" && args[1] != "apply") {
			return errors.New("policy show or apply required")
		}
		policyAction = args[1]
		args = append([]string{"policy"}, args[2:]...)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	flags := flag.NewFlagSet("companion", flag.ContinueOnError)
	dir := flags.String("state-dir", filepath.Join(home, ".local/share/souls-house-companion"), "private state directory")
	base := flags.String("url", "", "HTTPS house origin")
	requestID := flags.String("request-id", "", "device request ID bound to enrollment")
	policyFile := flags.String("policy", "", "local ceiling JSON")
	tokenStdin := flags.Bool("token-stdin", false, "read enrollment token from stdin")
	user := flags.Bool("user", false, "install ordinary user unit only")
	_ = flags.Bool("json", false, "machine-readable status")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	absolute, err := filepath.Abs(*dir)
	if err != nil {
		return err
	}
	*dir = absolute
	unit := filepath.Join(home, ".config/systemd/user/souls-house-companion.service")
	if args[0] == "pause" {
		// Cancellation begins before state mutation. No Close wait precedes it.
		if err := systemctl("stop", "souls-house-companion.service"); err != nil {
			return err
		}
	}
	var lock *os.File
	if args[0] != "status" && !(args[0] == "policy" && policyAction == "show") {
		lock, err = client.Lock(*dir)
		if err != nil {
			return err
		}
		defer lock.Close()
	}
	switch args[0] {
	case "volumes":
		mounts, snap, err := volumes.LoadSystem()
		if err != nil {
			return err
		}
		defer snap.Close()
		return json.NewEncoder(os.Stdout).Encode(mounts)
	case "enroll":
		if !*tokenStdin || *base == "" || *requestID == "" || *policyFile == "" {
			return errors.New("url, request-id, policy and token-stdin required")
		}
		if _, err := os.Lstat(filepath.Join(*dir, "state.json")); !os.IsNotExist(err) {
			return errors.New("already enrolled or state unavailable")
		}
		ceiling, err := loadPolicy(*policyFile)
		if err != nil {
			return err
		}
		token, err := io.ReadAll(io.LimitReader(os.Stdin, 257))
		if err != nil || len(token) > 256 {
			return errors.New("invalid token input")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		state, err := client.New(nil).Enroll(ctx, *base, *requestID, strings.TrimSpace(string(token)), ceiling)
		if err != nil {
			return err
		}
		return client.Save(*dir, state)
	case "install":
		if !*user {
			return errors.New("only user installation supported")
		}
		return installUnit(unit, *dir)
	case "status":
		state, err := client.Load(*dir)
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(map[string]any{
			"version": version, "device_id": state.DeviceID, "paused": state.Paused,
			"enrolled": true, "ceiling": state.Ceiling,
		})
	case "policy":
		state, err := client.Load(*dir)
		if err != nil {
			return err
		}
		if policyAction == "show" {
			return json.NewEncoder(os.Stdout).Encode(state.Ceiling)
		}
		if *policyFile == "" {
			return errors.New("policy apply requires policy file")
		}
		state.Ceiling, err = loadPolicy(*policyFile)
		if err != nil {
			return err
		}
		// Record the authority act before committing it. Do not record paths,
		// credential values or file contents in this safe local audit.
		audit, err := os.OpenFile(filepath.Join(*dir, "audit.jsonl"),
			os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0600)
		if err != nil {
			return err
		}
		_, writeErr := fmt.Fprintf(audit, "{\"event\":\"policy.apply\",\"time\":%q}\n", time.Now().UTC().Format(time.RFC3339Nano))
		syncErr := audit.Sync()
		audit.Close()
		if writeErr != nil || syncErr != nil {
			return errors.New("policy audit failed")
		}
		return client.Save(*dir, state)
	case "pause", "resume":
		state, err := client.Load(*dir)
		if err != nil {
			return err
		}
		state.Paused = args[0] == "pause"
		if err := client.Save(*dir, state); err != nil {
			return err
		}
		lock.Close()
		if args[0] == "resume" {
			return systemctl("start", "souls-house-companion.service")
		}
		return nil
	case "serve":
		return serve(*dir, unit)
	default:
		return errors.New("unknown command")
	}
}

func loadPolicy(path string) (policy.Policy, error) {
	var p policy.Policy
	f, err := os.Open(path)
	if err != nil {
		return p, err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 128<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return p, err
	}
	return p, p.Validate()
}

func systemctl(args ...string) error {
	cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	for _, key := range []string{"HOME", "PATH", "XDG_RUNTIME_DIR", "DBUS_SESSION_BUS_ADDRESS"} {
		if v, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+v)
		}
	}
	return cmd.Run()
}

func installUnit(path, dir string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil || strings.ContainsAny(exe+dir, "\n\r%\"\\") {
		return errors.New("unsupported service path")
	}
	content := "[Unit]\nDescription=souls.house Companion\nAfter=network-online.target\n\n[Service]\nType=simple\n" +
		fmt.Sprintf("ExecStart=\"%s\" serve --state-dir \"%s\"\n", exe, dir) +
		"Environment=PATH=/usr/local/bin:/usr/bin:/bin\nRestart=on-failure\nRestartSec=10\nUMask=0077\nNoNewPrivileges=yes\n\n[Install]\nWantedBy=default.target\n"
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if old, err := os.Lstat(path); err == nil && !old.Mode().IsRegular() {
		return errors.New("unit must be a regular file")
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	return systemctl("enable", "souls-house-companion.service")
}

func serve(dir, unit string) error {
	state, err := client.Load(dir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	handler := &service.Handler{ProtectedPaths: []string{dir, filepath.Join(dir, "state.json"), unit, exe}}
	c := client.New(handler)
	c.Save = func(s client.State) error { return client.Save(dir, s) }
	for ctx.Err() == nil {
		// Refuse unsupported mounts/protection before opening any socket.
		mounts, snap, err := volumes.LoadSystem()
		if err != nil {
			return err
		}
		protected, err := linuxfs.CaptureProtected(handler.ProtectedPaths...)
		if err != nil {
			snap.Close()
			return err
		}
		for _, mount := range mounts {
			if mount.Denied != "" || !locallyGranted(state.Ceiling, mount.Key) {
				continue
			}
			root, openErr := linuxfs.OpenVerified(mount, nil, protected)
			if openErr != nil {
				protected.Close()
				snap.Close()
				return openErr
			}
			root.Close()
		}
		protected.Close()
		c.BeforeDelivery = func() error {
			if err := snap.Validate(); err != nil {
				return err
			}
			current, err := client.Load(dir)
			if err != nil || current.Paused || !reflect.DeepEqual(current.Ceiling, state.Ceiling) {
				return errors.New("local authority changed")
			}
			return nil
		}
		_ = c.RunOnce(ctx, &state)
		snap.Close()
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(10 * time.Second):
		}
	}
	return nil
}

func locallyGranted(p policy.Policy, key string) bool {
	for _, resident := range p.Residents {
		for _, grant := range resident.Volumes {
			if grant.Volume == key && (grant.Access == policy.Read || grant.Access == policy.Write) {
				return true
			}
		}
	}
	return false
}
