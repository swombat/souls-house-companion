# souls.house Companion

Linux-first computer access for explicitly authorised souls.house residents.

**Status: S1 read plane and S2 mutation/runner source merged; S3 host acceptance
underway. This is not an installation or completed Linux acceptance receipt.**

Plan: souls.house Field note PevNZe, draft 4. Tracking: [issue #1](https://github.com/swombat/souls-house-companion/issues/1).
Mira leads implementation; Lume reviews.

Read/write permissions are per mounted volume; execute is per computer. A local
ceiling independently limits remote grants. Execute runs with the service user's
OS authority; broad write access can also enable persistent execution.
On each connection, after the house's `ready`, the companion reports its
ceiling's volume keys and access per resident (no paths, no execute flag),
tagged with that connection's epoch. The house uses it only to offer folder
changes on its owner page; it authorises nothing, and every request is still
checked against the local ceiling here.

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
change. A Linux-cross-built macOS command-line preview is under development;
see [its boundaries and acceptance gates](docs/macos.md). No Mac runtime
acceptance or Windows support is claimed.

## Releases

`.github/workflows/release.yml` runs on a pushed `v*` tag or manually
(`workflow_dispatch`), on `macos-15` (Apple Silicon; both binaries are checked
to be arm64). It builds the darwin/arm64
command-line companion (`scripts/build-macos.sh`) and the menu bar app
(`scripts/build-menubar.sh`) and packages both as
`souls-house-companion-<version>-darwin-arm64.zip` with a `SHA256SUMS` file
(version is the tag, or `dev-<shortsha>` on manual runs).

Repository secrets, all six required: `APPLE_DEVELOPER_ID_P12_BASE64`,
`APPLE_DEVELOPER_ID_P12_PASSWORD`, `APPLE_TEAM_ID`, `APPLE_NOTARY_KEY_ID`,
`APPLE_NOTARY_ISSUER_ID`, `APPLE_NOTARY_KEY_P8_BASE64`.

- **All six present:** both binaries are signed with the Developer ID
  Application identity (hardened runtime, timestamp), notarised with
  `notarytool`, the app is stapled, and the result is verified. Any failure in
  those steps fails the run; there is no fallback to unsigned. A pushed tag
  then gets a **draft** GitHub release with the zip and `SHA256SUMS`, created
  only after verification passed; a manual run only keeps the signed zip as a
  14-day workflow artifact.
- **Any missing:** the zip is built unsigned (`...-UNSIGNED.zip`) and uploaded
  only as a 14-day workflow artifact named `...UNSIGNED-not-for-distribution...`.
  No release is created and the run summary says so. Never link it publicly.
- **Early unsigned build, on purpose:** a manual run with `early_unsigned`
  checked, and only when the six secrets are absent (with them the run fails
  instead), also publishes that unsigned zip as a **prerelease** tagged
  `early-unsigned-<sha>`, titled UNSIGNED. That is what the house's "Download it
  early" button may link, with the sha256 from `SHA256SUMS` shown beside it. It is
  never the main release.

The house's main download button links a build only after a signed, notarised
build has been opened on a clean Mac. Publishing the draft release is a separate, manual step.
