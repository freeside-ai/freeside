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
# The daemon test cases run go test ./... whole; the partition cases near the
# end unset or set this themselves.
export DAEMON_TEST_INTEGRATION_PARTITIONS=1
# The Go cache trim asks go for GOCACHE before any daemon step; the other
# go stand-ins don't answer that. The trim cases at the end set it themselves.
export FREESIDE_GOCACHE_LIMIT_GB=0
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
assert_contains 'app          generate format test linux build-mac build-ios'
assert_contains 'api          lint'
assert_contains 'scripts      syntax shellcheck suites vocabulary tier-a-paths trackercollect'
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
VACUUM=$stub run_check api lint
assert_rc 0
assert_contains 'PASS: api lint'
expected=$'lint\n-r\napi/vacuum.ruleset.yaml\n-f\napi/functions\n--details\n--fail-severity\nwarn\napi/openapi.yaml'
if [ "$(cat "$stub.args")" = "$expected" ]; then
  pass=$((pass + 1))
else
  OUT=$(cat "$stub.args")
  report_failure "vacuum received unexpected arguments"
fi

begin_case "api policy fails when a fixture passes the lint"
stub=$(make_stub vacuum-clean 0)
VACUUM=$stub run_check api policy
assert_rc 1
assert_contains 'passed the lint; it must fail rule'

begin_case "api policy fails when a fixture fails without reporting its rule"
stub=$(make_stub vacuum-silent 1)
VACUUM=$stub run_check api policy
assert_rc 1
assert_contains 'failed without reporting rule'

begin_case "api policy passes when each fixture fails and reports its rule"
stub=$TMP/vacuum-rules
cat >"$stub" <<'STUB'
#!/usr/bin/env bash
sed -n "1s/^# expects-rule: //p" "${!#}"
exit 1
STUB
chmod +x "$stub"
VACUUM=$stub run_check api policy
assert_rc 0
assert_contains 'PASS: api policy'

begin_case "a failing step fails the run"
stub=$(make_stub vacuum-fail 3)
VACUUM=$stub run_check api
assert_rc 3
case $OUT in
  *'PASS: api'*) report_failure "PASS printed after a failed step" ;;
  *) pass=$((pass + 1)) ;;
esac

# The compat step runs in a fixture repository whose origin/main holds a
# spec at level 1, floor 1, after a first commit with no compatibility
# lines at all. Each case writes the working-tree spec and a stubbed
# oasdiff report; the check compares the two specs' levels and counts the
# report's ERR-level changes.
compat_root=$TMP/compat
mkdir -p "$compat_root/scripts" "$compat_root/api"
cp "$CHECK" "$SCRIPT_DIR/api-compatibility-check.sh" \
  "$SCRIPT_DIR/api-contract-digest.sh" "$compat_root/scripts/"
