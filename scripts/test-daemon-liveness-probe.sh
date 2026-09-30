#!/usr/bin/env bash
# test-daemon-liveness-probe.sh — regression suite for daemon-liveness-probe.sh.
#
# Runs the probe under a synthetic HOME with stand-ins for curl, launchctl,
# and the clock (`date +%s`) first on PATH, so it uses no network and never
# touches a real LaunchAgent. The curl stand-in answers health polls from a
# control file, records every ntfy send, and logs every argument list it
# receives. The suite checks the unreachable alarm, its recovery notice, and
# re-arm; the crash-loop alarm, its window, and re-arm; that a malformed
# health body fails the poll; that a failed send is retried, a crash-loop
# notice even once its window thins; that a non-private or symlinked config
# is refused; that the probe is executable; that the topic and token reach
# curl only on standard input and appear in no output, log, or argument; and
# that install and uninstall write and reverse the plist and launchctl calls,
# and that a failed unload keeps the agent's files.
# The probe runs under /bin/bash, which is Bash 3.2 on macOS, as the
# LaunchAgent runs it.
#
# Exit code: 0 when every assertion passes, 1 otherwise.
set -euo pipefail

SCRIPT=$(cd "$(dirname "$0")" && pwd)/daemon-liveness-probe.sh

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

TOPIC_URL=https://ntfy.example/SECRETTOPIC0123456789
TOKEN=tk_SECRETTOKEN0123456789
HOME_DIR=$TMP/home
BIN=$TMP/bin
mkdir -p "$BIN"

cat >"$BIN/curl" <<'EOF'
#!/usr/bin/env bash
# curl stand-in: a call with --config is an ntfy send, any other a health poll.
printf '%s\n' "$*" >>"$STANDIN/curl-args"
output='' message='' send=0
while (($# > 0)); do
  case $1 in
  --output) output=$2; shift ;;
  --data-binary) message=$2; shift ;;
  --config) send=1; shift ;;
  esac
  shift
done
if ((send)); then
  cat >>"$STANDIN/curl-config"
  code=$(cat "$STANDIN/ntfy-code")
  [[ "$code" != down ]] || exit 7
  [[ "$code" != 2?? ]] || printf '%s\n' "$message" >>"$STANDIN/sent"
  printf '%s' "$code"
  exit 0
fi
code=$(head -n 1 "$STANDIN/health")
[[ "$code" != down ]] || exit 7
tail -n +2 "$STANDIN/health" >"$output"
printf '%s' "$code"
EOF
cat >"$BIN/date" <<'EOF'
#!/usr/bin/env bash
if [[ "$*" == +%s ]]; then cat "$STANDIN/now"; else exec /bin/date "$@"; fi
EOF
cat >"$BIN/launchctl" <<'EOF'
#!/usr/bin/env bash
# launchctl stand-in: fails the next N bootstraps, N from launchctl-fail,
# and every bootout while launchctl-bootout-fail exists. launchctl-loaded
# marks the job loaded; print and bootout of an absent job fail as
# launchctl does.
printf '%s\n' "$*" >>"$STANDIN/launchctl-log"
case $1 in
bootstrap)
  if [[ -s "$STANDIN/launchctl-fail" ]]; then
    n=$(cat "$STANDIN/launchctl-fail")
    if ((n > 0)); then
      echo $((n - 1)) >"$STANDIN/launchctl-fail"
      exit 5
    fi
  fi
  touch "$STANDIN/launchctl-loaded"
  ;;
bootout)
  [[ ! -e "$STANDIN/launchctl-bootout-fail" ]] || exit 5
  [[ -e "$STANDIN/launchctl-loaded" ]] || exit 3
  rm -f "$STANDIN/launchctl-loaded"
  ;;
print) [[ -e "$STANDIN/launchctl-loaded" ]] || exit 113 ;;
esac
EOF
chmod +x "$BIN/curl" "$BIN/date" "$BIN/launchctl"

