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
  -v "${REPO_ROOT}/docs:/srv/jekyll" \
  docker-agent-docs \
  jekyll serve --host 0.0.0.0 --config _config.yml,_config.dev.yml

echo 'Waiting for Jekyll to start...'
for i in $(seq 1 30); do
  docker run --rm --network docs-linkcheck-net curlimages/curl -sf http://docs-linkcheck:4000/ > /dev/null 2>&1 && break
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
  http://docs-linkcheck:4000/
