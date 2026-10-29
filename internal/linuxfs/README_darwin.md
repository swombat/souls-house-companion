# Darwin filesystem boundary (experimental)

This is a pure-Go `x/sys/unix` macOS 11+ port of the `linuxfs` API, not Linux
acceptance and not a completed macOS client. Linux source files are unchanged.
Darwin fixtures must be run on a Mac; cross-compilation alone is not acceptance.

## Resolution and authority

Every absolute root/protected component and every relative request component
uses fd-relative `openat` with `O_NOFOLLOW_ANY | O_CLOEXEC`.
`O_NOFOLLOW_ANY` already rejects symlinks including the leaf; Darwin rejects
combining it with `O_NOFOLLOW` with `EINVAL` (verified on macOS 26.6.2).
Unsupported flags fail closed; there is no pathname fallback. Directory opens
also use `O_DIRECTORY`; metadata uses `O_EVTONLY`. No `F_GETPATH`, `/dev/fd`,
symlink canonicalization, shell command, or path-prefix containment is used.
Absolute root/protection inputs are trusted, canonical paths: e.g. select the
Data mount through `/System/Volumes/Data`, not `/Users` or `/var` aliases.

Each component and actual-parent ancestry step must have the root's `st_dev`
**and** `fstatfs` FSID. Nested mount/firmlink/APFS volume crossings fail closed.
A grant for sealed `/` is not a grant for the Data volume through `/Users`.
Actual ancestry must reach the pinned root within 256 steps. Protected inode
pins deny hardlink, case-insensitive and Unicode-normalization aliases, and
protected directory ancestors survive renames. Protected ancestors above the
shared root are also checked, including if the shared root moves.

### Measured APFS volume-group behaviour

On native arm64 macOS 26.6.2 (25G83), System and Data report the same `st_dev`
but distinct `fstatfs` FSIDs and real fd-derived UUIDs. Lume independently
measured Data's root inode as `0x0FFFFFFF00000002`; the volume group remaps
inodes to keep them unique in its shared device namespace. `/Users/...` and
`/System/Volumes/Data/Users/...` refer to the same Data objects.

Protected and retained identities are therefore `(fd fstatfs FSID, inode)`,
not `(st_dev, inode)`. Device equality alone neither merges volumes nor proves
containment. Every boundary still checks its own root's device **and** FSID.
The native firmlink test isolates FSID refusal with equal device values; a
temporary overlay removing only the FSID comparison must turn that test red.
An explicit home-fixture test checks both protection-alias directions, alongside
the case/NFC/NFD tests. Different future macOS topology produces a visible
failure or explicit skip, not an inferred grant.

Volume discovery rejects duplicate UUIDs/FSIDs. Shared device numbers remain
denied except for exactly two APFS members with distinct UUIDs/FSIDs, a sealed
ROOTFS System and writable non-root Data, with matching OS-reported role/group
metadata. There is no public `ATTR_VOL_*` role attribute; bounded, scrubbed
`diskutil apfs listVolumeGroups -plist` supplies roles/group membership, never
identity UUIDs. A sealed System snapshot's separate fd-derived UUID must match
`diskutil info -plist`'s exact snapshot/volume UUID and group ID. Missing,
ambiguous or mismatched metadata denies the exception. No path spelling grants
a role. Persisted keys still exclude ephemeral FSIDs and device numbers.

`OpenVerified` requires an exact fresh allowed volume row, delegates retained-fd
UUID/root binding to `volumes.VerifyFD`, owns its own mount snapshot, then
validates it. Keys are volume-helper policy (UUID + root + mount path), not
filesystem `st_dev`/FSID. `Open` is fixture-only, not approval. Mount snapshots
are checked during walks and before/after delivered work. Any observed change
invalidates the root permanently. Darwin snapshots compare mount tables; a
change-and-restore between checks can be missed. No mount-event guarantee is
claimed. The Linux `VerifyMount` tuple API returns `ErrUnsupported` and
invalidates the root; production must use `OpenVerified`.

`EPERM`/`EACCES`, including TCC/privacy refusals, remain OS errors; they are not
Companion grant approvals or grant denials. This engine does not bypass TCC or
request Full Disk Access. Do not retry with weaker path resolution.

## Implemented operations and deliberate refusals

* Bounded `Read`, `Stat`, paged `List`/`ListPage`, `Mkdir`, bounded `WriteCreate`,
  `WriteOverwrite` and `WriteAppend`, no-replacement regular-file `Rename`,
  explicit private quarantine `Trash`, and bounded regular-file `TransferTo`.