# reset: a fresh home and stand-in state; writes a valid prod config holding
# the extra lines given.
reset() {
  rm -rf "$HOME_DIR" "$TMP/standin"
  export STANDIN=$TMP/standin
  mkdir -p "$STANDIN"
  PROBE_DIR="$HOME_DIR/Library/Application Support/Freeside Liveness Probe/prod"
  mkdir -p "$PROBE_DIR"
  (
    umask 077
    printf 'ntfy_url=%s\nntfy_token=%s\n' "$TOPIC_URL" "$TOKEN" >"$PROBE_DIR/config"
    for line in "$@"; do printf '%s\n' "$line" >>"$PROBE_DIR/config"; done
  )
  echo 200 >"$STANDIN/ntfy-code"
  : >"$STANDIN/sent"
  : >"$STANDIN/curl-args"
  : >"$STANDIN/curl-config"
  echo 1000000 >"$STANDIN/now"
  up 2026-09-30T10:00:00.123456789Z
}

up() { printf '200\n{"status":"ok","contract_digest":"d","version":"v","started_at":"%s"}\n' "$1" >"$STANDIN/health"; }
down() { echo down >"$STANDIN/health"; }
raw() { printf '%s\n%s\n' "$1" "$2" >"$STANDIN/health"; }
advance_clock() { echo $(($(cat "$STANDIN/now") + $1)) >"$STANDIN/now"; }

# probe <args...>: runs the probe; sets RC and OUT, and keeps every output
# for the leak check.
probe() {
  set +e
  OUT=$(HOME=$HOME_DIR PATH="$BIN:$PATH" /bin/bash "$SCRIPT" "$@" 2>&1)
  RC=$?
  set -e
  printf '%s\n' "$OUT" >>"$TMP/all-output"
}

# poll: one check, one minute after the last.
poll() {
  advance_clock 60
  probe check
}

sent() { wc -l <"$STANDIN/sent" | tr -d ' '; }
last_sent() { tail -n 1 "$STANDIN/sent"; }

expect() { # <description> <condition...>
  local description=$1
  shift
  if "$@"; then
    pass=$((pass + 1))
  else
    fail=$((fail + 1))
    echo "FAIL: $description (rc=$RC)"
    printf '%s\n' "$OUT" | sed 's/^/    | /'
  fi
}

eq() { [[ "$1" == "$2" ]]; }
has() { [[ "$1" == *"$2"* ]]; }

echo "case: three failed polls alarm once"
reset
poll
expect "the first good poll sends nothing" eq "$(sent)" 0
down
poll
poll
expect "two failures send nothing" eq "$(sent)" 0
poll
expect "the third failure sends one notice" eq "$(sent)" 1
expect "it names the tier and the alarm" eq "$(last_sent)" "Freeside prod daemon unreachable"
expect "a poll during an outage exits 0" eq "$RC" 0
poll
poll
expect "later failures send nothing" eq "$(sent)" 1

echo "case: recovery sends one notice and re-arms"
up 2026-09-30T10:00:00.123456789Z
poll
expect "the good poll sends one recovery notice" eq "$(sent)" 2
expect "it says reachable again" eq "$(last_sent)" "Freeside prod daemon reachable again"
poll
expect "the next good poll sends nothing" eq "$(sent)" 2
down
poll
poll
poll
expect "three more failures alarm again" eq "$(sent)" 3

echo "case: a failure run broken by a good poll does not alarm"
reset
poll
down
poll
poll
up 2026-09-30T10:00:00.123456789Z
poll
down
poll
poll
expect "no three failures in a row, no notice" eq "$(sent)" 0

echo "case: a clean restart is one change and raises nothing"
reset
poll
up 2026-09-30T10:05:00Z
poll
poll
expect "one restart sends nothing" eq "$(sent)" 0

echo "case: three restarts in the window alarm once, then re-arm"
up 2026-09-30T10:06:00Z
poll
expect "two restarts send nothing" eq "$(sent)" 0
up 2026-09-30T10:07:00+02:00
poll
expect "the third restart sends one notice" eq "$(sent)" 1
expect "it says crash-looping" eq "$(last_sent)" "Freeside prod daemon crash-looping"
up 2026-09-30T10:08:00Z
poll
up 2026-09-30T10:09:00Z
poll
expect "more restarts send nothing" eq "$(sent)" 1
advance_clock 600
probe check
expect "a quiet window sends nothing" eq "$(sent)" 1
up 2026-09-30T11:00:00Z
poll
up 2026-09-30T11:01:00Z
poll
expect "two restarts after re-arm send nothing" eq "$(sent)" 1
up 2026-09-30T11:02:00Z
poll
expect "a third after re-arm alarms again" eq "$(sent)" 2

