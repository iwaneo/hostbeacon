#!/bin/sh
# Builds the Agent's .deb, .rpm, and tarball for one architecture.
# Usage: packaging/build.sh <version> <amd64|arm64> <output directory>
# Needs Go and nfpm.
set -eu
version=$1
arch=$2
out=$(mkdir -p "$3" && cd "$3" && pwd)
cd "$(dirname "$0")/.."

tarball=hostbeacon_${version}_linux_${arch}
bin=$out/$tarball
mkdir -p "$bin"
CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath \
	-ldflags="-s -w -X github.com/iwaneo/hostbeacon/agent/internal/version.Version=$version" \
	-o "$bin/" ./cmd/...
cp ../LICENSE "$bin/"

for packager in deb rpm; do
	VERSION=$version GOARCH=$arch BIN=$bin nfpm package --config packaging/nfpm.yaml --packager "$packager" --target "$out/"
done
tar -C "$out" -czf "$out/$tarball.tar.gz" --owner=0 --group=0 "$tarball"
rm -r "$bin"
ls -l "$out"
