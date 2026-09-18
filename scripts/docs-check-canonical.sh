#!/usr/bin/env bash
# scripts/docs-check-canonical.sh — every content page mounted on
# docs.docker.com must declare the canonical: front matter value
# derived from its path, so the github.io page defers to the stable
# docs (issue #3371, phase 3.2). Missing or stale values (e.g. a page
# scaffolded by copying another one) fail here and in the docs CI workflow.
#
# Scope: docs/<section>/<page>/index.md and deeper. The homepage,
# 404.md and section _index.md files are not github.io content pages
# and are intentionally not checked (see docs/STYLE.md).

set -euo pipefail

cd "$(dirname "$0")/.."

status=0
checked=0
while IFS= read -r file; do
  rel=${file#docs/}
  rel=${rel%/index.md}
  want="canonical: https://docs.docker.com/ai/docker-agent/${rel}/"
  if ! awk '/^---$/ { fm++; next } fm == 1' "$file" | grep -Fxq "$want"; then
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
  echo "canonical front matter OK on ${checked} mounted content pages"
fi
exit "$status"
