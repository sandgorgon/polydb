#!/bin/sh
# Builds PolyDB's release archives:
#
#   scripts/release.sh 1.0.0
#
# writes dist/polydb_<version>_<os>_<arch>.tar.gz for Linux and macOS on
# amd64 and arm64, each holding the polydb binary, the example
# configuration and systemd unit, and the README, LICENSE and CHANGELOG
# when present, plus dist/SHA256SUMS. The binaries are static (no cgo),
# built with -trimpath, and report the version and commit in
# `polydb version`.
set -eu

version=${1:?usage: scripts/release.sh <version, e.g. 1.0.0>}
version=${version#v}
cd "$(dirname "$0")/.."
out=dist
rm -rf "$out"
mkdir -p "$out"

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
	os=${target%/*}
	arch=${target#*/}
	name=polydb_${version}_${os}_${arch}
	dir=$out/$name
	mkdir -p "$dir"
	CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath \
		-ldflags "-s -w -X github.com/sandgorgon/polydb/version.Version=$version" \
		-o "$dir/polydb" ./cmd/polydb
	cp packaging/polydb.conf packaging/polydb.service "$dir/"
	for f in README.md LICENSE CHANGELOG.md; do
		if [ -f "$f" ]; then cp "$f" "$dir/"; fi
	done
	tar -C "$out" -czf "$out/$name.tar.gz" "$name"
	rm -rf "$dir"
	echo "built $out/$name.tar.gz"
done

cd "$out"
if command -v sha256sum >/dev/null 2>&1; then
	sha256sum ./*.tar.gz | sed 's| \./| |' >SHA256SUMS
else
	shasum -a 256 ./*.tar.gz | sed 's| \./| |' >SHA256SUMS
fi
echo "wrote $out/SHA256SUMS"
