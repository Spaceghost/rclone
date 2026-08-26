# Architecture

## Invariants

1. A visible path resolves to a protocol-neutral plan before any upstream I/O.
2. HTTP, S3, and mount adapters consume the same resolver contract.
3. Manifest evaluation is deterministic and fail-closed in the static phase.
4. Credentials never belong in the projection manifest.
5. A requested redirect may degrade to proxy when the backend cannot safely
   create a bounded URL; it must not expose credentials.
6. Write-through and write-back modes are configuration intent only until
   durability, conflict, retry, and recovery semantics have conformance tests.

## Flow

```text
CUE source -> manifest loader -> validated model -> resolver -> Resolution
                                                             /     |      \
                                                         HTTP     S3     mount
                                                             \     |      /
                                                               backends
                                                     rclone / HTTP / S3
```

The initial implementation stops after producing `Resolution`. Its HTTP API is
an inspection surface for this result, not yet an object-serving gateway.

## Manifest placement

Phase 1 accepts one standalone CUE file. A later discovery layer will support a
root manifest plus files embedded in or adjacent to visible directories. That
layer must specify precedence, shadowing, cycles, reload atomicity, error
reporting, and whether metadata files themselves are visible.

## Dynamic resolution

Dynamic routes are postponed. Before adopting Lua or another restricted
runtime, an RFC must define CPU, memory, recursion, output, filesystem, network,
and clock/randomness limits. Script output must still produce the same validated
`Resolution` shape and cannot directly perform upstream I/O.
