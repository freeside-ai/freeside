#!/usr/bin/env bash
# dev-instance.sh — one command for an ephemeral daemon, seeded, with an app.
#
# Usage: dev-instance.sh [--daemon-only] [--no-seed] [--scenario NAME]
#                        [--root DIR] [--listen ADDR]
#        dev-instance.sh --attach ROOT
#
# Builds freesided from daemon/ and the Debug FreesideMac app, then starts
# freesided as an `ephemeral` daemon in a fresh root,
# `<worktree>/.dev-instance.XXXXXX`, listening on 127.0.0.1:0 with the
# `representative` fixture seeded (#1503). Once the daemon is ready it runs the
# app with `-FreesideReadinessDir <root>/daemon`, so the app finds the daemon
# and its pairing code without typing either. It also prints two lines on
# stdout for a calling script to parse (three with --scenario):
#
#   root=<root>
#   api_url=<url>
#
# Progress goes to stderr. The script stays in the foreground until Ctrl-C,
# the daemon exits, or the app quits; then it stops both processes and removes
# the root, unless --root named it. The daemon's and app's logs live in the
# root and go with it; an early daemon exit prints the log's tail first.
#
#   --daemon-only    skip the app build and launch (for scripts; any platform)
#   --no-seed        start with an empty store
#   --scenario NAME  instead of the fixture, run `-driver scenario -scenario
#                    NAME` and submit the scenario's task once the daemon is
#                    ready, printing a third line, `task=<task-id>`
#                    (`freesided -list-scenarios` names them; daemon/README.md,
#                    Run A Scenario)
#   --root DIR       create the root at DIR, which must not exist yet, and keep
#                    it on exit; scripts/exit-root.sh uses this for a scenario
#                    pass the operator's devices reach
#   --listen ADDR    listen on ADDR instead of 127.0.0.1:0; freesided accepts a
#                    loopback or Tailscale address (its -listen flag)
#   --attach ROOT    attach only an app to a daemon that already runs, or is
#                    starting, in ROOT (a deputy's root, or another run's
#                    --root): build the Debug app, wait with no deadline until
#                    ROOT/daemon/readiness.json names an api_url that answers
#                    /health (a stopped deputy's file stays behind, so a
#                    restarting one is waited for), then run the app on it
#                    as above, logging to ROOT/app.log. It never
#                    starts, stops, or removes a daemon or ROOT, takes no
#                    other option, and prints nothing on stdout. The app keeps
#                    its pairing in ROOT/daemon, so it reopens paired until
#                    ROOT is deleted. scripts/exit-root.sh prints this command
#                    from the deputy's own source tree, so the app matches it.
#
# The app needs macOS and Xcode. Its derived data stays in the ignored
# app/DerivedData/dev-instance so rebuilds are incremental. ntfy points at a
# dead loopback address, so a dev instance never posts notifications.
#
# Safety: the worktree, and a --root, are refused, before anything is
# created, when either resolves under a supervised state root
# (supervised-paths.sh); freesided's
# own environment guard (#1501) stays the authority. The script never touches
# a launchd service or another FreesideMac process: the installed app runs
# the same executable name, so it holds its own child's PID instead. A SIGKILL
# skips cleanup and leaves the root behind; a root under the worktree ignores
# itself in git, so remove it by hand.
#
# Exit codes: 0 when the app quits normally, 130 after Ctrl-C, 143 after
# SIGTERM, 1 on a build or daemon failure or an abnormal app exit, 2 on a
# usage error or refusal. A failed scenario submission exits 1. --attach
# exits as the app does, and 2 when ROOT is missing or refused.
set -euo pipefail

usage() {
  echo 'usage: dev-instance.sh [--daemon-only] [--no-seed] [--scenario NAME] [--root DIR] [--listen ADDR]' >&2
  echo '       dev-instance.sh --attach ROOT' >&2
  exit 2
}

