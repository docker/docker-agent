#!/usr/bin/env bash
# scripts/models-delta.sh — semantic diff of the models.dev catalog snapshot.
#
# Usage:
#   scripts/models-delta.sh [--json-out FILE] [OLD NEW]
#
# With no positional arguments, compares the version of
# pkg/modelsdev/snapshot.json at HEAD against the current working-tree copy —
# i.e. "what would `task update-models` change if I committed it right now".
# This is also exactly what the update-models workflow runs after refreshing
# the snapshot, so the local and CI code paths never drift apart.
#
# OLD and NEW, when given, are paths to two snapshot.json files to compare
# instead (e.g. two saved copies from different runs).
#
# The rendered markdown always goes to stdout. --json-out additionally writes
# the structural delta (added/removed/changed) as JSON, which the workflow
# uses to compute counts and a commit subject without re-running jq.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DELTA_JQ="$ROOT/.github/scripts/models-delta.jq"
RENDER_JQ="$ROOT/.github/scripts/models-delta-render.jq"

json_out=""
positional=()
end_of_opts=false
while [ $# -gt 0 ]; do
  if [ "$end_of_opts" = false ]; then
    case "$1" in
      --json-out)
        [ $# -ge 2 ] || { echo "models-delta.sh: --json-out requires a path" >&2; exit 2; }
        json_out="$2"
        shift 2
        continue
        ;;
      --)
        end_of_opts=true
        shift
        continue
        ;;
      -*)
        echo "models-delta.sh: unknown option $1" >&2
        exit 2
        ;;
    esac
  fi
  positional+=("$1")
  shift
done

old="${positional[0]:-}"
new="${positional[1]:-}"

cleanup_old=""
delta_json=""
trap '[ -n "$cleanup_old" ] && rm -f "$cleanup_old"; [ -n "$delta_json" ] && rm -f "$delta_json"' EXIT

if [ -z "$old" ]; then
  cleanup_old="$(mktemp)"
  old="$cleanup_old"
  git -C "$ROOT" show "HEAD:pkg/modelsdev/snapshot.json" > "$old"
fi
if [ -z "$new" ]; then
  new="$ROOT/pkg/modelsdev/snapshot.json"
fi

for f in "$old" "$new"; do
  [ -r "$f" ] || { echo "models-delta.sh: cannot read $f" >&2; exit 1; }
done

delta_json="$(mktemp)"

jq -n --slurpfile old "$old" --slurpfile new "$new" -f "$DELTA_JQ" > "$delta_json"

if [ -n "$json_out" ]; then
  cp "$delta_json" "$json_out"
fi

jq -r -f "$RENDER_JQ" "$delta_json"
