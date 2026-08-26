# Manifest reference: `v1alpha1`

A standalone CUE file exposes one concrete top-level `manifest` value.

- `upstreams.<name>.remote` is an rclone remote specification such as
  `archive:` or `media:bucket/prefix`. It may refer to any registered backend.
- `routes[].match.kind` is `exact` or `prefix`. Prefix paths other than `/` end
  in `/`; the unmatched suffix is appended to the target path.
- `routes[].target.upstream` names a manifest upstream.
- `routes[].target.path` is clean, relative, and cannot escape the upstream root.
- `routes[].access` must be `read-only` in `v1alpha1`.

Exact routes win, followed by the longest matching prefix. Duplicate matches,
file/directory collisions, unclean visible paths, and target traversal fail
remote construction.

Manifest files do not contain credentials, HTTP delivery modes, S3 credentials,
or VFS cache settings. Those belong to rclone remote configuration and command
flags.

See [`examples/basic/projection.cue`](../examples/basic/projection.cue).
