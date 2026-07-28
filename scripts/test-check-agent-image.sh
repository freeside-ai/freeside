#!/usr/bin/env bash
# test-check-agent-image.sh — regression suite for check-agent-image.sh.
#
# Drives the checker against a scripted `container` stand-in (the checker
# takes the container executable as its second argument), so every runtime
# response — create, inspect, list, delete — is a per-case fixture and no
# real container runtime is needed. Covers the duplicate-key JSON defense
# (#353: jq is last-value-wins, so a duplicated key must reject before any
# field comparison or force-delete, mirroring ward.RejectDuplicateJSONKeys)
# and the cidfile-less ownership recovery (#355: an interrupted create that
# never produced the cidfile is recovered by listing containers and deleting
# only an inspected ownership-token match).
#
# Exit code: 0 when every assertion passes, 1 otherwise.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
CHECKER=$SCRIPT_DIR/check-agent-image.sh

command -v jq >/dev/null 2>&1 || {
  echo "test-check-agent-image: jq is required" >&2
  exit 1
}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# Scripted Apple `container` stand-in. STUB_DIR selects the case fixtures:
#   create_mode        ok (registers probe-1, writes the cidfile)
#                      fail-no-cidfile (registers nothing on disk, exits 1)
#                      fail-with-cidfile (writes the cidfile, then exits 1)
#   inspect-<id>.json  inspect response; @TOKEN@ is replaced by the --label
#                      token captured from create; inspect-<id>-<n>.json
#                      (n = 1-based call count per id) overrides call n
#   list.json          list response (a missing file fails the list call)
#   drain_stdin        when present, inspect consumes its stdin first (a
#                      runtime that reads stdin inside the recovery loop)
#   delete_fail        when present, delete exits 1 after logging
#   inspects.log       appended with the id of every inspect call
#   deletes.log        appended with the arguments of every delete call
STUB=$TMP/container
cat >"$STUB" <<'STUB_EOF'
#!/usr/bin/env bash
set -euo pipefail
dir=${STUB_DIR:?}
cmd=${1:-}
shift || true
case "$cmd" in
create)
  cidfile="" token=""
  while [ "$#" -gt 0 ]; do
    case "$1" in
    --cidfile)
      cidfile=$2
      shift 2
      ;;
    --label)
      token=${2#*=}
      shift 2
      ;;
    *) shift ;;
    esac
  done
  printf '%s' "$token" >"$dir/token"
  case "$(cat "$dir/create_mode")" in
  ok) printf 'probe-1' >"$cidfile" ;;
  fail-no-cidfile) exit 1 ;;
  fail-with-cidfile)
    printf 'probe-1' >"$cidfile"
    exit 1
    ;;
  esac
  ;;
inspect)
  id=$1
  printf '%s\n' "$id" >>"$dir/inspects.log"
  [ ! -f "$dir/drain_stdin" ] || cat >/dev/null
  count_file=$dir/inspect-count-$id
  n=$(($(cat "$count_file" 2>/dev/null || echo 0) + 1))
  printf '%s' "$n" >"$count_file"
  template=$dir/inspect-$id-$n.json
  [ -f "$template" ] || template=$dir/inspect-$id.json
  [ -f "$template" ] || exit 1
  token=$(cat "$dir/token" 2>/dev/null || true)
  sed -e "s/@TOKEN@/${token}/g" "$template"
  ;;
list)
  [ -f "$dir/list.json" ] || exit 1
  cat "$dir/list.json"
  ;;
delete)
  printf '%s\n' "$*" >>"$dir/deletes.log"
  [ ! -f "$dir/delete_fail" ]
  ;;
*)
  exit 1
  ;;
esac
STUB_EOF
chmod +x "$STUB"

# A report satisfying every image-side precondition the checker compares.
compliant_report() { # <container-id>
  printf '%s' '[{"id":"@ID@","configuration":{"id":"@ID@","image":{"reference":"example.test/agent:1"},"labels":{"ai.freeside.project-image.owner":"@TOKEN@"},"initProcess":{"executable":"sh","arguments":["-c","true"],"environment":["PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"],"workingDirectory":"/"},"ssh":false,"publishedPorts":[],"publishedSockets":[],"networks":[],"mounts":[]}}]' |
    sed "s/@ID@/$1/g"
}

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
  export STUB_DIR=$CASE_DIR
  printf 'ok' >"$CASE_DIR/create_mode"
  echo "case: $CASE"
}

