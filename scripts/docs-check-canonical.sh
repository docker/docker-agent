#!/usr/bin/env bash
# Validate authoritative URLs for mounted technical pages and github.io Community.
# Scope: docs/<section>/<page>/index.md and deeper (see docs/STYLE.md).

set -euo pipefail

cd "$(dirname "$0")/.."

status=0
checked=0
while IFS= read -r file; do
  rel=${file#docs/}
  rel=${rel%/index.md}
  case "$rel" in
    community/*) origin="https://docker.github.io/docker-agent" ;;
    *) origin="https://docs.docker.com/ai/docker-agent" ;;
  esac
  want="canonical: ${origin}/${rel}/"
  actual=$(awk '/^---$/ { fm++; next } fm == 1 && /^canonical:/ { print }' "$file")
  if [ "$actual" != "$want" ]; then
    echo "${file}: missing or stale canonical, expected exactly: ${want}"
    status=1
  fi
  checked=$((checked + 1))
done < <(find docs -mindepth 3 -name index.md -not -path 'docs/public/*' -not -path 'docs/node_modules/*' | sort)

if [ "$checked" -eq 0 ]; then
  echo "no content pages found under docs/ — wrong working directory?"
  exit 1
fi

if [ "$status" -eq 0 ]; then
  echo "canonical front matter OK on ${checked} content pages"
fi
./scripts/docs-check-community-links.sh || status=1
exit "$status"
