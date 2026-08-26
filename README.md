# rclone-projection-vfs

`rclone-projection-vfs` is an experimental programmable projection layer for
presenting objects from multiple upstreams as one declarative virtual tree. It
is new, private work: this repository is intentionally independent of
[`Spaceghost/rclone`](https://github.com/Spaceghost/rclone) and does not modify
or fork that repository.

The first safety boundary is deliberately small:

- manifests are CUE files containing a concrete `manifest` value;
- routes are static exact or prefix matches;
- one resolver result is shared by protocol adapters;
- upstream kinds are declared but I/O backends are not yet implemented;
- HTTP currently exposes a read-only resolution API, not file proxying;
- Lua or another restricted scripting runtime is deferred until the static
  manifest and conformance suites are trustworthy.

## Quick start

```powershell
go run ./cmd/projectiond -manifest ./examples/basic/projection.cue
Invoke-RestMethod 'http://127.0.0.1:8080/v1/resolve?path=/docs/readme.txt'
```

The daemon also exposes `GET /healthz`. A missing or invalid manifest fails at
startup. See [the architecture](docs/architecture.md), [manifest reference](docs/manifest.md),
and [backlog](docs/backlog.md) for boundaries and planned work.

## Development

```powershell
go test ./...
go vet ./...
go build ./cmd/projectiond
```

CI runs the same checks on GitHub-hosted Ubuntu and Windows runners. No
self-hosted runner is configured.

## Status

This is an initial development program, not a production VFS. In particular,
mounting, upstream I/O, S3 compatibility, redirects, proxying, caching, writes,
embedded-manifest discovery, and dynamic scripting remain backlog items.