printf '%s\n' 'response-property-one-of-added err' >"$compat_root/api/oasdiff-severity.txt"
compat_git() {
  env -u GIT_DIR -u GIT_WORK_TREE -u GIT_INDEX_FILE GIT_CONFIG_GLOBAL=/dev/null \
    GIT_CONFIG_NOSYSTEM=1 git -C "$compat_root" -c user.name=fixture \
    -c user.email=fixture@example.invalid "$@"
}
compat_spec() { # [<level> <floor>]; writes the fixture spec, no lines without arguments
  {
    printf 'openapi: 3.0.3\ninfo:\n  title: fixture\n  version: 0.1.0\n'
    if [ $# -eq 2 ]; then
      printf '  x-compatibility-level: %s\n  x-compatibility-floor: %s\n' "$1" "$2"
    fi
    printf 'paths: {}\n'
  } >"$compat_root/api/openapi.yaml"
}
compat_git init -q
compat_spec
compat_git add -A
compat_git commit -q -m 'spec before compatibility lines'
compat_pre=$(compat_git rev-parse HEAD)
compat_spec 1 1
compat_git commit -q -am 'spec at level 1, floor 1'
compat_git update-ref refs/remotes/origin/main HEAD
oasdiff_stub=$TMP/oasdiff
cat >"$oasdiff_stub" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$@" >"$0.args"
printf '%s' "${OASDIFF_REPORT-[]}"
exit "${OASDIFF_RC:-0}"
STUB
chmod +x "$oasdiff_stub"
compat_breaking='[{"id":"response-property-one-of-added","text":"added a union arm","level":3}]'
compat_additive='[{"id":"response-property-enum-value-added","text":"added an enum value","level":1}]'
run_compat() { # <env-assignments...> [-- <compat script args...>]; sets OUT and RC
  local assignments=() args=()
  while [ $# -gt 0 ] && [ "$1" != -- ]; do
    assignments+=("$1")
    shift
  done
  if [ $# -gt 0 ]; then
    shift
    args=("$@")
  fi
  set +e
  if [ ${#args[@]} -eq 0 ]; then
    OUT=$(env -u GIT_DIR -u GIT_WORK_TREE -u GIT_INDEX_FILE OASDIFF="$oasdiff_stub" \
      ${assignments[@]+"${assignments[@]}"} "$compat_root/scripts/check.sh" api compat 2>&1)
  else
    OUT=$(env -u GIT_DIR -u GIT_WORK_TREE -u GIT_INDEX_FILE OASDIFF="$oasdiff_stub" \
      ${assignments[@]+"${assignments[@]}"} \
      bash "$compat_root/scripts/api-compatibility-check.sh" "${args[@]}" 2>&1)
  fi
  RC=$?
  set -e
}

begin_case "api compat fails a breaking change that leaves the level unchanged"
compat_spec 1 1
run_compat OASDIFF_REPORT="$compat_breaking"
assert_rc 1
assert_contains 'response-property-one-of-added: added a union arm'
assert_contains 'x-compatibility-level raised from 1 to 2'

begin_case "api compat passes an additive change and runs the pinned oasdiff invocation"
compat_spec 1 1
run_compat OASDIFF_REPORT="$compat_additive"
assert_rc 0
assert_contains 'PASS: api compat'
case $(cat "$oasdiff_stub.args") in
  breaking$'\n'*/base.yaml$'\n'api/openapi.yaml$'\n'--flatten-allof$'\n'--severity-levels$'\n'api/oasdiff-severity.txt$'\n'--format$'\n'json)
    pass=$((pass + 1))
    ;;
  *)
    OUT=$(cat "$oasdiff_stub.args")
    report_failure "oasdiff received unexpected arguments"
    ;;
esac

begin_case "api compat passes a breaking change that raises the level"
for levels in '2 1' '2 2'; do
  # shellcheck disable=SC2086 # two words: the level and the floor
  compat_spec $levels
  run_compat OASDIFF_REPORT="$compat_breaking"
  assert_rc 0
  assert_contains 'PASS: api compat'
done

begin_case "api compat fails a floor above the level"
compat_spec 1 2
run_compat OASDIFF_REPORT="$compat_additive"
assert_rc 1
assert_contains 'x-compatibility-floor (2) exceeds x-compatibility-level (1)'

begin_case "api compat fails a level or floor that goes down"
compat_spec 1 0
run_compat OASDIFF_REPORT="$compat_additive"
assert_rc 1
assert_contains 'x-compatibility-floor went down from 1 to 0'
compat_spec 0 0
run_compat OASDIFF_REPORT="$compat_additive"
assert_rc 1
assert_contains 'x-compatibility-level went down from 1 to 0'

begin_case "api compat fails when oasdiff fails or reports an unexpected shape"
compat_spec 1 1
run_compat OASDIFF_RC=102
assert_rc 1
assert_contains 'oasdiff failed'
run_compat OASDIFF_REPORT='[{"id":"x","text":"y","level":"error"}]'
assert_rc 1
assert_contains 'oasdiff level is not a number'

begin_case "api compat reads a base with no compatibility lines as level 0, floor 0"
compat_spec 0 0
run_compat OASDIFF_REPORT="$compat_additive" -- --base "$compat_pre"
assert_rc 0
assert_contains 'level 0 (base 0), floor 0 (base 0)'
run_compat OASDIFF_REPORT="$compat_breaking" -- --base "$compat_pre"
assert_rc 1
assert_contains 'x-compatibility-level raised from 0 to 1'

begin_case "api compat fails without origin/main instead of passing"
compat_git update-ref -d refs/remotes/origin/main
compat_spec 1 1
run_compat OASDIFF_REPORT="$compat_additive"
assert_rc 1
assert_contains 'origin/main is not available'

lint_root_a=$TMP/lint-a
lint_root_b=$TMP/lint\ b
lint_cache_home=$TMP/cache\ home
for lint_root in "$lint_root_a" "$lint_root_b"; do
  mkdir -p "$lint_root/scripts" "$lint_root/daemon"
  cp "$CHECK" "$lint_root/scripts/check.sh"
done
lint_stub=$TMP/golangci-lint
cat >"$lint_stub" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "cache-set=${GOLANGCI_LINT_CACHE+yes}" \
  "cache=${GOLANGCI_LINT_CACHE-}" "pwd=$PWD" "argc=$#" "$@" >"$LINT_RECORD"
exit "${LINT_RC:-0}"
STUB
chmod +x "$lint_stub"

run_lint_check() { # <fixture-root> <env-args...>; sets OUT and RC
  local root=$1
  shift
  set +e
  OUT=$(env "$@" GOLANGCI_LINT="$lint_stub --fixture-argument" \
    LINT_RECORD="$TMP/lint.receipt" "$root/scripts/check.sh" daemon lint 2>&1)
  RC=$?
  set -e
}

assert_lint_receipt() { # <fixture-root> <expected-cache>
  local expected
  expected=$(printf '%s\n' 'cache-set=yes' "cache=$2" "pwd=$1/daemon" \
    'argc=2' '--fixture-argument' 'run')
  if [ "$(cat "$TMP/lint.receipt")" = "$expected" ]; then
    pass=$((pass + 1))
  else
    OUT=$(cat "$TMP/lint.receipt")
    report_failure "linter received unexpected environment, directory, or arguments"
  fi
}

begin_case "daemon lint defaults are distinct across checkouts and stable on reuse"
for lint_root in "$lint_root_a" "$lint_root_b" "$lint_root_a"; do
  run_lint_check "$lint_root" -u GOLANGCI_LINT_CACHE XDG_CACHE_HOME="$lint_cache_home"
  assert_rc 0
  assert_lint_receipt "$lint_root" "$lint_cache_home/freeside/golangci-lint$lint_root"
done

begin_case "daemon lint falls back to HOME when XDG_CACHE_HOME is unset"
run_lint_check "$lint_root_a" -u GOLANGCI_LINT_CACHE -u XDG_CACHE_HOME
assert_rc 0
assert_lint_receipt "$lint_root_a" "$HOME/.cache/freeside/golangci-lint$lint_root_a"

begin_case "daemon lint preserves an explicit cache containing spaces"
run_lint_check "$lint_root_b" XDG_CACHE_HOME="$lint_cache_home" \
  GOLANGCI_LINT_CACHE="$TMP/explicit cache"
assert_rc 0
assert_lint_receipt "$lint_root_b" "$TMP/explicit cache"

begin_case "daemon lint preserves an explicitly empty cache"
run_lint_check "$lint_root_a" XDG_CACHE_HOME="$lint_cache_home" GOLANGCI_LINT_CACHE=
assert_rc 0
assert_lint_receipt "$lint_root_a" ''

begin_case "daemon lint propagates the tool exit code"
run_lint_check "$lint_root_b" -u GOLANGCI_LINT_CACHE \
  XDG_CACHE_HOME="$lint_cache_home" LINT_RC=7
assert_rc 7
assert_lint_receipt "$lint_root_b" "$lint_cache_home/freeside/golangci-lint$lint_root_b"
case $OUT in
  *'PASS: daemon'*) report_failure "PASS printed after a failed lint step" ;;
  *) pass=$((pass + 1)) ;;
esac

# Each stand-in appends the GOFLAGS it inherits, so the receipt shows what
# every go invocation and the linter saw. uname reports Linux so the test
# step keeps PATH's git instead of resolving xcrun's.
mkdir "$TMP/goflags-tools"
printf '#!/bin/bash\necho Linux\n' >"$TMP/goflags-tools/uname"
cat >"$TMP/goflags-tools/go" <<'STUB'
#!/bin/bash
printf 'go %s|%s\n' "$1" "${GOFLAGS-unset}" >>"$GOFLAGS_RECORD"
STUB
cat >"$TMP/goflags-tools/golangci-lint" <<'STUB'
#!/bin/bash
printf 'lint|%s\n' "${GOFLAGS-unset}" >>"$GOFLAGS_RECORD"
STUB
chmod +x "$TMP/goflags-tools/"*
assert_goflags() { # <expected-flags>
  local expected
  expected=$(printf 'go %s|%s\n' build "$1" build "$1" test "$1" vet "$1")
  expected+=$'\n'"lint|$1"
  if [ "$(cat "$TMP/goflags.receipt")" = "$expected" ]; then
    pass=$((pass + 1))
  else
    # Keep check.sh's output, and survive a receipt no stand-in wrote.
    OUT+=$'\n'"receipt: $(cat "$TMP/goflags.receipt" 2>/dev/null || echo missing)"
    report_failure "a daemon step received unexpected GOFLAGS"
  fi
}

begin_case "daemon steps hand go and the linter -trimpath when GOFLAGS is unset"
rm -f "$TMP/goflags.receipt"
set +e
OUT=$(env -u GOFLAGS PATH="$TMP/goflags-tools:$PATH" \
  GOLANGCI_LINT="$TMP/goflags-tools/golangci-lint" \
  GOFLAGS_RECORD="$TMP/goflags.receipt" "$CHECK" daemon build test vet lint 2>&1)
RC=$?
set -e
assert_rc 0
assert_goflags '-trimpath'

begin_case "daemon steps append -trimpath to a caller's GOFLAGS"
rm -f "$TMP/goflags.receipt"
PATH=$TMP/goflags-tools:$PATH GOLANGCI_LINT=$TMP/goflags-tools/golangci-lint \
  GOFLAGS_RECORD=$TMP/goflags.receipt GOFLAGS=-p=2 run_check daemon build test vet lint
assert_rc 0
assert_goflags '-p=2 -trimpath'

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

begin_case "app linux builds FreesideModel alone and runs only the Linux suites"
# The step calls swift twice, so this stand-in appends one line per call.
stub=$TMP/swift
printf '#!/usr/bin/env bash\necho "$*" >>"%s.args"\n' "$stub" >"$stub"
chmod +x "$stub"
PATH="$TMP:$PATH" run_check app linux
assert_rc 0
OUT=$(cat "$stub.args")
assert_contains 'build --only-use-versions-from-resolved-file --target FreesideModel'
assert_contains 'test --only-use-versions-from-resolved-file --filter FreesideAPITests|FreesideModelTests'
rm -f "$stub" "$stub.args"

# The executor cases compare each tool's whole argument list, so an unset
# APP_MACVERIFY shows CI's command line unchanged and a set one shows the
# exact flags the executor's room needs, nothing more.
app_args_match() { # <stub> <expected>; compares the stub's recorded arguments
  if [ "$(cat "$1.args")" = "$2" ]; then
    pass=$((pass + 1))
  else
    OUT=$(cat "$1.args")
    report_failure "unexpected arguments from $(basename "$1")"
  fi
}
app_root=$(cd "$SCRIPT_DIR/.." && pwd)
app_job=$(dirname "$app_root")
xcode_base() { # <scheme> <destination>; the arguments CI's xcodebuild gets
  printf '%s\n' -project Freeside.xcodeproj -scheme "$1" -destination "$2" \
    -onlyUsePackageVersionsFromResolvedFile -skipPackageUpdates \
    -skipPackagePluginValidation
}
xcode_room_options=$(printf '%s\n' -derivedDataPath "$app_job/derived-data" \
  -clonedSourcePackagesDirPath "$app_job/source-packages" \
  -disablePackageRepositoryCache -scmProvider system -skipMacroValidation \
  -IDEPackageSupportDisableManifestSandbox=YES \
  -IDEPackageSupportDisablePluginExecutionSandbox=YES)
xcode_settings=$'CODE_SIGNING_ALLOWED=NO\nCOMPILER_INDEX_STORE_ENABLE=NO'
# shellcheck disable=SC2016 # $(inherited) is xcodebuild's, not the shell's
xcode_room_setting='OTHER_SWIFT_FLAGS=$(inherited) -disable-sandbox'
ios_settings=$'ARCHS=arm64\nONLY_ACTIVE_ARCH=YES'

begin_case "app test, build-mac, and build-ios add nothing without APP_MACVERIFY"
swift_stub=$(make_stub swift 0)
xcode_stub=$(make_stub xcodebuild 0)
PATH="$TMP:$PATH" APP_MACVERIFY='' run_check app test
assert_rc 0
app_args_match "$swift_stub" $'test\n--only-use-versions-from-resolved-file'
PATH="$TMP:$PATH" APP_MACVERIFY='' run_check app build-mac
assert_rc 0
app_args_match "$xcode_stub" "$(xcode_base FreesideMac platform=macOS)
$xcode_settings
build"
PATH="$TMP:$PATH" APP_MACVERIFY='' run_check app build-ios
assert_rc 0
app_args_match "$xcode_stub" "$(xcode_base FreesideIOS 'generic/platform=iOS Simulator')
$xcode_settings
$ios_settings
build"

begin_case "APP_MACVERIFY adds exactly the executor flags to app test, build-mac, and build-ios"
PATH="$TMP:$PATH" APP_MACVERIFY=1 run_check app test
assert_rc 0
app_args_match "$swift_stub" $'test\n--only-use-versions-from-resolved-file\n--disable-sandbox'
PATH="$TMP:$PATH" APP_MACVERIFY=1 run_check app build-mac
assert_rc 0
app_args_match "$xcode_stub" "$(xcode_base FreesideMac platform=macOS)
$xcode_room_options
$xcode_settings
$xcode_room_setting
build"
PATH="$TMP:$PATH" APP_MACVERIFY=1 run_check app build-ios
assert_rc 0
app_args_match "$xcode_stub" "$(xcode_base FreesideIOS 'generic/platform=iOS Simulator')
$xcode_room_options
$xcode_settings
$xcode_room_setting
$ios_settings
build"
rm -f "$swift_stub" "$swift_stub.args" "$xcode_stub" "$xcode_stub.args"

begin_case "app format covers hand-written Swift and excludes generated sources"
stub=$(make_stub xcrun 0)
PATH="$TMP:$PATH" run_check app format
assert_rc 0
OUT=$(cat "$stub.args")
assert_contains 'Package.swift'
assert_contains 'Sources/FreesideAPI/MockServer.swift'
assert_contains 'Sources/FreesideModel/AppSession.swift'
assert_contains 'Sources/FreesideCore/UnifiedListView.swift'
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

# The stand-in go records its arguments and the GIT_DIR, GOMAXPROCS, and
# TMPDIR it inherits and, on request, writes through GIT_DIR the way an
# unscrubbed fixture helper would. The stand-in getconf reports 10 cores.
mkdir "$TMP/decoy-tests"
printf '#!/bin/bash\necho Linux\n' >"$TMP/decoy-tests/uname"
printf '#!/bin/bash\necho 10\n' >"$TMP/decoy-tests/getconf"
cat >"$TMP/decoy-tests/go" <<'STUB'
#!/bin/bash
printf '%s\n' "$@" >"$RUN.args"
printf '%s\n' "${GIT_DIR-unset}" >"$RUN.git-dir"
printf '%s\n' "${GOMAXPROCS-unset}" >"$RUN.gomaxprocs"
printf '%s\n' "${TMPDIR-unset}" >"$RUN.tmpdir"
if [[ ${TMPDIR-} == *.tmp.noindex ]]; then
  [[ -d $TMPDIR && -z $(ls -A "$TMPDIR") ]] || exit 5
  # A test its timeout killed leaves read-only directories behind.
  [[ -z ${LEAVE_READ_ONLY:-} ]] ||
    { mkdir -p "$TMPDIR/left/ro" && touch "$TMPDIR/left/ro/f" && chmod 500 "$TMPDIR/left/ro"; } || exit 6
fi
# Go's version-control stamping in a test build runs git status.
git status --porcelain >/dev/null || exit 4
[[ -z ${DECOY_WRITE:-} ]] || git config freeside.decoy touched || exit 3
exit "${GO_RC:-0}"
STUB
chmod +x "$TMP/decoy-tests/"*
decoy_path=$TMP/decoy-tests:$PATH
env GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 git init -q "$TMP/ambient"
cp -R "$TMP/ambient/.git" "$TMP/ambient-before"
assert_ambient_unchanged() {
  if diff -r "$TMP/ambient-before" "$TMP/ambient/.git" >/dev/null; then
    pass=$((pass + 1))
  else
    report_failure 'the ambient repository changed'
  fi
}

begin_case "tests run under the held slot's decoy GIT_DIR, never the ambient one"
RUN=$TMP/decoy PATH=$decoy_path GIT_DIR=$TMP/ambient/.git DAEMON_TEST_MAX_RUNS=1 \
  DAEMON_TEST_STATE_DIR=$TMP/decoy-state run_check daemon test
assert_rc 0
[[ $(cat "$TMP/decoy.git-dir") == "$TMP/decoy-state/slots/1.decoy/repo/.git/worktrees/wt" ]] ||
  report_failure "GIT_DIR was $(cat "$TMP/decoy.git-dir")"
[[ ! -e $TMP/decoy-state/slots/1.decoy ]] || report_failure 'the decoy was left behind'
assert_ambient_unchanged

begin_case "global index caches and a relative state directory leave the guard intact"
printf '[core]\n\tuntrackedCache = true\n\tsplitIndex = true\n' >"$TMP/cache.gitconfig"
mkdir "$TMP/relative-cwd"
set +e
OUT=$(cd "$TMP/relative-cwd" && RUN=$TMP/decoy PATH=$decoy_path DAEMON_TEST_MAX_RUNS=1 \
  GIT_CONFIG_GLOBAL=$TMP/cache.gitconfig DAEMON_TEST_STATE_DIR=state "$CHECK" daemon test 2>&1)
RC=$?
set -e
assert_rc 0
[[ $(cat "$TMP/decoy.git-dir") == "$TMP/relative-cwd/state/slots/1.decoy/repo/.git/worktrees/wt" ]] ||
  report_failure "GIT_DIR was $(cat "$TMP/decoy.git-dir")"

begin_case "a write to the decoy fails a passing run and prints the change"
RUN=$TMP/decoy PATH=$decoy_path GIT_DIR=$TMP/ambient/.git DAEMON_TEST_MAX_RUNS=1 \
  DAEMON_TEST_STATE_DIR=$TMP/decoy-state DECOY_WRITE=1 run_check daemon test
assert_rc 1
assert_contains 'a test wrote to the decoy repository'
assert_contains 'decoy = touched'
assert_ambient_unchanged

begin_case "without a slot the decoy is temporary and a test failure keeps its code"
RUN=$TMP/decoy PATH=$decoy_path DAEMON_TEST_STATE_DIR=$TMP/decoy-off \
  DECOY_WRITE=1 GO_RC=42 run_check daemon test
assert_rc 42
assert_contains 'decoy = touched'
case $(cat "$TMP/decoy.git-dir") in
  */freeside-decoy.*/repo/.git/worktrees/wt) pass=$((pass + 1)) ;;
  *) report_failure "GIT_DIR was $(cat "$TMP/decoy.git-dir")" ;;
