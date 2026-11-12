#!/usr/bin/env bash
# Community is not mounted on docs.docker.com; technical links must be portable.
set -euo pipefail

cd "$(dirname "$0")/.."

status=0
while IFS= read -r file; do
  case "$file" in
    docs/community/*) community=1 ;;
    *) community=0 ;;
  esac
  if ! awk -v file="$file" -v community="$community" '
    FNR == 1 && /^---$/ { frontmatter = 1; next }
    frontmatter && /^---$/ { frontmatter = 0; next }
    frontmatter { next }
    {
      stale = /community\/(telemetry|opentelemetry|troubleshooting)(\/|[)#?"[:space:]]|$)/ ||
        /https:\/\/docs\.docker\.com\/ai\/docker-agent\/community\//
      sibling = community && /\]\((\.\.\/)+(telemetry|opentelemetry|troubleshooting)(\/|[)#?"[:space:]]|$)/
      unmounted = !community && /\]\((\.\.\/)+community\//
      if (stale || sibling || unmounted) {
        printf "%s:%d: stale or unmounted community link: %s\n", file, FNR, $0
        failed = 1
      }
    }
    END { exit failed }
  ' "$file"; then
    status=1
  fi
done < <(find docs -type f \( -name '*.md' -o -name '*.html' -o -name '*.yml' -o -name '*.yaml' \) \
  -not -path 'docs/public/*' -not -path 'docs/node_modules/*' -not -path 'docs/resources/*' \
  -not -path 'docs/data/redirects.yml' | sort; printf '%s\n' README.md)

if [ "$status" -eq 0 ]; then
  echo 'community links OK: no legacy URLs or technical relative links into Community'
fi
exit "$status"
