#!/usr/bin/env bash
# daemon-liveness-probe.sh — tell the operator over ntfy when the daemon is
# dead or crash-looping.
#
# Usage: daemon-liveness-probe.sh <check|install|uninstall|notify-test>
#                                 [--tier prod|dev]
#
# The probe runs outside the daemon process (plan §5.2): the daemon's own
# ntfy channel cannot deliver its own death notice. Each `check` polls the
# unauthenticated `GET /health` once and exits; a per-user LaunchAgent runs
# it on a timer, so launchd starts every poll and a poll that crashes loses
# only that poll.
#
# Alarms (each fires once, then stays quiet until it re-arms):
#   unreachable   `failure_count` polls in a row fail. A poll fails unless it
#                 gets HTTP 200 and a body with "status":"ok" and an RFC 3339
#                 started_at. Re-arms on the next good poll, which sends one
#                 "reachable again" notice.
#   crash loop    started_at changes `restart_count` times within
#                 `restart_window` seconds, meaning the supervisor keeps
#                 respawning the daemon. One clean restart is one change and
#                 raises nothing. Re-arms once no change is left in the window.
# An alarm counts as raised or cleared only after ntfy accepts its notice; a
# failed send is retried on the next poll. A crash-loop notice stays owed
# until it is delivered, even once the window no longer holds enough
# changes. An unreachable notice is dropped if the daemon answers again
# before it is delivered: the operator was never told of the outage, so no
# "reachable again" follows either. A notice names only the tier and
# the alarm: no hostname, address, or version.
#
# Operator setup (macOS):
#   1. Choose an unguessable ntfy topic, for example the output of
#      `openssl rand -hex 16`, and subscribe to it in an ntfy client on the
#      phone. The topic is its own capability: anyone who knows it can read
#      it. It cannot be a device topic from pairing, because the daemon
#      derives those from a key only it holds.
#   2. Write the config for the tier, one key=value per line, private to you:
#        dir="$HOME/Library/Application Support/Freeside Liveness Probe/prod"
#        mkdir -p "$dir" && chmod 700 "$dir"
#        (umask 077; printf 'ntfy_url=https://ntfy.sh/<topic>\n' >"$dir/config")
#      Keys (only these; the file is read, never sourced):
#        ntfy_url        required; https URL of the topic
#        ntfy_token      optional; access token for a protected topic
#        health_url      default http://127.0.0.1:7331/health (prod),
#                        http://127.0.0.1:7332/health (dev)
#        poll_interval   seconds between polls, default 60 (read by install)
#        failure_count   failed polls in a row that alarm, default 3
#        restart_count   started_at changes that alarm, default 3
#        restart_window  seconds those changes must fall within, default 600
#      The probe refuses a config that is not a private (0600) regular file.
#   3. From the repository root, `scripts/daemon-liveness-probe.sh
#      notify-test` sends one test notice, so you can confirm the
#      subscription before relying on it.
#   4. `scripts/daemon-liveness-probe.sh install` copies this script into
#      the tier's probe directory, writes
#      ~/Library/LaunchAgents/<label>.plist, and loads it. Labels:
#      ai.freeside.liveness-probe (prod), ai.freeside.liveness-probe.dev
#      (dev). Rerun it after changing poll_interval or updating the
#      checkout.
#   5. `scripts/daemon-liveness-probe.sh uninstall` unloads the agent and
#      removes the plist, the installed copy, and the alarm state. It keeps
#      config and the log. If launchd cannot unload a loaded agent, it
#      removes nothing and exits 1.
#   To see a real alert safely, give a dev-tier config a health_url on a port
#   nothing listens on (http://127.0.0.1:9/health) and install it with
#   --tier dev; the unreachable notice arrives after failure_count polls.
#
# Files, per tier, in "~/Library/Application Support/Freeside Liveness
# Probe/<tier>/", beside the daemon's state roots, never inside them:
# config, state (the alarm state), probe.log, and the installed script.
#
# Limits: the reference install runs on the daemon's host as a per-user
# agent, so it stops with the host and at logout, as the daemon does. It
# cannot report a dead host or a logged-out session. health_url accepts
# another host for a probe on a second machine, but that setup is not
# built here. /health carries no operational state, so a stopped or stalled
# daemon that still answers raises nothing. A refused config or state
# makes every poll exit 2 and send nothing; the reason is in probe.log.
# `check` is portable, so cron or a systemd timer can call it; only the
# LaunchAgent install is built.
#
# The ntfy topic URL and token go to curl on standard input (--config -),
# never as arguments, which every process on the host can read through ps.
# Neither is ever printed or logged; a failed send is logged by HTTP status
# alone. curl runs with -q, so ~/.curlrc does not apply.
#
# Bash 3.2 compatible (macOS /bin/bash, which the LaunchAgent runs).
#
# Exit codes:
#   0  the command finished; for check, the poll ran, whatever the daemon's
#      state
#   1  a notice could not be sent, or launchctl failed
#   2  usage or config error
set -euo pipefail