daemon_only=false
seed=true
scenario=''
root_arg=''
listen=127.0.0.1:0
attach_root=''
# Any option but --attach; --attach refuses them all.
instance_option=false
while [[ $# -gt 0 ]]; do
  case $1 in
  --daemon-only) daemon_only=true instance_option=true ;;
  --no-seed) seed=false instance_option=true ;;
  --scenario)
    [[ $# -ge 2 && -n "$2" ]] || usage
    scenario=$2 instance_option=true
    shift
    ;;
  --root)
    [[ $# -ge 2 && -n "$2" ]] || usage
    root_arg=$2 instance_option=true
    [[ "$root_arg" == /* ]] || root_arg="$PWD/$root_arg"
    shift
    ;;
  --listen)
    [[ $# -ge 2 && -n "$2" ]] || usage
    listen=$2 instance_option=true
    shift
    ;;
  --attach)
    [[ $# -ge 2 && -n "$2" && -z "$attach_root" ]] || usage
    attach_root=$2
    [[ "$attach_root" == /* ]] || attach_root="$PWD/$attach_root"
    shift
    ;;
  *) usage ;;
  esac
  shift
done
if [[ -n "$attach_root" && "$instance_option" == true ]]; then
  echo 'dev-instance: --attach takes no other option' >&2
  usage
fi

repo_root="$(cd -P "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
# shellcheck source=scripts/supervised-paths.sh
source "$repo_root/scripts/supervised-paths.sh"
refuse_supervised_path "dev-instance worktree" "$repo_root" || exit 2
if [[ -n "$attach_root" ]]; then
  refuse_supervised_path "dev-instance attach root" "$attach_root" || exit 2
  if [[ ! -d "$attach_root" ]]; then
    echo "dev-instance: --attach $attach_root is not a directory; name a running daemon's root" >&2
    exit 2
  fi
fi
if [[ -n "$root_arg" ]]; then
  refuse_supervised_path "dev-instance root" "$root_arg" || exit 2
  if [[ -e "$root_arg" || -L "$root_arg" ]]; then
    echo "dev-instance: --root $root_arg already exists; name a new directory" >&2
    exit 2
  fi
fi
if [[ "$daemon_only" == false && "$(uname)" != Darwin ]]; then
  if [[ -n "$attach_root" ]]; then
    echo 'dev-instance: --attach runs the app, which needs macOS' >&2
  else
    echo 'dev-instance: the app needs macOS; use --daemon-only elsewhere' >&2
  fi
  exit 2
fi

root=''
# Only a root this run's mktemp created is removed; a --root is kept.
remove_root=false
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
  if [[ "$remove_root" == true && -n "$root" && -d "$root" && "$root" == "$repo_root"/.dev-instance.* ]]; then
    rm -rf -- "$root"
    echo "dev-instance: removed $root" >&2
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# build_app: builds the Debug app and prints its executable's path.
build_app() {
  local derived="$repo_root/app/DerivedData/dev-instance" exe
  echo "dev-instance: building FreesideMac (Debug) into $derived" >&2
  # Callers run this in $(...), where set -e does not stop it, so a failed
  # build must exit here or an earlier build's app would launch.
  xcodebuild -project "$repo_root/app/Freeside.xcodeproj" -scheme FreesideMac \
    -configuration Debug -destination 'platform=macOS' -skipPackagePluginValidation \
    CODE_SIGNING_ALLOWED=NO -derivedDataPath "$derived" -quiet build >&2 ||
    { echo 'dev-instance: the FreesideMac build failed' >&2; exit 1; }
  exe="$derived/Build/Products/Debug/FreesideMac.app/Contents/MacOS/FreesideMac"
  [[ -x "$exe" ]] || { echo "dev-instance: no app executable at $exe" >&2; exit 1; }
  printf '%s\n' "$exe"
}

# launch_app <exec> <root>: runs the app on <root>/daemon in the background.
# FREESIDE_ENV overrides a stray value in the caller's shell; any tier but
# ephemeral makes the app refuse -FreesideReadinessDir.
launch_app() {
  echo "dev-instance: launching FreesideMac (log: $2/app.log)" >&2
  FREESIDE_ENV=ephemeral "$1" -FreesideReadinessDir "$2/daemon" >"$2/app.log" 2>&1 &
  app_pid=$!
}

# app_exited: reports how the app ended and exits with the script's status.
app_exited() {
  local app_status=0
  wait "$app_pid" || app_status=$?
  app_pid=''
  if ((app_status == 0)); then
    echo 'dev-instance: FreesideMac quit' >&2
    exit 0
  fi
  echo "dev-instance: FreesideMac exited with status $app_status; last lines of $1/app.log:" >&2
  tail -n 20 "$1/app.log" >&2 || true
  exit 1
}

if [[ -n "$attach_root" ]]; then
  root_physical="$(cd -P "$attach_root" && pwd -P)"
  app_exec="$(build_app)"
  # A deputy serves only after its checks and ward conformance, which can
  # take minutes, so the wait has no deadline; Ctrl-C ends it. A stopped
  # daemon leaves its readiness file behind, so the file's URL must also
  # answer: the app reads the file once, and a dead daemon's code is useless.
  readiness="$root_physical/daemon/readiness.json"
  waited=0
  while :; do
    url=$(sed -n 's/.*"api_url":"\([^"]*\)".*/\1/p' "$readiness" 2>/dev/null || true)
    if [[ -n "$url" ]] && curl -fsS --max-time 2 -o /dev/null "$url/health" 2>/dev/null; then
      break
    fi
    if ((waited % 30 == 0)); then
      echo "dev-instance: waiting for $readiness to name a serving daemon (it publishes the file once it serves)" >&2
    fi
    waited=$((waited + 1))
    sleep 1
  done
  launch_app "$app_exec" "$root_physical"
  echo "dev-instance: attached; quitting the app or Ctrl-C leaves the daemon and $root_physical alone" >&2
  while kill -0 "$app_pid" 2>/dev/null; do
    sleep 1
  done
  app_exited "$root_physical"
fi

if [[ -n "$root_arg" ]]; then
  mkdir "$root_arg"
  root="$(cd -P "$root_arg" && pwd -P)"
else
  root="$(mktemp -d "$repo_root/.dev-instance.XXXXXX")"
  remove_root=true
fi
if [[ "$root" == "$repo_root"/* ]]; then
  printf '*\n' >"$root/.gitignore"
fi
mkdir -m 0700 "$root/daemon"
daemon_log="$root/daemon/freesided.log"

echo "dev-instance: building freesided" >&2
(cd "$repo_root/daemon" && go build -o "$root/bin/freesided" ./cmd/freesided)

app_exec=''
if [[ "$daemon_only" == false ]]; then
  app_exec="$(build_app)"
fi

# -driver stays at its default, disabled, which -seed-fixture requires; a
# scenario replaces the fixture, since freesided refuses the two together.
daemon_args=(
  -environment ephemeral
  -db "$root/daemon/freeside.db"
  -state-dir "$root/daemon"
  -listen "$listen"
  -ntfy-url http://127.0.0.1:1
)
if [[ -n "$scenario" ]]; then
  daemon_args+=(-driver scenario -scenario "$scenario")
elif [[ "$seed" == true ]]; then
  daemon_args+=(-seed-fixture representative)
fi
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

if [[ -n "$scenario" ]]; then
  # The scenario daemon writes its task's submit inputs under its root.
  inputs="$root/daemon/scenario"
  echo "dev-instance: submitting the $scenario scenario's task" >&2
  submitted=''
  if ! submitted="$("$root/bin/freesided" submit -db "$root/daemon/freeside.db" \
    -task "$inputs/task.md" -policy "$inputs/policy.json" \
    -publication "$inputs/publication.json" -work-unit "$inputs/work-unit.json" \
    -project project-scenario)"; then
    daemon_failed 'scenario submission failed'
  fi
  task_id="$(printf '%s\n' "$submitted" | sed -n 's/.*"task_id":"\([^"]*\)".*/\1/p')"
  [[ -n "$task_id" ]] || daemon_failed "scenario submission printed no task_id: $submitted"
  printf 'task=%s\n' "$task_id"
fi

if [[ -n "$app_exec" ]]; then
  launch_app "$app_exec" "$root"
fi
if [[ "$remove_root" == true ]]; then
  echo "dev-instance: ready; Ctrl-C stops everything and removes the root" >&2
else
  echo "dev-instance: ready; Ctrl-C stops everything and keeps $root" >&2
fi

# Bash 3.2 has no `wait -n`, so poll both children.
while :; do
  kill -0 "$daemon_pid" 2>/dev/null || daemon_failed 'freesided exited'
  if [[ -n "$app_pid" ]] && ! kill -0 "$app_pid" 2>/dev/null; then
    # The log goes with the root, so app_exited shows its tail before
    # cleanup removes it.
    app_exited "$root"
  fi
  sleep 1
done
