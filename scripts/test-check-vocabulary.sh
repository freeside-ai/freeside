#!/usr/bin/env bash
# test-check-vocabulary.sh — fixtures for the retired-vocabulary check.
#
# Builds isolated synthetic repositories with fixed identity and no network.
# The retired stem is assembled at runtime so this file passes the real
# check too. Exit code: 0 when every assertion passes, 1 otherwise.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
CHECK=$SCRIPT_DIR/check-vocabulary.sh

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.invalid
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.invalid
unset GIT_DIR GIT_WORK_TREE

stem="elab""orat"
word="${stem}ion"

pass=0
fail=0
case_number=0
CASE=''
OUT=''
RC=0

begin_case() {
  case_number=$((case_number + 1))
  CASE=$(printf '%02d %s' "$case_number" "$1")
  echo "case: $CASE"
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

# new_repo <name> creates a repository with one committed clean file.
new_repo() {
  local dir=$TMP/$1
  mkdir -p "$dir"
  git -C "$dir" init -q
  mkdir -p "$dir/daemon/internal/domain"
  printf 'package domain\n' > "$dir/daemon/internal/domain/doc.go"
  git -C "$dir" add -A
  git -C "$dir" commit -q -m "Seed"
  echo "$dir"
}

# add_file <repo> <path> <content> writes and stages a tracked file.
add_file() {
  mkdir -p "$(dirname "$1/$2")"
  printf '%s\n' "$3" > "$1/$2"
  git -C "$1" add -- "$2"
}

run_check() {
  set +e
  OUT=$(bash "$CHECK" "$@" 2>&1)
  RC=$?
  set -e
}

begin_case "clean repository passes"
repo=$(new_repo clean)
run_check "$repo"
assert_rc 0
assert_contains "no retired vocabulary in scope"

begin_case "usage error on extra arguments"
run_check "$repo" extra
assert_rc 2

begin_case "a directory outside git is an environment error"
mkdir -p "$TMP/plain"
run_check "$TMP/plain"
assert_rc 2

begin_case "non-test daemon Go code fails and names the line"
repo=$(new_repo daemon-go)
add_file "$repo" daemon/internal/engine/stage.go "package engine // the $word stage"
run_check "$repo"
assert_rc 1
assert_contains "daemon/internal/engine/stage.go:1:"
assert_contains "retired vocabulary in 1 tracked file(s)"

begin_case "the match is case-insensitive"
repo=$(new_repo daemon-case)
add_file "$repo" daemon/internal/engine/stage.go "package engine // ${stem^^}ION"
run_check "$repo"
assert_rc 1

begin_case "daemon test files, legacy_vocabulary.go, and non-Go files are out of scope"
repo=$(new_repo daemon-exempt)
add_file "$repo" daemon/internal/engine/stage_test.go "package engine // $word"
add_file "$repo" daemon/internal/engine/legacy_vocabulary.go "package engine // $word"
add_file "$repo" daemon/internal/store/testdata/fixture.sql "INSERT INTO runs VALUES('run-$word-1');"
add_file "$repo" daemon/README.md "The $word stage."
run_check "$repo"
assert_rc 0

begin_case "a legacy_vocabulary.go must sit directly in its package"
repo=$(new_repo daemon-nested)
add_file "$repo" daemon/internal/engine/legacy_vocabulary.go "package engine // $word"
add_file "$repo" daemon/internal/engine/other_legacy_vocabulary.go "package engine // $word"
run_check "$repo"
assert_rc 1
assert_contains "other_legacy_vocabulary.go:1:"
assert_not_contains "engine/legacy_vocabulary.go:1:"

begin_case "migrations up to 0064 are exempt, later ones are checked"
repo=$(new_repo migrations)
add_file "$repo" daemon/migrations/0048_production_attempts.sql "ALTER TABLE t ADD ${word}_run_id TEXT;"
add_file "$repo" daemon/migrations/0064_specification_vocabulary.sql "ALTER TABLE t RENAME COLUMN ${word}_run_id TO specification_run_id;"
run_check "$repo"
assert_rc 0
add_file "$repo" daemon/migrations/0065_next.sql "-- mentions the $word column"
run_check "$repo"
assert_rc 1
assert_contains "0065_next.sql:1:"

begin_case "api, app, prompts, and scripts are checked in full, including tests"
for path in api/openapi.yaml app/Tests/Example.swift prompts/phase-1a/x.md scripts/example.sh; do
  repo=$(new_repo "surface-$(echo "$path" | tr '/.' '__')")
  add_file "$repo" "$path" "$word"
  run_check "$repo"
  assert_rc 1
  assert_contains "$path:1:"
done

begin_case "devlog, docs, and untracked files are out of scope"
repo=$(new_repo frozen)
add_file "$repo" devlog/2026-01-01-0000-note.md "$word"
add_file "$repo" docs/history/old.md "$word"
add_file "$repo" docs/plan.md "request further $word"
mkdir -p "$repo/scripts"
printf '%s\n' "$word" > "$repo/scripts/untracked.sh"
run_check "$repo"
assert_rc 0

begin_case "defaults to the current directory"
repo=$(new_repo cwd)
add_file "$repo" scripts/x.sh "$word"
set +e
OUT=$(cd "$repo" && bash "$CHECK" 2>&1)
RC=$?
set -e
assert_rc 1

echo
echo "passed $pass assertion(s), failed $fail"
[ "$fail" -eq 0 ]
