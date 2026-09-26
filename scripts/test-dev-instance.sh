#!/usr/bin/env bash
# test-dev-instance.sh — regression suite for dev-instance.sh.
#
# Copies dev-instance.sh and supervised-paths.sh into a synthetic repo under
# a temp directory, so the instance root lands there, and puts stand-ins on
# PATH: `go` writes a fake freesided that records its arguments and publishes
# readiness.json into its -state-dir; `xcodebuild` writes a fake FreesideMac
# bundle whose executable records its arguments and FREESIDE_ENV; `uname`
# reports the platform the case chooses, so the app path runs on Linux CI.
# The cases cover the daemon and app arguments, the printed root and URL,
# --no-seed, --daemon-only, cleanup of both processes and the root on SIGTERM
# (also when a second one arrives mid-cleanup), on an early daemon exit, and
# when the app quits, and the refusals that
# create nothing. SIGINT is not driven: a background job in a non-interactive
# shell starts with SIGINT ignored, so it cannot be trapped here; the script
# maps INT to the same exit path as TERM.
#
# Exit code: 0 when every assertion passes, 1 otherwise.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
CASE=''
CASE_DIR=''
OUT=''
RC=0
case_n=0

begin_case() {
  CASE=$1
  case_n=$((case_n + 1))
  CASE_DIR=$TMP/case-$case_n
  mkdir -p "$CASE_DIR"
  echo "case: $CASE"
}

report_failure() {
  fail=$((fail + 1))
  echo "FAIL [$CASE]: $*"
  printf '%s\n' "$OUT" | sed 's/^/    | /'
}

ok() { pass=$((pass + 1)); }

# ---------------------------------------------------------------- stand-ins
STUB_BIN=$TMP/bin
mkdir -p "$STUB_BIN"

