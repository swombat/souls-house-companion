package main

import (
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/swombat/souls-house-companion/internal/client"
	"github.com/swombat/souls-house-companion/internal/desktop"
)

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func agentDesktopBinary(home string) string {
	return filepath.Join(home, ".local", "agent-desktop", "agent-desktop")
}

// desktopCommand needs no state lock: the switch and scope are separate
// atomically replaced files, so the menu bar can drive them while the service
// runs. Absence or damage of either reads as off.
//
//	desktop on (--minutes N | --until-off)
//	desktop off
//	desktop status
//	desktop scope --resident ID --app NAME [--app NAME...] | --resident ID --clear
func desktopCommand(home string, args []string) error {
	if len(args) < 1 {
		return errors.New("desktop on|off|status|scope required")
	}
	flags := flag.NewFlagSet("desktop", flag.ContinueOnError)
	dir := flags.String("state-dir", filepath.Join(home, ".local/share/souls-house-companion"), "private state directory")
	minutes := flags.Int("minutes", 0, "minutes until desktop control turns itself off (1-720)")
	untilOff := flags.Bool("until-off", false, "stay on until turned off (still off after any restart)")
	resident := flags.String("resident", "", "resident ID for scope")
	clear := flags.Bool("clear", false, "remove the resident from the desktop scope")
	var apps stringList
	flags.Var(&apps, "app", "app the resident may operate (repeatable)")
	_ = flags.Bool("json", false, "machine-readable status")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if abs, err := filepath.Abs(*dir); err == nil {
		*dir = abs
	}
	if _, err := client.Load(*dir); err != nil {
		return errors.New("companion not enrolled or state unavailable")
	}
	now := time.Now()
	switch args[0] {
	case "on":
		if (*minutes == 0) == !*untilOff {
			return errors.New("exactly one of --minutes or --until-off")
		}
		_, err := desktop.TurnOn(*dir, now, time.Duration(*minutes)*time.Minute, *untilOff)
		return err
	case "off":
		err := desktop.TurnOff(*dir)
		return errors.Join(err, desktop.WipeRoots(*dir))
	case "scope":
		if *resident == "" || (*clear == (len(apps) > 0)) {
			return errors.New("--resident with --app (repeatable) or --clear")
		}
		scope, err := desktop.LoadScope(*dir)
		if errors.Is(err, os.ErrNotExist) {
			scope, err = desktop.Scope{Version: 1}, nil
		}
		if err != nil {
			return err
		}
		next := desktop.Scope{Version: 1}
		for _, r := range scope.Residents {
			if r.ID != *resident {
				next.Residents = append(next.Residents, r)
			}
		}
		if !*clear {
			next.Residents = append(next.Residents, desktop.ResidentScope{ID: *resident, Apps: apps})
		}
		return desktop.SaveScope(*dir, next)
	case "status":
		sw, reason := desktop.State(*dir, now)
		out := map[string]any{"on": reason == "", "reason": reason}
		if reason == "" {
			out["until_off"] = sw.UntilOff
			if !sw.UntilOff {
				out["until"] = sw.Until
			}
		}
		if scope, err := desktop.LoadScope(*dir); err == nil {
			out["scope"] = scope.Residents
		} else {
			out["scope"] = []desktop.ResidentScope{}
		}
		if last := desktop.LastAction(*dir); last != nil {
			out["last_action"] = last
		}
		out["agent_desktop"] = agentDesktopBinary(home)
		return json.NewEncoder(os.Stdout).Encode(out)
	}
	return errors.New("desktop on|off|status|scope required")
}
