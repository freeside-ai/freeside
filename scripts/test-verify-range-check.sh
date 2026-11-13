#!/usr/bin/env bash
# Hermetic fixtures for verify-range-check.sh, with no network access. The
# range cases run both real check scripts through the wrapper the way
# Freeside's recipe does: from the repository root, by relative path, with
# the range in FREESIDE_VERIFY_BASE_SHA and FREESIDE_VERIFY_HEAD_SHA.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.invalid
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.invalid
export GIT_AUTHOR_DATE='2026-01-01T00:00:00Z'
export GIT_COMMITTER_DATE='2026-01-01T00:00:00Z'
unset GIT_DIR GIT_WORK_TREE FREESIDE_VERIFY_BASE_SHA FREESIDE_VERIFY_HEAD_SHA

WRAPPER=scripts/verify-range-check.sh
COMMIT_CHECK=scripts/check-commit-messages.sh
MIGRATION_CHECK=scripts/check-migration-history.sh

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
}

assert_contains() {
  if [[ $OUT == *"$1"* ]]; then
    pass=$((pass + 1))
  else
    echo "FAIL [$CASE]: missing '$1': $OUT"
    fail=$((fail + 1))
  fi
}

commit() { # <subject> [body]
  git -C "$REPO" add -A
  if [ "$#" -eq 2 ]; then
    git -C "$REPO" commit -qm "$1" -m "$2"
  else
    git -C "$REPO" commit -qm "$1"
  fi
}

# A repository whose base commit holds a merged migration and the scripts
# under test, with a feature branch checked out at the base.
new_repo() {
  CASE=$1
  REPO=$TMP/$CASE
  git init -q -b main "$REPO"
  mkdir -p "$REPO/daemon/migrations" "$REPO/scripts"
  printf 'SELECT 1;\n' >"$REPO/daemon/migrations/0001_init.sql"
  cp "$SCRIPT_DIR/verify-range-check.sh" "$SCRIPT_DIR/check-commit-messages.sh" \
    "$SCRIPT_DIR/check-migration-history.sh" "$REPO/scripts/"
  commit 'Add the first migration' 'The base the candidate branches from.'
  BASE=$(git -C "$REPO" rev-parse HEAD)
  git -C "$REPO" checkout -qb feature
}

run_wrapper() { # <base-sha|-> <head-sha|-> [wrapper arguments...]; - unsets
  local base=$1 head=$2
  shift 2
  local env_args=(env -u FREESIDE_VERIFY_BASE_SHA -u FREESIDE_VERIFY_HEAD_SHA)
  [ "$base" = - ] || env_args+=("FREESIDE_VERIFY_BASE_SHA=$base")
  [ "$head" = - ] || env_args+=("FREESIDE_VERIFY_HEAD_SHA=$head")
  RC=0
  OUT=$(cd "$REPO" && "${env_args[@]}" bash "$WRAPPER" "$@" 2>&1) || RC=$?
}

# Usage errors: exit 2 before running anything.
new_repo usage
HEAD_SHA=$(git -C "$REPO" rev-parse HEAD)
expect_usage() { # <case> <run_wrapper arguments...>
  CASE=usage-$1
  shift
  run_wrapper "$@"
  assert_rc 2
  assert_contains 'usage: '
}
expect_usage no-argument "$BASE" "$HEAD_SHA"
expect_usage empty-argument "$BASE" "$HEAD_SHA" ''
expect_usage two-arguments "$BASE" "$HEAD_SHA" "$COMMIT_CHECK" extra
expect_usage base-unset - "$HEAD_SHA" "$COMMIT_CHECK"
expect_usage base-empty '' "$HEAD_SHA" "$COMMIT_CHECK"
expect_usage head-unset "$BASE" - "$COMMIT_CHECK"
expect_usage head-empty "$BASE" '' "$COMMIT_CHECK"

# Pass-through: the named script gets base then head and its exit code.
CASE='pass-through'
cat >"$REPO/stub.sh" <<'STUB'
echo "args=$1,$2"
exit 7
STUB
run_wrapper "$BASE" "$HEAD_SHA" stub.sh
assert_rc 7
assert_contains "args=$BASE,$HEAD_SHA"

# A good range passes both checks.
new_repo good-range
printf 'SELECT 2;\n' >"$REPO/daemon/migrations/0002_next.sql"
commit 'Add the second migration' 'Grows the schema without touching history.'
HEAD_SHA=$(git -C "$REPO" rev-parse HEAD)
CASE=good-range-commit-messages
run_wrapper "$BASE" "$HEAD_SHA" "$COMMIT_CHECK"
assert_rc 0
CASE=good-range-migration-history
run_wrapper "$BASE" "$HEAD_SHA" "$MIGRATION_CHECK"
assert_rc 0

# A refused commit message fails the commit-message check.
new_repo bad-message
printf 'SELECT 2;\n' >"$REPO/daemon/migrations/0002_next.sql"
commit wip
HEAD_SHA=$(git -C "$REPO" rev-parse HEAD)
CASE=bad-message
run_wrapper "$BASE" "$HEAD_SHA" "$COMMIT_CHECK"
assert_rc 1
assert_contains wip-marker

# A deleted migration fails the migration-history check.
new_repo deleted-migration
git -C "$REPO" rm -q daemon/migrations/0001_init.sql
commit 'Drop the first migration' 'Removes a merged migration, which CI refuses.'
HEAD_SHA=$(git -C "$REPO" rev-parse HEAD)
CASE=deleted-migration
run_wrapper "$BASE" "$HEAD_SHA" "$MIGRATION_CHECK"
assert_rc 1
assert_contains daemon/migrations/0001_init.sql

echo "verify-range-check fixtures: $pass assertions passed, $fail failed"
[ "$fail" -eq 0 ]
