#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin"

cat > "$TMP/bin/go" <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
printf '%s GOMAXPROCS=%s\n' "$*" "${GOMAXPROCS-unset}" >> "$MOCK_DIR/calls"
generator=github.com/docker/docker-agent/sdk/typescript/gen
if [ "$1" = list ]; then
  case "$SCENARIO" in
    discovery-failure) echo "$generator"; exit 1 ;;
    empty) exit 0 ;;
    missing) printf 'example/one\nexample/two\n'; exit 0 ;;
    duplicate) printf 'example/one\n%s\n%s\n' "$generator" "$generator"; exit 0 ;;
    target-only) echo "$generator"; exit 0 ;;
    *) printf 'example/one\n%s\nexample/two\n' "$generator"; exit 0 ;;
  esac
fi
if [ "$SCENARIO" = fail-ordinary ] && [[ "$*" == *example/one* ]]; then exit 1; fi
if [ "$SCENARIO" = fail-generator ] && [[ "$*" == *"$generator"* ]]; then exit 1; fi
MOCK
chmod +x "$TMP/bin/go"
export PATH="$TMP/bin:$PATH" MOCK_DIR="$TMP" GOMAXPROCS=7

run_case() {
  local scenario=$1 expected=$2 status=0
  : > "$TMP/calls"
  SCENARIO="$scenario" bash "$ROOT/scripts/test-race-ci.sh" > "$TMP/output" 2>&1 || status=$?
  if [ "$expected" = success ] && [ "$status" -ne 0 ]; then
    echo "$scenario unexpectedly failed" >&2; cat "$TMP/output" >&2; exit 1
  fi
  if [ "$expected" = failure ] && [ "$status" -eq 0 ]; then
    echo "$scenario unexpectedly succeeded" >&2; exit 1
  fi
}

cat > "$TMP/expected" <<'EOF'
list -race ./... GOMAXPROCS=7
test -race -shuffle=on -count=1 example/one example/two GOMAXPROCS=7
test -race -shuffle=on -count=1 github.com/docker/docker-agent/sdk/typescript/gen GOMAXPROCS=1
EOF
run_case success success
diff -u "$TMP/expected" "$TMP/calls"

for scenario in discovery-failure empty missing duplicate target-only; do
  run_case "$scenario" failure
  head -n 1 "$TMP/expected" > "$TMP/expected-calls"
  diff -u "$TMP/expected-calls" "$TMP/calls"
done
run_case fail-ordinary failure
head -n 2 "$TMP/expected" > "$TMP/expected-calls"
diff -u "$TMP/expected-calls" "$TMP/calls"
run_case fail-generator failure
diff -u "$TMP/expected" "$TMP/calls"

echo 'test-race-ci regression tests passed'