report_failure() {
  fail=$((fail + 1))
  echo "FAIL [$CASE]: $*"
  printf '%s\n' "$OUT" | sed 's/^/    | /'
}

run_checker() {
  set +e
  OUT=$("$CHECKER" example.test/agent:1 "$STUB" 2>&1)
  RC=$?
  set -e
}

assert_rc() {
  if [ "$RC" -eq "$1" ]; then
    pass=$((pass + 1))
  else
    report_failure "expected rc=$1, got rc=$RC"
  fi
}

assert_contains() {
  case "$OUT" in
  *"$1"*) pass=$((pass + 1)) ;;
  *) report_failure "output does not contain: $1" ;;
  esac
}

assert_lacks() {
  case "$OUT" in
  *"$1"*) report_failure "output unexpectedly contains: $1" ;;
  *) pass=$((pass + 1)) ;;
  esac
}

assert_deletes() { # exact expected deletes.log content; '' means no deletes
  actual=$(cat "$CASE_DIR/deletes.log" 2>/dev/null || true)
  if [ "$actual" = "$1" ]; then
    pass=$((pass + 1))
  else
    report_failure "deletes.log: expected '$1', got '$actual'"
  fi
}

assert_never_inspected() { # <id> must not appear in inspects.log
  if grep -qxF "$1" "$CASE_DIR/inspects.log" 2>/dev/null; then
    report_failure "candidate '$1' was inspected"
  else
    pass=$((pass + 1))
  fi
}

# ------------------------------------------------- existing behavior holds
begin_case "1 compliant image passes and the probe is removed"
compliant_report probe-1 >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 0
assert_contains "satisfies the ward allowlist"
assert_deletes "--force probe-1"

begin_case "2 image-contributed environment still fails the allowlist"
compliant_report probe-1 |
  jq '.[0].configuration.initProcess.environment += ["EXTRA=1"]' \
    >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 1
assert_contains "environment beyond the fixed PATH"
assert_deletes "--force probe-1"

begin_case "3 cidfile written by a failing create still recovers the probe"
printf 'fail-with-cidfile' >"$CASE_DIR/create_mode"
compliant_report probe-1 >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 1
assert_lacks "ownership recovery"
assert_deletes "--force probe-1"

begin_case "4 failing delete is reported"
printf '' >"$CASE_DIR/delete_fail"
compliant_report probe-1 >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 1
assert_contains "could not remove probe container probe-1"

# --------------------------------------------- cleanup ownership gating
begin_case "5 probe carrying a foreign ownership token is refused"
compliant_report probe-1 | sed 's/@TOKEN@/someone-else/' \
  >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 1
assert_contains "refusing to remove unowned probe container probe-1"
assert_deletes ""

begin_case "6 probe whose configuration id disagrees is refused"
compliant_report probe-1 |
  sed 's/"configuration":{"id":"probe-1"/"configuration":{"id":"probe-2"/' \
    >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 1
assert_contains "refusing to remove unowned probe container probe-1"
assert_deletes ""

begin_case "7 multi-entry probe inspection is refused"
compliant_report probe-1 | jq -c '. + .' >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 1
assert_contains "refusing to remove unowned probe container probe-1"
assert_deletes ""

# --------------------------------------- #353: duplicate-key JSON defense
begin_case "8 duplicated top-level key rejects before field comparison"
compliant_report probe-1 |
  sed 's/^\[{"id":"probe-1",/[{"id":"probe-1","id":"probe-1",/' \
    >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 1
assert_contains "runtime inspection JSON is ambiguous"
assert_lacks "would fail the ward allowlist"
assert_deletes ""

begin_case "9 duplicated nested key rejects before field comparison"
compliant_report probe-1 |
  sed 's/"executable":"sh",/"executable":"sh","executable":"sh",/' \
    >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 1
