# souls.house Companion

Linux-first computer access for explicitly authorised souls.house residents.

**Status: S1 read plane and S2 mutation/runner source merged; S3 host acceptance
underway. This is not an installation or completed Linux acceptance receipt.**

Plan: souls.house Field note PevNZe, draft 4. Tracking: [issue #1](https://github.com/swombat/souls-house-companion/issues/1).
Mira leads implementation; Lume reviews.

Read/write permissions are per mounted volume; execute is per computer. A local
ceiling independently limits remote grants. Execute runs with the service user's
OS authority; broad write access can also enable persistent execution.

Go 1.27.2 is pinned. Development checks:

```sh
go test -race ./...
go vet ./...
go build ./...
```

The source includes authenticated enrollment, house transport, a user service,
local policy ceilings, fd-rooted reads and bounded directory continuation,
regular-file mutations and supervised execution. See [S1](docs/s1.md),
[S2](docs/s2.md), and the [filesystem boundary](internal/linuxfs/README.md).
Directory support remains staged: bounded listing is not unrestricted recursive
search, skipped scope is incomplete, and mkdir creates only one directory.
Directory copy/rename/move/trash are not supported.

Btrfs discovery and opened-root UUID binding are implemented fail-closed;
fixture tests do not establish actual-host subvolume or lifecycle acceptance.
No installed service or completed Dell acceptance is claimed by this source
change. No Mac or Windows support is claimed.
