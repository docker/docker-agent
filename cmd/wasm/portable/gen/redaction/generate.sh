#!/bin/sh
# Export the private rules from the pinned module without patching the Go cache.
set -eu
CDPATH= cd "$(dirname "$0")"
src=$(go list -m -f '{{.Dir}}' github.com/docker/portcullis)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
for f in "$src"/*.go; do
    case "$f" in *_test.go) continue;; esac
    sed 's/^package portcullis$/package main/' "$f" > "$tmp/$(basename "$f")"
done
cp export.go.txt "$tmp/export.go"
version=$(go list -m -f '{{.Path}}@{{.Version}}' github.com/docker/portcullis)
go run "$tmp"/*.go "$version" > "$tmp/catalog.json"
if [ "${1:-}" = --check ]; then
    cmp "$tmp/catalog.json" ../../src/hooks/rules.json
else
    cp "$tmp/catalog.json" ../../src/hooks/rules.json
fi