* File mutations require singly linked regular targets. Directory transport,
  recursive mutation, cross-mount fallback and permanent deletion are absent.
* Creates/replacements stage in exclusive random 0600 names in the retained
  parent, fsync, recheck identities, then publish atomically. Darwin has no
  `O_TMPFILE`: partial staging is visible to local directory readers, including
  other independently opened roots. `Pending` can name **incomplete or
  complete** staging after a failure; it is never a recovery capability.
  Replacement does not preserve ownership, ACLs or xattrs.
* Create/rename/trash use `RenameatxNp(..., RENAME_EXCL)` without a weak
  fallback. Overwrite uses the same API with zero flags only after checking an
  existing singly linked regular target.
* Reads/appends/transfer source reads reopen through a **pinned parent fd and
  one leaf name**, after checking its regular-file identity. Access opens use
  `O_NOFOLLOW_ANY | O_NONBLOCK | O_NOCTTY | O_CLOEXEC`, never
  `O_TRUNC`. The resulting fd's regular type, exact identity, FSID/device,
  protected identity and actual parent ancestry are checked before any data
  I/O. Append also rechecks single-link status before each bounded write.
  Darwin has no verified procfd-style retained-inode reopen here. **An external
  writer can substitute a device between metadata check and access open; the
  open itself may cause device-specific effects before post-open rejection.**
  `O_NONBLOCK` does not prevent all device-open effects. This is an explicitly
  accepted preview boundary (Lume, GePXae/YRzkrY), not Linux's stronger
  type-before-access-open guarantee or hostile-local-writer safety.
  An ordinary same-user process cannot create device nodes with `mknod`; it can
  substitute a FIFO, which NONBLOCK opens without hanging and post-open type
  checks reject. A pre-existing same-volume device node created by root could
  still be renamed into place by a local writer with the relevant authority.
  Same-user processes already hold that user's local authority. The Mac test
  plan includes an injected FIFO swap precisely between check and access-open,
  expecting rejection without a hang; it is not runtime-proven by compilation.
* Transfer copies at most `MaxWriteBytes`, publishes through no-replacement
  create, and rechecks source identity/size/mtime/ctime. A move attempts source
  unlink only after successful publication and another exact source recheck.
  Any uncertainty before unlink retains the source; errors can report a
  committed destination with source retained. No recursive/cross-volume rename
  fallback exists; cross-root bounded file copy is explicit.

## Race and delivery limits

This is not an atomic filesystem policy transaction against a hostile local
namespace/content writer. Rechecks detect observed movement but another
process can move a parent, swap a name, add a hardlink or change staging between
the last check and a rename/unlink/write. Darwin cannot condition those syscalls
on the expected inode. Locks serialize only this Root's operations, not other
roots/processes. Namespace races must remain explicit to callers; no claim of
hostile-local-writer safety is made.

Directory protection follows current ancestry, not historical membership.
Capture individual sensitive file identities too when external hardlink aliases
matter; recapture after a trusted replacement. Inode pins prevent inode reuse.
Listings are bounded filesystem-order rescans with inode/mtime/ctime cursors,
not snapshots. Mount-boundary skips must make service search incomplete.
Cancellation is checked between syscalls, not inside blocked kernel work.
Close waits, and neither metadata verification nor mutation receipts guarantee
crash durability or atomic authorization with network delivery. The dispatcher
still owns pause/revoke, cancellation, grant binding and late-result suppression.

## Fixture verification

Run with an allowlisted environment and temporary fixtures only:

```
env -i PATH="$GO_BIN:/usr/bin:/bin" HOME="$FIXTURE_HOME" TMPDIR="$FIXTURE_TMP" \
  GOPATH="$FIXTURE_GOPATH" GOCACHE="$FIXTURE_GOCACHE" \
  go test -race ./internal/linuxfs
```

Darwin tests cover protected hardlinks/case/NFC-NFD aliases, symlink refusal,
renamed parents, escape/replacement rechecks, bounded reads/writes/transfers, no-overwrite
rename, quarantine, cursor changes, injected device/FSID mismatches and snapshot
invalidation. The one system-metadata test opens `/` and attempts only the
`/Users` directory boundary; it never lists accounts or reads personal files.
Case-fold/normalization tests explicitly skip if the fixture filesystem treats
the names as distinct. No test mounts/unmounts a live volume. Injected boundary
tests are not physical mount-race acceptance.
