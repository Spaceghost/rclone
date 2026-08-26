# Initial development backlog

The machine-readable source for publishing these items as GitHub issues is
[`backlog/issues.json`](../backlog/issues.json). Priorities describe sequencing,
not promises of completion.

## Core resolver and backends

- **P0 — Manifest schema and discovery:** formal CUE constraints, standalone
  loading, embedded/adjacent discovery, precedence, atomic reload, diagnostics.
- **P0 — Resolver conformance:** exact, prefix, future pattern matching,
  deterministic ordering, path encoding, metadata, negative and fuzz tests.
- **P0 — Backend contracts:** common errors, range reads, stat/list capability,
  redirect capability, cancellation, retries, observability.
- **P1 — Upstreams:** rclone remote adapter, HTTP adapter, and S3 adapter with a
  shared conformance suite.
- **P1 — Cache:** bounded read-through cache first; only then specify and build
  write-through/write-back durability and recovery.
- **P2 — Restricted dynamic resolver RFC:** sandbox and resource limits before
  choosing Lua or another runtime.

## HTTP adapter

- **P0 — Object gateway:** GET/HEAD/range/conditional requests using the core
  resolver, with explicit redirect-versus-proxy fallback behavior.
- **P1 — Operations:** structured errors, request IDs, metrics, tracing, reload
  health, and security limits.

## S3 adapter

- **P0 — Read-only surface:** GetObject, HeadObject, and listing semantics over
  the same resolver; document path/key normalization and error translation.
- **P1 — Compatibility:** signed requests, multipart/range behavior, conformance
  tests against common SDKs, and capability-driven redirect/proxy behavior.
- **P2 — Writes:** only after cache/write consistency semantics are approved.

## Desktop and mount

- **P1 — Mount spike:** evaluate WinFsp, FUSE, and rclone integration boundaries;
  measure cancellation, random reads, directory enumeration, and unmount safety.
- **P2 — Desktop control plane:** manifest selection, validation diagnostics,
  status, logs, cache controls, safe credential-provider integration, upgrades.
