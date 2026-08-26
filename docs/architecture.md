# Architecture

## Position in rclone

`projection` is an overlay backend implementing rclone's `fs.Fs` and
`fs.Object` contracts. A manifest resolves visible paths to another rclone
remote and a relative object path. The backend delegates object operations to
that upstream filesystem.

```text
rclone command / mount / serve http / serve s3
                       |
                 projection fs.Fs
                       |
              CUE manifest + resolver
                       |
        configured rclone upstream fs.Fs instances
          local / S3 / HTTP / any other backend
```

There is no separate HTTP or S3 gateway. `rclone serve http projection:` and
`rclone serve s3 projection:` consume the same backend through standard rclone
interfaces. Likewise, desktop mounting is `rclone mount projection:` and cache
behavior is controlled by rclone/VFS flags and wrapper backends.

## Phase-one semantics

The backend registers through `fs.Register`, receives its manifest path through
the normal config mapper, and constructs manifest upstreams through rclone's
filesystem cache. It advertises itself as an overlay.

`NewObject` resolves one exact or longest-prefix route and wraps the upstream
object with the visible remote name. `List` delegates within a prefix route and
synthesizes ancestor directories or exact-file entries required by the
manifest. More-specific manifest entries override delegated directory entries.

Phase one is read-only. Although `fs.Fs` requires mutation methods, they return
`fs.ErrorPermissionDenied`. The backend does not claim server-side copy/move,
recursive listing, public links, metadata propagation, change notification, or
write support until each capability has rclone conformance tests.

## Invariants

1. All upstreams are rclone remotes; credentials stay in rclone configuration.
2. Resolution is deterministic and performs no I/O.
3. Visible paths and target paths are clean and cannot traverse roots.
4. Listing and direct lookup must agree on the same visible object identity.
5. Unsupported writes fail before consuming input or changing an upstream.
6. Optional rclone features are advertised only when their semantics are proven.
7. Scripting can emit only a validated resolution and cannot call upstreams.

## Cache and delivery

Redirect-versus-proxy is not a core manifest concern. A future `PublicLink`
capability may delegate to upstreams where rclone consumers can use it safely,
but `serve` behavior remains owned by rclone.

Read/write caching should first reuse rclone's VFS cache and cache-related
backends. Any backend-level write-through or write-back design needs an rclone
semantics RFC covering commit points, conflicts, retry, recovery, and interaction
with `mount` before implementation.
