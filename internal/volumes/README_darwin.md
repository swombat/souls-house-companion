# Darwin volume identities

This backend is pure Go with `CGO_ENABLED=0`. `LoadSystem` uses bounded
`Getfsstat`, opens mount roots with `O_NOFOLLOW_ANY`, binds `fstat`/`fstatfs`
metadata, and requests the volume's real UUID using fd-based `fgetattrlist`.
No command execution, diskutil, backing-device guesses, or path-derived UUIDs.

Only local APFS and HFS are candidates. Unknown, network, FUSE, virtual,
ambiguous, and UUID-unavailable volumes remain denied. Visible duplicate UUIDs,
devices, FSIDs, or mountpoints are denied. UUID clones not mounted/visible to the
current kernel table cannot be detected. Read-only/snapshot APFS System `/`
is explicitly denied; independently proven Data can still be granted.

`Mount.ID` packs the two kernel FSID words; `Mount.Device` is decimal Darwin
`st_dev`. Neither is persistent identity. The stable `darwin-volume:` key hashes
only the real volume UUID, root `/`, and selected mountpoint.

`VerifyFD(fd int, m Mount) error` requires the **selected mount root**, not a
descendant: it checks the current unique discovery row, UUID, FSID, device,
filesystem, mountpoint, and root inode. APFS System and Data require actual
distinct `st_dev` evidence when both rows are present. Firmlink-visible paths
such as `/Users` do not become grant roots or aliases. The filesystem layer
must reject device transitions at every descendant lookup, including firmlinks,
and continue checking retained descriptor identity.

`Snapshot.Validate` compares normalized kernel mount tables and invalidates
permanently on any observed error/change. Capacity/free-space counters and
enumeration order are excluded. Unlike Linux's retained mountinfo event fd,
this does **not** detect a change-then-restore between checks. `Close` does not
cancel a blocked syscall. FD validation and before/after snapshot checks are
still required; neither is an atomic namespace lock.

## Tests

Darwin fixtures cover packed UUID ABI/decoding, count/truncation bounds,
malformed metadata, table normalization, sticky invalidation, duplicate
identities, actual root/Data device separation, sealed System denial, and
retained temporary descriptors. Tests can be cross-compiled on Linux, but that
does not execute Darwin syscalls.

On a real Mac, opt in to actual discovery and temporary-directory acceptance
with a deliberately allowlisted environment (adjust absolute paths):

```sh
env -i PATH=/path/to/go/bin:/usr/bin:/bin HOME=/tmp \
  TMPDIR=/tmp GOPATH=/tmp/companion-gopath GOCACHE=/tmp/companion-gocache \
  CGO_ENABLED=0 COMPANION_DARWIN_ACCEPTANCE=1 \
  /path/to/go/bin/go test ./internal/volumes
```

The acceptance test reads only kernel metadata and newly created temporary
fixtures. It opens mount-root descriptors without enumerating their contents,
accepts the actual selected root, and rejects a child and a wrong UUID.
Real syscall/ABI acceptance is still required before claiming Mac readiness.
