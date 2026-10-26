#!/bin/sh
set -eu

output=${1:-build/melodica-rclone}
target_dir=${output%/*}
[ "$target_dir" = "$output" ] || mkdir -p "$target_dir"

CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
    -o "$output" ./cmd/melodica-rclone
