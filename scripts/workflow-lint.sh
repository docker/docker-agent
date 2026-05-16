#!/usr/bin/env bash
# scripts/workflow-lint.sh — project-specific invariants on
# .github/workflows/*.yml that actionlint does not enforce.
#
# Three checks, each tied to a documented incident or AGENTS.md entry:
#
#   1. concurrency:           every PR-triggered workflow declares a
#                             concurrency: group (AGENTS.md §
#                             GitHub Actions), EXCEPT pr-review-trigger.yml
#                             which intentionally runs all events to completion
#                             (see PR #2789);
#
#   2. pinned-by-sha:         every third-party `uses:` reference is
#                             pinned by a 40-char SHA, not a tag/branch
#                             (AGENTS.md § GitHub Actions);
#
#   3. payload-field deny:    no `github.event.X.Y` reference on the
#                             documented broken list — currently the
#                             `github.event.issue.type` family from
#                             PR #2645 (the `issues: labeled` payload
#                             carries no `type` field, so the workflow
#                             never runs).
#
# Failures print the file:line of the offending construct and the
# rule that fired. The script exits non-zero on any finding so CI
# fails the lint job; pre-existing findings should be fixed at
# source rather than suppressed inline.

set -euo pipefail

# Allow running from any directory: prefer cwd if it already has
# .github/workflows, else jump to the project root relative to the
# script's own location. Tests can `cp` the script next to a fixture
# tree without needing to exec from the project root.
if [ -d ".github/workflows" ]; then
  :
else
  cd "$(dirname "$0")/.."
fi

WORKFLOWS_DIR=".github/workflows"
errors=0

note() {
  printf '%s: %s\n' "$1" "$2" >&2
  errors=$((errors + 1))
}

# Check 1: PR-triggered workflows declare concurrency.
#
# We treat a workflow as PR-triggered if its `on:` block mentions
# `pull_request` in any of the three forms GitHub Actions accepts:
#
#   on:
#     pull_request:           # mapping form
#       branches: [main]
#
#   on: [pull_request, push]  # compact array form
#
#   on:                       # sequence form
#     - pull_request
#
# A `\bpull_request\b` word-boundary match catches all three without
# matching `pull_request_target` (a separate event with different
# concurrency semantics). The check is grep-based rather than
# yq-based so it runs in the lint job without extra deps; the
# trade-off is a false positive if `pull_request` appears in a
# comment, which we accept.
#
# EXCEPTION: pr-review-trigger.yml is exempt from this check because
# it intentionally runs all events to completion (the workflow is cheap,
# and deduplication happens downstream in pr-review.yml). See PR #2789.
for f in "$WORKFLOWS_DIR"/*.yml "$WORKFLOWS_DIR"/*.yaml; do
  [ -e "$f" ] || continue
  
  # Skip pr-review-trigger.yml
  if [[ "$f" == *"pr-review-trigger.yml" ]]; then
    continue
  fi
  
  if grep -qE '\bpull_request\b' "$f"; then
    if ! grep -qE '^\s*concurrency:' "$f"; then
      note "$f" "PR-triggered workflow has no concurrency: block (AGENTS.md § GitHub Actions)"
    fi
  fi
done

# Check 2: third-party `uses:` references pinned by 40-char SHA.
#
# Local references (`./...`, `../...`) and re-usable workflow refs
# without an `@` (handled by the regex below) are exempt; everything
# else, including the `docker/` namespace, must look like
# `owner/repo@<40hex>`. The trailing comment with the human-readable
# version is encouraged but not required by the cop — actionlint's
# pinned-action check covers that ergonomically separately.
while IFS= read -r line; do
  # Match shape: <file>:<lineno>:<content>
  file="${line%%:*}"
  rest="${line#*:}"
  lineno="${rest%%:*}"
  content="${rest#*:}"

  # Pull the value after `uses:` and trim surrounding whitespace and
  # any trailing `# version` comment.
  ref="${content#*uses:}"
  # Strip leading whitespace (POSIX-style: drop one leading [ \t]).
  ref="${ref##+([[:space:]])}"
  # Bash 3.2 / macOS bash 3 doesn't support extglob without setting
  # it. Fall back to a sed pipe for portability.
  ref="$(printf '%s' "$ref" | sed -E 's/^[[:space:]]+//; s/[[:space:]]+#.*$//; s/[[:space:]]+$//')"

  # Skip empty / local / re-usable workflow refs.
  case "$ref" in
    '' | './'* | '../'*)
      continue
      ;;
  esac

  if [[ ! "$ref" =~ @[0-9a-f]{40}$ ]]; then
    note "$file:$lineno" "third-party action $ref is not pinned by a 40-char SHA"
  fi
done < <(grep -nE '^\s*-?\s*uses:' "$WORKFLOWS_DIR"/*.yml "$WORKFLOWS_DIR"/*.yaml 2>/dev/null || true)

# Check 3: known-broken event-payload field references.
#
# This is a small allow-list maintained alongside post-mortems. Each
# entry is an exact string that, if present in any workflow, is wrong:
#
#   github.event.issue.type           PR #2645 (issues:labeled has no .type field)
#
# Add new entries here when a similar bug is reviewed. The check is a
# literal (fixed-string) grep so entries are easy to read and keep in
# sync with the post-mortem notes — the dots in `github.event.X.Y`
# are matched literally rather than as regex "any character" wildcards.
broken_refs=(
  "github.event.issue.type"
)
for ref in "${broken_refs[@]}"; do
  matches=$(grep -nF -- "$ref" "$WORKFLOWS_DIR"/*.yml "$WORKFLOWS_DIR"/*.yaml 2>/dev/null || true)
  [ -z "$matches" ] && continue
  while IFS= read -r m; do
    [ -z "$m" ] && continue
    file="${m%%:*}"
    rest="${m#*:}"
    lineno="${rest%%:*}"
    note "$file:$lineno" "$ref is not a real event-payload field — see PR #2645 post-mortem"
  done <<< "$matches"
done

if [ "$errors" -gt 0 ]; then
  printf '\nworkflow-lint: %d issue(s) found\n' "$errors" >&2
  exit 1
fi

printf 'workflow-lint: OK (%d files checked)\n' "$(find "$WORKFLOWS_DIR" -maxdepth 1 -type f \( -name '*.yml' -o -name '*.yaml' \) | wc -l | tr -d ' ')"
