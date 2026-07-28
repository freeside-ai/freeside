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
#                      fail-with-cidfile (writes the cidfile, then exits 1)
#   inspect-<id>.json  inspect response; @TOKEN@ is replaced by the --label
#                      token captured from create; inspect-<id>-<n>.json
#                      (n = 1-based call count per id) overrides call n
#   delete_fail        when present, delete exits 1 after logging
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
  fail-with-cidfile)
    printf 'probe-1' >"$cidfile"
    exit 1
    ;;
  esac
  ;;
inspect)
  id=$1
  count_file=$dir/inspect-count-$id
  n=$(($(cat "$count_file" 2>/dev/null || echo 0) + 1))
  printf '%s' "$n" >"$count_file"
  template=$dir/inspect-$id-$n.json
  [ -f "$template" ] || template=$dir/inspect-$id.json
  [ -f "$template" ] || exit 1
  token=$(cat "$dir/token" 2>/dev/null || true)
  sed -e "s/@TOKEN@/${token}/g" "$template"
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

# Runtime output past the daemon-parity 16 MiB cap must fail the capture
# before any parse, not fill the filesystem (17 MiB of 'x' here).
oversized_report() { head -c 17000000 /dev/zero | tr '\0' 'x'; }

begin_case "29 oversized inspect report fails the capture"
oversized_report >"$CASE_DIR/inspect-probe-1.json"
run_checker
assert_rc 1
assert_contains "could not capture the probe inspection"
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