assert_contains "runtime inspection JSON is ambiguous"
assert_deletes ""

begin_case "10 duplicated labels object rejects and blocks the delete"
compliant_report probe-1 |
  sed 's/"labels":{[^}]*},/&&/' >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 1
assert_contains "runtime inspection JSON is ambiguous"
assert_contains "refusing to remove probe container probe-1: ambiguous runtime JSON"
assert_deletes ""

begin_case "11 duplicate keys appearing only at cleanup refuse the delete"
compliant_report probe-1 >"$CASE_DIR/inspect-probe-1-1.json"
compliant_report probe-1 |
  sed 's/"labels":{[^}]*},/&&/' >"$CASE_DIR/inspect-probe-1-2.json"
run_checker
assert_rc 1
assert_contains "refusing to remove probe container probe-1: ambiguous runtime JSON"
assert_deletes ""

# ------------------------------------- #355: cidfile-less probe recovery
begin_case "12 recovery deletes only the ownership-token match"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1"}},{"id":"foreign-1","configuration":{"id":"foreign-1"}}]' \
  >"$CASE_DIR/list.json"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1","labels":{"ai.freeside.project-image.owner":"@TOKEN@"}}}]' \
  >"$CASE_DIR/inspect-owned-1.json"
printf '%s' '[{"id":"foreign-1","configuration":{"id":"foreign-1","labels":{"ai.freeside.project-image.owner":"someone-else"}}}]' \
  >"$CASE_DIR/inspect-foreign-1.json"
run_checker
assert_rc 1
assert_contains "recovered orphaned probe container owned-1"
assert_deletes "--force owned-1"

begin_case "13 recovery without an owned container deletes nothing"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '[{"id":"foreign-1","configuration":{"id":"foreign-1"}}]' \
  >"$CASE_DIR/list.json"
printf '%s' '[{"id":"foreign-1","configuration":{"id":"foreign-1","labels":{"ai.freeside.project-image.owner":"someone-else"}}}]' \
  >"$CASE_DIR/inspect-foreign-1.json"
run_checker
assert_rc 1
assert_lacks "recovered orphaned probe container"
assert_deletes ""

begin_case "14 recovery skips a candidate whose inspected id disagrees"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1"}}]' \
  >"$CASE_DIR/list.json"
printf '%s' '[{"id":"other-1","configuration":{"id":"owned-1","labels":{"ai.freeside.project-image.owner":"@TOKEN@"}}}]' \
  >"$CASE_DIR/inspect-owned-1.json"
run_checker
assert_rc 1
assert_lacks "recovered orphaned probe container"
assert_deletes ""

begin_case "15 recovery skips a candidate whose inspected configuration id disagrees"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1"}}]' \
  >"$CASE_DIR/list.json"
printf '%s' '[{"id":"owned-1","configuration":{"id":"other-1","labels":{"ai.freeside.project-image.owner":"@TOKEN@"}}}]' \
  >"$CASE_DIR/inspect-owned-1.json"
run_checker
assert_rc 1
assert_lacks "recovered orphaned probe container"
assert_deletes ""

begin_case "16 recovery refuses an ambiguous container listing"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '[{"id":"owned-1","id":"owned-1","configuration":{"id":"owned-1"}}]' \
  >"$CASE_DIR/list.json"
run_checker
assert_rc 1
assert_contains "refusing ownership recovery on ambiguous container listing"
assert_deletes ""

begin_case "17 recovery refuses a non-array container listing"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '{"k":{"id":"owned-1","configuration":{"id":"owned-1"}}}' \
  >"$CASE_DIR/list.json"
run_checker
assert_rc 1
assert_contains "container listing returned an unexpected object"
assert_never_inspected "owned-1"
assert_deletes ""

begin_case "18 recovery flags a listed identity with hostile characters"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '[{"id":"bad id!","configuration":{"id":"bad id!"}}]' \
  >"$CASE_DIR/list.json"
run_checker
assert_rc 1
assert_contains "ownership recovery listed an invalid container identity"
assert_never_inspected "bad id!"
assert_deletes ""

begin_case "19 recovery flags a listed identity disagreeing with its configuration"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '[{"id":"mismatch-a","configuration":{"id":"mismatch-b"}}]' \
  >"$CASE_DIR/list.json"
