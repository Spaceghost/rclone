# Initial rclone-backend backlog

The machine-readable issue source is
[`backlog/issues.json`](../backlog/issues.json).

## P0: backend correctness

- Formalize the CUE schema, manifest diagnostics, config options, and safe
  reload behavior.
- Make `List`, `NewObject`, root scoping, Unicode, path encoding, exact overlays,
  and longest-prefix routing pass one conformance matrix.
- Preserve upstream object metadata, hashes, MIME types, ranges, mandatory open
  options, cancellation, and canonical rclone errors.
- Add unit, fuzz, and `fstest` integration coverage against real configured
  remotes.

## P1: rclone capabilities

- Define route-specific write ownership and implement `Put`, `Update`, `Remove`,
  `Mkdir`, and `Rmdir` without partial or cross-route ambiguity.
- Advertise `ListR`, copy/move, directory move, purge, public link, metadata,
  change notification, shutdown, and usage only when upstream capability
  negotiation is correct.
- Validate standard `copy`, `sync`, `bisync`, `check`, `lsjson`, `mount`, RC,
  `serve http`, and `serve s3` workflows.
- Specify how rclone VFS cache modes interact with projected writes before
  considering backend-specific write-through or write-back behavior.

## P2: programmability and contribution

- Add embedded/adjacent manifest discovery with deterministic shadowing and
  atomic reload.
- Write a sandbox RFC before selecting Lua or another restricted dynamic
  resolver.
- Move the package into a confirmed `Spaceghost/rclone` feature branch, add
  rclone docs/registration/test configuration, and open an upstream design
  discussion before a merge request.
