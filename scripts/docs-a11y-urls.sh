#!/usr/bin/env bash
# scripts/docs-a11y-urls.sh — map changed docs Markdown files to rendered
# URLs for Tier 2 of the docs-a11y gate (see docs-a11y-smart-gate plan and
# .github/workflows/docs.yml, a11y job). Reads changed repo-root-relative paths
# on stdin (one per line, e.g. from `git diff --name-only`), applies the
# mapping rules below, de-dupes against the static archetype list already
# in docs/.pa11yci.json, optionally live-probes the mapped pages, caps the
# result, and prints fully-qualified both-theme URLs on stdout (one per
# line) — ready to be jq-merged into a generated pa11y-ci config.
#
# Mapping rules (this site's Hugo config: disableKinds includes "section",
# so _index.md renders nothing; content lives at content/<section>/<page>/
# index.md leaf bundles — see docs/hugo.yaml):
#
#   docs/index.md                          -> /
#   docs/404.md                            -> /404.html
#   docs/<section>/<page>/index.md         -> /<section>/<page>/
#     for <section> in the mounted set below
#   docs/**/_index.md                      -> dropped (section kind disabled)
#   docs/STYLE.md                          -> dropped (not mounted)
#   any other docs/**/*.md                 -> dropped, with an ::notice
#                                              (an unexpected new location)
#   anything not under docs/ or not *.md   -> ignored silently (systemic
#                                              files are the archetypes'
#                                              job to catch, not Tier 2's)
#
# Env vars:
#   A11Y_MAX_PAGES   deterministic cap on mapped pages (default 10)
#   A11Y_BASE_URL    Hugo server base (default http://127.0.0.1:1313)
#   A11Y_PA11YCI     path to the static config used for de-dup
#                    (default <repo-root>/docs/.pa11yci.json)
#
# Usage:
#   git diff --name-only --diff-filter=d HEAD^1 HEAD | ./scripts/docs-a11y-urls.sh
#   ./scripts/docs-a11y-urls.sh --self-test

set -euo pipefail

# Allow running from any directory, like scripts/workflow-lint.sh.
if [ -d ".github/workflows" ]; then
  REPO_ROOT="$(pwd)"
else
  REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
fi

PA11YCI="${A11Y_PA11YCI:-$REPO_ROOT/docs/.pa11yci.json}"
BASE_PATH="/docker-agent"

# Sections actually mounted as content in docs/hugo.yaml.
MOUNTED_SECTIONS="getting-started concepts configuration tools providers features guides community"

# base_url / max_pages — read fresh on every call (rather than snapshotted
# once at parse time) so tests can override them per-invocation via env
# var prefixes on a function call, same as they would on a subprocess.
base_url() { printf '%s' "${A11Y_BASE_URL:-http://127.0.0.1:1313}"; }
max_pages() { printf '%s' "${A11Y_MAX_PAGES:-10}"; }

notice() {
  printf '::notice::%s\n' "$1" >&2
}

is_mounted_section() {
  local section="$1"
  for s in $MOUNTED_SECTIONS; do
    [ "$s" = "$section" ] && return 0
  done
  return 1
}

# map_path <repo-relative-path> — prints the rendered path (e.g.
# "/configuration/sandbox/") on stdout if the input maps to a rendered
# page, or nothing (with a logged ::notice for the unexpected-location
# case) if it doesn't.
map_path() {
  local path="$1"

  case "$path" in
    *.md) ;;
    *) return 0 ;;  # not Markdown: ignored silently
  esac

  case "$path" in
    docs/index.md)
      printf '/\n'
      return 0
      ;;
    docs/404.md)
      printf '/404.html\n'
      return 0
      ;;
    docs/STYLE.md)
      return 0  # known, never mounted — dropped silently
      ;;
  esac

  # docs/**/_index.md never renders (disableKinds: section).
  case "$path" in
    */_index.md)
      return 0
      ;;
  esac

  # docs/<section>/<page>/index.md -> /<section>/<page>/
  if [[ "$path" =~ ^docs/([^/]+)/([^/]+)/index\.md$ ]]; then
    local section="${BASH_REMATCH[1]}" page="${BASH_REMATCH[2]}"
    if is_mounted_section "$section"; then
      printf '/%s/%s/\n' "$section" "$page"
      return 0
    fi
  fi

  notice "docs-a11y-urls: unmapped markdown file, dropped: $path"
  return 0
}

