#!/usr/bin/env bash
# test-check.sh — fixtures for scripts/check.sh, the component check entry
# point. Tool commands are replaced by recording stand-ins, so the suite
# needs neither Go, Swift, nor network. Exit code: 0 when every assertion
# passes, 1 otherwise.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
CHECK=$SCRIPT_DIR/check.sh

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
case_number=0
CASE=''
OUT=''
RC=0

begin_case() {
  case_number=$((case_number + 1))
  CASE=$(printf '%02d %s' "$case_number" "$1")
  echo "case: $CASE"
}

report_failure() {
  fail=$((fail + 1))
  echo "FAIL [$CASE]: $*"
  printf '%s\n' "$OUT" | sed 's/^/    | /'
}

assert_rc() {
  if [ "$RC" -eq "$1" ]; then
    pass=$((pass + 1))
  else
    report_failure "expected rc=$1, got rc=$RC"
  fi
}

assert_contains() {
  case $OUT in
    *"$1"*) pass=$((pass + 1)) ;;
    *) report_failure "output does not contain: $1" ;;
  esac
}

run_check() { # <args...>; sets OUT and RC
  set +e
  OUT=$("$CHECK" "$@" 2>&1)
  RC=$?
  set -e
}

make_stub() { # <name> <exit-code>; prints a stand-in that records its args
  local stub=$TMP/$1
  printf '#!/usr/bin/env bash\nprintf "%%s\\n" "$@" >"%s.args"\nexit %s\n' \
    "$stub" "$2" >"$stub"
  chmod +x "$stub"
  printf '%s' "$stub"
}

begin_case "--list prints every component with its steps"
run_check --list
assert_rc 0
assert_contains 'daemon       build test vet lint'
assert_contains 'app          generate format test build-mac build-ios'
assert_contains 'api          lint'
assert_contains 'scripts      syntax shellcheck suites vocabulary trackercollect'
assert_contains 'convergence  run'
assert_contains 'docs         plan-links'

begin_case "no arguments is a usage error"
run_check
assert_rc 2
assert_contains 'usage:'

begin_case "unknown component is a usage error"
run_check nosuch
assert_rc 2
assert_contains "unknown component 'nosuch'"

begin_case "unknown step is a usage error before any step runs"
stub=$(make_stub vacuum-unused 0)
VACUUM=$stub run_check api lint nosuch
assert_rc 2
assert_contains "unknown step 'nosuch' for api"
if [ ! -e "$stub.args" ]; then
  pass=$((pass + 1))
else
  report_failure "a step ran despite the usage error"
fi

begin_case "api lint runs the pinned vacuum invocation through VACUUM"
stub=$(make_stub vacuum 0)
VACUUM=$stub run_check api
assert_rc 0
assert_contains 'PASS: api lint'
expected=$'lint\n-r\napi/vacuum.ruleset.yaml\n--details\n--fail-severity\nwarn\napi/openapi.yaml'
if [ "$(cat "$stub.args")" = "$expected" ]; then
  pass=$((pass + 1))
else
  OUT=$(cat "$stub.args")
  report_failure "vacuum received unexpected arguments"
fi

begin_case "a failing step fails the run"
stub=$(make_stub vacuum-fail 3)
VACUUM=$stub run_check api
assert_rc 3
case $OUT in
  *'PASS: api'*) report_failure "PASS printed after a failed step" ;;
  *) pass=$((pass + 1)) ;;
esac

begin_case "scripts shellcheck covers scripts, app scripts, and hooks"
stub=$(make_stub shellcheck 0)
SHELLCHECK=$stub run_check scripts shellcheck
assert_rc 0
assert_contains 'PASS: scripts shellcheck'
OUT=$(cat "$stub.args")
assert_contains '/scripts/check.sh'
assert_contains '/app/scripts/generate-api-client.sh'
assert_contains '/.githooks/commit-msg'

begin_case "app build-ios keeps the single-architecture simulator settings"
stub=$(make_stub xcodebuild 0)
set +e
OUT=$(PATH="$TMP:$PATH" "$CHECK" app build-ios 2>&1)
RC=$?
set -e
assert_rc 0
OUT=$(cat "$stub.args")
assert_contains 'FreesideIOS'
assert_contains 'ARCHS=arm64'
assert_contains 'ONLY_ACTIVE_ARCH=YES'

echo "assertions: $pass passed, $fail failed"
if [ "$fail" -ne 0 ]; then
  exit 1
fi
