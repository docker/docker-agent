#!/usr/bin/env bash
# scripts/models-delta-test.sh — golden-fixture regression test for the
# models-delta jq scripts (.github/scripts/models-delta*.jq).
#
# These scripts have no Go equivalent to exercise them under `task test`,
# and they run for real, unattended, only once a week (the update-models
# schedule) — a silent regression here would go unnoticed until then.
# This runs the exact scripts against small fixtures and diffs the output
# against checked-in golden files; run it directly or via `task test-models-delta`.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FIXTURES="$ROOT/.github/scripts/testdata"
DELTA_JQ="$ROOT/.github/scripts/models-delta.jq"
RENDER_JQ="$ROOT/.github/scripts/models-delta-render.jq"

tmp_delta="$(mktemp)"
trap 'rm -f "$tmp_delta"' EXIT

# Normalize line endings before comparing: on Windows, a native jq.exe
# writing through the CRT's default text-mode stdio can emit CRLF even
# though the checked-in golden files are forced to LF by .gitattributes
# (`* text=auto eol=lf`) regardless of platform. Without this, every
# line registers as changed despite being textually identical.
jq -n --slurpfile old "$FIXTURES/old.json" --slurpfile new "$FIXTURES/new.json" -f "$DELTA_JQ" | tr -d '\r' > "$tmp_delta"

status=0

if ! diff -u <(tr -d '\r' < "$FIXTURES/expected-delta.json") "$tmp_delta"; then
  echo "models-delta-test.sh: models-delta.jq output does not match testdata/expected-delta.json" >&2
  status=1
fi

if ! diff -u <(tr -d '\r' < "$FIXTURES/expected-render.md") <(jq -r -f "$RENDER_JQ" "$tmp_delta" | tr -d '\r'); then
  echo "models-delta-test.sh: models-delta-render.jq output does not match testdata/expected-render.md" >&2
  status=1
fi

if [ "$status" -eq 0 ]; then
  echo "models-delta-test.sh: OK"
else
  echo "models-delta-test.sh: regenerate the golden files if this change is intentional:" >&2
  echo "  jq -n --slurpfile old $FIXTURES/old.json --slurpfile new $FIXTURES/new.json -f $DELTA_JQ > $FIXTURES/expected-delta.json" >&2
  echo "  jq -r -f $RENDER_JQ $FIXTURES/expected-delta.json > $FIXTURES/expected-render.md" >&2
fi

exit "$status"
