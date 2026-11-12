#!/usr/bin/env bash
set -euo pipefail

REPO_ROOT=$(cd .. && pwd)

cleanup() {
  docker rm -f docs-linkcheck 2>/dev/null
  docker network rm docs-linkcheck-net 2>/dev/null
}
trap cleanup EXIT

cleanup || true
docker build -t docker-agent-docs .
docker network create docs-linkcheck-net
docker run -d --rm \
  --name docs-linkcheck \
  --network docs-linkcheck-net \
  -v "${REPO_ROOT}/docs:/src" \
  docker-agent-docs \
  hugo server --bind 0.0.0.0 --baseURL http://docs-linkcheck:1313/

echo 'Waiting for Hugo to start...'
for _ in $(seq 1 30); do
  docker run --rm --network docs-linkcheck-net curlimages/curl -sf http://docs-linkcheck:1313/ > /dev/null 2>&1 && break
  sleep 2
done

docker run --rm \
  --network docs-linkcheck-net \
  raviqqe/muffet \
  --buffer-size 16384 \
  --exclude 'fonts.googleapis.com' \
  --exclude 'fonts.gstatic.com' \
  --exclude 'console.mistral.ai' \
  --exclude 'console.x.ai' \
  --rate-limit 20 \
  http://docs-linkcheck:1313/
