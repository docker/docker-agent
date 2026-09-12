#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPORTER="$ROOT/scripts/ci-report-failures.sh"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin" "$TMP/capture"

cat > "$TMP/bin/gh" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
printf '%q ' "$@" >> "$GH_MOCK_DIR/calls"
printf '\n' >> "$GH_MOCK_DIR/calls"

if [ "$1 $2" = 'api repos/docker/docker-agent/labels/flaky-test' ]; then
  if [ "$SCENARIO" = missing-label ]; then
    exit 1
  fi
  printf '{}\n'
  exit 0
fi

if [ "$1" = api ] && [[ "$*" == *'/actions/runs/123/jobs?per_page=100'* ]]; then
  case "$SCENARIO" in
    no-failures) ;;
    create | existing-recent | existing-old)
      printf '1\ttest-linux\thttps://example.test/jobs/1\n'
      if [ "$SCENARIO" = create ]; then
        printf '2\ttest-windows\thttps://example.test/jobs/2\n'
        printf '3\ttest-race\thttps://example.test/jobs/3\n'
      fi
      ;;
    collapse) printf '4\ttest-linux\thttps://example.test/jobs/4\n' ;;
  esac
  exit 0
fi

if [ "$1 $2" = 'run view' ]; then
  case "$7" in
    1)
      cat <<'EOF'
test-linux	Test	2026-01-01T00:00:00Z	--- FAIL: TestShared (0.01s)
test-linux	Test	2026-01-01T00:00:00Z	    shared_test.go:10: expected true
EOF
      ;;
    2)
      printf '%s\n' $'test-windows\tTest\t2026-01-01T00:00:00Z\tpanic: test timed out after 10m0s'
      ;;
    3)
      cat <<'EOF'
test-race	Test	2026-01-01T00:00:00Z	WARNING: DATA RACE
test-race	Test	2026-01-01T00:00:00Z	--- FAIL: TestShared (0.02s)
EOF
      ;;
    4)
      for i in {1..11}; do
        printf '%s\n' "test-linux"$'\t'"Test"$'\t'"2026-01-01T00:00:00Z"$'\t'"--- FAIL: TestMany$i (0.01s)"
      done
      ;;
  esac
  exit 0
fi

if [ "$1 $2" = 'issue list' ]; then
  case "$SCENARIO" in
    existing-recent | existing-old)
      printf '%s\n' '[{"number":42,"title":"[Flaky test] TestShared","body":"<!-- ci-main-failure-key: test:ea6083bed7216dffa478fb24b0830c3e42f97b535132b81b2f76c00352a6d261 -->","createdAt":"2000-01-01T00:00:00Z"}]'
      ;;
    *) printf '[]\n' ;;
  esac
  exit 0
fi

if [ "$1" = api ] && [[ "$*" == *'/issues/42/comments?per_page=100'* ]]; then
  if [ "$SCENARIO" = existing-recent ]; then
    printf '%s\n' '[{"user":{"type":"Bot"},"body":"<!-- ci-main-failure-run -->","created_at":"2999-01-01T00:00:00Z"}]'
  else
    printf '%s\n' '[{"user":{"type":"Bot"},"body":"<!-- ci-main-failure-run -->","created_at":"2000-01-01T00:00:00Z"}]'
  fi
  exit 0
fi

if [ "$1 $2" = 'issue create' ]; then
  body=''
  while [ $# -gt 0 ]; do
    if [ "$1" = --body-file ]; then
      body="$2"
      break
    fi
    shift
  done
  count="$(find "$GH_MOCK_DIR" -name 'body-*.md' | wc -l | tr -d ' ')"
  cp "$body" "$GH_MOCK_DIR/body-$count.md"
  exit 0
fi

if [ "$1 $2" = 'issue comment' ]; then
  touch "$GH_MOCK_DIR/commented"
  exit 0
fi

echo "unexpected gh invocation: $*" >&2
exit 2
MOCK
chmod +x "$TMP/bin/gh"

run_reporter() {
  local scenario="$1"
  local dry_run="${2:-false}"
  local max_new_issues="${3:-5}"
  SCENARIO="$scenario" \
  GH_MOCK_DIR="$TMP/capture" \
  GH_TOKEN=test-token \
  GH_REPO=docker/docker-agent \
  RUN_ID=123 \
  BEFORE_SHA=1111111111111111111111111111111111111111 \
  AFTER_SHA=2222222222222222222222222222222222222222 \
  DRY_RUN="$dry_run" \
  MAX_NEW_ISSUES="$max_new_issues" \
  PATH="$TMP/bin:$PATH" \
    "$REPORTER"
}

rm -f "$TMP/capture"/*
output="$(run_reporter no-failures)"
grep -q 'No failed target test jobs found.' <<< "$output"

rm -f "$TMP/capture"/*
output="$(run_reporter create)"
grep -q 'Failure reporting complete: 2 new issue(s), 0 deferred candidate(s).' <<< "$output"
[ "$(find "$TMP/capture" -name 'body-*.md' | wc -l | tr -d ' ')" -eq 2 ]
grep -q "The top-level test \`TestShared\` failed" "$TMP/capture"/body-*.md
grep -q 'test-linux.*assertion' "$TMP/capture"/body-*.md
grep -q 'test-race.*data race' "$TMP/capture"/body-*.md
grep -q 'no top-level failed tests' "$TMP/capture"/body-*.md
grep -q -- '--type Bug' "$TMP/capture/calls"
grep -q -- '--assignee dgageot' "$TMP/capture/calls"
grep -q -- '--label flaky-test\\,automated\\,area/testing\\,status/needs-triage\\,area/ci' "$TMP/capture/calls"

rm -f "$TMP/capture"/*
output="$(run_reporter create true 1)"
grep -q 'DRY RUN: would create issue' <<< "$output"
grep -q 'Deferred new candidate after reaching the per-run limit' <<< "$output"
grep -q 'Failure reporting complete: 1 new issue(s), 1 deferred candidate(s).' <<< "$output"

rm -f "$TMP/capture"/*
output="$(run_reporter collapse)"
grep -q 'Failure reporting complete: 1 new issue(s), 0 deferred candidate(s).' <<< "$output"
grep -q '11 distinct top-level tests failed' "$TMP/capture"/body-*.md
grep -q 'ci-main-failure-key: job:test-linux' "$TMP/capture"/body-*.md

rm -f "$TMP/capture"/*
output="$(run_reporter existing-recent)"
grep -q 'already updated by automation within 24 hours' <<< "$output"
[ ! -e "$TMP/capture/commented" ]

rm -f "$TMP/capture"/*
output="$(run_reporter existing-old true)"
grep -q 'DRY RUN: would comment on existing issue #42' <<< "$output"
[ ! -e "$TMP/capture/commented" ]

rm -f "$TMP/capture"/*
if run_reporter missing-label >"$TMP/missing-label.out" 2>&1; then
  echo 'expected a missing label to fail' >&2
  exit 1
fi
grep -q "required label 'flaky-test' is missing" "$TMP/missing-label.out"
if grep -q 'issue create' "$TMP/capture/calls"; then
  echo 'unexpected issue creation after missing label' >&2
  exit 1
fi

printf '%s\n' 'ci-report-failures-test.sh: OK'
