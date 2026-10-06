#!/usr/bin/env bash
# check-commit-messages.sh — mechanical commit-message policy check.
#
# Usage: check-commit-messages.sh <base-ref> <head-ref>
#        check-commit-messages.sh --message-file <path>
#
# Range mode resolves both refs, finds their merge base, and checks every
# non-merge commit in <merge-base>..<head-ref>. It reads no merge commit's
# message. A merge that brings the base into the branch is a violation of
# its own (base-merge, below), because the project refreshes a pull-request
# branch by rebasing it (AGENTS.md, Integration Ordering and Merge-Result
# Audit). The merge base keeps the mainline commits such a merge brought in
# out of the checked set, so the report names the merge and not them. The
# script performs no network I/O and does not mutate the repository.
#
# Message-file mode gives .githooks/commit-msg a best-effort early check for
# the normal editor path: fixed/default core.commentChar and strip cleanup.
# It cuts the verbose-template scissors line, then uses Git's
# `stripspace --strip-comments` cleanup. The hook receives neither the later
# cleanup mode nor the effective character selected by core.commentChar=auto,
# so range mode over recorded commits is the exact, authoritative check.
# Message-file mode needs git on PATH but no repository.
#
# Local examples:
#   bash scripts/check-commit-messages.sh origin/main HEAD
#   git config core.hooksPath .githooks   # once per clone; enables the hook
#
# Rules, each reported by name:
#   subject-required; subject-length (at most 72 characters);
#   subject-period (no trailing period); sentence-case (no leading
#   lowercase ASCII letter; acronym-, identifier-, and digit-led subjects
#   pass); conventional-prefix (build, chore, ci, docs, feat, fix, perf,
#   refactor, revert, style, test, including scoped and breaking forms
#   such as feat(api): and refactor!:); autosquash-prefix (fixup!,
#   squash!); wip-marker (a standalone WIP); review-cleanup (Address
#   review, Address PR review, Address pull request review, Apply review
#   feedback with optional PR or pull request before review, PR feedback,
#   Pull request feedback); blank-separator (line 2 blank); body-required
#   (at least one non-blank body line); body-line-length (at most 72
#   characters, except a line with no whitespace, so an unbreakable URL,
#   object ID, or ref stays intact). Prefix and marker matches are
#   case-insensitive.
#
# One rule reads history, not a message: base-merge (a merge commit in the
# range with a parent the base ref already contains). A merge of two lines
# of work the base does not contain stays exempt, so the rule misses a base
# brought in through a side branch that carries commits of its own. When
# <base-ref> is older than the tip the branch merged, the range also holds
# the mainline history that merge brought in, and the commit reported is
# the first mainline merge commit in it, not the branch's own merge. A
# mainline that advanced without a merge commit is missed in that case.
#
# Exit codes:
#   0  every checked commit satisfies the mechanical policy
#   1  one or more commit messages violate the policy
#   2  usage/environment error or git could not check the requested range
set -euo pipefail

PROG=check-commit-messages

usage() {
  echo "usage: $PROG <base-ref> <head-ref> | $PROG --message-file <path>" >&2
}

fail_git() {
  echo "$PROG: $*" >&2
  exit 2
}

find_utf8_locale() {
  local candidate charmap
  for candidate in C.UTF-8 C.utf8 en_US.UTF-8 en_US.utf8; do
    if charmap=$(LC_ALL=$candidate locale charmap 2>/dev/null) \
      && [ "$charmap" = UTF-8 ]; then
      printf '%s' "$candidate"
      return 0
    fi
  done
  return 1
}

character_length() { # <value>; prints its locale-independent character count
  local value=$1
  (
    export LC_ALL=$UTF8_LOCALE
    printf '%d' "${#value}"
  )
}

MODE='range'
if [ "$#" -eq 2 ] && [ "$1" = --message-file ]; then
  MODE='file'
elif [ "$#" -ne 2 ]; then
  usage
  exit 2
fi

UTF8_LOCALE=$(find_utf8_locale) \
  || fail_git "no supported UTF-8 locale for character-length checks"

violations=0
checked=0

report_violation() { # <label> <subject> <rule> <detail>
  local label=$1 subject=$2 rule=$3 detail=$4
  printf '%s "%s": [%s] %s\n' "$label" "$subject" "$rule" "$detail" >&2
  violations=$((violations + 1))
}

CONVENTIONAL_RE='^(build|chore|ci|docs|feat|fix|perf|refactor|revert|style|test)(\([^)]*\))?!?:'
WIP_RE='(^|[^[:alnum:]_])wip([^[:alnum:]_]|$)'
ADDRESS_RE='^address ((pr|pull request) )?review([[:space:]]|$)'
APPLY_RE='^apply ((pr|pull request) )?review feedback([[:space:]]|$)'
PR_FEEDBACK_RE='^(pr|pull request) feedback([[:space:]]|$)'
# git's own cut_line: a scissors line is the comment character, a space,
# and this string (builtin/commit.c, wt_status_locate_end).
SCISSORS='------------------------ >8 ------------------------'

