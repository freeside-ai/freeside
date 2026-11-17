#!/usr/bin/env bash
# test-check-credential-literals.sh — fixtures for the credential-literal check.
#
# Builds synthetic git repos and runs the check against a stand-in scanner
# (CREDENTIAL_LITERALS), so it needs no Go and no network: the scanner's own
# matching is tested in daemon/cmd/freeside-credential-literals. This suite
# covers the plumbing (which paths reach the scanner, how its exit maps to
# the check's) and that every pull request runs the check. Exit code: 0 when
# every assertion passes, 1 otherwise.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/.." && pwd)
CHECK=$SCRIPT_DIR/check-credential-literals.sh

# An inherited GIT_DIR would point the fixture repos' git at the caller's.
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE GIT_COMMON_DIR

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# The stand-in records the NUL-separated paths it reads, one per line, and
# exits with STUB_EXIT after printing STUB_OUT.
STUB=$TMP/stub-scanner
cat >"$STUB" <<'EOF'
#!/usr/bin/env bash
tr '\0' '\n' >"$STUB_RECORD"
[ -n "${STUB_OUT:-}" ] && printf '%s\n' "$STUB_OUT"
exit "${STUB_EXIT:-0}"
EOF
chmod +x "$STUB"

pass=0
fail=0
case_number=0
CASE=''
ROOT=''
OUT=''
RC=0

# begin_case <name> starts a case with a fresh repo tracking two files, one
# with a space in its name, plus an untracked file.
begin_case() {
  case_number=$((case_number + 1))
  CASE=$(printf '%02d %s' "$case_number" "$1")
  echo "case: $CASE"
  ROOT=$TMP/case-$case_number
  mkdir -p "$ROOT/sub"
  git -C "$ROOT" init -q
  printf 'tracked\n' >"$ROOT/a.txt"
  printf 'tracked\n' >"$ROOT/sub/with space.md"
  git -C "$ROOT" add a.txt "sub/with space.md"
  printf 'untracked\n' >"$ROOT/untracked.txt"
  export STUB_RECORD=$TMP/record-$case_number
  unset STUB_OUT STUB_EXIT
}

run_check() {
  set +e
  OUT=$(CREDENTIAL_LITERALS=$STUB bash "$CHECK" "$@" 2>&1)
  RC=$?
  set -e
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
    report_failure "expected exit $1, got $RC"
  fi
}

assert_contains() {
  if printf '%s\n' "$OUT" | grep -qF -- "$1"; then
    pass=$((pass + 1))
  else
    report_failure "expected output to contain: $1"
  fi
}

assert_not_contains() {
  if printf '%s\n' "$OUT" | grep -qF -- "$1"; then
    report_failure "expected output not to contain: $1"
  else
    pass=$((pass + 1))
  fi
}

assert_file_contains() {
  if grep -qF -- "$2" "$1"; then
    pass=$((pass + 1))
  else
    report_failure "expected $(basename "$1") to contain: $2"
  fi
}

begin_case 'clean tree passes and the scanner gets exactly the tracked paths'
run_check "$ROOT"
assert_rc 0
assert_not_contains 'credential-shaped'
if [ "$(cat "$STUB_RECORD")" = "$(printf 'a.txt\nsub/with space.md')" ]; then
  pass=$((pass + 1))
else
  report_failure "scanner read: $(cat "$STUB_RECORD")"
fi

begin_case 'a subdirectory argument scans from the repository top'
run_check "$ROOT/sub"
assert_rc 0
assert_file_contains "$STUB_RECORD" 'a.txt'

begin_case 'a match fails with the listing and the fix hint'
STUB_OUT='a.txt:1: anthropic_token' STUB_EXIT=1 run_check "$ROOT"
assert_rc 1
assert_contains 'a.txt:1: anthropic_token'
assert_contains 'Build fixtures at runtime'

begin_case 'a scanner failure is an environment error, not a pass'
STUB_EXIT=2 run_check "$ROOT"
assert_rc 2
assert_contains 'the scanner failed (exit 2)'

begin_case 'a directory outside git is refused'
mkdir -p "$TMP/not-a-repo"
run_check "$TMP/not-a-repo"
assert_rc 2
assert_contains 'is not a git work tree'

begin_case 'extra arguments are refused'
run_check "$ROOT" extra
assert_rc 2
assert_contains 'usage:'

# Every pull request runs the check and `required` waits for it, so a
# docs-only change can't bring a literal back unchecked.
begin_case 'every pull request runs the check through required'
workflow=$REPO_ROOT/.github/workflows/credential-literals.yml
pr=$REPO_ROOT/.github/workflows/pull-request.yml
assert_file_contains "$workflow" 'run: bash scripts/check.sh scripts credential-literals'
assert_file_contains "$workflow" 'go-version-file: daemon/go.mod'
assert_file_contains "$pr" 'uses: ./.github/workflows/credential-literals.yml'
if awk '/^  required:/,0' "$pr" | grep -qE '^      - credential-literals$' &&
  grep -qF '.["credential-literals"].result == "success"' "$pr"; then
  pass=$((pass + 1))
else
  report_failure 'required does not wait for credential-literals'
fi

echo "test-check-credential-literals: $pass passed, $fail failed"
[ "$fail" -eq 0 ]
