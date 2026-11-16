#!/usr/bin/env bash
set -euo pipefail

generator=github.com/docker/docker-agent/sdk/typescript/gen
packages=$(go list -race ./...)
others=()
found=0
while IFS= read -r package; do
  [ -n "$package" ] || continue
  if [ "$package" = "$generator" ]; then
    found=$((found + 1))
  else
    others+=("$package")
  fi
done <<< "$packages"

if [ "$found" -ne 1 ] || [ "${#others[@]}" -eq 0 ]; then
  echo "race package discovery must include exactly one $generator and other packages" >&2
  exit 1
fi

go test -race -shuffle=on -count=1 "${others[@]}"
# Temporary mitigation for https://github.com/golang/go/issues/81122:
# x/tools sizes its type-check semaphore from GOMAXPROCS at startup. Serializing
# the generator reduces race coverage for parallel type checks; it is not a fix.
GOMAXPROCS=1 go test -race -shuffle=on -count=1 "$generator"
