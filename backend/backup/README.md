# Native Kopia integration

This implementation embeds Kopia v0.23.1 and exposes existing snapshots through
rclone's normal filesystem API. See [the user guide](../../docs/content/backup.md)
for configuration, restore examples and current restrictions.

## Current architecture

```text
rclone ls / copy / range reads
             |
backend/backup: immutable Fs + Object views
             |
Kopia snapshot / snapshotfs / repository / object readers
             |
lib/backup/kopiablob: blob.Storage over an rclone Fs
             |
existing sharded filesystem/rclone-provider repository
```

`lib/backup/kopiarepo` owns repository lifecycle, private connection metadata,
cache configuration and embedded fatal-error handling. There is no subprocess,
loopback server or VFS in this path. Ordinary filesystem mutation methods are
read-only. The local storage writer is a development/library capability, not a
public backup-creation command.

## Existing work reused

- Kopia's [rclone provider](https://github.com/kopia/kopia/blob/v0.23.1/repo/blob/rclone/rclone_storage.go)
  establishes the existing storage relationship. Its subprocess, WebDAV, RC and
  VFS-cache invalidation machinery is not imported here.
- Kopia's [sharded storage](https://github.com/kopia/kopia/tree/v0.23.1/repo/blob/sharded)
  is reused directly, including `.shards` discovery, path layout and enumeration.
  No independent repository format or chunk index is implemented.
- Kopia's repository, authenticated object readers and snapshot filesystem are
  used directly. Integration tests use its existing local filesystem adapter and
  snapshot uploader to create real encrypted snapshots.
- rclone's config/password handling, backend registration, Fs/Object contracts,
  range options and ordinary copy implementation provide the user-facing surface.

Two CLI-oriented behaviors are intentionally not reused: the default fatal
handler that exits the process, and bulk snapshot loading that logs and drops
unreadable manifests. Embedded repository failure must not kill unrelated rcd
jobs or silently select an older backup.

## Invariants

1. An opened snapshot catalogue is immutable. `latest` cannot switch while a
   restore is reading it, and it cannot choose silently among unrelated sources.
2. Incomplete/error-marked snapshots are separate from successful snapshots.
3. Reads do not initialize storage, publish manifests or run repository actions.
4. Read errors, invalid ranges and catalogue errors propagate. Missing bytes are
   not converted to successful output.
5. Unsupported storage guarantees fail closed. Generic `Move` support does not
   prove atomic replacement; conditional-create and retention options must not be
   emulated with a race-prone existence check.
6. Local caches are disposable; independent stock Kopia recovery remains possible.
7. Keyed chunk/object IDs are not misrepresented as whole-file checksums.

## Tests

```sh
go test -race -count=1 ./lib/backup/... ./backend/backup/
CGO_ENABLED=0 go build .
```

Storage tests cover precise ranges, missing blobs, cancellation, callback errors,
short uploads, failed replacement preservation, parallel writes, idempotent
removal, read-only access, traversal rejection and stock sharded-layout exchange.

End-to-end tests create encrypted snapshots with Kopia's uploader, check shared
file-content objects within and across snapshots, check that an unchanged backup
adds no data packs, restore a single file through `operations.Copy`, exercise
ranges/seeks, pin `latest`, select sources, reject mutation, and recover through
stock Kopia's filesystem provider without the rclone storage adapter. Lifecycle
tests check password-free connection JSON and temporary/cache cleanup.

These fixtures do not qualify every remote, every filesystem or abrupt power
loss. A passing in-memory/unit or local test is not a durability certification.

## Next implementation gates

- Add explicit init and transactional backup commands using Kopia's uploader and
  write-session APIs. Include dry-run behavior, source/repository overlap checks,
  stable source identity, filter fingerprints, cancellation and commit semantics.
- Qualify one object-store write adapter with storage-contract and fault-injection
  tests, then expand coverage. Keep storage layout selection explicit so native
  flat-cloud repositories are never confused with the existing sharded layout.
- Test power loss, failed flushes, concurrent writers/maintenance, corrupt packs,
  full metadata restores, filesystem cache lifetime and all supported platforms.
- Integrate rclone transfer accounting, logging, RC jobs and configurable cache
  budgets before claiming polished operational parity.
- Only then add ZFS/cloud change cursors and changed-range acceleration.

Retention, pruning, garbage collection, new crypto and new chunking formats are
not reimplemented in this backend.