esac
[[ ! -e $TMP/decoy-off ]] || report_failure 'a run without a slot created state'

begin_case "without -- the test step still runs every package"
RUN=$TMP/full PATH=$decoy_path run_check daemon test
assert_rc 0
[[ $(cat "$TMP/full.args") == $'test\n-timeout\n30m\n./...' ]] ||
  report_failure "go received $(cat "$TMP/full.args")"
[[ $(tail -1 <<<"$OUT") == 'PASS: daemon test' ]] ||
  report_failure 'a full run named a selection'

begin_case "arguments after -- replace ./... under the held slot's decoy"
RUN=$TMP/narrow PATH=$decoy_path GIT_DIR=$TMP/ambient/.git DAEMON_TEST_MAX_RUNS=1 \
  DAEMON_TEST_STATE_DIR=$TMP/narrow-state run_check daemon test -- ./internal/domain/... -run 'Test A'
assert_rc 0
[[ $(cat "$TMP/narrow.args") == $'test\n-timeout\n30m\n./internal/domain/...\n-run\nTest A' ]] ||
  report_failure "go received $(cat "$TMP/narrow.args")"
[[ $(cat "$TMP/narrow.git-dir") == "$TMP/narrow-state/slots/1.decoy/repo/.git/worktrees/wt" ]] ||
  report_failure "GIT_DIR was $(cat "$TMP/narrow.git-dir")"
