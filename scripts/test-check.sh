#!/usr/bin/env bash
# test-check.sh — fixtures for scripts/check.sh, the component check entry
# point. Tool commands are replaced by recording stand-ins, so the suite
# needs neither Go, Swift, nor network. Exit code: 0 when every assertion
# passes, 1 otherwise.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
CHECK=$SCRIPT_DIR/check.sh

TMP=$(mktemp -d)
export DAEMON_TEST_MAX_RUNS=0 DAEMON_TEST_STATE_DIR=$TMP/state
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

begin_case "app format covers hand-written Swift and excludes generated sources"
stub=$(make_stub xcrun 0)
PATH="$TMP:$PATH" run_check app format
assert_rc 0
OUT=$(cat "$stub.args")
assert_contains 'Package.swift'
assert_contains 'Sources/FreesideAPI/MockServer.swift'
assert_contains 'Sources/FreesideCore/AppSession.swift'
assert_contains 'Tests/FreesideAPITests/'
assert_contains 'Apps/macOS/'
case $OUT in
  *GeneratedSources*|*--recursive*) report_failure "format includes generated sources or recursive roots" ;;
  *) pass=$((pass + 1)) ;;
esac

# These children use temporary state only, even when the real machine has
# daemon suites running. A deadline keeps failed fixtures from hanging CI.
mkdir "$TMP/local-tests"
cat >"$TMP/local-tests/go" <<'STUB'
#!/bin/bash
touch "$RUN.started"
end=$((SECONDS + 30))
while [[ ! -e $RUN.release ]]; do
  ((SECONDS < end)) || exit 1
  sleep .1
