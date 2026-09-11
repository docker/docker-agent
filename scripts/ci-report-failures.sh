#!/usr/bin/env bash
set -euo pipefail

readonly REPORT_MARKER='<!-- ci-main-failure-run -->'
readonly ISSUE_KEY_PREFIX='<!-- ci-main-failure-key: '
readonly NEW_ISSUE_LABELS='flaky-test,automated,area/testing,status/needs-triage'
readonly ASSIGNEE='dgageot'

require_command() {
  command -v "$1" >/dev/null 2>&1 || {
    echo "ci-report-failures.sh: required command not found: $1" >&2
    exit 1
  }
}

require_input() {
  local name="$1"
  [ -n "${!name:-}" ] || {
    echo "ci-report-failures.sh: $name is required" >&2
    exit 1
  }
}

normalize_log() {
  perl -pe 's/\e\[[0-?]*[ -\/]*[@-~]//g; s/\r$//; s/[^\x09\x0A\x20-\x7E]/?/g' "$1" > "$2"
}

classify_log() {
  local log_file="$1"
  if grep -q 'WARNING: DATA RACE' "$log_file"; then
    printf '%s\n' 'data race'
  elif grep -Eq 'panic: test timed out|test timed out after' "$log_file"; then
    printf '%s\n' 'timeout'
  else
    printf '%s\n' 'assertion'
  fi
}

failed_tests() {
  awk '
    {
      message = $0
      if (index($0, "\t") > 0) {
        split($0, fields, "\t")
        message = fields[4]
      }
      if (message ~ /^--- FAIL: Test[^[:space:]()]*/) {
        sub(/^--- FAIL: /, "", message)
        sub(/[[:space:](].*$/, "", message)
        print message
      }
    }
  ' "$1" | LC_ALL=C sort -u
}

log_excerpt() {
  local log_file="$1"
  local test_name="${2:-}"

  awk -v test_name="$test_name" '
    BEGIN {
      needle = test_name == "" ? "" : "--- FAIL: " test_name
    }
    {
      if (!found && (needle == "" || index($0, needle) > 0)) {
        found = 1
        lines = 12
      }
      if (found && lines > 0 && chars < 1600) {
        remaining = 1600 - chars
        text = length($0) > remaining ? substr($0, 1, remaining) : $0
        print text
        chars += length(text) + 1
        lines--
      }
    }
  ' "$log_file"
}

append_candidate() {
  local candidates_file="$1"
  local key="$2"
  local candidate_type="$3"
  local name="$4"
  local job="$5"
  local job_url="$6"
  local classification="$7"
  local reason="$8"
  local excerpt_file="$9"

  jq -cn \
    --arg key "$key" \
    --arg type "$candidate_type" \
    --arg name "$name" \
    --arg job "$job" \
    --arg job_url "$job_url" \
    --arg classification "$classification" \
    --arg reason "$reason" \
    --rawfile excerpt "$excerpt_file" \
    '{key: $key, type: $type, name: $name, job: $job, job_url: $job_url,
      classification: $classification, reason: $reason, excerpt: $excerpt}' >> "$candidates_file"
}

issue_title() {
  local candidate_type="$1"
  local name="$2"
  if [ "$candidate_type" = test ]; then
    printf '[Flaky test] %.220s\n' "$name"
  else
    printf '[CI failure] Main %s job failed\n' "$name"
  fi
}

write_issue_body() {
  local records_file="$1"
  local body_file="$2"
  local key candidate_type name reason
  key="$(jq -r '.[0].key' "$records_file")"
  candidate_type="$(jq -r '.[0].type' "$records_file")"
  name="$(jq -r '.[0].name' "$records_file")"
  reason="$(jq -r '.[0].reason' "$records_file")"

  {
    printf '%s%s -->\n\n' "$ISSUE_KEY_PREFIX" "$key"
    if [ "$candidate_type" = test ]; then
      printf '%s\n\n' "The top-level test \`$name\` failed on \`main\`."
    else
      printf '%s\n\n' "The \`$name\` test job failed on \`main\` and is tracked as a job-level CI failure."
      printf '**Reason:** %s\n\n' "$reason"
    fi
    printf '**Run:** [%s](%s)  \n' "$RUN_ID" "$RUN_URL"
    printf '%s\n\n' "**Commit range:** [\`${BEFORE_SHA:0:12}...${AFTER_SHA:0:12}\`]($COMPARE_URL)"
    printf '### Observed failures\n\n'
    jq -r '.[] | "- [`\(.job)`](\(.job_url)): \(.classification)"' "$records_file"
    printf '\n### Log excerpts\n'
    while IFS= read -r record; do
      local job classification
      job="$(jq -r '.job' <<< "$record")"
      classification="$(jq -r '.classification' <<< "$record")"
      printf '\n**%s (%s)**\n\n' "$job" "$classification"
      jq -r '.excerpt | split("\n")[] | "    " + .' <<< "$record"
    done < <(jq -c '.[]' "$records_file")
  } > "$body_file"
}

