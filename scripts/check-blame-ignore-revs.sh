#!/usr/bin/env bash
# check-blame-ignore-revs.sh: reject stale or malformed .git-blame-ignore-revs entries.
#
# Usage: bash scripts/check-blame-ignore-revs.sh <head-ref>
# Local verification:
#   bash scripts/check-blame-ignore-revs.sh HEAD
#   bash scripts/test-check-blame-ignore-revs.sh
#
# Rebasing a pull request onto a moved main gives its mechanical commits new
# SHAs, so entries added to .git-blame-ignore-revs before the rebase go stale
# and git blame stops skipping them. Every non-blank, non-comment line in
# .git-blame-ignore-revs at <head-ref> must be a full 40-character lowercase
# hex SHA naming a commit that is an ancestor of <head-ref> (or is <head-ref>).
#
# How to fix a failure:
#   Find each rebased commit's new SHA with:
#     git log --oneline origin/main..HEAD
#   Replace the named stale entry in .git-blame-ignore-revs and fold the
#   update into the commit that added the entry.
#
# This command reads .git-blame-ignore-revs from <head-ref> in Git, not from
# the working tree, without changing the repository or fetching history. Fetch
# complete history before running it in a shallow checkout.
#
# Exit codes:
#   0  every entry is a full 40-hex commit SHA on <head-ref>'s history
#   1  one or more entries are missing, not commits, not ancestors of
#      <head-ref>, or malformed (each named with its line number and comment)
#   2  usage/environment error, shallow repository, or Git could not check
#      the requested history
set -euo pipefail

# Missing objects in a partial clone must fail instead of fetching implicitly.
export GIT_NO_LAZY_FETCH=1

PROG=check-blame-ignore-revs
FILE=.git-blame-ignore-revs

fail_git() {
  echo "$PROG: $*" >&2
  exit 2
}

if [ "$#" -ne 1 ]; then
  echo "usage: $PROG <head-ref>" >&2
  exit 2
fi

ROOT=$(git rev-parse --show-toplevel) || fail_git "not inside a git worktree"
cd "$ROOT" || fail_git "cannot enter repository root"

shallow=$(git rev-parse --is-shallow-repository) \
  || fail_git "cannot determine whether repository is shallow"
if [ "$shallow" = true ]; then
  fail_git "repository is shallow; fetch complete history before checking ancestry"
fi

HEAD_SHA=$(git rev-parse --verify --quiet --end-of-options "${1}^{commit}") \
  || fail_git "cannot resolve head ref '$1' to a commit"

git cat-file -e "$HEAD_SHA:$FILE" 2>/dev/null \
  || fail_git "$FILE does not exist at $HEAD_SHA"

# Read the blob before parsing so a Git read error cannot look like an empty
# file.
ENTRIES=$(git show "$HEAD_SHA:$FILE") \
  || fail_git "cannot read $FILE at $HEAD_SHA"

report_violation() { # <lineno> <reason> <entry> <comment>
  local lineno=$1 reason=$2 entry=$3 comment=$4
  if [ -n "$comment" ]; then
    printf '%s: %s:%d: %s: %s (%s)\n' "$PROG" "$FILE" "$lineno" "$reason" "$entry" "$comment" >&2
  else
    printf '%s: %s:%d: %s: %s\n' "$PROG" "$FILE" "$lineno" "$reason" "$entry" >&2
  fi
}

violations=0
checked=0
lineno=0
comment=''

while IFS= read -r line || [ -n "$line" ]; do
  lineno=$((lineno + 1))
  if [[ $line =~ ^[[:space:]]*$ ]]; then
    comment=''
    continue
  fi
  if [[ $line == \#* ]]; then
    comment=$line
    continue
  fi
  # Git's blame.ignoreRevsFile and GitHub blame require full 40-hex SHAs.
  if [[ ! $line =~ ^[0123456789abcdef]{40}$ ]]; then
    report_violation "$lineno" "malformed entry (expected 40 lowercase hex SHA)" "$line" "$comment"
    violations=$((violations + 1))
    continue
  fi
  checked=$((checked + 1))
  # Check the object's own type rather than peeling with ^{commit}, because
  # git blame matches commit object IDs and ignores annotated tag SHAs.
  obj_info=$(git cat-file --batch-check='%(objecttype)' <<<"$line" 2>&1) \
    || fail_git "cannot inspect object $line: $obj_info"
  case "$obj_info" in
    commit) ;;
    blob | tree | tag)
      report_violation "$lineno" "object is not a commit (blame needs a commit SHA)" "$line" "$comment"
      violations=$((violations + 1))
      continue
      ;;
    "$line missing")
      report_violation "$lineno" "commit not found in repository" "$line" "$comment"
      violations=$((violations + 1))
      continue
      ;;
    *)
      fail_git "cannot inspect object $line: $obj_info"
      ;;
  esac
  rc=0
  git merge-base --is-ancestor "$line" "$HEAD_SHA" 2>/dev/null || rc=$?
  if [ "$rc" -eq 0 ]; then
    continue
  elif [ "$rc" -eq 1 ]; then
    report_violation "$lineno" "commit is not an ancestor of $HEAD_SHA" "$line" "$comment"
    violations=$((violations + 1))
  else
    fail_git "cannot check ancestry of $line against $HEAD_SHA"
  fi
done <<<"$ENTRIES"

if [ "$violations" -gt 0 ]; then
  printf '%s: replace each stale entry with its rebased commit SHA (git log --oneline origin/main..HEAD) and fold it into the commit that added the entry\n' "$PROG" >&2
  exit 1
fi

echo "PASS: all $FILE entries ($checked) are ancestors of $HEAD_SHA"
