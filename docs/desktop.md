# Desktop control (macOS)

A fourth operation kind beside read, write and execute: `desktop` operates
allowlisted apps through the installed agent-desktop CLI
(`~/.local/agent-desktop/agent-desktop`, vercel-labs v1.2.0) with a fixed
grammar. It never runs a shell, launches or quits an app, or implies any
other authority, and no other authority implies it.

**It is not a shell-prevention boundary.** Driving an app that can run
commands (Terminal, an IDE, a browser console, System Settings) is shell
authority through the UI. The local scope therefore names apps per resident,
and the first accepted scope is Finder only.

## Authority (all four, checked before every step)

1. House grant: `desktop: true` for the resident in the per-request policy.
2. Local scope: `desktop-scope.json` lists the resident and the step's app.
3. Local switch: `desktop-switch.json` present, valid and not expired.
4. Not paused (marker file or state).

A refusal names one reason: `not_granted_by_house`,
`resident_not_in_local_scope`, `app_not_in_local_scope`, `switch_off`,
`switch_expired`, `switch_unreadable`, `switch_changed_since_request`, `paused`, `invalid_request`,
`agent_desktop_unavailable`, `platform_not_supported`, `timeout`.

The scope and switch are separate files, not fields in `state.json`, so a
rollback to a binary without desktop control still loads its state. A local
ceiling that sets `desktop` is refused (`policy apply`/`enroll`).

```
souls-house-companion desktop scope --resident AYawJx --app Finder
souls-house-companion desktop on --minutes 15     # or 60, or --until-off
souls-house-companion desktop status --json
souls-house-companion desktop off
```

These need no state lock, so the menu bar can drive them while the service
runs. The service removes the switch and every resident's refs at start: a
restart always comes up off.

## Grammar

Arguments are typed JSON fields; the companion builds argv itself. Every step
names `app`, which must be in scope. Verbs: `snapshot`, `find`, `get`, `is`,
`click`, `type`, `press`, `scroll`, `open`, and `batch` (1–16 of those, no nesting,
each validated and re-authorised before it runs). Unknown keys refuse the
whole request. Never emitted: `--headed`, `--force`, `--session`,
`--agent-id`, `--trace*`, `--screenshot`, `--debug`, `-v`, any output path.
Surfaces are limited to the app's own (window, menu, menubar, sheet, popover,
alert). `press` always carries `--app`, so keys go to that app's process and
never to whatever is frontmost. `type` text (≤10,000 bytes) goes over stdin,
never argv. `find` selections (`first`, `last`, `count`, `nth`) are mutually
exclusive and exclude `limit`; a plain list search gets `--limit=50` unless a
`limit` (1–200) is given. Values are `--flag=value` and refs/combos follow `--`, so no value
can become a flag. Waits ≤30 s, `find` limit ≤200 (default 50),
`timeout_seconds` 1–120 (house deadline ≤121 s).

`open` takes `app` and `url` and runs `/usr/bin/open -g -a <app> <url>`, not
agent-desktop: Launch Services hands the URL to the app without bringing it
forward, and no key is sent. Only absolute `http`/`https` URLs with a host,
no userinfo, in printable ASCII (percent-encoded), ≤2048 bytes. A browser
opens it as a new tab in its front window, which may be the owner's; whether
focus stays put is measured at acceptance, not assumed from `-g`.

## Process lifetime

No agent-desktop process outlives its call. Desktop workers are drained at
connection exit and service shutdown like execute workers (bounded grace). In v1.2.0 a macOS ref is
re-identified from an element token saved in the state root (strict
resolution, `retained_object_stale` when gone); no long-lived host is
needed, so none is kept. The supervisor serialises calls, starts each in its
own process group with a scrubbed environment (`HOME`, a fixed `PATH`,
`TMPDIR` and `AGENT_DESKTOP_HOME` inside a 0700 per-resident root, `LANG`),
polls authority every 250 ms while it runs, and on switch-off, expiry, pause
or deadline sends TERM then KILL to the group. After every call the group is
killed again. An interrupted step is reported as `stopped` and never
replayed; later batch steps do not run.