cat >"$STUB_BIN/go" <<'GO_STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${STUB_DIR:?}/go.log"
out=''
while [[ $# -gt 0 ]]; do
  [[ "$1" != -o ]] || out=$2
  shift
done
mkdir -p "$(dirname "$out")"
cat >"$out" <<'DAEMON'
#!/usr/bin/env bash
printf '%s\n' "$@" >"$STUB_DIR/daemon.args"
printf '%s\n' "$$" >"$STUB_DIR/daemon.pid"
state=''
while [[ $# -gt 0 ]]; do
  [[ "$1" != -state-dir ]] || state=$2
  shift
done
if [[ "${FAKE_DAEMON_MODE:-}" == exit-early ]]; then
  echo 'fake freesided: refusing to start' >&2
  exit 3
fi
if [[ "${FAKE_DAEMON_MODE:-}" == slow-stop ]]; then
  trap 'echo term >"$STUB_DIR/daemon.term"; sleep 2; exit 0' TERM
else
  trap 'exit 0' TERM
fi
printf '{"api_url":"http://127.0.0.1:43210","pairing_code":"ABCD","environment":"ephemeral","run_id":"r1"}\n' \
  >"$state/readiness.json"
for _ in $(seq 1 300); do sleep 0.1; done
DAEMON
chmod +x "$out"
GO_STUB

cat >"$STUB_BIN/xcodebuild" <<'XCODE_STUB'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"${STUB_DIR:?}/xcodebuild.log"
derived=''
while [[ $# -gt 0 ]]; do
  [[ "$1" != -derivedDataPath ]] || derived=$2
  shift
done
exe="$derived/Build/Products/Debug/FreesideMac.app/Contents/MacOS/FreesideMac"
mkdir -p "$(dirname "$exe")"
cat >"$exe" <<'APP'
#!/usr/bin/env bash
printf '%s\n' "$@" >"$STUB_DIR/app.args"
printf '%s\n' "${FREESIDE_ENV:-}" >"$STUB_DIR/app.env"
printf '%s\n' "$$" >"$STUB_DIR/app.pid"
[[ "${FAKE_APP_MODE:-}" != quit ]] || exit 0
if [[ "${FAKE_APP_MODE:-}" == crash ]]; then
  echo 'fake FreesideMac: crashed at startup'
  exit 3
fi
trap 'exit 0' TERM
for _ in $(seq 1 300); do sleep 0.1; done
APP
chmod +x "$exe"
XCODE_STUB

cat >"$STUB_BIN/uname" <<'UNAME_STUB'
#!/usr/bin/env bash
printf '%s\n' "${FAKE_UNAME:-Darwin}"
UNAME_STUB
chmod +x "$STUB_BIN/go" "$STUB_BIN/xcodebuild" "$STUB_BIN/uname"

# make_repo <dir>: a synthetic worktree holding the scripts under test.
make_repo() {
  mkdir -p "$1/scripts" "$1/daemon" "$1/app"
  cp "$SCRIPT_DIR/dev-instance.sh" "$SCRIPT_DIR/supervised-paths.sh" "$1/scripts/"
}

# start_instance <repo> [arg...]: runs the script in the background with the
# case's stand-ins; the caller waits for it with finish_instance.
INSTANCE_PID=''
start_instance() {
  local repo=$1
  shift
  STUB_DIR=$CASE_DIR PATH="$STUB_BIN:$PATH" FREESIDE_ENV=prod \
    bash "$repo/scripts/dev-instance.sh" "$@" \
    >"$CASE_DIR/stdout" 2>"$CASE_DIR/stderr" &
  INSTANCE_PID=$!
}

# wait_for <file> <pattern>: bounded wait for a line matching pattern.
wait_for() {
  local _
  for _ in $(seq 1 100); do
    grep -q -- "$2" "$1" 2>/dev/null && return 0
    kill -0 "$INSTANCE_PID" 2>/dev/null || break
    sleep 0.1
  done
  grep -q -- "$2" "$1" 2>/dev/null
}

# finish_instance: waits for the script, bounded, so a regression that leaves
# it running (a missing refusal, say) fails the case instead of hanging.
finish_instance() {
  local _ hung=false
  for _ in $(seq 1 150); do
    kill -0 "$INSTANCE_PID" 2>/dev/null || break
    sleep 0.1
  done
  if kill -0 "$INSTANCE_PID" 2>/dev/null; then
    hung=true
    kill -TERM "$INSTANCE_PID" 2>/dev/null || true
  fi
  set +e
  wait "$INSTANCE_PID"
  RC=$?
  set -e
  OUT=$(cat "$CASE_DIR/stdout" "$CASE_DIR/stderr")
  [[ "$hung" == false ]] || report_failure "still running after 15s; sent SIGTERM"
}

# run_instance <repo> [arg...]: runs the script to completion.
run_instance() {
  start_instance "$@"
  finish_instance
}

assert_rc() {
  if [ "$RC" -eq "$1" ]; then ok; else report_failure "expected rc=$1, got rc=$RC"; fi
}

assert_line() { # <file> <exact line>
  if grep -qxF -- "$2" "$1" 2>/dev/null; then ok; else report_failure "$(basename "$1") lacks line: $2"; fi
}

assert_no_line() { # <file> <exact line>
  if grep -qxF -- "$2" "$1" 2>/dev/null; then report_failure "$(basename "$1") has line: $2"; else ok; fi
}

assert_contains() {
  case "$OUT" in
  *"$1"*) ok ;;
  *) report_failure "output does not contain: $1" ;;
  esac
}

assert_not_exists() {
  if [ -e "$1" ]; then report_failure "unexpected path exists: $1"; else ok; fi
}

assert_stopped() { # <pid file>
  local pid
  pid=$(cat "$1" 2>/dev/null || true)
  if [[ -n "$pid" ]] && ! kill -0 "$pid" 2>/dev/null; then
    ok
  else
    report_failure "process from $(basename "$1") (${pid:-none}) is still running or never started"
  fi
}

assert_no_roots() { # <repo>
  local roots
  roots=$(find "$1" -maxdepth 1 -name '.dev-instance.*' | head -n 1)
  if [[ -z "$roots" ]]; then ok; else report_failure "instance root left behind: $roots"; fi
}

# -------------------------------------------------------------------- cases
begin_case "default run seeds the daemon, launches the app, and cleans up on SIGTERM"
REPO=$CASE_DIR/repo
make_repo "$REPO"
start_instance "$REPO"
if wait_for "$CASE_DIR/stdout" '^api_url=' && wait_for "$CASE_DIR/app.pid" '.'; then ok; else report_failure "never became ready"; fi
root=$(sed -n 's/^root=//p' "$CASE_DIR/stdout")
if [[ -d "$root" && "$root" == "$(cd -P "$REPO" && pwd -P)"/.dev-instance.* ]]; then ok; else report_failure "root not a fresh directory in the worktree: $root"; fi
if [[ -f "$root/daemon/readiness.json" ]]; then ok; else report_failure "readiness.json not under $root/daemon"; fi
if [[ "$(cat "$root/.gitignore" 2>/dev/null)" == '*' ]]; then ok; else report_failure "root does not ignore itself"; fi
assert_line "$CASE_DIR/stdout" 'api_url=http://127.0.0.1:43210'
assert_line "$CASE_DIR/daemon.args" '-environment'
assert_line "$CASE_DIR/daemon.args" 'ephemeral'
assert_line "$CASE_DIR/daemon.args" '-listen'
assert_line "$CASE_DIR/daemon.args" '127.0.0.1:0'
assert_line "$CASE_DIR/daemon.args" '-state-dir'
assert_line "$CASE_DIR/daemon.args" "$root/daemon"
assert_line "$CASE_DIR/daemon.args" '-seed-fixture'
assert_line "$CASE_DIR/daemon.args" 'representative'
assert_line "$CASE_DIR/daemon.args" 'http://127.0.0.1:1'
assert_line "$CASE_DIR/app.args" '-FreesideReadinessDir'
assert_line "$CASE_DIR/app.args" "$root/daemon"
assert_line "$CASE_DIR/app.env" 'ephemeral'
kill -TERM "$INSTANCE_PID"
finish_instance
assert_rc 143
assert_stopped "$CASE_DIR/daemon.pid"
assert_stopped "$CASE_DIR/app.pid"
assert_not_exists "$root"
assert_no_roots "$REPO"

begin_case "a second SIGTERM during cleanup does not abandon it"
REPO=$CASE_DIR/repo
make_repo "$REPO"
FAKE_DAEMON_MODE=slow-stop start_instance "$REPO" --daemon-only
if wait_for "$CASE_DIR/stdout" '^api_url='; then ok; else report_failure "never became ready"; fi
kill -TERM "$INSTANCE_PID"
if wait_for "$CASE_DIR/daemon.term" term; then ok; else report_failure "cleanup never signalled the daemon"; fi
kill -TERM "$INSTANCE_PID" 2>/dev/null || true
finish_instance
assert_rc 143
assert_contains "dev-instance: removed"
assert_stopped "$CASE_DIR/daemon.pid"
assert_no_roots "$REPO"

begin_case "--no-seed starts the daemon without the fixture"
REPO=$CASE_DIR/repo
make_repo "$REPO"
start_instance "$REPO" --no-seed
if wait_for "$CASE_DIR/stdout" '^api_url='; then ok; else report_failure "never became ready"; fi
assert_no_line "$CASE_DIR/daemon.args" '-seed-fixture'
assert_line "$CASE_DIR/daemon.args" 'ephemeral'
kill -TERM "$INSTANCE_PID"
finish_instance
assert_stopped "$CASE_DIR/daemon.pid"
assert_no_roots "$REPO"

begin_case "--daemon-only skips the app build and launch, off macOS too"
REPO=$CASE_DIR/repo
make_repo "$REPO"
FAKE_UNAME=Linux start_instance "$REPO" --daemon-only
if wait_for "$CASE_DIR/stdout" '^api_url='; then ok; else report_failure "never became ready"; fi
assert_line "$CASE_DIR/daemon.args" 'representative'
kill -TERM "$INSTANCE_PID"
finish_instance
assert_rc 143
assert_not_exists "$CASE_DIR/xcodebuild.log"
assert_not_exists "$CASE_DIR/app.args"
assert_stopped "$CASE_DIR/daemon.pid"
assert_no_roots "$REPO"

begin_case "an early daemon exit fails, shows the log, and removes the root"
REPO=$CASE_DIR/repo
make_repo "$REPO"
FAKE_DAEMON_MODE=exit-early run_instance "$REPO"
assert_rc 1
assert_contains "freesided exited before readiness"
assert_contains "fake freesided: refusing to start"
assert_not_exists "$CASE_DIR/app.args"
assert_no_roots "$REPO"

begin_case "quitting the app stops the daemon and removes the root"
REPO=$CASE_DIR/repo
make_repo "$REPO"
FAKE_APP_MODE=quit run_instance "$REPO"
assert_rc 0
assert_contains "FreesideMac quit"
assert_line "$CASE_DIR/app.env" 'ephemeral'
assert_stopped "$CASE_DIR/daemon.pid"
assert_no_roots "$REPO"

begin_case "an abnormal app exit fails, shows the app log, and removes the root"
REPO=$CASE_DIR/repo
make_repo "$REPO"
FAKE_APP_MODE=crash run_instance "$REPO"
assert_rc 1
assert_contains "FreesideMac exited with status 3"
assert_contains "fake FreesideMac: crashed at startup"
assert_stopped "$CASE_DIR/daemon.pid"
assert_no_roots "$REPO"

begin_case "a worktree under a supervised root is refused and nothing is created"
FAKE_HOME=$CASE_DIR/home
REPO="$FAKE_HOME/Library/Application Support/Freeside Dev/repo"
make_repo "$REPO"
HOME=$FAKE_HOME run_instance "$REPO"
assert_rc 2
assert_contains "resolves under the dev state root"
assert_not_exists "$CASE_DIR/go.log"
assert_no_roots "$REPO"

begin_case "the app mode refuses off macOS and nothing is created"
REPO=$CASE_DIR/repo
make_repo "$REPO"
FAKE_UNAME=Linux run_instance "$REPO"
assert_rc 2
assert_contains "the app needs macOS"
assert_not_exists "$CASE_DIR/go.log"
assert_no_roots "$REPO"

begin_case "an unknown option is a usage error"
REPO=$CASE_DIR/repo
make_repo "$REPO"
run_instance "$REPO" --bogus
assert_rc 2
assert_contains "usage: dev-instance.sh"
assert_no_roots "$REPO"

echo
echo "passed $pass assertion(s), failed $fail"
if [ "$fail" -gt 0 ]; then
  exit 1
fi
