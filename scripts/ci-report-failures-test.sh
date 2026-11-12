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
        printf '5\ttest-windows\thttps://example.test/jobs/5\n'
        printf '6\ttest-linux\thttps://example.test/jobs/6\n'
        printf '7\ttest-linux\thttps://example.test/jobs/7\n'
      fi
      ;;
    collapse) printf '4\ttest-linux\thttps://example.test/jobs/4\n' ;;
  esac
  exit 0
fi

if [ "$1" = api ] && [[ "$*" == *'/actions/jobs/'*'/logs'* ]]; then
  # Real gh refuses to print a response containing ANSI escapes without this.
  [[ "$*" == *'--allow-escape-sequences'* ]] || {
    echo 'job log fetch must pass --allow-escape-sequences' >&2
    exit 1
  }
  args="$*"
  job_id="${args##*/actions/jobs/}"
  job_id="${job_id%/logs}"
  case "$job_id" in
    1)
      cat <<'EOF'
2026-01-01T00:00:00.0000000Z === RUN   TestShared
2026-01-01T00:00:00.0000000Z === RUN   TestShared/real-subtest
2026-01-01T00:00:00.0000000Z     shared_test.go:10:
2026-01-01T00:00:00.0000000Z         Error Trace: shared_test.go:10
2026-01-01T00:00:00.0000000Z         Error:       Not equal:
2026-01-01T00:00:00.0000000Z                      expected:
2026-01-01T00:00:00.0000000Z                      true
2026-01-01T00:00:00.0000000Z                      actual:
2026-01-01T00:00:00.0000000Z                      false
2026-01-01T00:00:00.0000000Z --- FAIL: TestShared/real-subtest (0.01s)
2026-01-01T00:00:00.0000000Z --- FAIL: TestShared (0.01s)
2026-01-01T00:00:00.0000000Z FAIL
2026-01-01T00:00:00.0000000Z FAIL	example/shared	0.02s
EOF
      ;;
    2)
      cat <<'EOF'
2026-01-01T00:00:00.0000000Z ##[group]Run actions/checkout
2026-01-01T00:00:00.0000000Z Syncing repository: docker/docker-agent
2026-01-01T00:00:00.0000000Z ##[endgroup]
2026-01-01T00:00:00.0000000Z ##[group]Run task test
2026-01-01T00:00:00.0000000Z ##[endgroup]
2026-01-01T00:00:00.0000000Z ok  	example/fast	0.01s
2026-01-01T00:00:00.0000000Z ok  	example/fast2	0.01s
2026-01-01T00:00:00.0000000Z ok  	example/fast3	0.01s
2026-01-01T00:00:00.0000000Z ok  	example/fast4	0.01s
2026-01-01T00:00:00.0000000Z ok  	example/fast5	0.01s
2026-01-01T00:00:00.0000000Z ok  	example/fast6	0.01s
2026-01-01T00:00:00.0000000Z ok  	example/fast7	0.01s
2026-01-01T00:00:00.0000000Z ok  	example/fast8	0.01s
2026-01-01T00:00:00.0000000Z panic: test timed out after 10m0s
2026-01-01T00:00:00.0000000Z FAIL	example/slow	600.00s
2026-01-01T00:00:00.0000000Z ##[error]Process completed with exit code 1.
EOF
      ;;
    3)
      cat <<'EOF'
2026-01-01T00:00:00.1234567Z WARNING: DATA RACE
2026-01-01T00:00:00.1234567Z --- FAIL: TestShared/race-subtest (0.01s)
2026-01-01T00:00:00.1234567Z --- FAIL: TestShared (0.02s)
EOF
      ;;
    5)
      cat <<'EOF'
=== RUN   TestJSONReporter
=== RUN   TestJSONReporter/json-subtest
    reporter_test.go:20: assertion diagnostic before root marker
--- FAIL: TestJSONReporter/json-subtest (0.01s)
--- FAIL: TestJSONReporter (0.02s)
    reporter_test.go:21: assertion diagnostic after root marker
FAIL	example/reporter	0.02s
EOF
      ;;
    6)
      cat <<'EOF'
test-race	Test	2026-01-01T00:00:00.1234567Z	--- FAIL: TestLegacyFirst (0.01s)
test-race	Test	2026-01-01T00:00:00.1234567Z	    first_test.go:10: first root diagnostic
test-race	Test	2026-01-01T00:00:00.1234567Z	--- FAIL: TestLegacySecond (0.02s)
test-race	Test	2026-01-01T00:00:00.1234567Z	    second_test.go:20: second root diagnostic
test-race	Test	2026-01-01T00:00:00.1234567Z	FAIL
test-race	Test	2026-01-01T00:00:00.1234567Z	FAIL	example/legacy	0.02s
EOF
      ;;
    7)
      cat <<'EOF'
=== RUN   TestParallelFirst
=== PAUSE TestParallelFirst
=== CONT  TestParallelFirst
=== RUN   TestParallelFirst/child
    parallel_test.go:10: first parallel diagnostic
--- FAIL: TestParallelFirst (0.01s)
    --- FAIL: TestParallelFirst/child (0.01s)
=== RUN   TestParallelSecond
=== PAUSE TestParallelSecond
=== CONT  TestParallelSecond
=== NAME  TestParallelSecond/child
    parallel_test.go:20: second parallel diagnostic
--- FAIL: TestParallelSecond (0.02s)
    --- FAIL: TestParallelSecond/child (0.02s)