assert_contains 'PASS: daemon test -- ./internal/domain/... -run Test A'
assert_ambient_unchanged

begin_case "a narrow run that writes to the decoy still fails"
RUN=$TMP/narrow PATH=$decoy_path DAEMON_TEST_MAX_RUNS=1 \
  DAEMON_TEST_STATE_DIR=$TMP/narrow-state DECOY_WRITE=1 run_check daemon test -- ./internal/domain/...
assert_rc 1
[[ $(cat "$TMP/narrow.args") == $'test\n-timeout\n30m\n./internal/domain/...' ]] ||
  report_failure "go received $(cat "$TMP/narrow.args")"
assert_contains 'a test wrote to the decoy repository'
case $OUT in
  *'PASS: daemon'*) report_failure "PASS printed after a decoy write" ;;
  *) pass=$((pass + 1)) ;;
esac

begin_case "a held slot sizes GOMAXPROCS to its share of the cores"
for want in 1:unset 2:5 3:4 9223372036854775808:2; do
  limit=${want%%:*}
  rm -rf "$TMP/procs-state"
  RUN=$TMP/procs PATH=$decoy_path DAEMON_TEST_MAX_RUNS=$limit \
    DAEMON_TEST_STATE_DIR=$TMP/procs-state run_check daemon test
  assert_rc 0
  [[ $(cat "$TMP/procs.gomaxprocs") == "${want#*:}" ]] ||
    report_failure "limit $limit gave GOMAXPROCS $(cat "$TMP/procs.gomaxprocs")"
