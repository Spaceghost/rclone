# Recommended rclone integration route

## Destination

The target should be a `backend/projection` feature branch based on current
rclone in `Spaceghost/rclone`, followed by an upstream design issue and pull
request when the backend is mature. The public fork has not been modified by
this staging work.

The in-tree change will include:

- `backend/projection` implementation and tests;
- a blank import in `backend/all/all.go`;
- backend data and documentation in rclone's expected locations;
- `fstest/test_all/config.yaml` coverage;
- an integration-test remote available to maintainers if upstream merge is
  requested;
- `go build`, `make quicktest`, backend unit tests, and a clean
  `fstest/test_all` run.

rclone v1.75 looks up embedded backend overview data during `fs.Register`. The
out-of-tree harness therefore logs a non-fatal missing-overview warning. The
required in-tree backend data file is part of the integration change, not a
reason to patch or replace the pinned rclone dependency locally.

## Why not a permanent plugin

Go plugins are only supported by rclone on macOS and Linux and must match the
exact rclone build. An out-of-tree custom rclone binary works on Windows and is
useful while iterating, but in-tree integration is the most compatible route
for the Swiss-Army-knife ecosystem.

## Privacy choice

If the design must remain private temporarily, create a private mirror of the
full rclone history and develop the same `backend/projection` commits there.
Later cherry-pick or rebase those commits onto a feature branch in
`Spaceghost/rclone`. This keeps code shape and tests aligned with rclone and
avoids turning the backend into a separate product.
