#!/usr/bin/env bash
# Hermetic migration-history and PR-workflow fixtures, with no network access.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
CHECK=$SCRIPT_DIR/check-migration-history.sh
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
  mkdir -p "$REPO/daemon/migrations"
  printf 'SELECT 1;\n' >"$REPO/daemon/migrations/0001_first.sql"
  printf 'SELECT 2;\n' >"$REPO/daemon/migrations/0002_head.sql"
  printf 'Notes\n' >"$REPO/daemon/migrations/README.md"
  printf 'Unrelated\n' >"$REPO/unrelated.sql"
  commit Base
  BASE=$(git -C "$REPO" rev-parse HEAD)
  git -C "$REPO" checkout -qb feature
}

run_check() { # <working-directory> [checker arguments...]
  local directory=$1
  shift
  RC=0
  OUT=$(cd "$directory" && bash "$CHECK" "$@" 2>&1) || RC=$?
}

for name in 0002_head.sql 0001_first.sql; do
  new_repo "delete-$name"
  git -C "$REPO" rm -q "daemon/migrations/$name"
  commit Delete
  run_check "$REPO" "$BASE" HEAD
  assert_rc 1
  assert_contains "daemon/migrations/$name"
done

for renames in true false copies; do
  for destination in daemon/migrations/0003_renamed.sql moved.sql daemon/migrations/0002_head.txt; do
    new_repo "rename-$renames-${destination//\//-}"
    git -C "$REPO" config diff.renames "$renames"
    git -C "$REPO" mv daemon/migrations/0002_head.sql "$destination"
    commit Rename
    run_check "$REPO" "$BASE" HEAD
    assert_rc 1
    assert_contains daemon/migrations/0002_head.sql
  done
done

new_repo rewritten-rename
git -C "$REPO" mv daemon/migrations/0002_head.sql daemon/migrations/0003_rewritten.sql
printf 'Entirely different content\n' >"$REPO/daemon/migrations/0003_rewritten.sql"
commit Rewrite
run_check "$REPO" "$BASE" HEAD
assert_rc 1
assert_contains daemon/migrations/0002_head.sql

new_repo allowed-final-changes
printf 'SELECT 3;\n' >"$REPO/daemon/migrations/0003_added.sql"
printf 'Changed content\n' >>"$REPO/daemon/migrations/0001_first.sql"
git -C "$REPO" rm -q unrelated.sql daemon/migrations/README.md
commit Add
run_check "$REPO" "$BASE" HEAD
assert_rc 0
git -C "$REPO" mv daemon/migrations/0003_added.sql daemon/migrations/0004_branch_only.sql
commit Rename
run_check "$REPO" "$BASE" HEAD
assert_rc 0
git -C "$REPO" rm -q daemon/migrations/0004_branch_only.sql
commit Remove
run_check "$REPO" "$BASE" HEAD
assert_rc 0
git -C "$REPO" rm -q daemon/migrations/0002_head.sql
commit Temporary-deletion
git -C "$REPO" checkout "$BASE" -- daemon/migrations/0002_head.sql
commit Restore
run_check "$REPO" "$BASE" HEAD
assert_rc 0

new_repo unusual-paths
for name in 'space name.sql' $'tab\tname.sql' $'line\nname.sql'; do
  printf 'SELECT 3;\n' >"$REPO/daemon/migrations/$name"
done
commit Paths
BASE=$(git -C "$REPO" rev-parse HEAD)
git -C "$REPO" rm -q 'daemon/migrations/*.sql'
commit Delete
# A subdirectory invocation must still use root-relative migration paths.
run_check "$REPO/daemon/migrations" "$BASE" HEAD
assert_rc 1
for name in 0001_first.sql 0002_head.sql 'space name.sql' $'tab\tname.sql' $'line\nname.sql'; do
  assert_contains "daemon/migrations/$name"
done

new_repo advanced-target
printf 'Branch\n' >"$REPO/branch.txt"
commit Branch
HEAD_SHA=$(git -C "$REPO" rev-parse HEAD)
git -C "$REPO" checkout -q main
printf 'SELECT 3;\n' >"$REPO/daemon/migrations/0003_target_only.sql"
commit Advance
TARGET_SHA=$(git -C "$REPO" rev-parse HEAD)
run_check "$REPO" "$TARGET_SHA" "$HEAD_SHA"
assert_rc 0
git -C "$REPO" checkout -q feature
git -C "$REPO" rm -q daemon/migrations/0002_head.sql
commit Delete
HEAD_SHA=$(git -C "$REPO" rev-parse HEAD)
git -C "$REPO" checkout -q main
git -C "$REPO" merge -q --no-ff --no-commit feature
git -C "$REPO" checkout "$BASE" -- daemon/migrations/0002_head.sql
commit Synthetic-merge-restores-path
run_check "$REPO" "$TARGET_SHA" HEAD
assert_rc 0
run_check "$REPO" "$TARGET_SHA" "$HEAD_SHA"
assert_rc 1
assert_contains daemon/migrations/0002_head.sql