Refs are scoped to resident, app and on-period. agent-desktop resolves a ref
(and a rooted snapshot/find) from its saved element token without applying
`--app`, so each resident+app pair has its own state root: a ref captured in
one app does not exist in another app's store. A root from an earlier
on-period is deleted before use. The on-period accepted when a request is
authorised is bound to it: a request queued, or a batch running, across an
off/on is refused or stopped (`switch_changed_since_request`), never resumed
in the new period.

This is separate code from the execute runner and does not change the
execute lifecycle contract (`docs/macos.md`, S2 "Execute and removal").

## Receipts

Results to the authorised caller carry each step's verb, app, exit code and
agent-desktop's JSON output (≤1 MiB; non-JSON or truncated output is
dropped, stderr is reported as a byte count only). The local audit
`desktop-audit.jsonl` and `desktop status` record time, resident, app, verb
and outcome only: never refs, typed text, window titles or snapshot contents.

## S2 clauses touched

- Protocol: `desktop` is input-required. The Cable envelope and the house's
  retained row carry only a receipt (verbs, apps, `input_size`); the full
  payload (typed text, refs) is fetched once over the device-authenticated
  input endpoint, after the house grant and the local scope/switch/pause are
  checked, and every check runs again on the payload before anything acts.
  Envelope arguments never run: a desktop request without fetched input is
  refused. A lost response is not replayed.
- "Execute is general service-user authority": unchanged; desktop is not
  execute and a desktop grant never satisfies `AllowsExecute`.
- Execute runner cleanup contract: unchanged; desktop uses its own
  supervisor. The pre-approved host-lifetime exception is not used.
- Recorded lifecycle follow-ups: the same deadline caveat applies; a stopped
  or timed-out step may already have acted. Inspect by observation.

## Acceptance gates (not passed by this source change)

TCC: whether the companion's Accessibility grant covers agent-desktop as its
child (responsible process) on the real Mac. Finder snapshot → one harmless
action → snapshot in separate calls with refs resolving; refusal with the
switch off and expired; a wait/batch expiring mid-call; a nested disallowed
batch entry; a non-desktop operation regression; `execute:false` throughout.

## Menu bar (`menubar/`)

A small AppKit status item, built on the Mac with Swift 5.9+
(`scripts/build-menubar.sh` → `dist/SoulsHouseMenu.app`, ad-hoc signed like
the companion) and installed per user with `scripts/install-menubar.sh`
(`~/Applications/SoulsHouseMenu.app`, LaunchAgent `house.souls.menu` at
login, no sudo). The installer stages and verifies the new copy (bundle,
executable, `codesign --verify`) before touching the installed one, keeps the
previous copy as `.previous`, and if loading fails puts the previous copy back
and loads it. Roll back with `scripts/install-menubar.sh --rollback`; an
installed copy is refused as a source. `scripts/test-install-menubar.sh` runs
the installer against stub launchctl/codesign in a throwaway HOME.

It holds no credentials and talks only to the installed companion CLI, with
a deadline that does not depend on the child (TERM, then KILL), a 1 MiB
output cap and a bounded drain (`MenuCore.runCLI`). Changes run one at a time
in the order asked for, and a status read taken before or during a change is
dropped. A status it cannot read is shown as unknown, never as off, and in
that state the menu offers only Turn off. A failed change stays visible.
Swift tests: `cd menubar && swift test`. Calls:
`status --json` and `desktop status --json` every 5 s and when opened;
`desktop on --minutes 15|60`, `desktop on --until-off`, `desktop off`,
`pause`/`resume`, `accessibility --request`, and a button that opens the
Accessibility pane. It shows whether control is on and until when,
Accessibility granted / needs you, and the last action as resident, verb,
app and time. Expiry is enforced by the companion, not by the menu: if the
menu quits, control still turns itself off on time.

The menu does not set the app scope; `desktop scope` stays a deliberate
command.
