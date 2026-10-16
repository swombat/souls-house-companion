# souls.house Companion

Linux-first computer access for explicitly authorised souls.house residents.

**Status: S0 validation spike. Not an installed or usable remote-access service.**

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

The offline spike contains policy intersection, mountinfo identity validation
with an injected UUID map, and an fd-rooted bounded read/stat/list engine.
These are not yet connected into a service. See [the S0 checkpoint](docs/s0.md)
for verified receipts, explicit security limitations and continuation.

Linux installation, authenticated enrollment, house transport and Dell acceptance
are not yet implemented. No Mac or Windows support is claimed.
