---
title: "Kopia snapshots"
description: "Read encrypted Kopia snapshots through an rclone backup remote"
versionIntroduced: "development"
---

# Kopia snapshots (experimental)

The `backup` backend embeds Kopia in rclone and exposes an existing repository as
an immutable, read-only filesystem. No Kopia executable, WebDAV bridge or mounted
repository is used by this backend.

This is an experimental implementation. It is not yet a complete `rclone backup`
command or a production-qualified replacement for Kopia's backup and restore CLI.

## Configure an existing repository

Run `rclone config`, create a remote named `vault`, and choose `backup`. Set
`remote` to the directory containing an existing Kopia **filesystem or rclone
provider** repository, and enter its repository password when prompted.

The resulting configuration has this shape:

```ini
[vault]
type = backup
remote = /path/to/kopia-repository
password = <value written by rclone config>
```

The password field uses rclone's normal obscured-password handling. Obscuring is
not encryption of the configuration file; protect the configuration itself.

A storage remote such as `onedrive:Backups` may be used for an existing sharded
repository. Compatibility of individual remote implementations must still be
validated. The adapter does not currently open flat repositories created with
Kopia's native S3, Azure or other native-cloud providers. Do not rename or move
repository objects to try to convert their layout.

## Browse and restore

```sh
rclone lsd vault:snapshots
rclone ls vault:latest
rclone copyto vault:latest/documents/report.pdf ./restored-report.pdf
rclone copy vault:snapshots/SNAPSHOT_ID/documents ./restored-documents
rclone backend sources vault:
```

`SNAPSHOT_ID` is the full immutable manifest ID shown by the listing, not a date.
Each `snapshots/<id>` entry exposes that snapshot's root. Snapshots whose root is
a single file are exposed as files rather than artificial directories.

`latest` selects the latest complete snapshot **only when the selected catalogue
contains one source**. It is absent when multiple sources would make the choice
ambiguous. Set `source` to an exact identifier returned by `backend sources`, or
use a snapshot ID directly:

```ini
source = user@hostname:/path/to/source
```

The catalogue, including `latest`, is pinned for the lifetime of the filesystem
connection. A new rclone invocation sees newly committed snapshots. Long-lived
mounts and cached remote-control filesystems do not automatically advance their
view; reopen the connection to refresh it. This prevents one restore from mixing
files from different snapshots.

Byte-range and seek requests are passed to Kopia's seekable object reader. Only
required content is reconstructed, although metadata packs, chunk boundaries and
storage-provider behavior can add read amplification.

## Incomplete snapshots and errors

Incomplete or error-marked snapshots are exposed under `incomplete/<id>` and are
never chosen as `latest`. A completed file can be read from such a snapshot when
its referenced data is present and valid. This does not make an interrupted file
complete or turn damaged content into zero-filled output.

An unreadable snapshot manifest fails catalogue loading instead of being silently
ignored. This version does not provide a damaged-repository salvage mode.

Ordinary writes, deletions and timestamp changes are denied. Do not use `sync`,
`purge` or `delete` to manage repository history. Retention and maintenance remain
Kopia operations until explicit transaction-aware commands are implemented.

## Metadata and supported entries

Regular files, directories, logical sizes and modification times are supported.
Symlinks and unsupported entry types produce an explicit error; they are not
silently followed or omitted. Full permissions, ownership, ACLs, extended
attributes, hardlink and sparse-file fidelity require Kopia's own restore path.
No whole-file hash is advertised: a keyed Kopia content ID is not a portable
rclone file checksum.

## Local state and lifecycle

Repository passwords are passed in memory, not written into the temporary Kopia
connection JSON. That JSON refers to rclone storage configuration rather than
copying resolved provider credentials. Keep sensitive credentials in the normal
rclone config rather than embedding them in an on-the-fly remote string.

By default, connection files and caches are temporary and removed during clean
shutdown. A process crash can leave temporary directories for later cleanup.
`cache_dir` optionally selects a caller-owned persistent local cache; it is not
removed at shutdown and must not be placed inside the repository. Cache sweep
targets are 256 MiB for content and 64 MiB for metadata. These are cache targets,
not a hard bound on total memory, index data or disk usage.

No local cache is required for independent recovery. The repository format and
sharded layout remain Kopia's, so stock Kopia can connect using its matching
filesystem or rclone provider and restore without this branch.

## Implementation status

Implemented: in-process repository connection; sharded blob adapter; immutable
snapshot catalogue; regular-file reads, seeks and ranges; ordinary rclone copy;
local encrypted-snapshot interoperability tests; read-only mutation guards.

Not implemented: user-facing snapshot creation, retention, garbage collection,
remote write qualification, flat native-cloud layout support, complete restore
metadata, automatic view refresh, source change cursors or changed-byte-range
acceleration. The low-level storage library has experimental direct-local write
support for development and tests, not an exposed backup command. Power-loss,
filesystem-specific durability and concurrent maintenance require further testing.
