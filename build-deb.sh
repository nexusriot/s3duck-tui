#!/usr/bin/env bash
# Build a s3duck-tui .deb:  ./build-deb.sh [amd64|arm64|riscv64]
#
# Fails on the first error. It used to carry on: a missing dpkg-deb was only
# reported, and a failed `go build` went unnoticed, so the package could ship a
# stale ./s3duck-tui left over from an earlier build (or an empty /usr/bin) and
# the script still exited 0.
set -euo pipefail

version=0.11.0
arch="${1:-amd64}"

# Every path below is relative to the repository, wherever this is run from.
cd "$(dirname "$0")"

echo "building deb for s3duck-tui $version ($arch)"

if ! type "dpkg-deb" > /dev/null 2>&1; then
  echo "please install required build tools first (dpkg-deb not found)" >&2
  exit 1
fi

case "$arch" in
  amd64)    goarch="amd64" ;;
  arm64)    goarch="arm64" ;;
  riscv64)  goarch="riscv64" ;;
  *)        echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac

project="s3duck-tui_${version}_${arch}"
folder_name="build/$project"
echo "creating $folder_name"
rm -rf "$folder_name"
mkdir -p "$folder_name"
cp -r DEBIAN/ "$folder_name"
bin_dir="$folder_name/usr/bin"
mkdir -p "$bin_dir"
if [ "$arch" = "$(go env GOARCH)" ] && [ "$(go env GOOS)" = "linux" ]; then
  go build -ldflags "-linkmode external -extldflags -static" -o "$bin_dir/s3duck-tui" ./cmd/s3duck-tui
else
  # GOOS is set explicitly: from macOS this used to package a Darwin binary.
  CGO_ENABLED=0 GOOS=linux GOARCH="$goarch" go build -ldflags "-s -w" -o "$bin_dir/s3duck-tui" ./cmd/s3duck-tui
fi

sed -i "s/_version_/$version/g" "$folder_name/DEBIAN/control"
sed -i "s/Architecture: .*/Architecture: $arch/" "$folder_name/DEBIAN/control"
# Measured from what is actually staged, in KiB, as dpkg expects: the number
# used to be a hard-coded guess that drifted with every dependency added.
sed -i "s/_installed_size_/$(du -k -s "$folder_name/usr" | cut -f1)/" "$folder_name/DEBIAN/control"

cd build/ && dpkg-deb --build -Z gzip --root-owner-group "$project"