done
rm -rf "$TMP/procs-state"
RUN=$TMP/procs PATH=$decoy_path DAEMON_TEST_MAX_RUNS=3 GOMAXPROCS=7 \
  DAEMON_TEST_STATE_DIR=$TMP/procs-state run_check daemon test
assert_rc 0
[[ $(cat "$TMP/procs.gomaxprocs") == 7 ]] ||
  report_failure "a caller's GOMAXPROCS became $(cat "$TMP/procs.gomaxprocs")"
mkdir "$TMP/no-cores"
printf '#!/bin/bash\nexit 1\n' >"$TMP/no-cores/getconf"
chmod +x "$TMP/no-cores/getconf"
rm -rf "$TMP/procs-state"
RUN=$TMP/procs PATH=$TMP/no-cores:$decoy_path DAEMON_TEST_MAX_RUNS=2 \
  DAEMON_TEST_STATE_DIR=$TMP/procs-state run_check daemon test
assert_rc 0
assert_contains "cannot count cores; keeping Go's GOMAXPROCS"
[[ $(cat "$TMP/procs.gomaxprocs") == unset ]] ||
  report_failure "a failed core count gave GOMAXPROCS $(cat "$TMP/procs.gomaxprocs")"

begin_case "a held slot gives the tests a per-slot .noindex TMPDIR, removed afterwards"
RUN=$TMP/tmpdir PATH=$decoy_path DAEMON_TEST_MAX_RUNS=1 \
  DAEMON_TEST_STATE_DIR=$TMP/tmpdir-state run_check daemon test
assert_rc 0
[[ $(cat "$TMP/tmpdir.tmpdir") == "$TMP/tmpdir-state/slots/1.tmp.noindex" ]] ||
  report_failure "TMPDIR was $(cat "$TMP/tmpdir.tmpdir")"
[[ ! -e $TMP/tmpdir-state/slots/1.tmp.noindex ]] || report_failure 'the TMPDIR was left behind'
mkdir -p "$TMP/tmpdir-state/slots/1.tmp.noindex/stale/ro"
touch "$TMP/tmpdir-state/slots/1.tmp.noindex/stale/ro/f"
chmod 500 "$TMP/tmpdir-state/slots/1.tmp.noindex/stale/ro"
RUN=$TMP/tmpdir PATH=$decoy_path DAEMON_TEST_MAX_RUNS=1 LEAVE_READ_ONLY=1 \
  DAEMON_TEST_STATE_DIR=$TMP/tmpdir-state run_check daemon test
assert_rc 0
[[ ! -e $TMP/tmpdir-state/slots/1.tmp.noindex ]] || report_failure 'read-only leftovers survived'
mkdir -p "$TMP/caller-tmp"
RUN=$TMP/tmpdir PATH=$decoy_path TMPDIR=$TMP/caller-tmp DAEMON_TEST_MAX_RUNS=0 \
  DAEMON_TEST_STATE_DIR=$TMP/tmpdir-off run_check daemon test
assert_rc 0
[[ $(cat "$TMP/tmpdir.tmpdir") == "$TMP/caller-tmp" ]] ||
  report_failure "TMPDIR without a slot was $(cat "$TMP/tmpdir.tmpdir")"
[[ ! -e $TMP/tmpdir-off ]] || report_failure 'a run without a slot created state'

begin_case "a misplaced or empty -- is a usage error before go runs"
for args in 'daemon test --' 'daemon -- ./x' 'daemon build test -- ./x' \
    'daemon test build -- ./x' 'api lint -- ./x'; do
  rm -f "$TMP/usage.args"
  # shellcheck disable=SC2086 # deliberate word split of each argument list
  RUN=$TMP/usage PATH=$decoy_path run_check $args
  assert_rc 2
  assert_contains "'--' must follow 'daemon test'"
  [[ ! -e $TMP/usage.args ]] || report_failure "go ran for: $args"
done

if [[ -x /usr/bin/git ]]; then
  begin_case "macOS shim uses a stable git prefix and later steps keep PATH"
  mkdir -p "$TMP/git-tools" "$TMP/prefix/bin" "$TMP/prefix/libexec/git-core"
  # Only --exec-path is faked: the test step's decoy repository needs git.
  cat >"$TMP/prefix/bin/git" <<'STUB'
#!/bin/bash
[[ $1 == --exec-path ]] || exec /usr/bin/git "$@"
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
  if [[ $(cat "$TMP/git-args") == $'test\n-timeout\n30m\n./...' ]]; then
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

# The partition stand-in serves `go list` and `go test -json -list` for a
# module with two packages beside internal/integration, appends one line per
# call to $RUN.record (arguments, GIT_DIR, GOFLAGS, and whether SIGINT is
# ignored, tab-separated), and prints each test call's output. PARTITION_LIST=fail fails discovery with a
# build error; PARTITION_FAIL names a family whose partition exits 42;
# REST_RC sets the other packages' code; DECOY_WRITE makes each partition
# write through GIT_DIR.
mkdir "$TMP/partition-tests"
printf '#!/bin/bash\necho Linux\n' >"$TMP/partition-tests/uname"
cat >"$TMP/partition-tests/go" <<'STUB'
#!/usr/bin/env python3
import json, os, signal, subprocess, sys
args = sys.argv[1:]
integration = 'module/internal/integration'
families = ['TestFixtureA', 'TestFixtureB', 'TestFixtureC', 'TestFixtureD']
with open(os.environ['RUN'] + '.record', 'a') as record:
    ignored = signal.getsignal(signal.SIGINT) == signal.SIG_IGN
    record.write('\t'.join([' '.join(args), os.environ.get('GIT_DIR', 'unset'),
                            os.environ.get('GOFLAGS', 'unset'),
                            'sigint-ignored' if ignored else 'sigint-default']) + '\n')
if args == ['list', './...']:
    print('\n'.join(['module/a', integration, 'module/b']))
    sys.exit(0)