FAIL
FAIL	example/parallel	0.03s
EOF
      ;;
    4)
      for i in {1..11}; do
        printf '%s\n' "2026-01-01T00:00:00.0000000Z --- FAIL: TestMany$i (0.01s)"
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
  local max_new_issues="${3:-10}"
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
grep -q 'Failure reporting complete: 7 new issue(s), 0 deferred candidate(s).' <<< "$output"
[ "$(find "$TMP/capture" -name 'body-*.md' | wc -l | tr -d ' ')" -eq 7 ]
grep -q "The top-level test \`TestShared\` failed" "$TMP/capture"/body-*.md
grep -q "The top-level test \`TestJSONReporter\` failed" "$TMP/capture"/body-*.md
grep -q "The top-level test \`TestLegacyFirst\` failed" "$TMP/capture"/body-*.md
grep -q "The top-level test \`TestLegacySecond\` failed" "$TMP/capture"/body-*.md
grep -q "The top-level test \`TestParallelFirst\` failed" "$TMP/capture"/body-*.md
grep -q "The top-level test \`TestParallelSecond\` failed" "$TMP/capture"/body-*.md
grep -q 'test-linux.*assertion' "$TMP/capture"/body-*.md
grep -q 'test-race.*data race' "$TMP/capture"/body-*.md
legacy_first_body="$(grep -l "The top-level test \`TestLegacyFirst\` failed" "$TMP/capture"/body-*.md)"
legacy_second_body="$(grep -l "The top-level test \`TestLegacySecond\` failed" "$TMP/capture"/body-*.md)"
grep -q -- '^    --- FAIL: TestLegacyFirst (0.01s)$' "$legacy_first_body"
grep -q 'first root diagnostic' "$legacy_first_body"
if grep -q 'TestLegacySecond\|second root diagnostic' "$legacy_first_body"; then
  echo 'second root test leaked into the first legacy excerpt' >&2
  exit 1
fi
grep -q -- '^    --- FAIL: TestLegacySecond (0.02s)$' "$legacy_second_body"
grep -q 'second root diagnostic' "$legacy_second_body"
if grep -q 'TestLegacyFirst\|first root diagnostic' "$legacy_second_body"; then
  echo 'first root test leaked into the second legacy excerpt' >&2
  exit 1
fi
if grep -q 'FAIL.*example/legacy' "$legacy_first_body" "$legacy_second_body"; then
  echo 'package-level FAIL noise leaked into a legacy root test excerpt' >&2
  exit 1
fi
parallel_first_body="$(grep -l "The top-level test \`TestParallelFirst\` failed" "$TMP/capture"/body-*.md)"
parallel_second_body="$(grep -l "The top-level test \`TestParallelSecond\` failed" "$TMP/capture"/body-*.md)"
grep -q 'first parallel diagnostic' "$parallel_first_body"
if grep -q 'TestParallelSecond\|second parallel diagnostic' "$parallel_first_body"; then
  echo 'second parallel root leaked into the first excerpt' >&2
  exit 1
fi
grep -q 'second parallel diagnostic' "$parallel_second_body"
if grep -q 'TestParallelFirst\|first parallel diagnostic' "$parallel_second_body"; then
  echo 'first parallel root leaked into the second excerpt' >&2
  exit 1
fi
grep -q 'Error Trace: shared_test.go:10' "$TMP/capture"/body-*.md
grep -q 'expected:' "$TMP/capture"/body-*.md
grep -q 'actual:' "$TMP/capture"/body-*.md
if grep -q 'FAIL.*example/shared' "$TMP/capture"/body-*.md; then
  echo 'package-level FAIL noise leaked into the root test excerpt' >&2
  exit 1
fi
json_body="$(grep -l "The top-level test \`TestJSONReporter\` failed" "$TMP/capture"/body-*.md)"
json_marker_line="$(grep -n -- '^    --- FAIL: TestJSONReporter (0.02s)$' "$json_body" | cut -d: -f1)"
json_context_line="$(grep -n 'assertion diagnostic before root marker' "$json_body" | cut -d: -f1)"
[ "$json_marker_line" -lt "$json_context_line" ]
grep -q 'assertion diagnostic after root marker' "$json_body"
if grep -q 'FAIL.*example/reporter' "$json_body"; then
  echo 'package-level FAIL noise leaked into the JSON root test excerpt' >&2
  exit 1
fi
if grep -q 'The top-level test `Test.*/' "$TMP/capture"/body-*.md || \
  grep -q -- '--title \[Flaky\\ test\]\\ Test.*/' "$TMP/capture/calls"; then
  echo 'subtest failure became an issue candidate' >&2
  exit 1
fi
grep -q 'no top-level failed tests' "$TMP/capture"/body-*.md
grep -q 'test-windows (timeout)' "$TMP/capture"/body-*.md
timeout_body="$(grep -l 'test-windows (timeout)' "$TMP/capture"/body-*.md)"
grep -q 'panic: test timed out after 10m0s' "$timeout_body"
if grep -q 'Syncing repository' "$timeout_body"; then
  echo 'job-level excerpt should show the log tail, not the setup steps' >&2
  exit 1
fi
if grep -q 'run view\|log-failed' "$TMP/capture/calls"; then
  echo 'reporter must read job logs through the API, not gh run view' >&2
  exit 1
fi
grep -q -- '--type Bug' "$TMP/capture/calls"
grep -q -- '--assignee dgageot' "$TMP/capture/calls"
grep -q -- '--label flaky-test\\,automated\\,area/testing\\,status/needs-triage\\,area/ci' "$TMP/capture/calls"

rm -f "$TMP/capture"/*
output="$(run_reporter create true 1)"
grep -q 'DRY RUN: would create issue' <<< "$output"
grep -q 'Deferred new candidate after reaching the per-run limit' <<< "$output"
grep -q 'Failure reporting complete: 1 new issue(s), 6 deferred candidate(s).' <<< "$output"

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
