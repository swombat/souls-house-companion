# Companion development

The plan of record is souls.house Field note PevNZe, draft 4; implementation
conversation PevQaY; management vexAXj. Issue #1 tracks Linux v0.

- Linux first. No Bluetooth, index, browser, GUI or platform packaging work.
- Use Go 1.27.2. Prefer standard library; Linux containment uses x/sys/unix.
- Major changes require an issue, a PR branch and reciprocal review of the exact
  head before merge. Mira implements; Lume reviews. Do not publish unreviewed
  implementation directly to master.
- Run `go test -race ./...`, `go vet ./...`, `go build ./...` and check gofmt.
- Tests use temporary fixtures and an allowlisted environment, never live
  resident credentials, personal files or network destinations.
- No credentials, content, arbitrary argv, or command output in audit logs.
- Device enrollment requires explicit house authority and independent local
  ceiling approval. Local OS access is not house enrollment authority.
- Do not claim completion at scaffolding, CI, merge or deployment alone: Linux
  acceptance requires actual fixture operations on the Dell and persistence,
  denial, pause/revoke/reconnect receipts.
- Preserve user work and concurrent sessions. Keep previous binary for rollback.
  No root daemon, no broad account credential on the Dell, no private-runtime
  invocation, and no replacement of Tailscale.