if '-list' in args:
    if os.environ.get('PARTITION_LIST') == 'fail':
        print(json.dumps({'ImportPath': integration, 'Action': 'build-output',
                          'Output': 'integration_test.go:7: undefined: fixtureThing\n'}))
        sys.exit(1)
    for name in families:
        print(json.dumps({'Package': integration, 'Action': 'output', 'Output': name + '\n'}))
    print(json.dumps({'Package': integration, 'Action': 'pass'}))
    sys.exit(0)
# Go's version-control stamping in a test build runs git status. Only a test
# step runs under the decoy; a build step outside a checkout has no repository
# to stamp from (#3013).
if args[0] == 'test':
    subprocess.run(['git', 'status', '--porcelain'], stdout=subprocess.DEVNULL, check=True)
if '-run' not in args:
    print('ok\t' + ' '.join(args[1:]))
    sys.exit(int(os.environ.get('REST_RC', '0')))
pattern = args[args.index('-run') + 1]
if os.environ.get('DECOY_WRITE'):
    subprocess.run(['git', 'config', 'freeside.decoy', 'touched'], check=True)
print('partition output for ' + pattern)
if os.environ.get('PARTITION_FAIL', '-') in pattern:
    print('--- FAIL: ' + os.environ['PARTITION_FAIL'])
    sys.exit(42)
print('ok\t' + integration)
STUB
chmod +x "$TMP/partition-tests/"*
partition_path=$TMP/partition-tests:$PATH
run_partitioned() { # <name> <env-args...>; full run under one slot, default count
  rm -f "$TMP/$1.record"
  set +e
  OUT=$(env -u DAEMON_TEST_INTEGRATION_PARTITIONS RUN="$TMP/$1" PATH="$partition_path" \
    GIT_DIR="$TMP/ambient/.git" DAEMON_TEST_MAX_RUNS=1 \
    DAEMON_TEST_STATE_DIR="$TMP/$1-state" "${@:2}" "$CHECK" daemon test 2>&1)
  RC=$?
  set -e
}
record_count() { # <name> <grep-pattern>; prints how many recorded calls match
  grep -c -e "$2" "$TMP/$1.record" || true
}
assert_no_pass() {
  case $OUT in
    *'PASS: daemon'*) report_failure 'PASS printed after a failed run' ;;
    *) pass=$((pass + 1)) ;;
  esac
}

begin_case "a full run tests integration as three partitions beside the other packages"
run_partitioned partitioned
assert_rc 0
partition_decoy=$TMP/partitioned-state/slots/1.decoy/repo/.git/worktrees/wt
[[ $(record_count partitioned $'^test -timeout 30m module/a module/b\t') == 1 ]] ||
  report_failure 'the other packages did not run once together'
[[ $(record_count partitioned $'^test -timeout 30m -run ^(.*)$ module/internal/integration\t') == 3 ]] ||
  report_failure 'integration did not run as three partitions'
[[ $(cut -f1 "$TMP/partitioned.record" | grep -c '^test -timeout 30m -run' | tr -d ' ') == \
  $(cut -f1 "$TMP/partitioned.record" | grep '^test -timeout 30m -run' | sort -u | wc -l | tr -d ' ') ]] ||
  report_failure 'two partitions ran the same pattern'
[[ $(cut -f2 "$TMP/partitioned.record" | sort -u) == "$partition_decoy" ]] ||
  report_failure "a go call saw another GIT_DIR: $(cut -f2 "$TMP/partitioned.record" | sort -u)"
# Background commands start with SIGINT ignored; Ctrl-C must still end them.
# Discovery runs in the foreground and keeps whatever check.sh inherited.
[[ $(grep -e '^test -timeout 30m module/' -e '^test -timeout 30m -run' "$TMP/partitioned.record" | cut -f4 | sort -u) == sigint-default ]] ||
  report_failure 'a go call started with SIGINT ignored, so Ctrl-C would orphan it'
[[ $(grep '^== .* partition ' <<<"$OUT") == "$(printf '== module/internal/integration partition %s/3\n' 1 2 3)" ]] ||
  report_failure 'partition output was not printed in partition order'
assert_contains $'partition 1/3\npartition output for ^('
[[ $(tail -1 <<<"$OUT") == 'PASS: daemon test' ]] || report_failure 'a full run named a selection'
assert_ambient_unchanged

begin_case "a partitioned run hands every go call -trimpath, discovery included"
run_partitioned partitioned-flags GOFLAGS=-p=2
assert_rc 0
[[ $(cut -f3 "$TMP/partitioned-flags.record" | sort -u) == '-p=2 -trimpath' ]] ||
  report_failure "a go call saw GOFLAGS $(cut -f3 "$TMP/partitioned-flags.record" | sort -u)"
[[ $(record_count partitioned-flags $'^list ./...\t') == 1 &&
   $(record_count partitioned-flags $'^test -json -list . module/internal/integration\t') == 1 ]] ||
  report_failure 'discovery did not run once'

begin_case "a failing partition fails the step with its code and its whole output"
run_partitioned partition-fail PARTITION_FAIL=TestFixtureB
assert_rc 42
assert_contains '--- FAIL: TestFixtureB'
assert_no_pass
[[ $(record_count partition-fail $'^test -timeout 30m -run') == 3 ]] ||
  report_failure 'a failing partition stopped the others'

begin_case "the first failing invocation in launch order sets the code"
run_partitioned partition-first PARTITION_FAIL=TestFixtureB REST_RC=5
assert_rc 5
assert_contains '--- FAIL: TestFixtureB'
assert_no_pass

begin_case "a partition's write to the decoy fails the step after every invocation"
run_partitioned partition-decoy DECOY_WRITE=1
assert_rc 1
assert_contains 'a test wrote to the decoy repository'
assert_contains 'decoy = touched'
assert_contains 'partition 3/3'
assert_no_pass
assert_ambient_unchanged

begin_case "a failed discovery prints Go's build output and tests nothing"
run_partitioned partition-list PARTITION_LIST=fail
assert_rc 1
assert_contains 'integration_test.go:7: undefined: fixtureThing'
assert_no_pass
[[ $(record_count partition-list '^test -json -list') == 1 &&
   $(record_count partition-list '^test -[rt]\|^test module') == 0 ]] ||
  report_failure 'a package was tested after discovery failed'

begin_case "a partition count that is not a positive integer fails before go runs"
for value in 0 00 abc '' -1 '1 2'; do
  run_partitioned partition-usage DAEMON_TEST_INTEGRATION_PARTITIONS="$value"
  assert_rc 2
  assert_contains 'DAEMON_TEST_INTEGRATION_PARTITIONS must be a positive integer'
  [[ ! -e $TMP/partition-usage.record ]] || report_failure "go ran with count '$value'"
done