write_run_comment() {
  local comment_file="$1"
  {
    printf '%s\n\n' "Observed again in [CI run $RUN_ID]($RUN_URL) for [\`${AFTER_SHA:0:12}\`]($COMPARE_URL)."
    printf '%s\n' "$REPORT_MARKER"
  } > "$comment_file"
}

comment_is_due() {
  local issue_number="$1"
  local issue_created_at="$2"
  local comments_file="$3"
  local most_recent

  gh api --paginate "repos/$GH_REPO/issues/$issue_number/comments?per_page=100" > "$comments_file"
  most_recent="$(jq -sr --arg marker "$REPORT_MARKER" --arg created "$issue_created_at" '
    add
    | [.[] | select(.user.type == "Bot" and (.body | contains($marker))) | .created_at]
    | max // $created
  ' "$comments_file")"

  jq -en --arg timestamp "$most_recent" '
    ($timestamp | fromdateiso8601) < (now - 86400)
  ' >/dev/null
}

summary() {
  printf '%s\n' "$1"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    printf '%s\n' "$1" >> "$GITHUB_STEP_SUMMARY"
  fi
}

main() {
  require_command gh
  require_command jq
  require_command perl
  require_input GH_TOKEN
  require_input GH_REPO
  require_input RUN_ID
  require_input BEFORE_SHA
  require_input AFTER_SHA

  MAX_NEW_ISSUES="${MAX_NEW_ISSUES:-5}"
  [[ "$MAX_NEW_ISSUES" =~ ^[0-9]+$ ]] || {
    echo 'ci-report-failures.sh: MAX_NEW_ISSUES must be a non-negative integer' >&2
    exit 1
  }
  case "${DRY_RUN:-false}" in
    true | 1) DRY_RUN=true ;;
    false | 0 | '') DRY_RUN=false ;;
    *)
      echo 'ci-report-failures.sh: DRY_RUN must be true or false' >&2
      exit 1
      ;;
  esac

  RUN_URL="https://github.com/$GH_REPO/actions/runs/$RUN_ID"
  COMPARE_URL="https://github.com/$GH_REPO/compare/$BEFORE_SHA...$AFTER_SHA"
  export RUN_URL COMPARE_URL MAX_NEW_ISSUES DRY_RUN

  local jobs_file candidates_file issues_file
  TMP_DIR="$(mktemp -d)"
  trap 'rm -rf "$TMP_DIR"' EXIT
  jobs_file="$TMP_DIR/jobs.tsv"
  candidates_file="$TMP_DIR/candidates.jsonl"
  issues_file="$TMP_DIR/issues.json"
  : > "$candidates_file"

  # An absent label would make every candidate look new before creation fails.
  gh api "repos/$GH_REPO/labels/flaky-test" >/dev/null || {
    echo "ci-report-failures.sh: required label 'flaky-test' is missing or unavailable" >&2
    exit 1
  }

  gh api --paginate "repos/$GH_REPO/actions/runs/$RUN_ID/jobs?per_page=100" \
    --jq '.jobs[] | select(.conclusion == "failure") | select(.name == "test-linux" or .name == "test-windows" or .name == "test-race") | [.id, .name, .html_url] | @tsv' \
    > "$jobs_file"

  if [ ! -s "$jobs_file" ]; then
    summary 'No failed target test jobs found.'
    return
  fi

  local job_id job_name job_url raw_log clean_log tests_file excerpt_file classification test_count reason test_name
  while IFS=$'\t' read -r job_id job_name job_url; do
    raw_log="$TMP_DIR/$job_id.raw.log"
    clean_log="$TMP_DIR/$job_id.log"
    tests_file="$TMP_DIR/$job_id.tests"
    gh run view "$RUN_ID" --repo "$GH_REPO" --job "$job_id" --log-failed > "$raw_log"
    normalize_log "$raw_log" "$clean_log"
    failed_tests "$clean_log" > "$tests_file"
    classification="$(classify_log "$clean_log")"
    test_count="$(wc -l < "$tests_file" | tr -d ' ')"

    if [ "$test_count" -eq 0 ] || [ "$test_count" -gt 10 ]; then
      excerpt_file="$TMP_DIR/$job_id.excerpt"
      log_excerpt "$clean_log" > "$excerpt_file"
      if [ "$test_count" -eq 0 ]; then
        reason='no top-level failed tests were found in the failed-step log'
      else
        reason="$test_count distinct top-level tests failed; individual issues were collapsed"
      fi
      append_candidate "$candidates_file" "job:$job_name" job "$job_name" "$job_name" \
        "$job_url" "$classification" "$reason" "$excerpt_file"
      continue
    fi

    while IFS= read -r test_name; do
      excerpt_file="$TMP_DIR/$job_id.$(printf '%s' "$test_name" | shasum | cut -d' ' -f1).excerpt"
      log_excerpt "$clean_log" "$test_name" > "$excerpt_file"
      local test_key
      test_key="$(printf '%s' "$test_name" | shasum -a 256 | cut -d' ' -f1)"
      append_candidate "$candidates_file" "test:$test_key" test "$test_name" "$job_name" \
        "$job_url" "$classification" '' "$excerpt_file"
    done < "$tests_file"
  done < "$jobs_file"

  gh issue list --repo "$GH_REPO" --state open --label flaky-test --limit 1000 \
    --json number,title,body,createdAt > "$issues_file"

  local created=0 deferred=0 key records_file candidate_type name title marker issue_number issue_created_at body_file comments_file comment_file
  while IFS= read -r key; do
    records_file="$TMP_DIR/records.json"
    jq -s --arg key "$key" '[.[] | select(.key == $key)]' "$candidates_file" > "$records_file"
    candidate_type="$(jq -r '.[0].type' "$records_file")"
    name="$(jq -r '.[0].name' "$records_file")"
    title="$(issue_title "$candidate_type" "$name")"
    marker="$ISSUE_KEY_PREFIX$key -->"
    issue_number="$(jq -r --arg marker "$marker" '[.[] | select(.body | contains($marker))][0].number // empty' "$issues_file")"

    if [ -n "$issue_number" ]; then
      issue_created_at="$(jq -r --argjson number "$issue_number" '.[] | select(.number == $number) | .createdAt' "$issues_file")"
      comments_file="$TMP_DIR/$issue_number.comments.json"
      if comment_is_due "$issue_number" "$issue_created_at" "$comments_file"; then
        comment_file="$TMP_DIR/$issue_number.comment.md"
        write_run_comment "$comment_file"
        if [ "$DRY_RUN" = true ]; then
          summary "DRY RUN: would comment on existing issue #$issue_number: $title"
        else
          gh issue comment "$issue_number" --repo "$GH_REPO" --body-file "$comment_file" >/dev/null
          summary "Commented on existing issue #$issue_number: $title"
        fi
      else
        summary "Existing issue #$issue_number was already updated by automation within 24 hours: $title"
      fi
      continue
    fi

    if [ "$created" -ge "$MAX_NEW_ISSUES" ]; then
      deferred=$((deferred + 1))
      summary "Deferred new candidate after reaching the per-run limit: $title"
      continue
    fi

    body_file="$TMP_DIR/new-$created.md"
    write_issue_body "$records_file" "$body_file"
    local labels="$NEW_ISSUE_LABELS"
    if [ "$candidate_type" = job ]; then
      labels+=',area/ci'
    fi
    if [ "$DRY_RUN" = true ]; then
      summary "DRY RUN: would create issue: $title"
    else
      gh issue create --repo "$GH_REPO" --title "$title" --body-file "$body_file" \
        --type Bug --label "$labels" --assignee "$ASSIGNEE" >/dev/null
      summary "Created issue: $title"
    fi
    created=$((created + 1))
  done < <(jq -r '.key' "$candidates_file" | LC_ALL=C sort -u)

  summary "Failure reporting complete: $created new issue(s), $deferred deferred candidate(s)."
}

main "$@"