done
exit "${GO_RC:-0}"
STUB
printf '#!/bin/bash\necho Linux\n' >"$TMP/local-tests/uname"
chmod +x "$TMP/local-tests/"*
test_path=$TMP/local-tests:$PATH
wait_for() { # <file> [content]; records a failed assertion on timeout
  local end=$((SECONDS + 30))
  until [[ -e $1 ]] && { [[ $# == 1 ]] || grep -q "$2" "$1"; }; do
    if ((SECONDS >= end)); then
      report_failure "timed out waiting for $*"
      return 1
    fi
    sleep .1
  done
}
start_run() { # <name> <limit> <state>; caller takes $! from background call
  exec env RUN="$TMP/$1" DAEMON_TEST_MAX_RUNS="$2" DAEMON_TEST_STATE_DIR="$3" PATH="$test_path" \
    python3 -c 'import os,sys; os.setsid(); os.execvp(sys.argv[1],sys.argv[1:])' \
    "$CHECK" daemon test >"$TMP/$1.log" 2>&1
}

begin_case "one slot queues a run, reports holders, and preserves its exit code"
start_run holder 1 "$TMP/queue" & holder=$!
wait_for "$TMP/holder.started"
GO_RC=42 start_run waiter 1 "$TMP/queue" & waiter=$!
wait_for "$TMP/waiter.log" 'waiting for a slot'
wait_for "$TMP/waiter.log" 'run PID'
OUT=$(cat "$TMP/waiter.log")
assert_contains 'limit 1'
assert_contains "$TMP/queue"
assert_contains 'run PID'
assert_contains 'DAEMON_TEST_MAX_RUNS=0'
[[ ! -e $TMP/waiter.started ]] || report_failure 'queued go started early'
touch "$TMP/holder.release" "$TMP/waiter.release"
wait "$holder"
set +e
wait "$waiter"
RC=$?
set -e
assert_rc 42

begin_case "SIGKILL frees a slot without cleanup"
start_run killed 1 "$TMP/killed-state" & holder=$!
wait_for "$TMP/killed.started"
start_run survivor 1 "$TMP/killed-state" & waiter=$!
wait_for "$TMP/survivor.log" 'waiting for a slot'
kill -KILL -- "-$holder"
wait "$holder" 2>/dev/null || :
wait_for "$TMP/survivor.started"
touch "$TMP/survivor.release"
wait "$waiter"

begin_case "killing the reported holder PID frees its slot"
start_run reported 1 "$TMP/reported-state" & holder=$!
wait_for "$TMP/reported.started"
read -r _ _ holder_pid _ <"$TMP/reported-state/slots/1"
kill -KILL "${holder_pid%:}"
wait "$holder" 2>/dev/null || :
start_run next 1 "$TMP/reported-state" & waiter=$!
wait_for "$TMP/next.started"
touch "$TMP/next.release"
wait "$waiter"
# The accepted single-shell kill leaves go running; end this fixture's group.
kill -KILL -- "-$holder" 2>/dev/null || :

begin_case "an explicit limit of two lets both runs start"
start_run first 2 "$TMP/two" & holder=$!
wait_for "$TMP/first.started"
start_run second 2 "$TMP/two" & waiter=$!
wait_for "$TMP/second.started"
touch "$TMP/first.release" "$TMP/second.release"
wait "$holder"
wait "$waiter"

begin_case "off and CI defaults create no state; local default takes a slot"
touch "$TMP/immediate.release"
RUN=$TMP/immediate PATH=$test_path DAEMON_TEST_STATE_DIR=$TMP/off run_check daemon test
assert_rc 0
[[ ! -e $TMP/off ]] || report_failure 'disabled limit created state'
set +e
OUT=$(env -u DAEMON_TEST_MAX_RUNS CI=true RUN="$TMP/immediate" PATH="$test_path" \
  DAEMON_TEST_STATE_DIR="$TMP/ci" "$CHECK" daemon test 2>&1)
RC=$?
set -e
assert_rc 0
[[ ! -e $TMP/ci ]] || report_failure 'CI default created state'
env -u CI -u DAEMON_TEST_MAX_RUNS RUN="$TMP/immediate" PATH="$test_path" \
  DAEMON_TEST_STATE_DIR="$TMP/default" "$CHECK" daemon test >"$TMP/default.log" 2>&1
[[ -e $TMP/default/slots/1 ]] || report_failure 'local default took no slot'
RUN=$TMP/immediate PATH=$test_path DAEMON_TEST_MAX_RUNS=9223372036854775808 \
  DAEMON_TEST_STATE_DIR=$TMP/large run_check daemon test
assert_rc 0
[[ -e $TMP/large/slots/1 ]] || report_failure 'large valid limit took no slot'

begin_case "the local default queues behind one active run"
start_run default-holder 1 "$TMP/default-limit" & holder=$!
wait_for "$TMP/default-holder.started"
env -u CI -u DAEMON_TEST_MAX_RUNS RUN="$TMP/default-waiter" PATH="$test_path" \
  DAEMON_TEST_STATE_DIR="$TMP/default-limit" "$CHECK" daemon test >"$TMP/default-waiter.log" 2>&1 & waiter=$!
wait_for "$TMP/default-waiter.log" 'waiting for a slot'
[[ ! -e $TMP/default-waiter.started ]] || report_failure 'default exceeded one run'
touch "$TMP/default-holder.release" "$TMP/default-waiter.release"
wait "$holder"
wait "$waiter"

begin_case "malformed limit is a usage error before go runs"
RUN=$TMP/invalid PATH=$test_path DAEMON_TEST_MAX_RUNS=abc run_check daemon test
assert_rc 2
assert_contains 'must be a non-negative integer'
[[ ! -e $TMP/invalid.started ]] || report_failure 'invalid limit ran go'
RUN=$TMP/invalid PATH=$test_path DAEMON_TEST_MAX_RUNS='' run_check daemon test
assert_rc 2

begin_case "unwritable state runs without the limit"
touch "$TMP/not-a-directory"
RUN=$TMP/immediate PATH=$test_path DAEMON_TEST_MAX_RUNS=1 \
  DAEMON_TEST_STATE_DIR=$TMP/not-a-directory/state run_check daemon test
assert_rc 0
assert_contains 'running without the limit'

begin_case "test failure keeps its exit code"
RUN=$TMP/immediate PATH=$test_path DAEMON_TEST_MAX_RUNS=1 GO_RC=42 run_check daemon test
assert_rc 42

begin_case "an orphaned test child does not keep the slot"
cat >"$TMP/local-tests/go" <<'STUB'
#!/bin/bash
if [[ ! -e $RUN.child ]]; then
  sleep 30 &
  echo "$!" >"$RUN.child"
fi
STUB
RUN=$TMP/orphan PATH=$test_path DAEMON_TEST_MAX_RUNS=1 \
  DAEMON_TEST_STATE_DIR=$TMP/orphan-state "$CHECK" daemon test >"$TMP/orphan.log" 2>&1
RUN=$TMP/orphan PATH=$test_path DAEMON_TEST_MAX_RUNS=1 \
  DAEMON_TEST_STATE_DIR=$TMP/orphan-state "$CHECK" daemon test >"$TMP/orphan-second.log" 2>&1 & waiter=$!
wait_for "$TMP/orphan-second.log" 'PASS: daemon test'
kill "$(cat "$TMP/orphan.child")"
wait "$waiter"

if [[ -x /usr/bin/git ]]; then
  begin_case "macOS shim uses a stable git prefix and later steps keep PATH"
  mkdir -p "$TMP/git-tools" "$TMP/prefix/bin" "$TMP/prefix/libexec/git-core"
  cat >"$TMP/prefix/bin/git" <<'STUB'
#!/bin/bash
echo "${0%/bin/git}/libexec/git-core"
STUB
  cp "$TMP/prefix/bin/git" "$TMP/prefix/libexec/git-core/git"
  printf '#!/bin/bash\necho Darwin\n' >"$TMP/git-tools/uname"
  printf '#!/bin/bash\necho "%s/prefix/bin/git"\n' "$TMP" >"$TMP/git-tools/xcrun"
  cat >"$TMP/git-tools/go" <<STUB
#!/bin/bash
command -v git >>"$TMP/git-paths"
printf '%s\n' "\$PATH" >"$TMP/git-environment"
printf '%s\n' "\$@" >"$TMP/git-args"
STUB
  chmod +x "$TMP/git-tools/"* "$TMP/prefix/bin/git" "$TMP/prefix/libexec/git-core/git"
  git_path=$TMP/git-tools:/usr/bin:/bin
  CI='' PATH=$git_path run_check daemon test vet
  assert_rc 0
  expected=$DAEMON_TEST_STATE_DIR/git$TMP/prefix/bin/git
  OUT=$(cat "$TMP/git-paths")
  [[ $OUT == "$expected"$'\n/usr/bin/git' ]] || report_failure 'git PATH leaked to vet'
  CI='' PATH=$git_path run_check daemon test
  assert_rc 0
  [[ $(tail -1 "$TMP/git-paths") == "$expected" ]] || report_failure 'git path changed across runs'

  begin_case "CI keeps the git shim and creates no default state"
  set +e
  OUT=$(env -u DAEMON_TEST_MAX_RUNS CI=true PATH="$git_path" \
    DAEMON_TEST_STATE_DIR="$TMP/ci-git" "$CHECK" daemon test 2>&1)
  RC=$?
  set -e
  assert_rc 0
  if [[ $(cat "$TMP/git-environment") == "$git_path" &&
      $(tail -1 "$TMP/git-paths") == /usr/bin/git ]]; then
    pass=$((pass + 1))
  else
    report_failure 'CI changed git PATH'
  fi
  if [[ $(cat "$TMP/git-args") == $'test\n./...' ]]; then
    pass=$((pass + 1))
  else
    report_failure 'CI changed the test command'
  fi
  if [[ ! -e $TMP/ci-git ]]; then
    pass=$((pass + 1))
  else
    report_failure 'CI git setup created state'
  fi
  CI=true PATH=$git_path DAEMON_TEST_MAX_RUNS=1 \
    DAEMON_TEST_STATE_DIR=$TMP/ci-git-limit run_check daemon test
  assert_rc 0
  if [[ -e $TMP/ci-git-limit/slots/1 && ! -e $TMP/ci-git-limit/git &&
      $(cat "$TMP/git-environment") == "$git_path" ]]; then
    pass=$((pass + 1))
  else
    report_failure 'explicit CI limit failed to preserve git PATH and take a slot'
  fi

  begin_case "another git first, Linux, and failed xcrun keep PATH"
  cp "$TMP/prefix/bin/git" "$TMP/git-tools/git"
  CI='' PATH=$git_path run_check daemon test
  assert_rc 0
  [[ $(tail -1 "$TMP/git-paths") == "$TMP/git-tools/git" ]] || report_failure 'other git replaced'
  rm "$TMP/git-tools/git"
  printf '#!/bin/bash\necho Linux\n' >"$TMP/git-tools/uname"
  CI='' PATH=$git_path run_check daemon test
  assert_rc 0
  [[ $(tail -1 "$TMP/git-paths") == /usr/bin/git ]] || report_failure 'Linux PATH changed'
  printf '#!/bin/bash\necho Darwin\n' >"$TMP/git-tools/uname"
  for response in 'exit 1' 'exit 0' 'echo /missing/bin/git'; do
    printf '#!/bin/bash\n%s\n' "$response" >"$TMP/git-tools/xcrun"
    CI='' PATH=$git_path run_check daemon test
    assert_rc 0
    [[ $(tail -1 "$TMP/git-paths") == /usr/bin/git ]] || report_failure 'bad xcrun changed PATH'
  done
else
  echo 'skip: git PATH fixtures require /usr/bin/git'
fi

echo "assertions: $pass passed, $fail failed"
if [ "$fail" -ne 0 ]; then
  exit 1
fi
