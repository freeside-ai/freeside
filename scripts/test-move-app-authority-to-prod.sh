#!/usr/bin/env bash
# test-move-app-authority-to-prod.sh — regression suite for
# move-app-authority-to-prod.sh.
#
# Sources the script and drives move_app_authority against synthetic sources, a
# synthetic prod root, and a synthetic lease root. Checks that a complete move
# places every file with owner-only modes, keeps prod's other files, and renames
# both sources aside intact; that every refusal (prod already holding App state
# or credentials, a rig manifest, an unrecognized or symlinked source entry, a
# source missing its authority) changes nothing; that a failure at the
# credentials rename, or a hangup there, removes every copy it placed,
# restores an empty prod credentials directory, and leaves the sources; that a
# hangup after that rename keeps the complete move; that a prod holding its
# database lock refuses; and
# that the script refuses a HOME whose prod root differs from the passwd home's.
# Nothing touches the real prod root.
#
# Exit code: 0 when every assertion passes, 1 otherwise.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
SCRIPT=$SCRIPT_DIR/move-app-authority-to-prod.sh

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
TMP=$(cd -P "$TMP" && pwd -P)

pass=0
fail=0
CASE=''
CASE_DIR=''
OUT=''
RC=0

report_failure() {
  fail=$((fail + 1))
  echo "FAIL [$CASE]: $*"
  printf '%s\n' "$OUT" | sed 's/^/    | /'
}

ok() {
  pass=$((pass + 1))
}

