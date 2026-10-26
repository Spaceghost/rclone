#!/bin/sh
set -eu

build_one() {
    target_os=$1
    target_arch=$2
    output=$3
    CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" \
        go build -trimpath -ldflags='-s -w' -o "$output" ./cmd/melodica-rclone
}

if [ "${1:-}" = "--matrix" ]; then
    target_dir=${2:-build/private}
    mkdir -p "$target_dir"
    build_one windows amd64 "$target_dir/melodica-rclone-windows-amd64.exe"
    build_one windows arm64 "$target_dir/melodica-rclone-windows-arm64.exe"
    build_one darwin arm64 "$target_dir/melodica-rclone-darwin-arm64"
    build_one linux amd64 "$target_dir/melodica-rclone-linux-amd64"
    build_one linux arm64 "$target_dir/melodica-rclone-linux-arm64"
    build_one android arm64 "$target_dir/melodica-rclone-android-arm64"
    sha256sum "$target_dir"/melodica-rclone-* >"$target_dir/SHA256SUMS"
    exit 0
fi

output=${1:-build/melodica-rclone}
target_dir=${output%/*}
[ "$target_dir" = "$output" ] || mkdir -p "$target_dir"

CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' \
    -o "$output" ./cmd/melodica-rclone
