# S2 Linux execute supervision primitive

`Execute(ctx, Request{Argv, Cwd, Stdin, Timeout}) (Result, error)` is a standalone
internal primitive, **not wired to policy, service, client, or CLI**. Integration
must authorize full local-user execution, manage pause/revoke cancellation, and
suppress stale delivery. Execute is not filesystem confinement or a sandbox:
an explicit `/bin/sh -c ...` is permitted. A program with full user authority
can deliberately start another user unit or otherwise evade its original
cgroup; this protects ordinary descendants (including setsid/double-fork), not
hostile code with the same user-manager authority.

## Runtime contract

- Requires Linux, systemd **254+** (for `--expand-environment=no`), ordinary
  `/usr/bin/systemd-run`, `/usr/bin/systemctl`, and the current effective UID's
  running user manager at `/run/user/UID/bus`. There is no sudo, system-manager,
  linger enablement, daemon setup, or process-group fallback.
- Each operation generates a random 128-bit `companion-exec-….service` name.
  No request-controlled unit name, description, property, or environment is
  accepted. Description is always `Companion managed execute`.
- Uses `systemd-run --user --pipe --wait --collect --quiet`, `Type=exec`,
  `KillMode=control-group`, `SendSIGKILL=yes`, `TimeoutStopSec=1s`,
  `TimeoutStartSec=5s`, `RuntimeMaxSec=<timeout>`, `Restart=no`, and `Delegate=no`.
  Manager stdout/stderr pipe forwarding avoids journaling command output.
  Command/arguments remain visible to the OS/user manager as expected for full
  local execution; host-level audit policy is outside this primitive.
- `Cwd` must be absolute. Relative executable paths with a slash are joined to
  `Cwd`. Bare executable names use the launcher's fixed `/usr/bin:/bin` PATH.
  Systemd executes the command directly, never a shell. `$` arguments stay
  literal; only a caller-explicit shell interprets them. **Do not add an
  `env -i` wrapper**: `env`'s execvp ENOEXEC fallback can implicitly run a shell.
- Supervisor clients receive only fixed PATH, LANG, and kernel-UID-derived
  XDG_RUNTIME_DIR/DBUS_SESSION_BUS_ADDRESS. They do not inherit the caller's
  HOME, loader variables, tokens, or DBus overrides.
- The **service** receives fixed `PATH=/usr/bin:/bin`, `LANG=C.UTF-8`, and the
  user manager's existing trusted login environment (such as HOME/USER/LOGNAME).
  This is not a fresh login shell; profile files are not read. The package
  accepts no environment input and does not import the caller environment.
  A dirty/compromised manager environment can still affect programs. Fully
  scrubbing all service variables requires a trusted direct-exec wrapper or
  another design; it is not honestly provided by a CLI-only `env` trick.
- Stdin is bytes (nil means EOF), never a shell string. Both streams preserve
  binary data, retain at most **1 MiB each**, mark truncation, and keep draining
  overflow. The caller bounds request size/concurrency; this package adds no
  stdin or argv-size limit. No output, argv, cwd, stdin, or systemctl diagnostic
  is included in returned errors or package logs (there are no package logs).
  `Result` is sensitive data, including possible systemd-run diagnostics on
  stderr; never audit/log it wholesale.
- Timeout must be positive and <=300 seconds. Context cancellation and
  deadline use independent bounded cleanup. Normal completion also cleans up,
  including descendants left after the main process exits.

## Cancellation protocol and honest limits

Killing the `systemd-run` client is **not** containment: it can leave its service
running, or race a late `StartTransientUnit` request. The primitive retains the
client while issuing bounded `systemctl --user stop`, then `kill --kill-whom=all
--signal=KILL`, and structured `show` checks. Missing/inactive units are not
accepted until the client has settled **and a subsequent full stop/kill/check
round** confirms inactive loaded/not-found state. This handles immediate
cancellation during ordinary startup, and does not mistake an initial missing
unit for a canceled submission. A random name avoids old-unit collisions.

Cleanup gets its own 10-second context, with individual controls bounded to two
seconds. Only after that budget fails is the client itself killed/reaped
(up to three additional seconds, with a two-second pipe wait delay). The client
kill is resource cleanup, not cgroup proof. Under an unresponsive/disconnected
manager or ambiguous receipt, **ErrCleanup means quiescence is unconfirmed**;
do not report a successful cancellation, retry the operation, or imply that no
command can execute later. Manager startup/runtime/stop limits provide a
backstop if the manager is functional, not a guarantee against manager/kernel
failure. A pending submission may still run later. CLI supervision cannot
atomically eliminate that possibility under arbitrary bus/manager failure.
All operations assume functioning, trusted systemd binaries and manager.

`ExitCode` is systemd-run's exit status (normally the command's forwarded
status), or -1 when none is available. Nonzero exits do not return a Go error.
The CLI can also exit nonzero for a startup/manager/unsupported-option failure;
without a separate structured manager receipt it is **not possible to
distinguish every such failure from command exit**. Errors are redacted sentinel
values; a joined ErrCleanup takes precedence for supervision decisions. Failed,
active, activating, deactivating, partial, or ambiguous unit states never count
as confirmed cleanup. There is no direct cgroup filesystem emptiness proof;
confirmation relies on the trusted manager's stop/kill and inactive state.

## Evidence, not simulations of containment

Unit tests inject a small private backend to check argv/env construction,
bounded binary captures and continued drain, timeout/cancel sequencing, late
registration fencing, redaction, state parsing, and explicit cleanup failure.
They do **not** establish process-tree containment.

Opt-in actual-manager test:

```sh
COMPANION_RUNNER_SYSTEMD_TEST=1 go test -race -v ./internal/runner -run TestSystemdUserIntegration
```

It first verifies real client binaries and an accessible ordinary user manager.
It skips otherwise; it never changes linger or requires sudo. Only temporary
fixtures and allowlisted subprocess environments are used. It exercises stdin,
cwd, literal argv, exit status, no implicit shell for a no-shebang executable,
and real setsid/double-fork descendants on timeout, cancellation, and ordinary
main-process exit. Fixtures also have local stop files and a 20-second maximum
life so a failed test does not create indefinite background children. No live
credentials or network are read. This is host-specific evidence, not Dell
acceptance or a promise against deliberately self-escaping user-authority code.