echo "case: a clock stepped back does not pin the crash-loop alarm"
reset
poll
for s in 01 02 03; do
  up "2026-09-30T13:$s:00Z"
  poll
done
expect "three restarts alarm" eq "$(sent)" 1
advance_clock -86400
poll
up 2026-09-30T14:00:00Z
poll
up 2026-09-30T14:01:00Z
poll
up 2026-09-30T14:02:00Z
poll
expect "changes from before the step drop out, so the alarm re-arms" eq "$(sent)" 2

echo "case: restarts spread wider than the window raise nothing"
reset
poll
for s in 01 02 03 04 05; do
  up "2026-09-30T12:$s:00Z"
  advance_clock 300
  probe check
done
expect "one restart every five minutes sends nothing" eq "$(sent)" 0

echo "case: thresholds come from the config"
reset failure_count=1 restart_count=2 restart_window=120
poll
down
poll
expect "failure_count=1 alarms on the first failure" eq "$(sent)" 1
up 2026-09-30T10:00:00.123456789Z
poll
expect "then recovers" eq "$(sent)" 2
up 2026-09-30T10:01:00Z
poll
up 2026-09-30T10:02:00Z
poll
expect "restart_count=2 alarms on the second restart" eq "$(sent)" 3
reset restart_count=2 restart_window=120
poll
up 2026-09-30T10:01:00Z
poll
up 2026-09-30T10:02:00Z
advance_clock 120
probe check
expect "restarts a full window apart fall outside restart_window=120" eq "$(sent)" 0

echo "case: a malformed health answer is a failed poll"
# shellcheck disable=SC2016 # the $(...) body is a literal injection attempt
for answer in \
  '500|{"status":"ok","started_at":"2026-09-30T10:00:00Z"}' \
  '200|not json' \
  '200|{"status":"degraded","started_at":"2026-09-30T10:00:00Z"}' \
  '200|{"status":"ok"}' \
  '200|{"status":"ok","started_at":"yesterday"}' \
  '200|{"status":"ok","started_at":"2026-09-30 10:00:00"}' \
  '200|{"status":"ok","started_at":"$(touch pwned)"}'; do
  reset failure_count=1
  poll
  raw "${answer%%|*}" "${answer#*|}"
  poll
  expect "answer ${answer} alarms" eq "$(sent)" 1
  expect "answer ${answer} is logged as a failed poll" has "$OUT" "poll failed:"
done
expect "no health body was executed" test ! -e pwned
reset
raw 200 '{ "status" : "ok", "started_at" : "2026-09-30T10:00:00Z" }'
poll
expect "a spaced body is a good poll" eq "$OUT" ""

echo "case: a failed send is retried on the next poll"
reset
poll
down
echo 503 >"$STANDIN/ntfy-code"
poll
poll
poll
expect "a rejected send is not delivered" eq "$(sent)" 0
expect "the failed send exits 1" eq "$RC" 1
expect "it is logged by status" has "$OUT" "ntfy send failed: http 503"
echo down >"$STANDIN/ntfy-code"
poll
expect "an unreachable ntfy is logged" has "$OUT" "ntfy send failed: no answer"
echo 200 >"$STANDIN/ntfy-code"
poll
expect "the next poll sends the notice" eq "$(sent)" 1
poll
expect "and only once" eq "$(sent)" 1
up 2026-09-30T10:00:00.123456789Z
echo 503 >"$STANDIN/ntfy-code"
poll
echo 200 >"$STANDIN/ntfy-code"
poll
expect "an undelivered recovery notice is retried" eq "$(sent)" 2
expect "as the recovery notice" eq "$(last_sent)" "Freeside prod daemon reachable again"

echo "case: an undelivered crash-loop notice outlives the window thinning"
# Changes at minutes 0, 8, and 9 alarm; the retry at minute 10 sees the
# first change leave the window, leaving two, below restart_count.
reset
poll
up 2026-09-30T15:00:00Z
poll
up 2026-09-30T15:08:00Z
advance_clock 420
poll
up 2026-09-30T15:09:00Z
echo 503 >"$STANDIN/ntfy-code"
poll
expect "the failed crash-loop send is not delivered" eq "$(sent)" 0
echo 200 >"$STANDIN/ntfy-code"
poll
expect "the retry still sends it" eq "$(sent)" 1
expect "as the crash-loop notice" eq "$(last_sent)" "Freeside prod daemon crash-looping"
poll
expect "and only once" eq "$(sent)" 1

