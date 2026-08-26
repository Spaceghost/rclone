# rclone projection backend

This repository is a pre-integration staging harness for an rclone backend named
`projection`. The deliverable is not a competing VFS: it is an rclone remote
that maps a declarative CUE tree onto other configured rclone remotes.

The backend participates in normal rclone commands and integrations:

```powershell
go build -o rclone-projection.exe .
./rclone-projection.exe config create projected projection manifest=examples/basic/projection.cue
./rclone-projection.exe lsf projected:
./rclone-projection.exe copy projected: local-copy:
./rclone-projection.exe mount projected: X: --read-only
./rclone-projection.exe serve http projected: --read-only
./rclone-projection.exe serve s3 projected: --read-only
```

The custom binary is rclone with one out-of-tree backend imported, following
rclone's supported out-of-tree pattern. HTTP, S3, mount, RC, filtering,
bandwidth control, and VFS caching remain rclone responsibilities.

## Current safety boundary

- one standalone CUE manifest is loaded at remote construction;
- upstreams are ordinary rclone remote specifications;
- static exact-file and longest-prefix routes are supported;
- `NewObject`, directory listing, object metadata, ranges, and reads delegate to
  the selected upstream object;
- all mutations fail with rclone's permission-denied error;
- scripting, embedded manifests, reload, writes, and advanced optional features
  remain backlog work.

rclone v1.75 logs `no overview data found for "projection"` for an out-of-tree
backend because backend overview data is embedded in the rclone module. The
warning is non-fatal in this staging build; adding
`docs/data/backends/projection.yaml` in the in-tree feature branch removes it.

## Development

```powershell
go test ./...
go vet ./...
go build .
```

CI uses GitHub-hosted Ubuntu and Windows runners. No self-hosted runner is
configured or claimed.

## Repository route

The recommended integration destination is a feature branch in
`Spaceghost/rclone`, with the package moved to `backend/projection`, registered
in `backend/all`, documented, and exercised by rclone's backend test suite.
That public fork has not been altered. If development must remain private before
an upstream discussion, use a private mirror of the full rclone repository and
later move the backend commits onto the public fork; do not ship this staging
harness as an unrelated storage product.

See [architecture](docs/architecture.md), [manifest](docs/manifest.md),
[integration route](docs/integration.md), and [backlog](docs/backlog.md).
