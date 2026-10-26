# Melodica private rclone build

This entry point retains only the local, S3 and crypt backends and the small
set of commands used by the Melodica content-addressed library. It is intended
for authenticated private artifact distribution, not the general rclone
release channel.

Build the host binary with:

```sh
contrib/melodica/build-private.sh
```

Cross-compilation uses the same entry point, for example:

```sh
GOOS=windows GOARCH=amd64 contrib/melodica/build-private.sh build/melodica-rclone.exe
GOOS=darwin GOARCH=arm64 contrib/melodica/build-private.sh build/melodica-rclone-darwin-arm64
GOOS=linux GOARCH=arm64 contrib/melodica/build-private.sh build/melodica-rclone-linux-arm64
```

Or build the full capability-gated release matrix and checksum manifest:

```sh
contrib/melodica/build-private.sh --matrix
```

The matrix covers Windows x64/ARM64, macOS ARM64, Linux x64/ARM64, and Android
ARM64. iOS consumes the native `melodica.h` library inside its app sandbox and
does not receive a standalone rclone executable.

Private binary artifacts must include `COPYING`, the exact source revision,
and a corresponding-source offer. rclone remains GPLv3 software.
