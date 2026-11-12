#!/usr/bin/env bash
# scripts/docs-check-llms-txt.sh — semantic check for the generated
# /llms.txt (https://llmstxt.org/) against its source of truth,
# docs/data/nav.yml (see docs/layouts/home.llms.txt and docs/STYLE.md
# "llms.txt"). A plain `hugo` build succeeding is not enough: a broken
# groups[].items traversal (or similar template regression) can
# silently drop entries — e.g. dropping from 99 to 64 with the
# groups-only sections such as "Built-in Tools" left empty —
# while the build itself stays green. This script builds the site and
# asserts the *content* of public/llms.txt matches nav.yml exactly:
# same sections, same order, same titles, same count, every link
# matching nav.yml's URL at the SAME position (not merely "absolute
# under the site baseURL", which a wrong-but-same-origin or duplicated
# link would also satisfy), and every entry carrying a non-empty
# (whitespace-only counts as empty) `: note`.
#
# Each entry's rendered URL is compared against the expected absolute
# URL derived from nav.yml at the SAME index (BASE_URL joined with the
# nav url) - a prefix-only check ("starts with BASE_URL") would pass
# even if every entry rendered the identical link.
#
# The expected (title, url) list is built by parsing nav.yml
# STRUCTURALLY, section by section (items first, then groups[].items),
# not by independent greps over title:/url: lines in physical file
# order: the template always emits items before groups regardless of
# which key comes first in a section's YAML block, so a physical-order
# grep would silently mismatch for a groups-before-items section.
#
# Bash-3.2-compatible (no mapfile/associative arrays), matching
# scripts/workflow-lint.sh's portability note, so it also runs under
# macOS's default /bin/bash.

set -euo pipefail

cd "$(dirname "$0")/.."

BASE_URL="https://docker.github.io/docker-agent/"
NAV="docs/data/nav.yml"
LLMS="docs/public/llms.txt"

echo "Building docs site..."
(cd docs && hugo --gc --baseURL "$BASE_URL")

[ -f "$LLMS" ] || { echo "missing $LLMS after build"; exit 1; }

status=0
fail() {
  echo "FAIL: $1" >&2
  status=1
}

# (a) first line is the H1.
first_line=$(sed -n '1p' "$LLMS")
if [ "$first_line" != "# Docker Agent" ]; then
  fail "first line is not '# Docker Agent': ${first_line}"
fi

# (b) a non-empty '> ' blockquote summary follows (skipping the blank
# line between the H1 and it). NR > 1 skips the H1 line in-process
# instead of piping through `tail`: under `set -o pipefail`, an early
# `exit` in a downstream awk/head/etc. can SIGPIPE a still-writing
# upstream `tail`, making the whole pipeline intermittently exit 141.
summary_line=$(awk 'NR > 1 && NF { print; exit }' "$LLMS")
case "$summary_line" in
"> "?*) ;;
*) fail "no non-empty '> ' blockquote summary after the H1 (got: ${summary_line})" ;;
esac

# Expected data straight from nav.yml: section names (`- section:`) and
# titles (`title:`, nested under either `items:` or `groups[].items:`),
# both in file order — nav.yml's authored order is the contract.
expected_sections=()
while IFS= read -r line; do
  expected_sections+=("$line")
done < <(grep -E '^- section:' "$NAV" | sed -E 's/^- section:[[:space:]]*//')

# Structural per-section parse (see header comment above) producing
# parallel title/url arrays in the exact order the template emits.
expected_titles=()
expected_urls=()
while IFS=$'\t' read -r title url; do
  expected_titles+=("$title")
  expected_urls+=("$url")
