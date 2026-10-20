#!/usr/bin/env bash
# check-migration-history.sh: reject removal of merged migration paths.
#
# Usage: bash scripts/check-migration-history.sh <base-ref> <head-ref>
# Local verification:
#   bash scripts/check-migration-history.sh origin/main HEAD
#   bash scripts/test-check-migration-history.sh
#
# Compare the refs' merge base to the explicit head, not the checkout's HEAD.
# Every base-side *.sql path under daemon/migrations/ must remain. Disabling
# rename detection makes moves and extension changes count as deletions.
# Only the final trees matter: content edits and branch-only files are outside
# this check. Target-branch additions after the branch point are not deletions.
# This command reads the caller's repository without changing it or fetching
# history. Fetch complete history before running it in a shallow checkout.
#
# Exit codes:
#   0  no protected path disappeared
#   1  one or more protected paths disappeared (each original path is named)
#   2  usage/environment error or Git could not check the requested history
set -euo pipefail

# Missing objects in a partial clone must fail instead of fetching implicitly.
export GIT_NO_LAZY_FETCH=1

PROG=check-migration-history

fail_git() {
  echo "$PROG: $*" >&2
  exit 2
}

if [ "$#" -ne 2 ]; then
  echo "usage: $PROG <base-ref> <head-ref>" >&2
  exit 2
fi

ROOT=$(git rev-parse --show-toplevel) || fail_git "not inside a git worktree"
cd "$ROOT" || fail_git "cannot enter repository root"

resolve_commit() { # <ref> <role>; prints the resolved commit SHA
  local ref=$1 role=$2 sha
  sha=$(git rev-parse --verify --quiet --end-of-options "${ref}^{commit}") \
    || fail_git "cannot resolve $role ref '$ref' to a commit"
  printf '%s' "$sha"
}

BASE_SHA=$(resolve_commit "$1" base)
HEAD_SHA=$(resolve_commit "$2" head)
MERGE_BASE=$(git merge-base "$BASE_SHA" "$HEAD_SHA") \
  || fail_git "no merge base between $BASE_SHA and $HEAD_SHA; fetch complete history"

DELETIONS=$(mktemp) || fail_git "cannot create temporary diff output"
trap 'rm -f "$DELETIONS"' EXIT
# Materialize and check the diff before reading it. A failed Git command must
# never look like an empty successful result or a partial list of violations.
git diff --no-renames --diff-filter=D --name-only -z \
  "$MERGE_BASE" "$HEAD_SHA" -- daemon/migrations/ >"$DELETIONS" \
  || fail_git "cannot compare $MERGE_BASE with $HEAD_SHA"

violations=0
while IFS= read -r -d '' path; do
  case $path in
    *.sql)
      printf '%s: removed merged migration: %s\n' "$PROG" "$path" >&2
      violations=$((violations + 1))
      ;;
  esac
done <"$DELETIONS"

if [ "$violations" -gt 0 ]; then
  exit 1
fi
echo "PASS: no merged migration paths removed ($MERGE_BASE..$HEAD_SHA)"