echo "case: the config must be a private regular file"
reset
chmod 644 "$PROBE_DIR/config"
probe check
expect "mode 0644 is refused" eq "$RC" 2
expect "with a reason" has "$OUT" "must be a private (0600) regular file"
reset
mv "$PROBE_DIR/config" "$TMP/real-config"
ln -s "$TMP/real-config" "$PROBE_DIR/config"
probe check
expect "a symlink is refused" eq "$RC" 2
rm "$PROBE_DIR/config"
ln "$TMP/real-config" "$PROBE_DIR/config"
probe check
expect "a hard link is refused" eq "$RC" 2
rm -f "$PROBE_DIR/config" "$TMP/real-config"
probe check
expect "a missing config is refused" eq "$RC" 2
reset
chmod 400 "$PROBE_DIR/config"
probe check
expect "mode 0400 is accepted" eq "$RC" 0
for bad in 'nfty_url=https://x' 'ntfy_url=https://again' 'no equals sign' \
  'failure_count=0' 'restart_window=10m' 'health_url=file:///etc/passwd' \
  'ntfy_token=has space' 'ntfy_token=quote"d'; do
  reset "$bad"
  probe check
  expect "config line \"$bad\" is refused" eq "$RC" 2
done
reset
printf 'ntfy_url=http://ntfy.example/t\n' >"$PROBE_DIR/config"
probe check
expect "a plain-http ntfy_url is refused" eq "$RC" 2
reset
printf 'ntfy_url=%s\n' "$TOPIC_URL" >"$PROBE_DIR/config"
probe notify-test
expect "a config without a token is accepted" eq "$RC" 0
expect "and sends no Authorization header" eq "$(grep -c Authorization "$STANDIN/curl-config")" 0

echo "case: a config error never echoes the line"
for bad in "$TOPIC_URL" "ntfy_url: $TOPIC_URL" "ntfy_token $TOKEN=" "$TOKEN=="; do
  reset "$bad"
  probe check
  expect "a stray secret line is refused" eq "$RC" 2
  expect "and the error names only the line number" has "$OUT" "line 3 of"
done

echo "case: the probe ignores ~/.curlrc and targets the configured health URL"
reset health_url=http://127.0.0.1:9999/health
poll
expect "curl runs with -q" has "$(head -n 1 "$STANDIN/curl-args")" "-q "
expect "the configured health URL is polled" has "$(head -n 1 "$STANDIN/curl-args")" "http://127.0.0.1:9999/health"
reset
poll
expect "prod defaults to port 7331" has "$(head -n 1 "$STANDIN/curl-args")" "http://127.0.0.1:7331/health"

echo "case: notify-test sends one notice"
reset
probe notify-test
expect "notify-test exits 0" eq "$RC" 0
expect "notify-test sends a test notice" eq "$(last_sent)" "Freeside prod liveness probe test notice"
echo 500 >"$STANDIN/ntfy-code"
probe notify-test
expect "a failed test notice exits 1" eq "$RC" 1

echo "case: the topic and token reach curl only on standard input"
expect "the stand-in received the topic on stdin" has "$(cat "$STANDIN/curl-config")" "url = \"$TOPIC_URL\""
expect "and the token as a header" has "$(cat "$STANDIN/curl-config")" "header = \"Authorization: Bearer $TOKEN\""
leaks=0
for f in "$TMP/all-output" "$STANDIN/curl-args" "$STANDIN/sent"; do
  if grep -q -e SECRETTOPIC -e SECRETTOKEN "$f"; then leaks=1; fi
done
if find "$HOME_DIR" -type f -exec grep -l -e SECRETTOPIC -e SECRETTOKEN {} + | grep -v '/config$' | grep -q .; then
  leaks=1
fi
expect "neither appears in output, curl arguments, notices, or probe files" eq "$leaks" 0