# begin_case <name>: a fresh case with valid sources, an idle prod root that
# already holds its own daemon files, and an empty lease root.
begin_case() {
  CASE=$1
  CASE_DIR=$(mktemp -d "$TMP/case.XXXXXX")
  echo "case: $CASE"
  STATE=$CASE_DIR/app-state
  CREDS=$CASE_DIR/app-creds
  PROD=$CASE_DIR/prod
  LEASE=$CASE_DIR/rig-locks
  mkdir -p "$STATE" "$CREDS/github-app/42" "$CREDS/github-app.quarantine" "$PROD/daemon" "$LEASE"
  printf 'authority\n' >"$STATE/installation-authority.json"
  printf 'journal\n' >"$STATE/installation-janitor-journal.json"
  : >"$STATE/installation-janitor-journal.lock"
  printf 'archive\n' >"$STATE/installation-recovery-42-7-3.json"
  printf 'meta\n' >"$CREDS/github-app/42/app.json"
  printf 'key\n' >"$CREDS/github-app/42/app.pem"
  chmod 700 "$STATE" "$CREDS" "$CREDS/github-app" "$CREDS/github-app/42" "$CREDS/github-app.quarantine"
  chmod 600 "$STATE"/* "$CREDS/github-app/42"/*
  printf 'db\n' >"$PROD/daemon/freeside.db"
  : >"$PROD/daemon/freeside.db.daemon.lock"
}

# run_move [PATH prefix]: runs move_app_authority on the case's roots under
# prod's database lock, as the script's main does.
run_move() {
  local path_prefix=${1:-}
  set +e
  OUT=$(
    PATH="${path_prefix:+$path_prefix:}$PATH" bash -c '
      set -euo pipefail
      source "$1"
      app_move_with_prod_lock "$4" move_app_authority "$2" "$3" "$4" "$5" 20260928T120000Z
    ' _ "$SCRIPT" "$STATE" "$CREDS" "$PROD" "$LEASE" 2>&1
  )
  RC=$?
  set -e
}

tree_snapshot() {
  (cd "$CASE_DIR" && find . -print | LC_ALL=C sort && find . -type f -exec cksum {} + | LC_ALL=C sort)
}

mode_of() {
  if stat -f '%Lp' "$1" >/dev/null 2>&1; then stat -f '%Lp' "$1"; else stat -c '%a' "$1"; fi
}

assert_unchanged_refusal() { # <before snapshot> <expected message>
  if [ "$RC" -eq 2 ]; then ok; else report_failure "expected rc=2, got rc=$RC"; fi
  if [[ "$OUT" == *"refusing"* && "$OUT" == *"$2"* ]]; then ok; else report_failure "output lacks refusal \"$2\""; fi
  if [ "$(tree_snapshot)" = "$1" ]; then ok; else report_failure "the refusal changed the tree"; fi
}

begin_case "a complete move places both halves and keeps the sources aside"
run_move
if [ "$RC" -eq 0 ]; then ok; else report_failure "expected rc=0, got rc=$RC"; fi
for f in installation-authority.json installation-janitor-journal.json installation-janitor-journal.lock installation-recovery-42-7-3.json; do
  if cmp -s "$STATE.moved-to-prod-20260928T120000Z/$f" "$PROD/daemon/$f"; then ok; else report_failure "$f was not placed intact"; fi
  if [ "$(mode_of "$PROD/daemon/$f")" = 600 ]; then ok; else report_failure "$f is not owner-only"; fi
done
if cmp -s "$CREDS.moved-to-prod-20260928T120000Z/github-app/42/app.pem" "$PROD/credentials/github-app/42/app.pem"; then ok; else report_failure "the key was not placed intact"; fi
if [ -d "$PROD/credentials/github-app.quarantine" ]; then ok; else report_failure "the quarantine was not placed"; fi
if [ "$(mode_of "$PROD/credentials")" = 700 ] && [ "$(mode_of "$PROD/credentials/github-app/42/app.pem")" = 600 ]; then ok; else report_failure "credentials are not owner-only"; fi
if [ "$(cat "$PROD/daemon/freeside.db")" = db ]; then ok; else report_failure "prod's own daemon files changed"; fi
if [ ! -e "$STATE" ] && [ ! -e "$CREDS" ]; then ok; else report_failure "a source was left under its old name"; fi
if [ -z "$(find "$PROD" -maxdepth 1 -name ".app-authority-move.*")" ]; then ok; else report_failure "a staging directory was left behind"; fi
if [[ "$OUT" == *"FREESIDE_REAL_RUN_APP_STATE=\"$PROD/daemon\""* ]]; then ok; else report_failure "the output does not name the new App state directory"; fi

begin_case "an empty prod credentials directory is replaced"
mkdir -m 700 "$PROD/credentials"
run_move
if [ "$RC" -eq 0 ] && [ -f "$PROD/credentials/github-app/42/app.pem" ]; then ok; else report_failure "expected a move over an empty directory, rc=$RC"; fi

begin_case "prod already holding an authority snapshot refuses"
printf 'prod\n' >"$PROD/daemon/installation-authority.json"
before=$(tree_snapshot)
run_move
assert_unchanged_refusal "$before" "prod already holds App state"

begin_case "prod already holding a janitor journal refuses"
printf 'prod\n' >"$PROD/daemon/installation-janitor-journal.json"
before=$(tree_snapshot)
run_move
assert_unchanged_refusal "$before" "prod already holds App state"

begin_case "prod already holding credentials refuses"
mkdir -p "$PROD/credentials/github-app"
before=$(tree_snapshot)
run_move
assert_unchanged_refusal "$before" "prod already holds App credentials"

begin_case "a production rig manifest refuses"
printf '{}\n' >"$LEASE/production-rig.json"
before=$(tree_snapshot)
run_move
assert_unchanged_refusal "$before" "production rig manifest"

begin_case "an unrecognized state file refuses"
: >"$STATE/notes.txt"
before=$(tree_snapshot)
run_move
assert_unchanged_refusal "$before" "notes.txt"

begin_case "a hidden state file refuses"
: >"$STATE/.installation-authority.json-1.tmp"
before=$(tree_snapshot)
run_move
assert_unchanged_refusal "$before" ".installation-authority.json-1.tmp"

begin_case "a symlinked state file refuses"
rm "$STATE/installation-janitor-journal.json"
ln -s "$CASE_DIR/elsewhere.json" "$STATE/installation-janitor-journal.json"
before=$(tree_snapshot)
run_move
assert_unchanged_refusal "$before" "installation-janitor-journal.json"

begin_case "an unrecognized credentials entry refuses"
mkdir "$CREDS/github-app.legacy"
before=$(tree_snapshot)
run_move
assert_unchanged_refusal "$before" "github-app.legacy"

begin_case "a symlink inside the credentials tree refuses"
ln -s "$CASE_DIR/elsewhere.pem" "$CREDS/github-app/42/extra.pem"
before=$(tree_snapshot)
run_move
assert_unchanged_refusal "$before" "symlink"

begin_case "a state source without an authority snapshot refuses"
rm "$STATE/installation-authority.json"
before=$(tree_snapshot)
run_move
assert_unchanged_refusal "$before" "no installation-authority.json"

begin_case "a credentials source without registrations refuses"
rm -rf "$CREDS/github-app"
before=$(tree_snapshot)
run_move
assert_unchanged_refusal "$before" "no github-app registrations"

begin_case "a credentials source with an empty registrations directory refuses"
rm -rf "$CREDS/github-app/42"
before=$(tree_snapshot)
run_move
assert_unchanged_refusal "$before" "no github-app registrations"

begin_case "a registration without its key refuses"
rm "$CREDS/github-app/42/app.pem"
before=$(tree_snapshot)
run_move
assert_unchanged_refusal "$before" "no github-app registrations"

begin_case "a failed credentials rename removes every copy and keeps the sources"
stub=$CASE_DIR/stub-bin
mkdir -p "$stub"
cat >"$stub/mv" <<'MV_STUB'
#!/usr/bin/env bash
last=${!#}
if [[ "$last" == */prod/credentials ]]; then
  echo "mv: simulated failure" >&2
  exit 1