run_checker
assert_rc 1
assert_contains "ownership recovery listed an invalid container identity"
assert_never_inspected "mismatch-a"
assert_deletes ""

begin_case "20 recovery flags a non-object listing entry"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '["garbage"]' >"$CASE_DIR/list.json"
run_checker
assert_rc 1
assert_contains "ownership recovery listed an invalid container identity"
assert_deletes ""

begin_case "21 recovery flags invalid listed identities, still recovers the owned probe"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '[{"id":"bad id!","configuration":{"id":"bad id!"}},{"id":"mismatch-a","configuration":{"id":"mismatch-b"}},{"id":"owned-1","configuration":{"id":"owned-1"}}]' \
  >"$CASE_DIR/list.json"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1","labels":{"ai.freeside.project-image.owner":"@TOKEN@"}}}]' \
  >"$CASE_DIR/inspect-owned-1.json"
run_checker
assert_rc 1
assert_contains "ownership recovery listed an invalid container identity"
assert_contains "recovered orphaned probe container owned-1"
assert_never_inspected "bad id!"
assert_never_inspected "mismatch-a"
assert_deletes "--force owned-1"

begin_case "22 recovery refuses an ambiguous candidate inspection"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1"}}]' \
  >"$CASE_DIR/list.json"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1","labels":{"k":"v"},"labels":{"k":"v"}}}]' \
  >"$CASE_DIR/inspect-owned-1.json"
run_checker
assert_rc 1
assert_contains "refusing to judge ownership of container owned-1"
assert_deletes ""

begin_case "23 recovery reports an uninspectable candidate and deletes nothing"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1"}}]' \
  >"$CASE_DIR/list.json"
run_checker
assert_rc 1
assert_contains "could not inspect container owned-1 for ownership recovery"
assert_deletes ""

begin_case "24 recovery refuses a listing that fails"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
run_checker
assert_rc 1
assert_contains "could not list containers for ownership recovery"
assert_deletes ""

begin_case "25 recovery survives a runtime that drains stdin"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '' >"$CASE_DIR/drain_stdin"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1"}},{"id":"owned-2","configuration":{"id":"owned-2"}}]' \
  >"$CASE_DIR/list.json"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1","labels":{"ai.freeside.project-image.owner":"@TOKEN@"}}}]' \
  >"$CASE_DIR/inspect-owned-1.json"
printf '%s' '[{"id":"owned-2","configuration":{"id":"owned-2","labels":{"ai.freeside.project-image.owner":"@TOKEN@"}}}]' \
  >"$CASE_DIR/inspect-owned-2.json"
run_checker
assert_rc 1
assert_deletes "$(printf -- '--force owned-1\n--force owned-2')"

begin_case "26 failed identity-file creation runs no recovery"
compliant_report probe-1 >"$CASE_DIR/inspect-probe-1.json"
set +e
OUT=$(TMPDIR=$CASE_DIR/no-such-dir "$CHECKER" example.test/agent:1 "$STUB" 2>&1)
RC=$?
set -e
assert_rc 1
assert_lacks "ownership recovery"
assert_deletes ""