check_message() { # <label> <message-file>; the file holds the bare message
  local label=$1 file=$2 lines line subject subject_length
  local body_present i body_line body_line_length
  checked=$((checked + 1))

  lines=()
  while IFS= read -r line || [ -n "$line" ]; do
    lines+=("$line")
  done <"$file"

  shopt -s nocasematch
  subject=${lines[0]-}
  subject_length=$(character_length "$subject")
  if [ -z "$subject" ]; then
    report_violation "$label" "$subject" subject-required \
      "subject must be present"
  fi
  if [ "$subject_length" -gt 72 ]; then
    report_violation "$label" "$subject" subject-length \
      "subject is $subject_length characters; maximum is 72"
  fi
  if [[ $subject == *. ]]; then
    report_violation "$label" "$subject" subject-period \
      "subject must not end with a period"
  fi
  shopt -u nocasematch
  if [[ $subject =~ ^[a-z] ]]; then
    report_violation "$label" "$subject" sentence-case \
      "subject must not start with a lowercase ASCII letter"
  fi
  shopt -s nocasematch
  if [[ $subject =~ $CONVENTIONAL_RE ]]; then
    report_violation "$label" "$subject" conventional-prefix \
      "Conventional Commit prefixes are not allowed"
  fi
  if [[ $subject =~ ^(fixup!|squash!) ]]; then
    report_violation "$label" "$subject" autosquash-prefix \
      "fixup! and squash! subjects are not allowed"
  fi
  if [[ $subject =~ $WIP_RE ]]; then
    report_violation "$label" "$subject" wip-marker \
      "standalone WIP markers are not allowed"
  fi
  if [[ $subject =~ $ADDRESS_RE ]] \
    || [[ $subject =~ $APPLY_RE ]] \
    || [[ $subject =~ $PR_FEEDBACK_RE ]]; then
    report_violation "$label" "$subject" review-cleanup \
      "review-cleanup subjects must be folded into the owning commit"
  fi
  shopt -u nocasematch

  if [ "${#lines[@]}" -lt 2 ] || [ -n "${lines[1]-}" ]; then
    report_violation "$label" "$subject" blank-separator \
      "line 2 must be blank"
  fi

  body_present=0
  i=2
  while [ "$i" -lt "${#lines[@]}" ]; do
    body_line=${lines[$i]}
    body_line_length=$(character_length "$body_line")
    if [[ $body_line =~ [^[:space:]] ]]; then
      body_present=1
    fi
    if [ "$body_line_length" -gt 72 ] \
      && [[ $body_line =~ [[:space:]] ]]; then
      report_violation "$label" "$subject" body-line-length \
        "body line $((i + 1)) is $body_line_length characters; maximum is 72"
    fi
    i=$((i + 1))
  done
  if [ "$body_present" -ne 1 ]; then
    report_violation "$label" "$subject" body-required \
      "body must contain a non-blank line after the separator"
  fi
}

MESSAGE_FILE=$(mktemp)
trap 'rm -f "$MESSAGE_FILE"' EXIT

if [ "$MODE" = file ]; then
  [ -r "$2" ] || fail_git "cannot read message file '$2'"
  COMMENT_CHAR=$(git config --get core.commentChar 2>/dev/null || printf '#')
  # Reproduce git's own pipeline so the hook judges the recorded message.
  # First the scissors cut: `git commit -v` (or commit.verbose) appends an
  # uncommented diff below "<comment char> $SCISSORS", and git drops
  # everything from that line down, so without the cut the diff would
  # count as the body. Then stripspace, which reads core.commentChar and
  # collapses blank lines the way git does.
  awk -v comment="$COMMENT_CHAR" -v cut="$SCISSORS" \
    '$0 == comment " " cut { exit } { print }' \
    "$2" | git stripspace --strip-comments >"$MESSAGE_FILE" \
    || fail_git "cannot clean up message file '$2'"
  check_message message "$MESSAGE_FILE"
  if [ "$violations" -ne 0 ]; then
    echo "FAIL: $violations violation(s) in the commit message" >&2
    exit 1
  fi
  echo "PASS: commit message satisfies the mechanical policy"
  exit 0
fi

git rev-parse --git-dir >/dev/null 2>&1 \
  || fail_git "not inside a git repository"

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
COMMITS=$(git rev-list --reverse --no-merges "$MERGE_BASE..$HEAD_SHA") \
  || fail_git "cannot enumerate commits in $MERGE_BASE..$HEAD_SHA"

RAW_FILE=$(mktemp)
trap 'rm -f "$MESSAGE_FILE" "$RAW_FILE"' EXIT

for sha in $COMMITS; do
  if ! git cat-file commit "$sha" >"$RAW_FILE"; then
    fail_git "cannot read commit object $sha"
  fi
  sed '1,/^$/d' "$RAW_FILE" >"$MESSAGE_FILE"
  check_message "$sha" "$MESSAGE_FILE"
done

MERGES=$(git rev-list --reverse --merges "$MERGE_BASE..$HEAD_SHA") \
  || fail_git "cannot enumerate merge commits in $MERGE_BASE..$HEAD_SHA"
merges=0

for sha in $MERGES; do
  merges=$((merges + 1))
  parents=$(git rev-list --parents -n 1 "$sha") \
    || fail_git "cannot read the parents of merge commit $sha"
  for parent in ${parents#* }; do
    contained=0
    git merge-base --is-ancestor "$parent" "$BASE_SHA" || contained=$?
    case $contained in
      0)
        report_violation "$sha" "$(git log -1 --format=%s "$sha")" base-merge \
          "parent ${parent:0:12} is already in the base, so the range holds a merge of the base; rebase the branch onto the base instead"
        break
        ;;
      1) ;;
      *) fail_git "cannot compare parent $parent of $sha with base $BASE_SHA" ;;
    esac
  done
done

if [ "$violations" -ne 0 ]; then
  echo "FAIL: $violations violation(s) across $checked non-merge commit(s) and $merges merge commit(s)" >&2
  exit 1
fi

echo "PASS: checked $checked non-merge commit(s)"
