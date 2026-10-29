# macOS command-line preview

Targets macOS 11+ arm64/amd64, built on Linux with Go 1.27.2, CGO_ENABLED=0.
**Cross-build is not runtime acceptance.** No GUI/Bluetooth/root daemon or
Apple toolchain is required. Pairing and remote grants remain in souls.house.
Current actual-host acceptance is not established; do not call this ready.

## Installation and trust

The Go internal linker adds an arm64 ad-hoc signature. This is **not Developer
ID signing or notarisation**. Verify archive checksums from a trusted channel
with `shasum -a 256`; a checksum is not an authenticity signature. Choose arm64
for Apple Silicon, amd64 for Intel. `arch -x86_64` on Apple Silicon is Rosetta
evidence, not native Intel evidence.

Curl downloads normally have no quarantine flag; browser downloads may.
The owner must deliberately inspect/approve the artifact through their OS.
The installer never removes quarantine, bypasses Gatekeeper, disables SIP/TCC
or grants Full Disk Access. Never paste credentials into chat.

Unpack, read this file and run `sh install.sh --accept-preview` as the owner,
not sudo. It installs under `~/.local/bin` and `~/Library/LaunchAgents`, refusing
to overwrite an existing installation. For upgrades pause first and retain the
previous binary. `install --user` writes configuration; explicit `resume`
starts an enrolled service. The GUI domain requires a logged-in GUI user.
SSH bootstrap failure is reported, not bypassed via the `user/` domain.
The installed plist can load at the next login.

## Pairing and permissions

1. `souls-house-companion volumes` shows real OS UUID-based identities.
   APFS Data is distinct from sealed System (denied); a `/` grant cannot
   traverse the `/Users` firmlink. Use Data mount-relative `Users/name/...`.
   Malformed or unexpected APFS volume-group metadata anywhere (including an
   unusual external group) disables the shared-device exception and can deny
   Data too; this is a fail-closed discovery refusal, not a privacy approval.
2. The house owner/admin creates a fresh one-use grant for this Mac and the
   selected residents. Never remint/reuse the Dell's consumed grant. Save an
   independent local ceiling JSON (docs/s1.md, docs/s2.md).
3. Feed enrollment through protected stdin, not argv/history:
   `souls-house-companion enroll --url https://souls.house --request-id ID --policy ceiling.json --token-stdin`.
4. `status --json` and `policy show` reveal no credential. State/credential is
   a 0600 file in an owned 0700 directory, not Keychain. House and local policy
   intersect; neither broadens the other.
5. `pause` durably denies before stopping; `resume` starts/reconnects.
   Status distinguishes owner-disabled Login Items from manager uncertainty.
   macOS 13+ may show “Background Items Added”; the owner may disable it.
  The companion never overrides that choice. It restarts on failure, not on
   a successful pause. Missing/unenrolled/uninstalled state exits successfully
   at login instead of entering a restart loop.

TCC may deny Documents/Desktop/Downloads, removable/network volumes and iCloud.
A background agent cannot show an interactive prompt. EPERM/EACCES receipts
say `os_permission_denied` with possible TCC/POSIX/ACL causes, separately from
a grant refusal; errno alone cannot prove TCC. Full Disk Access is the owner's
choice. Ad-hoc cdhash changes each build; permissions may need reapproval for
the new binary after upgrades. Verify instead of assuming they transfer.

Binary/plist/state/credential and LaunchAgents are protected by opened identity
from filesystem tools, including case/Unicode aliases. This is **not a
sandbox**: execute has full user authority, and broad writes elsewhere can
plant persistent execution. Protected paths do not restrict permitted commands.

## Versioned local execute acceptance

Contract ID: `darwin-process-group-preview-v1-not-proven-for-setsid-double-fork`

Each job has explicit argv/cwd/stdin, scrubbed environment, its own pgid,
1 MiB per-stream output cap (excess drained), and at most 300 seconds.
Empty stdin is EOF. On completion/cancel/pause/stop, TERM then KILL of the
group is attempted before shutdown returns. **setsid/double-fork descendants
can escape and remain running: their cleanup is NOT PROVEN.** This is weaker
than Linux cgroups, not containment.

A best-effort same-UID process-table sweep detects possible remaining group
members, newly started session members and newly reparented processes during
the operation. It may miss escapes or count unrelated apps. It never kills
ambiguous orphans. Receipts include at most 16 `possible_escaped_candidates`
(pid/pgid/ppid/comm/start time), the total count and a truncation flag; other
live companion jobs' pgids are excluded. These and `escape_scan_unavailable`
are detection receipts, never proof of containment. Never copy candidate
process names into audit logs.

Without this exact per-install local opt-in, execute is refused before input
download; read/write remain available:

```
souls-house-companion pause
souls-house-companion execute-lifecycle --accept-contract darwin-process-group-preview-v1-not-proven-for-setsid-double-fork
souls-house-companion resume
```

A changed contract ID requires fresh acceptance. Local status and every remote
execute receipt identify the weaker lifecycle. House's generic execute boolean
is authority, **not a platform cleanup promise**. This is a reviewed preview
boundary, not the final containment model.
To revoke acceptance: pause, run `execute-lifecycle --revoke`, then resume.
The service also checks unchanged acceptance immediately before delivery.

## Removal and rollback

`uninstall --user` durably denies, stops, revokes in house, erases the credential
only after confirmed revoke, and removes the plist. It retains nonsecret
receipts/ceiling; it never deletes personal files. If revoke fails, retry:
local deny and credential remain so revocation can finish. Remove binary
manually after confirmed uninstall. Re-enrollment requires a **new owner-created
grant**; never destroy the sole working enrollment without authority to restore.
Rollback uses the previous binary only when its state schema is compatible.

## Actual-host gates (not passed by cross-build)

Record OS/architecture, real UUID discovery/restart stability, case/NFC/NFD
protection, root-to-Data firmlink denial, fixture read/write/append/copy/move/
quarantine, denial without bytes, execute stdin/output/timeout/group cleanup
and an explicitly labelled escaping child, in-flight pause/reconnect,
owner-disabled Login Item, disposable-grant revoke/uninstall, next-login
persistence. Gatekeeper/TCC approvals remain the owner's. Blocked/untested
cases stay explicit. Pow's installation follows real-Mac validation.