done < <(awk '
  function unquote(s) {
    if (s ~ /^".*"$/) {
      sub(/^"/, "", s)
      sub(/"$/, "", s)
    }
    return s
  }
  # Flush the section just finished: its direct items, then its
  # groups[].items, in that order - matching the template exactly,
  # independent of which key was written first in this nav.yml block.
  function flush_section() {
    for (i = 0; i < ic; i++) print items_t[i] "\t" items_u[i]
    for (i = 0; i < gc; i++) print groups_t[i] "\t" groups_u[i]
    ic = 0
    gc = 0
  }
  /^- section:/ {
    flush_section()
    mode = ""
    next
  }
  /^  items:[[:space:]]*$/ { mode = "items"; next }
  /^  groups:[[:space:]]*$/ { mode = "groups"; next }
  mode == "items" && /^    - title:/ {
    t = $0
    sub(/^    - title:[[:space:]]*/, "", t)
    pending_title = unquote(t)
    next
  }
  mode == "items" && /^      url:/ {
    u = $0
    sub(/^      url:[[:space:]]*/, "", u)
    items_t[ic] = pending_title
    items_u[ic] = unquote(u)
    ic++
    next
  }
  mode == "groups" && /^        - title:/ {
    t = $0
    sub(/^        - title:[[:space:]]*/, "", t)
    pending_gtitle = unquote(t)
    next
  }
  mode == "groups" && /^          url:/ {
    u = $0
    sub(/^          url:[[:space:]]*/, "", u)
    groups_t[gc] = pending_gtitle
    groups_u[gc] = unquote(u)
    gc++
    next
  }
  END { flush_section() }
' "$NAV")

if [ "${#expected_urls[@]}" -ne "${#expected_titles[@]}" ]; then
  fail "${NAV} has ${#expected_titles[@]} title: entries but ${#expected_urls[@]} url: entries - expected a 1:1 pairing"
fi

# Sections declared via `groups:` (as opposed to a flat `items:`) —
# these are exactly the ones a broken groups[].items traversal would
# silently empty out (currently Built-in Tools).
groups_sections=()
while IFS= read -r line; do
  groups_sections+=("$line")
done < <(awk '
  /^- section:/ { s=$0; sub(/^- section:[[:space:]]*/, "", s) }
  /^  groups:/  { print s }
' "$NAV")

# (c) exactly 8 `## ` section headings, in nav.yml order.
actual_sections=()
while IFS= read -r line; do
  actual_sections+=("$line")
done < <(grep -E '^## ' "$LLMS" | sed -E 's/^## //')

if [ "${#expected_sections[@]}" -ne 8 ]; then
  fail "${NAV} defines ${#expected_sections[@]} sections, want 8"
fi
if [ "${#actual_sections[@]}" -ne "${#expected_sections[@]}" ]; then
  fail "${LLMS} has ${#actual_sections[@]} '## ' section(s), want ${#expected_sections[@]} (from ${NAV})"
fi
for i in "${!expected_sections[@]}"; do
  got="${actual_sections[$i]:-<missing>}"
  want="${expected_sections[$i]}"
  if [ "$got" != "$want" ]; then
    fail "section #$((i + 1)): got '${got}', want '${want}' (order must match ${NAV})"
  fi
done

# Entries: every `- [title](url): note` line, in file order.
entry_lines=()
while IFS= read -r line; do
  entry_lines+=("$line")
done < <(grep -E '^- \[' "$LLMS")

# (e) entry count and order match nav.yml's title: entries.
if [ "${#entry_lines[@]}" -ne "${#expected_titles[@]}" ]; then
  fail "${LLMS} has ${#entry_lines[@]} entries, want ${#expected_titles[@]} (from ${NAV}) — a groups[].items or items traversal likely dropped entries"
fi

entry_pattern='^- \[([^]]*)\]\(([^)]*)\): (.+)$'
for i in "${!entry_lines[@]}"; do
  line="${entry_lines[$i]}"

  # required '- [title](url): note' shape; title/url/note validated
  # individually below.
  if [[ "$line" =~ $entry_pattern ]]; then
    title="${BASH_REMATCH[1]}"
    url="${BASH_REMATCH[2]}"
    note="${BASH_REMATCH[3]}"
  else
    fail "entry #$((i + 1)) does not match '- [title](url): note': ${line}"
    continue
  fi

  want_title="${expected_titles[$i]:-<missing>}"
  if [ "$title" != "$want_title" ]; then
    fail "entry #$((i + 1)) title '${title}', want '${want_title}' (order mismatch vs ${NAV})"
  fi

  # (f) link matches the expected absolute URL for THIS position exactly
  # (BASE_URL + the nav url at the same index) - not merely "starts with
  # BASE_URL", which a wrong-but-same-origin (or duplicated) link would
  # also satisfy.
  want_url_path="${expected_urls[$i]:-}"
  if [ -z "$want_url_path" ]; then
    fail "entry #$((i + 1)) ('${title}') has no corresponding url: in ${NAV} at this index"
  else
    want_url="${BASE_URL}${want_url_path#/}"
    if [ "$url" != "$want_url" ]; then
      fail "entry #$((i + 1)) ('${title}') link '${url}' does not match expected '${want_url}' (position $((i + 1)) in ${NAV})"
    fi
  fi

  # (g) note must be non-empty once surrounding whitespace is trimmed,
  # so a whitespace-only description (e.g. " ") is treated as missing.
  trimmed_note="$(printf '%s' "$note" | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//')"
  [ -n "$trimmed_note" ] || fail "entry #$((i + 1)) ('${title}') has an empty (or whitespace-only) note"
done

# (d) items and groups[].items are both flattened: every groups-based
# section must contribute a non-empty run
# of entries between its heading and the next.
for section in "${groups_sections[@]}"; do
  count=$(awk -v s="## ${section}" '
    $0 == s { f=1; next }
    /^## /  { if (f) exit }
    f && /^- \[/ { n++ }
    END { print n+0 }
  ' "$LLMS")
  if [ "$count" -eq 0 ]; then
    fail "section '${section}' (declared via groups: in ${NAV}) has no entries in ${LLMS}"
  fi
done

if [ "$status" -eq 0 ]; then
  echo "llms.txt OK: ${#entry_lines[@]} entries across ${#actual_sections[@]} sections, matching ${NAV}"
fi
exit "$status"
