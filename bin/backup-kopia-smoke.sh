#!/usr/bin/env bash
# Create an isolated stock Kopia fixture and read it with the native rclone CLI.
# Test files are retained for inspection; no existing files are deleted.
# Usage: bash bin/backup-kopia-smoke.sh /absolute/rclone /absolute/kopia
set -euo pipefail

if [[ $# -ne 2 || "$1" != /* || "$2" != /* ]]; then
  echo "usage: $0 /absolute/rclone /absolute/kopia" >&2
  exit 2
fi
rclone=$1
kopia=$2
fixture=$(mktemp -d)
printf 'Isolated smoke-test directory: %s\n' "$fixture"
mkdir -p "$fixture/source" "$fixture/native-tmp" "$fixture/no-programs"
printf 'native Kopia selective restore\n' > "$fixture/source/report.txt"
cp "$fixture/source/report.txt" "$fixture/source/duplicate.txt"

export KOPIA_PASSWORD='rclone-kopia-smoke-test-password'
"$kopia" --config-file="$fixture/kopia.config" repository create filesystem --path="$fixture/repo"
"$kopia" --config-file="$fixture/kopia.config" snapshot create "$fixture/source"

export RCLONE_CONFIG="$fixture/empty-rclone.conf"
export RCLONE_CONFIG_VAULT_TYPE=backup
export RCLONE_CONFIG_VAULT_REMOTE="$fixture/repo"
RCLONE_CONFIG_VAULT_PASSWORD=$("$rclone" obscure "$KOPIA_PASSWORD")
export RCLONE_CONFIG_VAULT_PASSWORD

find "$fixture/repo" -type f -exec sha256sum {} + | sort > "$fixture/before.sha256"

# Only the process under test gets an empty executable search directory. This
# proves that reading does not require an external Kopia or rclone executable.
run_native() {
  PATH="$fixture/no-programs" TMPDIR="$fixture/native-tmp" "$rclone" "$@"
}
run_native lsd vault:snapshots
run_native backend sources vault:
run_native copyto vault:latest/report.txt "$fixture/restored.txt"
cmp "$fixture/source/report.txt" "$fixture/restored.txt"

find "$fixture/repo" -type f -exec sha256sum {} + | sort > "$fixture/after.sha256"
cmp "$fixture/before.sha256" "$fixture/after.sha256"
if [[ -n "$(find "$fixture/native-tmp" -mindepth 1 -print -quit)" ]]; then
  echo 'native CLI leaked temporary connection/cache state' >&2
  exit 1
fi
printf 'Stock Kopia recovery passed; repository unchanged; native temporary state cleaned.\n'
