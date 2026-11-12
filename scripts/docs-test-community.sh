#!/usr/bin/env bash
# Exercise URL policy in an isolated fixture so failures never modify real content.
set -euo pipefail

cd "$(dirname "$0")/.."
repo=$PWD
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
mkdir -p "$fixture/scripts" "$fixture/docs/community/resources" \
  "$fixture/docs/community/contributing" "$fixture/docs/configuration/telemetry"
cp scripts/docs-check-{canonical,community-links}.sh "$fixture/scripts/"
cd "$fixture"

cat > docs/community/resources/index.md <<'EOF'
---
canonical: https://docker.github.io/docker-agent/community/resources/
---
EOF
cat > docs/configuration/telemetry/index.md <<'EOF'
---
canonical: https://docs.docker.com/ai/docker-agent/configuration/telemetry/
---
EOF
printf '# README\n' > README.md
./scripts/docs-check-canonical.sh
./scripts/docs-check-community-links.sh

expect_failure() {
  if "$@" > failure.log 2>&1; then
    echo "expected failure: $*" >&2
    exit 1
  fi
  cat failure.log
}

for url in \
  https://docs.docker.com/ai/docker-agent/community/resources/ \
  https://docker.github.io/docker-agent/community/other/; do
  printf -- '---\ncanonical: %s\n---\n' "$url" > docs/community/resources/index.md
  expect_failure ./scripts/docs-check-canonical.sh
done
printf '\ncanonical: https://docker.github.io/docker-agent/community/resources/\n' > docs/community/resources/index.md
expect_failure ./scripts/docs-check-canonical.sh
cp "$repo/docs/community/resources/index.md" docs/community/resources/index.md
sed '/^canonical:/p' docs/community/resources/index.md > duplicate.md
mv duplicate.md docs/community/resources/index.md
expect_failure ./scripts/docs-check-canonical.sh
cp "$repo/docs/community/resources/index.md" docs/community/resources/index.md

for url in \
  https://docker.github.io/docker-agent/configuration/telemetry/ \
  https://docs.docker.com/ai/docker-agent/community/telemetry/; do
  printf -- '---\ncanonical: %s\n---\n' "$url" > docs/configuration/telemetry/index.md
  expect_failure ./scripts/docs-check-canonical.sh
done
cp "$repo/docs/configuration/telemetry/index.md" docs/configuration/telemetry/index.md
./scripts/docs-check-canonical.sh

for url in \
  ../../community/telemetry/index.md \
  /community/opentelemetry/ \
  'https://docker.github.io/docker-agent/community/troubleshooting/#debug-mode' \
  https://docs.docker.com/ai/docker-agent/community/contributing/ \
  https://docs.docker.com/ai/docker-agent/community/resources/; do
  printf '[Stale](%s)\n' "$url" > docs/link.md
  expect_failure ./scripts/docs-check-community-links.sh
done
printf '[Current](configuration/telemetry/index.md)\n' > docs/link.md
for url in ../../community/contributing/index.md ../../community/resources/index.md; do
  printf '[Unmounted](%s)\n' "$url" > docs/link.md
  expect_failure ./scripts/docs-check-community-links.sh
  expect_failure ./scripts/docs-check-canonical.sh
done
printf '[Current](https://docker.github.io/docker-agent/community/resources/)\n' > docs/link.md
for url in ../telemetry/index.md ../opentelemetry/index.md '../troubleshooting/index.md#http-413-request-body-too-large'; do
  printf '[Legacy](%s)\n' "$url" > docs/community/contributing/links.md
  expect_failure ./scripts/docs-check-community-links.sh
done
printf '[Current](../resources/index.md)\n' > docs/community/contributing/links.md
./scripts/docs-check-community-links.sh
printf '[Stale](https://docker.github.io/docker-agent/community/telemetry)\n' > README.md
expect_failure ./scripts/docs-check-community-links.sh
printf '[Current](https://docker.github.io/docker-agent/configuration/telemetry/)\n' > README.md
./scripts/docs-check-community-links.sh
printf '[Stale](https://docs.docker.com/ai/docker-agent/community/resources/)\n' > README.md
expect_failure ./scripts/docs-check-community-links.sh
printf '# README\n' > README.md
printf '[Outside front matter](community/resources/)\n' > docs/link.md
./scripts/docs-check-canonical.sh
echo 'community URL policy regression tests passed'