fi
exec /bin/mv "$@"
MV_STUB
chmod +x "$stub/mv"
run_move "$stub"
if [ "$RC" -eq 1 ]; then ok; else report_failure "expected rc=1, got rc=$RC"; fi
if [[ "$OUT" == *"nothing was moved"* ]]; then ok; else report_failure "the failure does not say nothing was moved"; fi
if [ ! -e "$PROD/daemon/installation-authority.json" ] && [ ! -e "$PROD/daemon/installation-recovery-42-7-3.json" ]; then ok; else report_failure "placed state files were not removed"; fi
if [ ! -e "$PROD/credentials" ]; then ok; else report_failure "credentials were left in prod"; fi
if [ -z "$(find "$PROD" -maxdepth 1 -name ".app-authority-move.*")" ]; then ok; else report_failure "a staging directory was left behind"; fi
if [ -f "$STATE/installation-authority.json" ] && [ -f "$CREDS/github-app/42/app.pem" ]; then ok; else report_failure "a source was changed"; fi
if [ "$(cat "$PROD/daemon/freeside.db")" = db ]; then ok; else report_failure "prod's own daemon files changed"; fi

begin_case "a failed rename over an empty credentials directory restores it"
mkdir -m 700 "$PROD/credentials"
run_move "$stub"
if [ "$RC" -eq 1 ]; then ok; else report_failure "expected rc=1, got rc=$RC"; fi
if [ -d "$PROD/credentials" ] && [ -z "$(ls -A "$PROD/credentials")" ] && [ "$(mode_of "$PROD/credentials")" = 700 ]; then ok; else report_failure "prod's empty credentials directory was not restored"; fi
if [ ! -e "$PROD/daemon/installation-authority.json" ]; then ok; else report_failure "placed state files were not removed"; fi