PROG=daemon-liveness-probe

usage() {
  echo "usage: $PROG <check|install|uninstall|notify-test> [--tier prod|dev]" >&2
  exit 2
}

die() { # <message>: config or usage error
  echo "$PROG: $1" >&2
  exit 2
}

log() { # <message>: one timestamped line on stdout, the agent's log
  printf '%s %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$1"
}

RFC3339='[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]{1,9})?(Z|[+-][0-9]{2}:[0-9]{2})'

# --- files -------------------------------------------------------------------

# private_file_ok <path>: succeeds when <path> is a regular file, not a
# symlink, with one link and no group or other permission bits, the rule
# daemon/internal/topicstore applies to the daemon's topic key.
private_file_ok() {
  local path=$1 meta mode links
  [[ -f "$path" && ! -L "$path" ]] || return 1
  if ! meta=$(stat -c '%a %h' -- "$path" 2>/dev/null); then
    meta=$(stat -f '%Lp %l' -- "$path") || return 1
  fi
  mode=${meta% *}
  links=${meta#* }
  [[ "$mode" =~ ^[0-7]+$ && "$links" == 1 ]] || return 1
  (((8#$mode & 8#077) == 0))
}

# read_keys <path> <prefix> <key>...: sets <prefix><key> for each key=value
# line; blank lines and # comments are skipped. An unknown key, a repeated
# key, or a malformed line fails, so a typo cannot silently fall back to a
# default. Values are assigned with printf -v, never evaluated. An error
# names the line number, never its text, which may hold the topic or token.
read_keys() {
  local path=$1 prefix=$2 line key value known k n=0 seen=' '
  shift 2
  while IFS= read -r line || [[ -n "$line" ]]; do
    n=$((n + 1))
    [[ -z "$line" || "$line" == \#* ]] && continue
    [[ "$line" == *=* ]] || { echo "$PROG: malformed line $n of $path" >&2; return 1; }
    key=${line%%=*}
    value=${line#*=}
    known=0
    for k in "$@"; do [[ "$key" == "$k" ]] && known=1; done
    if ((known == 0)); then
      echo "$PROG: unknown key on line $n of $path" >&2
      return 1
    fi
    if [[ "$seen" == *" $key "* ]]; then
      echo "$PROG: repeated key \"$key\" in $path" >&2
      return 1
    fi
    seen="$seen$key "
    printf -v "$prefix$key" '%s' "$value"
  done <"$path"
}

positive_int() { [[ "$1" =~ ^[1-9][0-9]{0,8}$ ]]; }

# load_config: reads and validates the tier's config into C_* variables.
load_config() {
  local port=7331
  [[ "$TIER" == prod ]] || port=7332
  C_ntfy_url='' C_ntfy_token='' C_health_url="http://127.0.0.1:$port/health"
  C_poll_interval=60 C_failure_count=3 C_restart_count=3 C_restart_window=600
  [[ -e "$CONFIG" || -L "$CONFIG" ]] || die "no config at \"$CONFIG\"; see the setup steps in this script's header"
  private_file_ok "$CONFIG" || die "refusing config \"$CONFIG\": it must be a private (0600) regular file, not a symlink"
  read_keys "$CONFIG" C_ ntfy_url ntfy_token health_url poll_interval \
    failure_count restart_count restart_window || exit 2
  # The URL and token go into a curl config file inside double quotes, so a
  # quote, backslash, or whitespace could not be carried safely.
  [[ "$C_ntfy_url" =~ ^https://[^[:space:]\"\\]+$ ]] || die "config ntfy_url must be an https URL"
  [[ -z "$C_ntfy_token" || "$C_ntfy_token" =~ ^[A-Za-z0-9._~+/=-]+$ ]] || die "config ntfy_token has characters a token cannot carry"
  [[ "$C_health_url" =~ ^https?://[^[:space:]\"\\]+$ ]] || die "config health_url must be an http or https URL"
  positive_int "$C_poll_interval" || die "config poll_interval must be a positive integer"
  positive_int "$C_failure_count" || die "config failure_count must be a positive integer"
  positive_int "$C_restart_count" || die "config restart_count must be a positive integer"
  positive_int "$C_restart_window" || die "config restart_window must be a positive integer"
}

# load_state: reads the alarm state into S_* variables; a missing file is
# the first poll ever.
load_state() {
  S_failures=0 S_unreachable=0 S_started_at='' S_changes='' S_crash_loop=0
  S_crash_loop_owed=0
  [[ -e "$STATE" || -L "$STATE" ]] || return 0
  private_file_ok "$STATE" || die "refusing state \"$STATE\": it must be a private regular file"
  read_keys "$STATE" S_ failures unreachable started_at changes crash_loop \
    crash_loop_owed || exit 2
  [[ "$S_failures" =~ ^[0-9]{1,9}$ && "$S_unreachable" =~ ^[01]$ &&
    "$S_crash_loop" =~ ^[01]$ && "$S_crash_loop_owed" =~ ^[01]$ && "$S_changes" =~ ^([0-9]{1,12}( [0-9]{1,12})*)?$ ]] ||
    die "state \"$STATE\" is invalid; remove it to start over"
  [[ -z "$S_started_at" || "$S_started_at" =~ ^$RFC3339$ ]] ||
    die "state \"$STATE\" is invalid; remove it to start over"
}

save_state() {
  local tmp
  tmp=$(mktemp "$PROBE_DIR/state.XXXXXX")
  printf 'failures=%s\nunreachable=%s\nstarted_at=%s\nchanges=%s\ncrash_loop=%s\ncrash_loop_owed=%s\n' \
    "$S_failures" "$S_unreachable" "$S_started_at" "$S_changes" "$S_crash_loop" \
    "$S_crash_loop_owed" >"$tmp"
  mv -f "$tmp" "$STATE"
}

# --- the alarm rules ---------------------------------------------------------

# advance <now> <ok:0|1> <started_at>: applies one poll to the S_* state and
# sets DUE to the notices this poll owes, as space-separated names from
# unreachable, recovered, and crash_loop. It performs no I/O. The alarm
# flags are left alone: the caller flips a flag only once its notice is
# delivered, so an undelivered notice is owed again on the next poll. The
# crash-loop alarm re-arms without a notice, so advance clears it itself.
# Whether a crash loop is owed is kept in S_crash_loop_owed rather than
# recomputed, because the change count can fall below the threshold, as the
# oldest change leaves the window, before a failed send is retried.
advance() {
  local now=$1 ok=$2 started_at=$3 t kept='' count=0
  DUE=''
  if ((ok)); then
    S_failures=0
    if [[ -n "$S_started_at" && "$started_at" != "$S_started_at" ]]; then
      S_changes="${S_changes:+$S_changes }$now"
    fi
    S_started_at=$started_at
    ((S_unreachable == 0)) || DUE="$DUE recovered"
  else
    S_failures=$((S_failures + 1))
    ((S_unreachable == 1 || S_failures < C_failure_count)) || DUE="$DUE unreachable"
  fi
  for t in $S_changes; do
    # A clock stepped back past a change would otherwise keep it in the
    # window, and the crash-loop alarm could not re-arm.
    if ((t <= now && now - t < C_restart_window)); then
      kept="${kept:+$kept }$t"
      count=$((count + 1))
    fi
  done
  S_changes=$kept
  if ((count == 0)); then
    S_crash_loop=0
  elif ((S_crash_loop == 0 && count >= C_restart_count)); then
    S_crash_loop_owed=1
  fi
  ((S_crash_loop_owed == 0)) || DUE="$DUE crash_loop"
  DUE=${DUE# }
}

# --- I/O ---------------------------------------------------------------------

# poll <body-file>: fetches the health URL once; sets POLL_OK and
# POLL_STARTED_AT, and POLL_WHY on failure. The body is trusted for nothing
# beyond the two checks: started_at is only compared and stored, and it
# must match the RFC 3339 pattern before either.
poll() {
  local body_file=$1 code body re
  POLL_OK=0 POLL_STARTED_AT='' POLL_WHY=''
  if ! code=$(curl -q --silent --proto =http,https --max-time 10 \
    --max-filesize 65536 --output "$body_file" --write-out '%{http_code}' \
    "$C_health_url" 2>/dev/null); then
    POLL_WHY="no answer"
    return 0
  fi
  if [[ "$code" != 200 ]]; then
    POLL_WHY="http $code"
    return 0
  fi
  body=$(head -c 65536 "$body_file")
  re='"status"[[:space:]]*:[[:space:]]*"ok"'
  if ! [[ "$body" =~ $re ]]; then
    POLL_WHY="body lacks status ok"
    return 0
  fi
  re="\"started_at\"[[:space:]]*:[[:space:]]*\"($RFC3339)\""
  if ! [[ "$body" =~ $re ]]; then
    POLL_WHY="body lacks a well-formed started_at"
    return 0
  fi
  POLL_OK=1
  POLL_STARTED_AT=${BASH_REMATCH[1]}
}

# send <message> <priority>: posts one notice; succeeds only when ntfy
# answers 2xx. The URL and token reach curl on standard input.
send() {
  local message=$1 priority=$2 code
  if ! code=$(
    {
      printf 'url = "%s"\n' "$C_ntfy_url"
      if [[ -n "$C_ntfy_token" ]]; then
        printf 'header = "Authorization: Bearer %s"\n' "$C_ntfy_token"
      fi
    } | curl -q --silent --proto =https --max-time 20 --output /dev/null \
      --write-out '%{http_code}' --header "Title: Freeside $TIER" \
      --header "Priority: $priority" --data-binary "$message" \
      --config - 2>/dev/null
  ); then
    log "ntfy send failed: no answer"
    return 1
  fi
  if [[ "$code" != 2?? ]]; then
    log "ntfy send failed: http $code"
    return 1
  fi
}

# notice <name>: sends the named notice and flips its alarm flag on delivery.
notice() {
  case $1 in
  unreachable)
    send "Freeside $TIER daemon unreachable" high || return 1
    S_unreachable=1
    ;;
  recovered)
    send "Freeside $TIER daemon reachable again" default || return 1
    S_unreachable=0
    ;;
  crash_loop)
    send "Freeside $TIER daemon crash-looping" high || return 1
    S_crash_loop=1 S_crash_loop_owed=0
    ;;
  esac
  log "sent $1 notice"
}

cmd_check() {
  local body_file name failed=0 now
  load_config
  load_state
  body_file=$(mktemp "$PROBE_DIR/body.XXXXXX")
  poll "$body_file"
  rm -f "$body_file"
  ((POLL_OK)) || log "poll failed: $POLL_WHY"
  now=$(date +%s)
  advance "$now" "$POLL_OK" "$POLL_STARTED_AT"
  for name in $DUE; do
    notice "$name" || failed=1
  done
  save_state
  return "$failed"
}

cmd_notify_test() {
  load_config
  send "Freeside $TIER liveness probe test notice" default || return 1
  echo "sent a test notice"
}

xml_escape() {
  local s=$1
  s=${s//&/&amp;}
  s=${s//</&lt;}
  s=${s//>/&gt;}
  printf '%s' "$s"
}

cmd_install() {
  local installed="$PROBE_DIR/$PROG.sh" plist_dir="$HOME/Library/LaunchAgents"
  local plist tmp uid attempt
  load_config
  command -v launchctl >/dev/null || die "launchctl not found; install is for macOS"
  plist="$plist_dir/$LABEL.plist"
  uid=$(id -u)
  tmp=$(mktemp "$PROBE_DIR/install.XXXXXX")
  cp -- "$0" "$tmp"
  chmod 700 "$tmp"
  mv -f "$tmp" "$installed"
  mkdir -p "$plist_dir"
  tmp=$(mktemp "$plist_dir/.$LABEL.XXXXXX")
  cat >"$tmp" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>$LABEL</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/bash</string>
		<string>$(xml_escape "$installed")</string>
		<string>check</string>
		<string>--tier</string>
		<string>$TIER</string>
	</array>
	<key>StartInterval</key>
	<integer>$C_poll_interval</integer>
	<key>RunAtLoad</key>
	<true/>
	<key>StandardOutPath</key>
	<string>$(xml_escape "$LOG")</string>
	<key>StandardErrorPath</key>
	<string>$(xml_escape "$LOG")</string>
</dict>
</plist>
EOF
  chmod 644 "$tmp"
  mv -f "$tmp" "$plist"
  # A reinstall replaces the loaded job; bootout fails when none is loaded.
  launchctl bootout "gui/$uid/$LABEL" >/dev/null 2>&1 || true
  # launchd can refuse the bootstrap (error 5) while the old job is still
  # being torn down, so a reinstall retries briefly.
  for attempt in 1 2 3; do
    launchctl bootstrap "gui/$uid" "$plist" && break
    if ((attempt == 3)); then
      echo "$PROG: launchctl bootstrap failed for $LABEL" >&2
      return 1
    fi
    sleep 1
  done
  echo "installed $LABEL, polling every ${C_poll_interval}s; log: $LOG"
}

cmd_uninstall() {
  local plist="$HOME/Library/LaunchAgents/$LABEL.plist" uid
  command -v launchctl >/dev/null || die "launchctl not found; uninstall is for macOS"
  uid=$(id -u)
  # bootout also fails when no job is loaded, so a failure counts only while
  # print still finds the job. Its files are then kept, so launchd never
  # runs a missing script.
  if ! launchctl bootout "gui/$uid/$LABEL" >/dev/null 2>&1; then
    if launchctl print "gui/$uid/$LABEL" >/dev/null 2>&1; then
      echo "$PROG: launchctl bootout failed for $LABEL; kept its files" >&2
      return 1
    fi
    echo "$LABEL was not loaded"
  fi
  rm -f -- "$plist" "$PROBE_DIR/$PROG.sh" "$STATE"
  echo "uninstalled $LABEL; kept $CONFIG"
}

# --- main --------------------------------------------------------------------

(($# >= 1)) || usage
COMMAND=$1
shift
TIER=prod
while (($# > 0)); do
  case $1 in
  --tier)
    (($# >= 2)) || usage
    TIER=$2
    shift 2
    ;;
  *) usage ;;
  esac
done
[[ "$TIER" == prod || "$TIER" == dev ]] || usage
[[ -n "${HOME:-}" ]] || die "HOME is not set"

PROBE_DIR="$HOME/Library/Application Support/Freeside Liveness Probe/$TIER"
CONFIG="$PROBE_DIR/config"
STATE="$PROBE_DIR/state"
LOG="$PROBE_DIR/probe.log"
LABEL=ai.freeside.liveness-probe
[[ "$TIER" == prod ]] || LABEL=$LABEL.dev
umask 077

case $COMMAND in
check) cmd_check ;;
install) cmd_install ;;
uninstall) cmd_uninstall ;;
notify-test) cmd_notify_test ;;
*) usage ;;
esac
