# Manifest reference: `v1alpha1`

A standalone CUE file must expose a concrete top-level `manifest` value.

- `upstreams`: named `rclone`, `http`, or `s3` declarations. Authentication is
  external to the manifest.
- `routes[].match.kind`: `exact` or `prefix`. Prefix paths other than `/` end
  with `/`; the unmatched suffix is appended to the target path.
- `routes[].target`: names an upstream and a relative, non-traversing path.
- `routes[].delivery.mode`: `proxy` or `redirect` policy intent.
- `routes[].cache.mode`: `disabled`, `read-through`, `write-through`, or
  `write-back`; write modes are reserved and not executable in the initial
  runtime.
- `routes[].cache.ttl`: optional Go duration such as `15m`.

Exact matches win. Otherwise the longest prefix wins. Duplicate match paths and
route names are rejected, as are non-clean paths and target traversal.

See [`examples/basic/projection.cue`](../examples/basic/projection.cue).