begin_case "a hangup before the credentials rename removes every copy"
interrupt_stub=$CASE_DIR/interrupt-bin
mkdir -p "$interrupt_stub"
cat >"$interrupt_stub/mv" <<'MV_STUB'
#!/usr/bin/env bash
last=${!#}
if [[ "$last" == */prod/credentials ]]; then
  kill -HUP "$PPID"
  exit 1
fi
exec /bin/mv "$@"
MV_STUB
chmod +x "$interrupt_stub/mv"
run_move "$interrupt_stub"
if [ "$RC" -eq 130 ]; then ok; else report_failure "expected rc=130, got rc=$RC"; fi
if [[ "$OUT" == *"interrupted; nothing was moved"* ]]; then ok; else report_failure "the interrupt does not say nothing was moved"; fi
if [ ! -e "$PROD/daemon/installation-authority.json" ] && [ ! -e "$PROD/credentials" ]; then ok; else report_failure "copies were left in prod"; fi
if [ -z "$(find "$PROD" -maxdepth 1 -name ".app-authority-move.*")" ]; then ok; else report_failure "a staging directory was left behind"; fi
if [ -f "$STATE/installation-authority.json" ] && [ -f "$CREDS/github-app/42/app.pem" ]; then ok; else report_failure "a source was changed"; fi

begin_case "a running prod holding its database lock refuses"
held=$CASE_DIR/lock-held
perl -MFcntl=:flock -e 'open(my $f, ">>", $ARGV[0]) or die; flock($f, LOCK_EX) or die; open(my $m, ">", $ARGV[1]) or die; close $m; sleep 60' \
  "$PROD/daemon/freeside.db.daemon.lock" "$held" &
holder=$!
for _ in $(seq 1 100); do [ -e "$held" ] && break; sleep 0.1; done
rm -f "$held"
before=$(tree_snapshot)
run_move
kill "$holder" 2>/dev/null || true
wait "$holder" 2>/dev/null || true
assert_unchanged_refusal "$before" "prod holds its database lock"

begin_case "a hangup right after a state file's rename removes it"
placed_stub=$CASE_DIR/placed-bin
mkdir -p "$placed_stub"
cat >"$placed_stub/mv" <<'MV_STUB'
#!/usr/bin/env bash
last=${!#}
/bin/mv "$@" || exit
if [[ "$last" == */prod/daemon/installation-authority.json ]]; then
  kill -HUP "$PPID"
fi
MV_STUB
chmod +x "$placed_stub/mv"
run_move "$placed_stub"
if [ "$RC" -eq 130 ]; then ok; else report_failure "expected rc=130, got rc=$RC"; fi
if [ ! -e "$PROD/daemon/installation-authority.json" ] && [ ! -e "$PROD/credentials" ]; then ok; else report_failure "a renamed state file was left in prod"; fi
if [ -f "$STATE/installation-authority.json" ]; then ok; else report_failure "a source was changed"; fi

begin_case "a hangup after the credentials rename keeps the complete move"
commit_stub=$CASE_DIR/commit-bin
mkdir -p "$commit_stub"
cat >"$commit_stub/mv" <<'MV_STUB'
#!/usr/bin/env bash
last=${!#}
/bin/mv "$@" || exit
if [[ "$last" == */prod/credentials ]]; then
  kill -HUP "$PPID"
fi
MV_STUB
chmod +x "$commit_stub/mv"
run_move "$commit_stub"
if [ "$RC" -eq 130 ]; then ok; else report_failure "expected rc=130, got rc=$RC"; fi
if [[ "$OUT" == *"after the authority reached prod"* ]]; then ok; else report_failure "the output does not say the authority reached prod"; fi
if [ -f "$PROD/daemon/installation-authority.json" ] && [ -f "$PROD/credentials/github-app/42/app.pem" ]; then ok; else report_failure "the committed move was rolled back"; fi
if [ -z "$(find "$PROD" -maxdepth 1 -name ".app-authority-move.*")" ]; then ok; else report_failure "a staging directory was left behind"; fi

begin_case "the script refuses a HOME whose prod root differs from the passwd home's"
mkdir -p "$CASE_DIR/home/Library/Application Support/Freeside"
before=$(tree_snapshot)
set +e
OUT=$(HOME=$CASE_DIR/home bash "$SCRIPT" "$STATE" "$CREDS" 2>&1)
RC=$?
set -e
assert_unchanged_refusal "$before" "passwd home"

echo
echo "passed $pass assertion(s), failed $fail"
if [ "$fail" -gt 0 ]; then
  exit 1
fi
