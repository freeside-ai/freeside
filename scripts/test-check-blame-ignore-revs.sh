#!/usr/bin/env bash
# Hermetic blame-ignore-revs and PR-workflow fixtures, with no network access.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
CHECK=$SCRIPT_DIR/check-blame-ignore-revs.sh
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.invalid
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.invalid
export GIT_AUTHOR_DATE='2026-01-01T00:00:00Z'
export GIT_COMMITTER_DATE='2026-01-01T00:00:00Z'
unset GIT_DIR GIT_WORK_TREE

pass=0
fail=0
OUT=''
RC=0
CASE=''

assert_rc() {
  if [ "$RC" -eq "$1" ]; then
    pass=$((pass + 1))
  else
    echo "FAIL [$CASE]: expected rc=$1, got rc=$RC: $OUT"
    fail=$((fail + 1))
  fi
  if [ "$RC" -ne 0 ] && [[ $OUT == *PASS:* ]]; then
    echo "FAIL [$CASE]: error reported success: $OUT"
    fail=$((fail + 1))
  fi
}

assert_contains() {
  if [[ $OUT == *"$1"* ]]; then
    pass=$((pass + 1))
  else
    echo "FAIL [$CASE]: missing '$1': $OUT"
    fail=$((fail + 1))
  fi
}

commit() {
  git -C "$REPO" add -A
  git -C "$REPO" commit -qm "$1"
}

new_repo() {
  CASE=$1
  REPO=$TMP/$CASE
  git init -q -b main "$REPO"
  printf '# Mechanical churn commits.\n' >"$REPO/.git-blame-ignore-revs"
  printf 'initial\n' >"$REPO/app.txt"
  commit Initial
}

run_check() { # <working-directory> [checker arguments...]
  local directory=$1
  shift
  RC=0
  OUT=$(cd "$directory" && bash "$CHECK" "$@" 2>&1) || RC=$?
}

# Valid: two mechanical commits listed, head on top.
new_repo valid-entries
mkdir -p "$REPO/sub"
printf 'move 1\n' >"$REPO/sub/one.txt"
commit "Mechanical move one"
SHA1=$(git -C "$REPO" rev-parse HEAD)
printf 'move 2\n' >"$REPO/sub/two.txt"
commit "Mechanical move two"
SHA2=$(git -C "$REPO" rev-parse HEAD)
cat >>"$REPO/.git-blame-ignore-revs" <<EOF

# Mechanical move one
$SHA1

# Mechanical move two
$SHA2
EOF
commit "Record mechanical commits"
# Dirty the working tree with an invalid entry to prove the checker reads
# <head-ref>, not the working tree.
printf '\nnot-a-sha\n' >>"$REPO/.git-blame-ignore-revs"
run_check "$REPO" HEAD
assert_rc 0
assert_contains "PASS:"
# Subdirectory invocation must still locate the repository root.
run_check "$REPO/sub" HEAD
assert_rc 0
assert_contains "PASS:"

# Rebased away: a branch lists its own mechanical commit, then rebases onto a
# moved base so the commit gets a new SHA while the old SHA stays in the file.
new_repo rebased-away
git -C "$REPO" checkout -qb feature
printf 'moved\n' >"$REPO/moved.txt"
commit "Move helper to new package"
OLD_SHA=$(git -C "$REPO" rev-parse HEAD)
cat >>"$REPO/.git-blame-ignore-revs" <<EOF

# Move helper to new package
$OLD_SHA
EOF
commit "List mechanical move"
run_check "$REPO" HEAD
assert_rc 0

git -C "$REPO" checkout -q main
printf 'upstream change\n' >"$REPO/upstream.txt"
commit "Advance main"
git -C "$REPO" checkout -q feature
git -C "$REPO" rebase -q main
run_check "$REPO" HEAD
assert_rc 1
assert_contains "$OLD_SHA"
assert_contains "# Move helper to new package"
assert_contains "git log --oneline origin/main..HEAD"

# Updating the entry to the rebased commit's SHA makes the check pass again.
NEW_SHA=$(git -C "$REPO" rev-parse HEAD~1)
cat >"$REPO/.git-blame-ignore-revs" <<EOF
# Mechanical churn commits.

# Move helper to new package
$NEW_SHA
EOF
commit "Update blame-ignore entry after rebase"
run_check "$REPO" HEAD
assert_rc 0
assert_contains "PASS:"

# Unknown SHA: 40 lowercase hex characters naming no object in the repository.
new_repo unknown-sha
MISSING_SHA=0000000000000000000000000000000000000000
cat >>"$REPO/.git-blame-ignore-revs" <<EOF

# Commit never existed here
$MISSING_SHA
EOF
commit "Add unknown SHA"
run_check "$REPO" HEAD
assert_rc 1
assert_contains "$MISSING_SHA"
assert_contains "# Commit never existed here"

# Non-commit object: 40-hex blob SHA is rejected.
new_repo non-commit-sha
BLOB_SHA=$(git -C "$REPO" rev-parse HEAD:app.txt)
cat >>"$REPO/.git-blame-ignore-revs" <<EOF

# Blob object, not a commit
$BLOB_SHA
EOF
commit "Add blob SHA"
run_check "$REPO" HEAD
assert_rc 1
assert_contains "$BLOB_SHA"
assert_contains "# Blob object, not a commit"