begin_case "a bad partition count stops a full daemon check before go build runs"
rm -f "$TMP/partition-usage.record"
RUN=$TMP/partition-usage PATH=$partition_path DAEMON_TEST_INTEGRATION_PARTITIONS=abc \
  run_check daemon build test
assert_rc 2
assert_contains 'DAEMON_TEST_INTEGRATION_PARTITIONS must be a positive integer'
[[ ! -e $TMP/partition-usage.record ]] || report_failure 'go ran before the count was checked'
RUN=$TMP/partition-usage PATH=$partition_path DAEMON_TEST_INTEGRATION_PARTITIONS=abc \
  run_check daemon build
assert_rc 0
[[ $(cut -f1 "$TMP/partition-usage.record") == $'build ./...\nbuild -o /dev/null ./cmd/freeside-export' ]] ||
  report_failure "a step without test checked the count: $(cut -f1 "$TMP/partition-usage.record")"

begin_case "a partition count of 1 runs go test ./... whole without discovery"
run_partitioned partition-whole DAEMON_TEST_INTEGRATION_PARTITIONS=001
assert_rc 0
[[ $(cut -f1 "$TMP/partition-whole.record") == 'test -timeout 30m ./...' ]] ||
  report_failure "go received $(cut -f1 "$TMP/partition-whole.record")"

begin_case "a slot under a limit above 1 runs whole unless the count is set"
run_partitioned partition-share DAEMON_TEST_MAX_RUNS=2
assert_rc 0
[[ $(cut -f1 "$TMP/partition-share.record") == 'test -timeout 30m ./...' ]] ||
  report_failure "a shared slot sent go $(cut -f1 "$TMP/partition-share.record")"
run_partitioned partition-share-set DAEMON_TEST_MAX_RUNS=2 DAEMON_TEST_INTEGRATION_PARTITIONS=3
assert_rc 0
[[ $(record_count partition-share-set $'^test -timeout 30m -run ') == 3 ]] ||
  report_failure 'an explicit count under a shared slot did not partition'

begin_case "arguments after -- run whole under the default partition count"
rm -f "$TMP/partition-narrow.record"
RUN=$TMP/partition-narrow PATH=$partition_path DAEMON_TEST_INTEGRATION_PARTITIONS=abc \
  run_check daemon test -- ./internal/integration/...
assert_rc 0
[[ $(cut -f1 "$TMP/partition-narrow.record") == 'test -timeout 30m ./internal/integration/...' ]] ||
  report_failure "go received $(cut -f1 "$TMP/partition-narrow.record")"
rm -f "$TMP/partition-narrow.record"
set +e
OUT=$(env -u DAEMON_TEST_INTEGRATION_PARTITIONS RUN="$TMP/partition-narrow" PATH="$partition_path" \
  "$CHECK" daemon test -- ./internal/integration/... 2>&1)
RC=$?
set -e
assert_rc 0
[[ $(cut -f1 "$TMP/partition-narrow.record") == 'test -timeout 30m ./internal/integration/...' ]] ||
  report_failure "go received $(cut -f1 "$TMP/partition-narrow.record")"

begin_case "partial and empty shard configuration fail before go runs"
DAEMON_TEST_SHARD_KIND='' run_check daemon test
assert_rc 2
DAEMON_TEST_EVIDENCE_DIR=$TMP/evidence run_check daemon test
assert_rc 2
DAEMON_TEST_SHARD_KIND=rest DAEMON_TEST_SHARD_INDEX=1 DAEMON_TEST_SHARD_COUNT=1 \
  DAEMON_TEST_INTEGRATION_SHARDS=0 DAEMON_TEST_EVIDENCE_DIR=$TMP/evidence run_check daemon test
assert_rc 2
DAEMON_TEST_SHARD_KIND=macos DAEMON_TEST_SHARD_INDEX=1 DAEMON_TEST_SHARD_COUNT=1 \
  DAEMON_TEST_INTEGRATION_SHARDS=3 DAEMON_TEST_EVIDENCE_DIR=$TMP/evidence run_check daemon test
assert_rc 2

begin_case "complete shard configuration dispatches through the existing wrapper"
stub=$(make_stub python3 0)
PATH="$TMP:$PATH" DAEMON_TEST_SHARD_KIND=integration DAEMON_TEST_SHARD_INDEX=2 \
  DAEMON_TEST_SHARD_COUNT=3 DAEMON_TEST_INTEGRATION_SHARDS=3 \
  DAEMON_TEST_EVIDENCE_DIR=$TMP/evidence run_check daemon test
assert_rc 0
OUT=$(cat "$stub.args")
assert_contains $'scripts/daemon-test-shards.py\nrun\n--kind\nintegration\n--index\n2\n--count\n3'
assert_contains $'--integration-count\n3\n--evidence-dir'
case $OUT in
  *-timeout*) report_failure 'the shard wrapper received -timeout' ;;
  *) pass=$((pass + 1)) ;;
esac
PATH="$TMP:$PATH" DAEMON_TEST_SHARD_KIND=darwin DAEMON_TEST_SHARD_INDEX=1 \
  DAEMON_TEST_SHARD_COUNT=1 DAEMON_TEST_INTEGRATION_SHARDS=3 \
  DAEMON_TEST_EVIDENCE_DIR=$TMP/evidence run_check daemon test
assert_rc 0
OUT=$(cat "$stub.args")
assert_contains $'scripts/daemon-test-shards.py\nrun\n--kind\ndarwin\n--index\n1\n--count\n1'
PATH="$TMP:$PATH" DAEMON_TEST_SHARD_KIND=freesided DAEMON_TEST_SHARD_INDEX=1 \
  DAEMON_TEST_SHARD_COUNT=1 DAEMON_TEST_INTEGRATION_SHARDS=3 \
  DAEMON_TEST_EVIDENCE_DIR=$TMP/evidence run_check daemon test
assert_rc 0
OUT=$(cat "$stub.args")
assert_contains $'scripts/daemon-test-shards.py\nrun\n--kind\nfreesided\n--index\n1\n--count\n1'

begin_case "shard runs ignore the integration partition count"
rm -f "$stub.args"
PATH="$TMP:$PATH" DAEMON_TEST_INTEGRATION_PARTITIONS=abc DAEMON_TEST_SHARD_KIND=rest \
  DAEMON_TEST_SHARD_INDEX=1 DAEMON_TEST_SHARD_COUNT=1 DAEMON_TEST_INTEGRATION_SHARDS=3 \
  DAEMON_TEST_EVIDENCE_DIR=$TMP/evidence run_check daemon test