begin_case "27 raw NUL byte inside the inspect report rejects"
# A raw NUL between members is invalid JSON, but command substitution
# strips it, leaving a compliant document; the checker must validate the
# raw bytes from a file, never a NUL-stripped shell variable.
whole=$(compliant_report probe-1)
left=${whole%%'"publishedPorts"'*}
right=${whole#"$left"}
{
  printf '%s' "$left"
  printf '\000'
  printf '%s' "$right"
} >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 1
assert_contains "runtime inspection JSON is ambiguous"
assert_deletes ""

begin_case "28 raw NUL byte inside the container listing refuses recovery"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
{
  printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1"}}'
  printf '\000'
  printf '%s' ']'
} >"$CASE_DIR/list.json"
run_checker
assert_rc 1
assert_contains "refusing ownership recovery on ambiguous container listing"
assert_never_inspected "owned-1"
assert_deletes ""

# Runtime output past the daemon-parity 16 MiB cap must fail the capture
# before any parse, not fill the filesystem (17 MiB of 'x' here).
oversized_report() { head -c 17000000 /dev/zero | tr '\0' 'x'; }

begin_case "29 oversized inspect report fails the capture"
oversized_report >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 1
assert_contains "could not capture the probe inspection"
assert_deletes ""

begin_case "30 oversized container listing refuses recovery"
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
oversized_report >"$CASE_DIR/list.json"
run_checker
assert_rc 1
assert_contains "could not list containers for ownership recovery"
assert_deletes ""

begin_case "31 capture truncated by a failing sink is rejected"
# Under a file-size limit the capture's sink (head) dies after writing a
# truncated prefix; with a compliant report padded past the limit, that
# prefix is a complete, compliant document, so only checking the sink's
# pipeline status rejects it. The 2-block (1024-byte) limit truncates the
# runtime_json capture while every other file the run writes stays small.
{
  compliant_report probe-1
  head -c 5000 /dev/zero | tr '\0' ' '
} >"$CASE_DIR/inspect-probe-1.json"
set +e
OUT=$( (
  ulimit -f 2
  "$CHECKER" example.test/agent:1 "$STUB"
) 2>&1)
RC=$?
set -e
assert_rc 1
assert_contains "could not capture the probe inspection"
assert_deletes ""

begin_case "32 ownership revoked between selection and deletion is refused"
# The pre-delete re-inspection (recovery_delete_owned) must catch a
# candidate whose ownership changed after the selection inspection: the
# first inspection shows this run's token, the second a foreign one.
printf 'fail-no-cidfile' >"$CASE_DIR/create_mode"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1"}}]' \
  >"$CASE_DIR/list.json"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1","labels":{"ai.freeside.project-image.owner":"@TOKEN@"}}}]' \
  >"$CASE_DIR/inspect-owned-1-1.json"
printf '%s' '[{"id":"owned-1","configuration":{"id":"owned-1","labels":{"ai.freeside.project-image.owner":"someone-else"}}}]' \
  >"$CASE_DIR/inspect-owned-1-2.json"
run_checker
assert_rc 1
assert_contains "refusing recovery deletion of container owned-1: ownership no longer verified"
assert_lacks "recovered orphaned probe container"
assert_deletes ""

# --------------------- detector battery: adversarial input-space fixtures
# Each document stands in for the whole inspect report. Duplicates of every
# shape must reject with the ambiguity error; structurally valid documents
# must never trip it, and must instead reach the field comparison (the
# pre-existing failure mode for a non-compliant report).
for doc in \
  '{"a":1,"a":2}' \
  '{"a":{"x":1},"a":{"y":2}}' \
  '{"a":{"x":1},"a":{"x":2}}' \
  '{"a":{"x":1},"a":2}' \
  '{"a":2,"a":{"x":1}}' \
  '{"a":{},"a":{}}' \
  '{"a":[1],"a":[2]}' \
  '{"A":1,"a":2}' \
  '{"a":1} {"b":2}' \
  '' \
  'not json' \
  '{"a":1,}'; do
  begin_case "detector rejects: ${doc:-<empty>}"
  printf '%s' "$doc" >"$CASE_DIR/inspect-probe-1.json"
  run_checker
  assert_rc 1
  assert_contains "runtime inspection JSON is ambiguous"
done

for doc in \
  '{"a":1,"b":2}' \
  '{"a":{"x":1},"b":{"x":2}}' \
  '[{"a":1},{"a":2}]' \
  '{"a":{"b":{"c":1}}}' \
  '{"a":{},"b":[]}' \
  '{"a":[[1],[2]],"b":[{"x":[]},{"x":{}}]}' \
  '42'; do
  begin_case "detector passes: $doc"
  printf '%s' "$doc" >"$CASE_DIR/inspect-probe-1.json"
  run_checker
  assert_rc 1
  assert_lacks "runtime inspection JSON is ambiguous"
  assert_contains "would fail the ward allowlist"
done

# ---------------------------------------------------------------- summary
echo
echo "passed $pass assertion(s), failed $fail"
if [ "$fail" -gt 0 ]; then
  exit 1
fi