# static_paths — the archetype paths already scanned every run, so Tier 2
# never re-lists them (compares on path, ignoring the ?theme= query and
# the /docker-agent baseURL prefix).
static_paths() {
  [ -f "$PA11YCI" ] || return 0
  jq -r '.urls[]' "$PA11YCI" \
    | sed -E "s#^https?://[^/]+$BASE_PATH##; s/\?theme=(light|dark)$//" \
    | LC_ALL=C sort -u
}

# probe <path> — returns 0 (keep) if the base URL is unreachable (nothing
# to verify against, e.g. during --self-test) or the page responds 200;
# returns 1 (drop, with a notice) only when the server IS reachable and
# this specific page 404s (renames/deletions the diff filter missed).
probe() {
  local path="$1" base
  base="$(base_url)"
  if [ -z "${_A11Y_SERVER_REACHABLE:-}" ]; then
    if curl -s -o /dev/null --max-time 2 "$base$BASE_PATH/" 2>/dev/null; then
      _A11Y_SERVER_REACHABLE=1
    else
      _A11Y_SERVER_REACHABLE=0
      notice "docs-a11y-urls: $base unreachable, skipping the live-probe safety net"
    fi
  fi
  [ "$_A11Y_SERVER_REACHABLE" = 0 ] && return 0

  local code
  code="$(curl -s -o /dev/null --max-time 5 -w '%{http_code}' "$base$BASE_PATH$path" 2>/dev/null || echo 000)"
  if [ "$code" != "200" ]; then
    notice "docs-a11y-urls: $path responded $code, dropping (renamed or deleted?)"
    return 1
  fi
  return 0
}