echo "case: install writes the plist and loads it; uninstall reverses both"
reset poll_interval=45
poll
probe install
PLIST=$HOME_DIR/Library/LaunchAgents/ai.freeside.liveness-probe.plist
expect "install exits 0" eq "$RC" 0
expect "the plist exists" test -f "$PLIST"
expect "the plist names the label" has "$(cat "$PLIST")" "<string>ai.freeside.liveness-probe</string>"
expect "the plist runs the installed copy" has "$(cat "$PLIST")" "<string>$PROBE_DIR/daemon-liveness-probe.sh</string>"
expect "the plist polls at poll_interval" has "$(cat "$PLIST")" "<integer>45</integer>"
expect "the plist runs at load" has "$(cat "$PLIST")" "<key>RunAtLoad</key>"
expect "the installed copy matches the script" cmp -s "$SCRIPT" "$PROBE_DIR/daemon-liveness-probe.sh"
if command -v plutil >/dev/null; then
  expect "the plist is well-formed" plutil -lint -s "$PLIST"
fi
uid=$(id -u)
expect "launchctl bootstraps the plist" has "$(cat "$STANDIN/launchctl-log")" "bootstrap gui/$uid $PLIST"
probe uninstall
expect "uninstall exits 0" eq "$RC" 0
expect "launchctl boots it out" eq "$(tail -n 1 "$STANDIN/launchctl-log")" "bootout gui/$uid/ai.freeside.liveness-probe"
expect "the plist is gone" test ! -e "$PLIST"
expect "the installed copy is gone" test ! -e "$PROBE_DIR/daemon-liveness-probe.sh"
expect "the alarm state is gone" test ! -e "$PROBE_DIR/state"
expect "the config is kept" test -f "$PROBE_DIR/config"
probe uninstall
expect "uninstall of an unloaded agent exits 0" eq "$RC" 0
expect "and says so" has "$OUT" "was not loaded"

echo "case: a failed bootout keeps the agent's files"
reset
probe install
touch "$STANDIN/launchctl-bootout-fail"
probe uninstall
expect "a failed bootout fails the uninstall" eq "$RC" 1
expect "with a reason" has "$OUT" "launchctl bootout failed"
expect "the plist is kept" test -f "$PLIST"
expect "the installed copy is kept" test -f "$PROBE_DIR/daemon-liveness-probe.sh"
rm "$STANDIN/launchctl-bootout-fail"
probe uninstall
expect "a retried uninstall succeeds" eq "$RC" 0
expect "and removes the plist" test ! -e "$PLIST"

echo "case: install retries a bootstrap refused during teardown"
reset
echo 1 >"$STANDIN/launchctl-fail"
probe install
expect "one refusal is retried" eq "$RC" 0
expect "bootstrap ran twice" eq "$(grep -c '^bootstrap' "$STANDIN/launchctl-log")" 2
reset
echo 3 >"$STANDIN/launchctl-fail"
probe install
expect "three refusals fail the install" eq "$RC" 1

echo "case: the dev tier uses its own label, directory, and port"
reset
DEV_DIR="$HOME_DIR/Library/Application Support/Freeside Liveness Probe/dev"
mkdir -p "$DEV_DIR"
(umask 077 && printf 'ntfy_url=%s\n' "$TOPIC_URL" >"$DEV_DIR/config")
probe install --tier dev
expect "dev install exits 0" eq "$RC" 0
expect "dev has its own plist" test -f "$HOME_DIR/Library/LaunchAgents/ai.freeside.liveness-probe.dev.plist"
expect "dev runs check --tier dev" has "$(cat "$HOME_DIR/Library/LaunchAgents/ai.freeside.liveness-probe.dev.plist")" "<string>dev</string>"
advance_clock 60
probe check --tier dev
expect "dev defaults to port 7332" has "$(tail -n 1 "$STANDIN/curl-args")" "http://127.0.0.1:7332/health"
rm -f "$PROBE_DIR/config"
probe install
expect "install refuses a missing config" eq "$RC" 2

echo "case: usage errors"
probe
expect "no command is a usage error" eq "$RC" 2
probe check --tier staging
expect "an unknown tier is a usage error" eq "$RC" 2
probe status
expect "an unknown command is a usage error" eq "$RC" 2

echo "case: the probe is executable, as the setup guide runs it"
expect "the script carries the executable bit" test -x "$SCRIPT"

echo
echo "passed $pass assertion(s), failed $fail"
if [ "$fail" -gt 0 ]; then
  exit 1
fi
