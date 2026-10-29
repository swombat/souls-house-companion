# Native Mac runtime checkpoint — 29 October 2026

Native arm64, macOS 26.6.2 (25G83), logged-in GUI owner, reached through the
existing Dell → Mac SSH route. No trust checks, TCC or quarantine flags changed.
No Go/Apple build infrastructure installed on the Mac: Linux-cross-compiled
test executables run with an allowlisted environment and disposable fixture
HOME/TMPDIR. All file contents accessed by tests are synthetic fixtures;
volume tests additionally inspect kernel mount metadata and mount-root fds.

## Failures found in e1fc753

- Darwin rejects `O_NOFOLLOW | O_NOFOLLOW_ANY` with `EINVAL`. Separate raw-open
  probes establish that either alone works. The component-wise walk now uses
  `O_NOFOLLOW_ANY` (including the leaf), without removing nofollow protection.
- The absent-state startup fixture used a non-private temporary directory on
  this host. More importantly, `privateDirectory` erased `os.ErrNotExist` for
  a genuinely absent directory, defeating idle startup. Preserve the original
  filesystem error; test absent directory, absent state, and unsafe directory
  separately. Do not treat unsafe/corrupt state as idle.
- Darwin listing silently omitted a FIFO without increasing `Skipped`, making
  recursive search claim completeness. Count unsupported file types as skipped,
  retaining hidden protected names as intentionally omitted.

After those bounded corrections the complete filesystem fixtures pass natively:
mutations, reads/appends/transfers, FIFO replacement without hanging,
case/NFC/NFD protection, parent/leaf races and root-to-Users firmlink denial.
The existing client, policy and process-group runner fixtures also pass.
Runner coverage is not proof of containment of detached descendants.

## Still blocked: System/Data identity

Actual fd probes successfully retrieve real, distinct System and Data UUIDs,
but report equal `st_dev` on this host. The reviewed discovery policy explicitly
requires distinct device evidence and therefore denies Data. This checkpoint
does not relax that rule or call the real mount-acceptance test passed.
The opt-in evidence test reports only comparisons and fixed denial strings,
not private mount paths, UUIDs or device identifiers.

A revised identity policy needs explicit review, including mount-boundary
checks and protected-inode semantics when device numbers are shared. Until
then: no install, owner enrollment, remote fixture round trip or Pow artifact.
The original candidate archive remains unchanged.

## Remaining gates

The Rails integration test is skipped without its dedicated fixture server.
LaunchAgent bootstrap/pause/resume, owner-disabled state, next-login persistence,
TCC/Gatekeeper context, fresh independently approved ceiling and owner enrollment,
end-to-end operations and revoke/uninstall are not established by these tests.
Execute opt-in remains the owner's decision. No Intel/Rosetta runtime result
is claimed. Dell enrollment is unchanged.