# Annotated tag object: peels to a commit, but git blame ignores tag SHAs.
new_repo annotated-tag-sha
printf 'tagged\n' >"$REPO/tagged.txt"
commit "Tagged mechanical move"
git -C "$REPO" tag -a v1 -m "Annotated tag on commit"
TAG_SHA=$(git -C "$REPO" rev-parse v1)
cat >>"$REPO/.git-blame-ignore-revs" <<EOF

# Annotated tag object, not a commit
$TAG_SHA
EOF
commit "Add annotated tag SHA"
run_check "$REPO" HEAD
assert_rc 1
assert_contains "$TAG_SHA"
assert_contains "object is not a commit (blame needs a commit SHA)"
assert_contains "# Annotated tag object, not a commit"

# Malformed lines: short SHA and uppercase SHA are both reported in one run.
new_repo malformed-lines
VALID_SHA=$(git -C "$REPO" rev-parse HEAD)
UPPER_SHA=$(printf '%s' "$VALID_SHA" | tr '[:lower:]' '[:upper:]')
SHORT_SHA=${VALID_SHA:0:12}
cat >>"$REPO/.git-blame-ignore-revs" <<EOF

# Short SHA entry
$SHORT_SHA

# Uppercase SHA entry
$UPPER_SHA
EOF
commit "Add malformed entries"
run_check "$REPO" HEAD
assert_rc 1
assert_contains "$SHORT_SHA"
assert_contains "# Short SHA entry"
assert_contains "$UPPER_SHA"
assert_contains "# Uppercase SHA entry"

# Shallow clone: ancestry cannot be trusted, so the checker exits 2.
new_repo shallow-clone
printf 'second\n' >"$REPO/second.txt"
commit "Second commit"
SECOND_SHA=$(git -C "$REPO" rev-parse HEAD)
cat >>"$REPO/.git-blame-ignore-revs" <<EOF

# Second commit
$SECOND_SHA
EOF
commit "List second commit"
HEAD_SHA=$(git -C "$REPO" rev-parse HEAD)
git clone -q --depth=1 "file://$REPO" "$TMP/shallow"
run_check "$TMP/shallow" HEAD
assert_rc 2
assert_contains "shallow"

# Execute the real workflow step from the shallow checkout against the local
# origin to prove --unshallow + HEAD_SHA restores ancestry and passes.
mkdir -p "$TMP/shallow/scripts"
cp "$CHECK" "$TMP/shallow/scripts/"
sed -n '/        run: |/,$p' "$SCRIPT_DIR/../.github/workflows/blame-ignore-revs.yml" \
  | sed '1d; s/^          //' >"$TMP/workflow-step.sh"
RC=0
OUT=$(cd "$TMP/shallow" && HEAD_SHA=$HEAD_SHA bash "$TMP/workflow-step.sh" 2>&1) || RC=$?
assert_rc 0
assert_contains "PASS:"

# Usage and environment errors exit 2.
CASE=usage-and-env-errors
run_check "$REPO"
assert_rc 2
run_check "$REPO" HEAD extra
assert_rc 2
run_check "$REPO" missing-ref
assert_rc 2
run_check "$REPO" --help
assert_rc 2
run_check "$REPO" "$(git -C "$REPO" rev-parse HEAD:app.txt)"
assert_rc 2
mkdir -p "$TMP/outside"
run_check "$TMP/outside" HEAD
assert_rc 2

# Ref without .git-blame-ignore-revs exits 2.
new_repo missing-file
git -C "$REPO" rm -q .git-blame-ignore-revs
commit "Remove blame ignore file"
run_check "$REPO" HEAD
assert_rc 2
assert_contains ".git-blame-ignore-revs"

# Verify workflow wiring: unconditional reusable workflow, required depends on it.
CASE=workflow-wiring
RC=0
OUT=$(python3 - "$SCRIPT_DIR/.." <<'PY' 2>&1
import re
import sys
from pathlib import Path

root = Path(sys.argv[1])
workflow = (root / '.github/workflows/blame-ignore-revs.yml').read_text()
assert '  workflow_call:' in workflow
assert '  contents: read' in workflow
assert 'group: blame-ignore-revs-${{' in workflow
assert 'HEAD_SHA: ${{ github.event.pull_request.head.sha }}' in workflow
assert 'bash scripts/check-blame-ignore-revs.sh "$HEAD_SHA"' in workflow
assert not re.search(r'^\s+if:', workflow, re.M)
caller = (root / '.github/workflows/pull-request.yml').read_text()
jobs = dict(re.findall(r'^  ([a-z-]+):\n(.*?)(?=^  [a-z-]+:|\Z)', caller, re.M | re.S))
job_lines = [line.strip() for line in jobs['blame-ignore-revs'].splitlines()
             if line.strip() and not line.lstrip().startswith('#')]
assert job_lines == ['uses: ./.github/workflows/blame-ignore-revs.yml'], job_lines
assert '      - blame-ignore-revs\n' in jobs['required']
assert '    if: always()\n' in jobs['required']
assert '"blame-ignore-revs"' in jobs['required'].split('keys ==')[1].split(']')[0]
assert '.["blame-ignore-revs"].result == "success"' in jobs['required']
print('PASS: blame-ignore-revs workflow is unconditional and required')
PY
) || RC=$?
assert_rc 0
assert_contains 'PASS: blame-ignore-revs workflow is unconditional and required'

echo "blame-ignore-revs fixtures: $pass assertions passed, $fail failed"
[ "$fail" -eq 0 ]
