# Linux read boundary (S1)

Production integration:

1. `volumes.LoadSystem() ([]Mount, *Snapshot, error)`; close the discovery snapshot.
   Only rows with nonempty `Key` and empty `Denied` may be offered for approval.
2. `CaptureProtected(absolutePaths...) (*Protected, error)` for existing trusted
   state directories **and individual credential/unit files**, even outside the
   shared root. Capture fails on absent paths or final symlinks. Create trusted
   state before capture; ancestors of these trusted paths must also be trusted.
3. `OpenVerified(mount, relativeProtectedPaths, capture) (*Root, error)` rediscovers
   the exact allowed row, binds the fd with `statx(AT_EMPTY_PATH, STATX_MNT_ID)` and
   device major/minor, and requires the fd's `STATX_ATTR_MOUNT_ROOT` support bit
   and attribute. A same-mount ancestor/subdirectory cannot substitute for the
   mount root. It owns duplicated identity pins and its own snapshot.
   For Btrfs it also compares the retained root's `BTRFS_IOC_FS_INFO` filesystem
   UUID with the discovered UUID, then revalidates the mount snapshot.
   The caller may then close the capture and discovery snapshot.
4. `Read`, `Stat`, `List` check mount snapshots before and after work. Call
   `root.Validate()` immediately before delivery as well. Every public root
   (including fixture `Open`) invalidates permanently on detected mount changes.
   Close and fully rediscover/re-authorize instead of reviving invalid roots.

No timers: snapshots retain a mountinfo fd for `POLLPRI`, plus fresh bounded
mountinfo comparisons before and after work. Any table change invalidates,
including unrelated mounts; a retained fd alone is not authority. A caller that
uses `VerifyMount(id, major, minor)` separately gets only fd binding, not discovery
approval. `Open` is for fixtures, not production approval.

All untrusted paths use no-symlink/no-mount-crossing `openat2`. Symlinks return
`ErrSymlink`. Trusted procfs is checked with `fstatfs` and its fd directory is
retained for type-checked inode reopen. Protected identities are checked on
O_PATH descriptors before I/O/metadata. Directory ancestry is walked through
retained actual parents, so a renamed protected directory still denies nested
descendants. Walks beyond 256 parents, escape, and uncertain lookups fail closed.
Listings prove the listed parent's ancestry and inspect child inode identities
without searching each child directory. An inaccessible child does not poison
its parent listing, but opening/traversing it remains denied. Directory proc
reopens are dev/inode/type-checked before reading names.

`ListPage(ctx, path, cursor, limit) (Page, error)` supports directory continuation
beyond the first 1024 raw names. Start with nil cursor; retain `Page.Next` (opaque
`*ListCursor`) in the dispatcher's server-side search state. `Next == nil` means
exhausted; filtered pages can be empty with nonnil `Next`. `Page.Skipped` (also
`Listing.Skipped`) counts raw entries omitted for mount/subvolume boundaries.
The service must accumulate it and never call a search complete when nonzero,
even when `Next == nil`. Protected names/symlinks remain excluded by policy
rather than counted as searched scope. Reopened roots of the
same identity can continue. Directory inode/mtime/ctime/path changes refuse the
continuation with `ErrDirectoryChanged`. Rescans check cancellation every 256
skipped names and stop at one million raw offsets with `ErrBounds`, **not** a
complete result. The dispatcher still binds residency/grant/volume/expiry and
must report incomplete search on work/error bounds. This is metadata-validated
rescan pagination, not an atomic filesystem snapshot.

## Limits that must survive integration

- This is not a filesystem transaction against concurrent local mutation.
  Checks cannot atomically combine filesystem authorization with network
  delivery. The dispatcher owns cancellation and pause/revoke/late-result
  suppression. `Close` waits for in-flight calls and can block.
- A protected directory does not label every historical descendant inode.
  Files moved out, or hardlinks to files inside it, need the **individual file**
  captured too. Existing identity pins prevent inode reuse; replacing a captured
  file requires recapturing its new inode. If that cannot be supervised, do not
  grant access to a root where aliases could disclose it.
- Every looked-up inode, walked ancestor, and readable reopen must match the
  root's actual `st_dev`; `NO_XDEV` alone is not a subvolume boundary.
  Both fixture `Open` and production `OpenVerified` on Btrfs require
  `STATX_SUBVOL` at opening and on
  every such handle, with an equal subvolume ID. Missing support fails closed.
  Production discovery opens each Btrfs mount, requires statx mount ID,
  device, `MOUNT_ROOT` and `SUBVOL` support, and obtains the filesystem UUID
  with `BTRFS_IOC_FS_INFO`. It does not map anonymous subvolume device IDs
  through backing block devices or trust mount-source strings. Metadata-only
  `volumes.Discover` cannot approve Btrfs; `LoadSystem` supplies the fd probe.
  Duplicate/bind aliases and stacked mounts remain denied.
  Root descriptors are `O_PATH`; FS_INFO uses an fd-relative `openat2(".")`
  readable directory reopen with no mount/symlink crossing and a dev/inode
  check, never a second mountpoint-path lookup. Unreadable directories or
  unavailable ioctls fail closed. The UUID is the filesystem FSID, not a
  subvolume UUID; leaf confinement separately requires the root's subvolume ID.
  Injected ioctl/statx tests are not physical Btrfs acceptance.
- Non-Btrfs live UUID resolution uses `/dev/disk/by-uuid` readlink plus block-device stat.
  Observed ambiguities fail closed, but udev's single symlink per UUID can hide a
  cloned filesystem. This is not a complete block-device UUID inventory.
  Btrfs fd probing likewise is not an inventory of unmounted filesystem clones;
  distinct mounted subvolumes of one filesystem legitimately share its UUID.
- Local tests establish temporary-fixture behavior and bounded proc metadata
  checks, not Dell acceptance. Actual Btrfs, mount mutation/restore events, and a
  successful allowed real-volume `OpenVerified` still need target-host evidence.
