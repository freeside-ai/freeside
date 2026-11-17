#!/usr/bin/env bash
# test-check-skill-mirrors.sh — fixtures for the skill mirror check.
#
# Builds synthetic repo roots (.claude/skills and .agents/skills trees) with
# no git and no network, then asserts CI runs the check whenever either tree
# changes. Exit code: 0 when every assertion passes, 1 otherwise.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
CHECK=$SCRIPT_DIR/check-skill-mirrors.sh

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
case_number=0
CASE=''
ROOT=''
OUT=''
RC=0

# begin_case <name> starts a case with a fresh root holding two skills,
# one with a references/ folder, mirrored exactly.
begin_case() {
  case_number=$((case_number + 1))
  CASE=$(printf '%02d %s' "$case_number" "$1")
  echo "case: $CASE"
  ROOT=$TMP/case-$case_number
  mkdir -p "$ROOT/.claude/skills/alpha/references" "$ROOT/.claude/skills/beta" \
    "$ROOT/.agents/skills"
  printf 'alpha skill\n' >"$ROOT/.claude/skills/alpha/SKILL.md"
  printf 'alpha reference\n' >"$ROOT/.claude/skills/alpha/references/notes.md"
  printf 'beta skill\n' >"$ROOT/.claude/skills/beta/SKILL.md"
  cp -R "$ROOT/.claude/skills/alpha" "$ROOT/.claude/skills/beta" "$ROOT/.agents/skills/"
}

run_check() {
  set +e
  OUT=$(bash "$CHECK" "$@" 2>&1)
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

begin_case "an exact mirror passes"
run_check "$ROOT"
assert_rc 0
assert_contains '2 skill(s) mirrored exactly'

begin_case "an edited source without its copy fails and names the file"
printf 'alpha reference, edited\n' >"$ROOT/.claude/skills/alpha/references/notes.md"
run_check "$ROOT"
assert_rc 1
assert_contains '.agents/skills/alpha differs from .claude/skills/alpha:'
assert_contains 'references/notes.md'
assert_not_contains "$ROOT"
assert_not_contains 'beta'

begin_case "a file only in the mirror fails"
printf 'stray\n' >"$ROOT/.agents/skills/beta/extra.md"
run_check "$ROOT"
assert_rc 1
assert_contains '.agents/skills/beta differs from .claude/skills/beta:'
assert_contains 'extra.md'

begin_case "a skill with no mirror fails"
rm -rf "$ROOT/.agents/skills/beta"
run_check "$ROOT"
assert_rc 1
assert_contains '.agents/skills/beta is missing'

begin_case "a mirror with no source fails"
mkdir -p "$ROOT/.agents/skills/gamma"
printf 'gamma\n' >"$ROOT/.agents/skills/gamma/SKILL.md"
run_check "$ROOT"
assert_rc 1
assert_contains '.agents/skills/gamma has no source'

begin_case "a symlinked mirror fails once, as a symlink"
rm -rf "$ROOT/.agents/skills/alpha"
ln -s ../../.claude/skills/alpha "$ROOT/.agents/skills/alpha"
run_check "$ROOT"
assert_rc 1
assert_contains '.agents/skills/alpha is a symlink'
assert_not_contains 'is missing'
assert_not_contains 'differs'
assert_not_contains 'has no source'

begin_case "a symlink inside a source fails even when mirrored"
ln -s SKILL.md "$ROOT/.claude/skills/beta/alias.md"
ln -s SKILL.md "$ROOT/.agents/skills/beta/alias.md"
run_check "$ROOT"
assert_rc 1
assert_contains '.claude/skills/beta/alias.md is a symlink'
assert_contains '.agents/skills/beta/alias.md is a symlink'

begin_case "a symlinked parent can't make the trees one"
rm -rf "$ROOT/.agents"
ln -s .claude "$ROOT/.agents"
run_check "$ROOT"
assert_rc 1
assert_contains '.agents is a symlink'
assert_not_contains 'mirrored exactly'

begin_case "a drifted file's name prints as it is"
mkdir -p "$ROOT/.claude/skills/alpha/v1." "$ROOT/.agents/skills/alpha/v1."
printf 'one\n' >"$ROOT/.claude/skills/alpha/v1./f.md"
printf 'two\n' >"$ROOT/.agents/skills/alpha/v1./f.md"
# check.sh passes no root, so the check runs with the default root '.'.
cd "$ROOT"
run_check
cd - >/dev/null
assert_rc 1
assert_contains '.claude/skills/alpha/v1./f.md'

begin_case "a missing mirror tree fails for every skill"
rm -rf "$ROOT/.agents"
run_check "$ROOT"
assert_rc 1
assert_contains '.agents/skills/alpha is missing'
assert_contains '.agents/skills/beta is missing'

begin_case "a missing source tree is an environment error"
rm -rf "$ROOT/.claude"
run_check "$ROOT"
assert_rc 2
assert_contains 'not found'

begin_case "a source tree with no skill can't pass vacuously"
rm -rf "$ROOT/.claude/skills/alpha" "$ROOT/.claude/skills/beta" "$ROOT/.agents/skills"/*
run_check "$ROOT"
assert_rc 2
assert_contains 'holds no skill directory'

begin_case "a root that is not a directory is an environment error"
run_check "$ROOT/absent"
assert_rc 2
assert_contains 'is not a directory'

begin_case "extra arguments are a usage error"
run_check "$ROOT" extra
assert_rc 2
assert_contains 'usage:'

begin_case "CI runs the check whenever a skill tree changes"
set +e
OUT=$(python3 - "$SCRIPT_DIR/.." 2>&1 <<'PY'
import re
import sys
from pathlib import Path

root = Path(sys.argv[1])
check = (root / 'scripts/check.sh').read_text()
assert re.search(r"^    scripts\) echo '[^']*\bskill-mirrors\b", check, re.M)
assert 'scripts_skill_mirrors() { in_dir "$ROOT" bash scripts/check-skill-mirrors.sh; }' in check
ci = (root / '.github/workflows/scripts-ci.yml').read_text()
assert re.search(r'^        run: bash scripts/check\.sh scripts skill-mirrors$', ci, re.M)
assert re.search(r'^      - "\.claude/skills/\*\*"$', ci, re.M)
assert re.search(r'^      - "\.agents/skills/\*\*"$', ci, re.M)
caller = (root / '.github/workflows/pull-request.yml').read_text()
selector = re.search(r'select_component scripts ((?:[^\n]*\\\n)*[^\n]*)', caller)
assert selector, 'no scripts selector'
paths = selector.group(1).replace('\\\n', ' ').split()
assert '.claude/skills/' in paths and '.agents/skills/' in paths, paths
print('PASS: skill mirror check is wired into CI')
PY
)
RC=$?
set -e
assert_rc 0
assert_contains 'PASS: skill mirror check is wired into CI'

echo
echo "passed $pass assertion(s), failed $fail"
[ "$fail" -eq 0 ]
