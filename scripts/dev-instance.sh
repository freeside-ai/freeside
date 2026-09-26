#!/usr/bin/env bash
# dev-instance.sh — one command for an ephemeral daemon, seeded, with an app.
#
# Usage: dev-instance.sh [--daemon-only] [--no-seed]
#
# Builds freesided from daemon/ and the Debug FreesideMac app, then starts
# freesided as an `ephemeral` daemon in a fresh root,
# `<worktree>/.dev-instance.XXXXXX`, listening on 127.0.0.1:0 with the
# `representative` fixture seeded (#1503). Once the daemon is ready it runs the
# app with `-FreesideReadinessDir <root>/daemon`, so the app finds the daemon
# and its pairing code without typing either. It also prints two lines on
# stdout for a calling script to parse:
#
#   root=<root>
#   api_url=<url>
#
# Progress goes to stderr. The script stays in the foreground until Ctrl-C,
# the daemon exits, or the app quits; then it stops both processes and removes
# the root. The daemon's and app's logs live in the root and go with it; an
# early daemon exit prints the log's tail first.
#
#   --daemon-only  skip the app build and launch (for scripts; any platform)
#   --no-seed      start with an empty store
#
# The app needs macOS and Xcode. Its derived data stays in the ignored
# app/DerivedData/dev-instance so rebuilds are incremental. ntfy points at a
# dead loopback address, so a dev instance never posts notifications.
#
# Safety: the worktree is refused, before anything is created, when it
# resolves under a supervised state root (supervised-paths.sh); freesided's
# own environment guard (#1501) stays the authority. The script never touches
# a launchd service or another FreesideMac process: the installed app runs
# the same executable name, so it holds its own child's PID instead. A SIGKILL
# skips cleanup and leaves the root behind; the root ignores itself in git,
# so remove it by hand.
#
# Exit codes: 0 when the app quits normally, 130 after Ctrl-C, 143 after
# SIGTERM, 1 on a build or daemon failure or an abnormal app exit, 2 on a
# usage error or refusal.
set -euo pipefail

usage() {
  echo 'usage: dev-instance.sh [--daemon-only] [--no-seed]' >&2
  exit 2
}

daemon_only=false
seed=true
while [[ $# -gt 0 ]]; do
  case $1 in
  --daemon-only) daemon_only=true ;;
  --no-seed) seed=false ;;
  *) usage ;;
  esac
  shift
done

repo_root="$(cd -P "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
# shellcheck source=scripts/supervised-paths.sh
source "$repo_root/scripts/supervised-paths.sh"
refuse_supervised_path "dev-instance worktree" "$repo_root" || exit 2
if [[ "$daemon_only" == false && "$(uname)" != Darwin ]]; then
  echo 'dev-instance: the app needs macOS; use --daemon-only elsewhere' >&2
  exit 2
fi

root=''
daemon_pid=''
app_pid=''

# stop_child <pid>: TERM, a bounded wait, then KILL, so the root is never
# removed under a live writer.
stop_child() {
  local pid=$1 _
  [[ -n "$pid" ]] || return 0
  kill -TERM "$pid" 2>/dev/null || return 0
  for _ in $(seq 1 100); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.1
  done
  kill -KILL "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
}

cleanup() {
  # A second Ctrl-C would otherwise exit mid-cleanup and leak the children
  # and the root; stop_child's wait is already bounded.
  trap '' INT TERM
  stop_child "$app_pid"
  stop_child "$daemon_pid"
  # Remove only the directory this run's mktemp created under the worktree.
  if [[ -n "$root" && -d "$root" && "$root" == "$repo_root"/.dev-instance.* ]]; then
    rm -rf -- "$root"
    echo "dev-instance: removed $root" >&2
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

root="$(mktemp -d "$repo_root/.dev-instance.XXXXXX")"
printf '*\n' >"$root/.gitignore"
mkdir -m 0700 "$root/daemon"
daemon_log="$root/daemon/freesided.log"

echo "dev-instance: building freesided" >&2
(cd "$repo_root/daemon" && go build -o "$root/bin/freesided" ./cmd/freesided)

app_exec=''
if [[ "$daemon_only" == false ]]; then
  derived="$repo_root/app/DerivedData/dev-instance"
  echo "dev-instance: building FreesideMac (Debug) into $derived" >&2
  xcodebuild -project "$repo_root/app/Freeside.xcodeproj" -scheme FreesideMac \
    -configuration Debug -destination 'platform=macOS' -skipPackagePluginValidation \
    CODE_SIGNING_ALLOWED=NO -derivedDataPath "$derived" -quiet build >&2
  app_exec="$derived/Build/Products/Debug/FreesideMac.app/Contents/MacOS/FreesideMac"
  [[ -x "$app_exec" ]] || { echo "dev-instance: no app executable at $app_exec" >&2; exit 1; }
fi

# -driver stays at its default, disabled, which -seed-fixture requires.
daemon_args=(
  -environment ephemeral
  -db "$root/daemon/freeside.db"
  -state-dir "$root/daemon"
  -listen 127.0.0.1:0
  -ntfy-url http://127.0.0.1:1
)
[[ "$seed" == false ]] || daemon_args+=(-seed-fixture representative)
echo "dev-instance: starting freesided (log: $daemon_log)" >&2
"$root/bin/freesided" "${daemon_args[@]}" >"$daemon_log" 2>&1 &
daemon_pid=$!

daemon_failed() {
  echo "dev-instance: $1; last lines of $daemon_log:" >&2
  tail -n 20 "$daemon_log" >&2 || true
  exit 1
}

# freesided publishes readiness.json atomically once it serves, so its
# presence with an api_url is the ready signal.
readiness="$root/daemon/readiness.json"
api_url=''
for _ in $(seq 1 300); do
  if [[ -f "$readiness" ]]; then
    api_url="$(sed -n 's/.*"api_url":"\([^"]*\)".*/\1/p' "$readiness")"
    [[ -z "$api_url" ]] || break
  fi
  kill -0 "$daemon_pid" 2>/dev/null || daemon_failed 'freesided exited before readiness'
  sleep 0.1
done
[[ -n "$api_url" ]] || daemon_failed 'no readiness within 30s'
printf 'root=%s\napi_url=%s\n' "$root" "$api_url"

if [[ -n "$app_exec" ]]; then
  # FREESIDE_ENV overrides a stray value in the caller's shell; any tier but
  # ephemeral makes the app refuse -FreesideReadinessDir.
  echo "dev-instance: launching FreesideMac (log: $root/app.log)" >&2
  FREESIDE_ENV=ephemeral "$app_exec" -FreesideReadinessDir "$root/daemon" \
    >"$root/app.log" 2>&1 &
  app_pid=$!
fi
echo "dev-instance: ready; Ctrl-C stops everything and removes the root" >&2

# Bash 3.2 has no `wait -n`, so poll both children.
while :; do
  kill -0 "$daemon_pid" 2>/dev/null || daemon_failed 'freesided exited'
  if [[ -n "$app_pid" ]] && ! kill -0 "$app_pid" 2>/dev/null; then
    app_status=0
    wait "$app_pid" || app_status=$?
    app_pid=''
    if ((app_status == 0)); then
      echo 'dev-instance: FreesideMac quit' >&2
      exit 0
    fi
    # The log goes with the root, so show its tail before cleanup removes it.
    echo "dev-instance: FreesideMac exited with status $app_status; last lines of $root/app.log:" >&2
    tail -n 20 "$root/app.log" >&2 || true
    exit 1
  fi
  sleep 1
done