CASE=invalid-inputs
run_check "$REPO"
assert_rc 2
run_check "$REPO" "$BASE" HEAD extra
assert_rc 2
run_check "$REPO" missing HEAD
assert_rc 2
run_check "$REPO" "$BASE" missing
assert_rc 2
run_check "$REPO" --help HEAD
assert_rc 2
run_check "$REPO" "$BASE" "$(git -C "$REPO" rev-parse HEAD:unrelated.sql)"
assert_rc 2
mkdir "$TMP/outside"
run_check "$TMP/outside" main HEAD
assert_rc 2

git -C "$REPO" checkout -q --orphan disconnected
git -C "$REPO" commit -qm Disconnected
CASE=absent-merge-base
run_check "$REPO" "$BASE" HEAD
assert_rc 2
assert_contains 'no merge base'

new_repo shallow-history
printf 'Branch\n' >"$REPO/branch.txt"
commit Branch
HEAD_SHA=$(git -C "$REPO" rev-parse HEAD)
git -C "$REPO" checkout -q main
printf 'Target\n' >"$REPO/target.txt"
commit Target
BASE_SHA=$(git -C "$REPO" rev-parse HEAD)
git clone -q --depth=1 "file://$REPO" "$TMP/shallow"
git -C "$TMP/shallow" fetch -q --depth=1 origin "$HEAD_SHA"
run_check "$TMP/shallow" "$BASE_SHA" "$HEAD_SHA"
assert_rc 2
assert_contains 'no merge base'

# Execute the real workflow step from a shallow checkout, using only a local
# origin. This proves the fetch supplies ancestry and the step uses both SHAs.
mkdir "$TMP/shallow/scripts"
cp "$CHECK" "$TMP/shallow/scripts/"
sed -n '/        run: |/,$p' "$SCRIPT_DIR/../.github/workflows/migration-history.yml" \
  | sed '1d; s/^          //' >"$TMP/workflow-step.sh"
RC=0
OUT=$(cd "$TMP/shallow" && BASE_SHA=$BASE_SHA HEAD_SHA=$HEAD_SHA \
  bash "$TMP/workflow-step.sh" 2>&1) || RC=$?
assert_rc 0
assert_contains PASS:

new_repo git-diff-failure
mkdir "$TMP/bin"
REAL_GIT=$(command -v git)
export REAL_GIT
cat >"$TMP/bin/git" <<'EOF'
#!/usr/bin/env bash
if [ "$1" = diff ]; then
  printf 'daemon/migrations/0002_head.sql\0'
  echo 'injected diff failure' >&2
  exit 42
fi
exec "$REAL_GIT" "$@"
EOF
chmod +x "$TMP/bin/git"
PATH="$TMP/bin:$PATH" run_check "$REPO" "$BASE" HEAD
assert_rc 2
assert_contains 'cannot compare'

python3 - "$SCRIPT_DIR/.." <<'PY'
import re
import sys
from pathlib import Path

root = Path(sys.argv[1])
workflow = (root / '.github/workflows/migration-history.yml').read_text()
assert '  workflow_call:' in workflow
assert '  contents: read' in workflow
assert 'group: migration-history-${{' in workflow
assert 'BASE_SHA: ${{ github.event.pull_request.base.sha }}' in workflow
assert 'HEAD_SHA: ${{ github.event.pull_request.head.sha }}' in workflow
assert 'bash scripts/check-migration-history.sh "$BASE_SHA" "$HEAD_SHA"' in workflow
assert not re.search(r'^\s+if:', workflow, re.M)
caller = (root / '.github/workflows/pull-request.yml').read_text()
jobs = dict(re.findall(r'^  ([a-z-]+):\n(.*?)(?=^  [a-z-]+:|\Z)', caller, re.M | re.S))
job_lines = [line.strip() for line in jobs['migration-history'].splitlines()
             if line.strip() and not line.lstrip().startswith('#')]
assert job_lines == ['uses: ./.github/workflows/migration-history.yml']
assert '      - migration-history\n' in jobs['required']
assert '    if: always()\n' in jobs['required']
print('PASS: migration-history workflow is unconditional and required')
PY

echo "migration-history fixtures: $pass assertions passed, $fail failed"
[ "$fail" -eq 0 ]
