#!/bin/env bash
# Build s3duck-tui for the LicheeRV Nano (W) — linux/riscv64.
#
#   ./build-licheerv.sh           -> bare static binary into dist/
#   ./build-licheerv.sh deb       -> riscv64 .deb into build/
set -e

dir="$(cd "$(dirname "$0")" && pwd)"

if [ "${1:-}" = "deb" ]; then
  exec "$dir/build-deb.sh" riscv64
fi

# Read from build-deb.sh rather than repeating it: one more copy of the version
# is one more thing to forget on a release bump.
version="$(sed -n 's/^version=//p' "$dir/build-deb.sh" | head -1)"

dist_dir="dist"
mkdir -p "$dist_dir"

echo "building s3duck-tui $version for LicheeRV Nano (linux/riscv64)"

CGO_ENABLED=0 GOOS=linux GOARCH=riscv64 \
  go build -trimpath -ldflags "-s -w" \
  -o "$dist_dir/s3duck-tui-${version}-licheerv-linux-riscv64" ./cmd/s3duck-tui
echo ">> $dist_dir/s3duck-tui-${version}-licheerv-linux-riscv64"