# run_mapping — reads changed paths on stdin, prints final both-theme URLs.
run_mapping() {
  local base cap
  base="$(base_url)"
  cap="$(max_pages)"
  unset _A11Y_SERVER_REACHABLE

  local mapped=()
  while IFS= read -r line; do
    [ -z "$line" ] && continue
    while IFS= read -r p; do
      [ -n "$p" ] && mapped+=("$p")
    done < <(map_path "$line")
  done

  # De-dupe against the static archetype list.
  local statics
  statics="$(static_paths)"
  local deduped=()
  for p in "${mapped[@]:-}"; do
    [ -z "$p" ] && continue
    if ! grep -qxF "$p" <<<"$statics" 2>/dev/null; then
      deduped+=("$p")
    fi
  done

  # Uniq + probe.
  local uniq_sorted
  uniq_sorted="$(printf '%s\n' "${deduped[@]:-}" | grep -v '^$' | LC_ALL=C sort -u || true)"

  local probed=()
  while IFS= read -r p; do
    [ -z "$p" ] && continue
    if probe "$p"; then
      probed+=("$p")
    fi
  done <<<"$uniq_sorted"

  # Deterministic cap: lexicographic, first N win; log the remainder.
  local total=${#probed[@]}
  local capped=("${probed[@]:0:$cap}")
  if [ "$total" -gt "$cap" ]; then
    local skipped=("${probed[@]:$cap}")
    notice "docs-a11y-urls: $total changed pages mapped, capped at $cap; skipped: ${skipped[*]}"
  fi

  for p in "${capped[@]:-}"; do
    [ -z "$p" ] && continue
    printf '%s%s%s?theme=light\n' "$base" "$BASE_PATH" "$p"
    printf '%s%s%s?theme=dark\n' "$base" "$BASE_PATH" "$p"
  done
}

self_test() {
  local failures=0

  check() {
    local desc="$1" expected="$2" actual="$3"
    if [ "$expected" = "$actual" ]; then
      printf 'ok - %s\n' "$desc"
    else
      printf 'not ok - %s\n  expected: %q\n  actual:   %q\n' "$desc" "$expected" "$actual" >&2
      failures=$((failures + 1))
    fi
  }

  # Individual mapping rules.
  check "docs/index.md maps to /" "/" "$(map_path docs/index.md)"
  check "docs/404.md maps to /404.html" "/404.html" "$(map_path docs/404.md)"
  check "leaf index.md maps to /<section>/<page>/" "/guides/go-sdk/" \
    "$(map_path docs/guides/go-sdk/index.md)"
  check "static-list page still maps (de-dup happens later)" "/configuration/sandbox/" \
    "$(map_path docs/configuration/sandbox/index.md)"
  check "_index.md is dropped" "" "$(map_path docs/concepts/_index.md 2>/dev/null)"
  check "docs/STYLE.md is dropped" "" "$(map_path docs/STYLE.md 2>/dev/null)"
  check "unmounted section is dropped (logs a notice)" "" \
    "$(map_path docs/unknown-section/page/index.md 2>/dev/null)"
  check "a CSS file is ignored" "" "$(map_path docs/css/style.css 2>/dev/null)"
  check "an image is ignored" "" "$(map_path docs/demo.gif 2>/dev/null)"

  # De-dup against the static archetype list, unreachable server (probe no-ops).
  local out
  out="$(A11Y_BASE_URL="http://127.0.0.1:1" run_mapping <<'EOF'
docs/configuration/sandbox/index.md
docs/guides/go-sdk/index.md
EOF
)"
  check "static-list page is de-duped away, non-static page survives" \
    "http://127.0.0.1:1/docker-agent/guides/go-sdk/?theme=light
http://127.0.0.1:1/docker-agent/guides/go-sdk/?theme=dark" "$out"

  # Every emitted URL carries theme= and comes in light+dark pairs.
  local non_theme_lines
  non_theme_lines="$(printf '%s\n' "$out" | grep -vc 'theme=' || true)"
  check "every emitted URL carries theme=" "0" "$non_theme_lines"

  # Cap at N, lexicographic winners, notice for the remainder.
  local many
  many="$(for i in $(seq -w 1 12); do printf 'docs/guides/page%s/index.md\n' "$i"; done)"
  local capped_out notice_out
  capped_out="$(printf '%s\n' "$many" | A11Y_BASE_URL="http://127.0.0.1:1" A11Y_MAX_PAGES=10 run_mapping 2>/tmp/docs-a11y-urls-selftest-notices)"
  notice_out="$(cat /tmp/docs-a11y-urls-selftest-notices)"
  rm -f /tmp/docs-a11y-urls-selftest-notices
  check "12 pages capped at 10 -> 20 URLs" "20" "$(printf '%s\n' "$capped_out" | grep -c 'theme=')"
  check "lexicographic winner: page01 kept (both themes)" "2" \
    "$(printf '%s\n' "$capped_out" | grep -c '/guides/page01/')"
  check "lexicographic loser: page11 skipped" "0" \
    "$(printf '%s\n' "$capped_out" | grep -c '/guides/page11/')"
  check "cap emits a notice naming the skipped remainder" "1" \
    "$(printf '%s\n' "$notice_out" | grep -c 'capped at 10')"

  if [ "$failures" -gt 0 ]; then
    printf '\ndocs-a11y-urls --self-test: %d failure(s)\n' "$failures" >&2
    return 1
  fi
  printf '\ndocs-a11y-urls --self-test: all checks passed\n' >&2
}

if [ "${1:-}" = "--self-test" ]; then
  self_test
else
  run_mapping
fi
