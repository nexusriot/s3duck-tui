#!/bin/env bash

version=0.0.30

echo "building deb for s3duck-tui $version"

if ! type "dpkg-deb" > /dev/null; then
  echo "please install required build tools first"
fi

project="s3duck-tui_${version}_amd64"
folder_name="build/$project"
echo "crating $folder_name"
mkdir -p $folder_name
cp -r DEBIAN/ $folder_name
bin_dir="$folder_name/usr/bin"
mkdir -p $bin_dir
go build -ldflags "-linkmode external -extldflags -static" -o s3duck-tui cmd/s3duck-tui/main.go

mv s3duck-tui $bin_dir
sed -i "s/_version_/$version/g" $folder_name/DEBIAN/control

cd build/ && dpkg-deb --build -Z gzip --root-owner-group $project