assert_rc 0
OUT=$(cat "$stub.args")
assert_contains $'scripts/daemon-test-shards.py\nrun\n--kind\nrest'

begin_case "shard runs refuse arguments after -- before go or the wrapper runs"
rm -f "$stub.args" "$TMP/shard-args.args"
RUN=$TMP/shard-args PATH="$TMP:$decoy_path" DAEMON_TEST_SHARD_KIND=rest DAEMON_TEST_SHARD_INDEX=1 \
  DAEMON_TEST_SHARD_COUNT=1 DAEMON_TEST_INTEGRATION_SHARDS=3 \
  DAEMON_TEST_EVIDENCE_DIR=$TMP/evidence run_check daemon test -- ./internal/domain/...
assert_rc 2
assert_contains "don't apply in shard mode"
[[ ! -e $stub.args ]] || report_failure 'the shard wrapper ran'
RUN=$TMP/shard-args PATH=$decoy_path DAEMON_TEST_EVIDENCE_DIR=$TMP/evidence \
  run_check daemon test -- ./internal/domain/...
assert_rc 2
assert_contains "don't apply in shard mode"
[[ ! -e $TMP/shard-args.args ]] || report_failure 'go ran in shard mode'

# The trim stand-ins append every go call and every python3 call to one
# record, in order: go answers `env GOCACHE` with FAKE_GOCACHE, and python3
# records its arguments before running the real interpreter, so a case sees
# which cache, limit, and state the helper got and that it ran before the
# step. The limit is whole gigabytes, so these cases never delete anything;
# test-go-cache-trim.sh covers the helper itself.
mkdir "$TMP/trim-tools" "$TMP/trim-cache"
real_python3=$(command -v python3)
printf '#!/bin/bash\necho Linux\n' >"$TMP/trim-tools/uname"
cat >"$TMP/trim-tools/go" <<'STUB'
#!/bin/bash
if [ "$*" = 'env GOCACHE' ]; then
  echo 'go env GOCACHE' >>"$TRIM_RECORD"
  printf '%s\n' "$FAKE_GOCACHE"
  exit 0
fi
echo "go $1" >>"$TRIM_RECORD"
STUB
cat >"$TMP/trim-tools/python3" <<STUB
#!/bin/bash
echo "python3 \$*" >>"\$TRIM_RECORD"
exec "$real_python3" "\$@"
STUB
chmod +x "$TMP/trim-tools/"*
trim_path=$TMP/trim-tools:$PATH
trim_state=$TMP/trim-xdg/freeside/go-cache-trim
trim_line() { # <limit-bytes>; prints the python3 record of one trim
  printf 'python3 %s --cache %s --limit-bytes %s --state %s' \
    "$(dirname "$SCRIPT_DIR")/scripts/go-cache-trim.py" "$TMP/trim-cache" "$1" "$trim_state"
}
run_trim_check() { # <env assignments...> -- <check.sh args...>; sets OUT and RC
  local vars=()
  while [ "$1" != -- ]; do vars+=("$1"); shift; done
  shift
  rm -f "$TMP/trim.record" "$trim_state/last-walk"
  set +e
  OUT=$(env -u CI -u FREESIDE_GOCACHE_LIMIT_GB PATH="$trim_path" TRIM_RECORD="$TMP/trim.record" \
    FAKE_GOCACHE="$TMP/trim-cache" XDG_CACHE_HOME="$TMP/trim-xdg" ${vars[@]+"${vars[@]}"} "$CHECK" "$@" 2>&1)
  RC=$?
  set -e
}
assert_record() { # <expected record>
  if [ "$(cat "$TMP/trim.record" 2>/dev/null)" = "$1" ]; then
    pass=$((pass + 1))
  else
    OUT+=$'\n'"record: $(cat "$TMP/trim.record" 2>/dev/null || echo missing)"
    report_failure "unexpected go and python3 calls"
  fi
}
no_trim=$'go build\ngo build'

begin_case "a daemon step trims the cache go env GOCACHE names, once, before the step"
run_trim_check -- daemon build
assert_rc 0
assert_record "go env GOCACHE"$'\n'"$(trim_line 40000000000)"$'\n'"$no_trim"
if [[ -e $trim_state/last-walk ]]; then pass=$((pass + 1)); else report_failure 'the trim wrote no stamp'; fi

begin_case "a narrow test run trims before go test"
run_trim_check -- daemon test -- ./internal/domain/...
assert_rc 0
assert_record "go env GOCACHE"$'\n'"$(trim_line 40000000000)"$'\n'"go test"

begin_case "a limit of 0, or CI with the limit unset, turns the trim off"
run_trim_check FREESIDE_GOCACHE_LIMIT_GB=0 -- daemon build
assert_rc 0
assert_record "$no_trim"
run_trim_check FREESIDE_GOCACHE_LIMIT_GB=000 -- daemon build
assert_rc 0
assert_record "$no_trim"
run_trim_check CI=true -- daemon build
assert_rc 0
assert_record "$no_trim"
if [[ ! -e $trim_state/last-walk ]]; then pass=$((pass + 1)); else report_failure 'an off trim wrote a stamp'; fi

begin_case "an explicit limit under CI turns the trim on, without leading zeroes"
run_trim_check CI=true FREESIDE_GOCACHE_LIMIT_GB=007 -- daemon build
assert_rc 0
assert_record "go env GOCACHE"$'\n'"$(trim_line 7000000000)"$'\n'"$no_trim"

begin_case "a malformed limit is a usage error before any step runs"
for limit in abc -1 1.5 ''; do
  run_trim_check FREESIDE_GOCACHE_LIMIT_GB="$limit" -- daemon build
  assert_rc 2
  assert_contains 'FREESIDE_GOCACHE_LIMIT_GB must be a non-negative integer'
  assert_record ''
done

begin_case "no cache directory skips the trim and runs the step"
for cache in off '' "$TMP/no-such-cache"; do
  run_trim_check FAKE_GOCACHE="$cache" -- daemon build
  assert_rc 0
  assert_contains 'skipping the trim'
  assert_record "go env GOCACHE"$'\n'"$no_trim"
done

begin_case "a failed trim prints a notice and runs the step"
touch "$TMP/trim-xdg-file"
run_trim_check XDG_CACHE_HOME="$TMP/trim-xdg-file" -- daemon build
assert_rc 0
assert_contains 'the Go cache trim failed; continuing'
assert_contains 'PASS: daemon build'

begin_case "another component doesn't run the trim"
run_trim_check -- scripts syntax
assert_rc 0
assert_record ''

echo "assertions: $pass passed, $fail failed"
if [ "$fail" -ne 0 ]; then
  exit 1
fi
