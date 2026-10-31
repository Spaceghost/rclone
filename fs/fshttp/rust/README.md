# Experimental Rust HTTP transport

This is the first transport slice of an incremental rclone migration. Existing
Go backends, configuration, authentication, provider SDKs, sync operations, and
VFS remain in use. The default build and default runtime path use Go networking.
Rust is enabled only by both the `rusthttp` build tag and
`RCLONE_EXPERIMENTAL_RUST_HTTP=1`.

The experiment improves recovery in a controlled stalled HTTP/2 stream test.
Current local throughput benchmarks are slower than Go; the general speed gate
has not been met. Keep results alongside the
tested upstream revision, toolchain versions, hardware, operating system, and
benchmark settings.

## Build and run

Install Go at the version required by the root `go.mod`, Rust/Cargo, and a C
compiler suitable for cgo. This first implementation is enabled on Linux only.
The Rust target and the C compiler must have compatible architectures and ABIs.
Build the Rust static library before the Go binary:

```sh
cargo build --release --manifest-path fs/fshttp/rust/Cargo.toml
CGO_ENABLED=1 go build -tags rusthttp -o rclone-rust .
RCLONE_EXPERIMENTAL_RUST_HTTP=1 ./rclone-rust version
```

On Windows, run these experimental build and test commands inside a Linux
environment such as WSL. Native Windows builds continue to use Go networking.
A Go-only build remains available with ordinary `go build`; it does not require
Cargo or a C compiler.

After rebuilding the external Rust archive, run `go clean -cache` before
rebuilding or testing Go. Go's build cache can otherwise reuse a binary linked
against an older archive. A distinct linker build ID, such as
`go test -ldflags=-buildid=rusthttp-rebuild-1 -tags rusthttp ./fs/fshttp`, also
forces a new link for a focused test run.

## Supported first slice

Rust handles supported, bodyless `GET` and `HEAD` requests over HTTP and HTTPS.
It uses a shared client and pool and streams response bodies with cancellation.
Each response body has its own progress deadline, so traffic on a sibling
HTTP/2 stream cannot conceal a stalled transfer. Healthy streams can continue
beyond the timeout duration while they keep delivering data. Response headers
have a separate finite budget that also stops a peer trickling header bytes.
The Go `http.Client` retains redirect and cookie policy. Request signing and
OAuth stay above the transport. The Rust engine does not follow redirects,
replay failed requests, or perform provider-level retries.

The integration chooses the Go transport before dispatch for unsupported
requests or configuration. This includes request bodies and methods outside
the initial slice, proxy connections, custom transport callbacks, and TLS or
socket configuration that has not been ported. A Rust failure is returned to
the caller; it never triggers an automatic replay through Go. FTP, SFTP, SMB,
and other non-HTTP protocols have separate networking paths.

The Rust path currently accepts at most 100 HTTP/1 response header fields, with
a 10 MiB header buffer limit. This is narrower than Go's header compatibility.
Use the Go path for endpoints that need a larger header count. Rust uses
rustls with native certificate roots; `SSL_CERT_FILE` and `SSL_CERT_DIR` select
native trust sources. It caches the TLS configuration by those environment
values. Go and Rust use different TLS implementations, so trust and protocol
compatibility still need testing for each deployment. Rclone's explicit custom
CA and client-certificate options retain the Go path.

## Timeout behavior

This experiment keeps rclone's existing defaults: `--contimeout 1m` and
`--timeout 5m`. The Rust TCP/TLS connection deadline uses `--contimeout`. Its
absolute response-header budget is conservatively the sum of `--contimeout`
and `--timeout`, measured from request start. Socket inactivity and each
response stream's wait for body progress use `--timeout`. These are separate
budgets; a healthy large transfer is not stopped merely for lasting longer
than `--timeout`. A zero timeout disables the corresponding deadline.

For a workload that needs shorter failure detection, choose explicit values,
for example `--contimeout 10s --timeout 30s`. Those values give this Rust path a
40-second response-header budget and a 30-second body progress budget. They
are an optional per-command choice, not a change to the default settings.
Transport errors identify the failing phase and retain Go `net.Error` timeout
semantics, so existing backend retry decisions remain available.

Canceling a request or closing its response body releases a pending stream or
bandwidth wait. New requests use Go while a transport bandwidth limit is active;
an already active Rust response observes a limit enabled during the transfer.

This is an opt-in experiment. Existing Go behavior is the compatibility
reference. Do not make Rust the default until configuration, streaming,
cancellation, TLS, rate limits, and backend integration tests cover the intended
deployment. The C ABI is an internal implementation detail, not a stable plugin
interface.

## Verify and measure

After building the Rust library:

```sh
CGO_ENABLED=1 go test -tags rusthttp ./fs/fshttp -run TestRustTransport -count=1
CGO_ENABLED=1 go test -tags rusthttp ./fs/fshttp -run '^$' -bench '^BenchmarkHTTPTransport$' -benchmem -count=8 -benchtime=1s
go test ./fs/fshttp
```

`BenchmarkHTTPTransport` compares the same rclone HTTP wrapper using Go and
Rust against the same local HTTP server. It measures empty, 4 KiB, and 1 MiB
responses, serially and in parallel, after warming the connection pool. Payloads
are consumed and checked, so bytes/second describe completed responses.
Allocation statistics describe Go allocations only; also measure total process
RSS and CPU to include Rust memory and its runtime.

Go's `-cpu` option changes `GOMAXPROCS`, not the Rust runtime's worker count.
Restricting only Go can give Rust more CPU capacity and produce a misleading
comparison. Keep the same total process CPU budget for both engines. The local
fixture also shares the Go test process; use a separate fixture process when
measuring CPU-normalized end-to-end results.

These local results expose transport and bridge overhead. They are not cloud
transfer or full-rclone benchmarks. Before a performance claim, compare the
unmodified upstream binary, this branch with Go selected, and this branch with
Rust selected on real rclone operations. Include metadata/listing workloads,
many small files, large streams, realistic latency, concurrency, and failures.
Keep request counts, retry policy, TLS verification, compression, and connection
settings equivalent. Use repeated runs, report variability, and check tail
latency, CPU, memory, and correctness as well as throughput. An experiment that
misses its agreed performance gate stays opt-in.

## Future Lua and Nelua remotes

Lua/Nelua remotes are not implemented by this transport. A later, versioned
remote contract should preserve rclone's `Fs`, `Object`, and optional feature
semantics while letting existing Go and new remotes coexist.

The proposed first contract includes remote configuration and capability
discovery; paginated listing and stat; streamed and ranged reads; streamed
create/update; directory and object removal; cancellation; and structured
errors. Optional capabilities advertise server-side copy/move, hashes,
metadata, and multipart operations instead of requiring every remote to
implement them.

The host should provide networking, reusable authentication and signing,
pagination helpers, pacing, bounded streams, logging, and secret references.
Remote authors implement provider-specific behavior and declare required
capabilities. Opaque resource handles and explicit lifetimes can support both
Lua bindings and compiled Nelua through the same host contract. The plugin ABI,
isolation model, authoring tools, and a conformance suite need a separate design
and implementation before any existing Go remote is retired.

Keep upstream history, licensing notices, backend tests, configuration
compatibility, and a practical Go contribution workflow throughout the
migration. New remotes should pass the same backend behavior tests as their Go
counterparts.
