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
#
# One removal is allowed: a migration squashed into a baseline. A removed
# daemon/migrations/NNNN_name.sql passes when the head's
# daemon/migrations/squashed_history.txt holds the line
# "<version> <file name> sha256:<hex>", where version is NNNN as a number and
# hex is the SHA-256 of the file's content at the merge base: the row the
# store recorded when it applied the file. The list is append-only: every
# line the merge base's list holds must appear unchanged in the head's list.
# daemon/README.md (Squashing Migrations) has the squash rule.
# This command reads the caller's repository without changing it or fetching
# history. Fetch complete history before running it in a shallow checkout.
#
# Exit codes:
#   0  no protected path disappeared except squashed ones
#   1  a protected path disappeared unlisted, or a list line was removed or
#      changed (each original path or line is named)
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

WORK=$(mktemp -d) || fail_git "cannot create temporary directory"
trap 'rm -rf "$WORK"' EXIT
DELETIONS=$WORK/deletions
# Materialize and check the diff before reading it. A failed Git command must
# never look like an empty successful result or a partial list of violations.
git diff --no-renames --diff-filter=D --name-only -z \
  "$MERGE_BASE" "$HEAD_SHA" -- daemon/migrations/ >"$DELETIONS" \
  || fail_git "cannot compare $MERGE_BASE with $HEAD_SHA"

LIST=daemon/migrations/squashed_history.txt

read_list() { # <commit> <output>; an absent list reads as empty
  local commit=$1 output=$2 entry
  entry=$(git ls-tree --name-only "$commit" -- "$LIST") \
    || fail_git "cannot list $LIST at $commit"
  if [ -z "$entry" ]; then
    : >"$output"
    return
  fi
  git cat-file blob "$commit:$LIST" >"$output" \
    || fail_git "cannot read $LIST at $commit"
}

# Choose the hash tool up front so a missing one is an environment error,
# not a digest that matches nothing.
if command -v sha256sum >/dev/null 2>&1; then
  SHA256=(sha256sum)
elif command -v shasum >/dev/null 2>&1; then
  SHA256=(shasum -a 256)
else
  fail_git "no sha256sum or shasum found"
fi

read_list "$MERGE_BASE" "$WORK/base-list"
read_list "$HEAD_SHA" "$WORK/head-list"

violations=0
while IFS= read -r line || [ -n "$line" ]; do
  if ! grep -Fxq -- "$line" "$WORK/head-list"; then
    printf '%s: removed or changed squashed-history line: %s\n' "$PROG" "$line" >&2
    violations=$((violations + 1))
  fi
done <"$WORK/base-list"

# squashed reports whether the head's list names a removed path with the
# digest of its content at the merge base.
squashed() { # <path>
  local path=$1 name hex
  name=${path#daemon/migrations/}
  [[ $name =~ ^([0-9]{4})_[a-z0-9_]+\.sql$ ]] || return 1
  git cat-file blob "$MERGE_BASE:$path" >"$WORK/blob" \
    || fail_git "cannot read $path at $MERGE_BASE"
  hex=$("${SHA256[@]}" <"$WORK/blob") || fail_git "cannot hash $path"
  hex=${hex%% *}
  grep -Fxq -- "$((10#${BASH_REMATCH[1]})) $name sha256:$hex" "$WORK/head-list"
}

while IFS= read -r -d '' path; do
  case $path in
    *.sql)
      if ! squashed "$path"; then
        printf '%s: removed merged migration: %s\n' "$PROG" "$path" >&2
        violations=$((violations + 1))
      fi
      ;;
  esac
done <"$DELETIONS"

if [ "$violations" -gt 0 ]; then
  exit 1
fi
echo "PASS: no merged migration paths removed outside the squashed history ($MERGE_BASE..$HEAD_SHA)"
