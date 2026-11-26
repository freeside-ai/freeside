#!/usr/bin/env bash
# tier-a-lookup.sh — say which AGENTS.md Tier A entry, if any, lists a path.
#
# Usage: tier-a-lookup.sh [--root <repo-root>] <path>...
#        tier-a-lookup.sh [--root <repo-root>] --range <base> <head>
#
# A session classifying a change otherwise resolves the `### Tier A Paths`
# entries by eye (#3639). This prints, for each repo-relative path, one line
# per entry that lists it, with the entry and its bullet's bold lead:
#
#   daemon/internal/importer/x.go: Tier A: entry internal/importer/ (Import and handoff)
#
# A path no entry lists gets one line saying so; its tier is then decided by
# category (AGENTS.md, Assurance Tiers, Classification), which this script
# does not judge, and neither does it tell Tier B from Tier C. A `_test.go`
# file, or a file under a `testdata/` directory, that an entry lists is
# reported as not Tier A by path, with a pointer to the gate-test rule in
# the same section; whether it is a gate test stays judgment. A bullet whose
# entries are conditional, such as `migrations/*.sql`, is Tier A only under
# the condition its bullet states, so read the bullet the line names.
#
# --range classifies the files `git diff --name-only --no-renames
# <base>...<head>` lists in <repo-root>, so a rename reports its old and new
# path. --root defaults to the working directory.
#
# The section is read through tier-a-entries.sh, the parser
# check-tier-a-paths.sh uses. Matching is lexical, against the entries' text
# and not the filesystem, so a deleted path still classifies. Only a path
# under daemon/ can match. A directory entry lists everything under it,
# unless its bullet's bare names resolve against it (as in "in
# `cmd/freesided/` the files `auth*.go`"), when it lists nothing itself; a
# file entry lists itself; in a pattern, * matches within one path segment,
# as the checker's filesystem glob does. A bare name with no directory
# before it in its bullet lists nothing; the checker fails CI on it.
#
# Limits: the list is read from <repo-root>/AGENTS.md on disk, in range mode
# too, never from either ref. The script performs no network I/O and does
# not mutate the repository.
#
# Exit codes:
#   0  every path was reported, whatever its tier
#   2  usage/environment error: no paths, --range without two refs, a
#      path that is not in repo-relative form (absolute, or holding a `.`,
#      `..`, or empty segment, which would otherwise read as unlisted),
#      <repo-root> is not a directory, AGENTS.md or the section heading is
#      missing, the section yields no entries, or git diff fails
set -euo pipefail

PROG=tier-a-lookup
# shellcheck source=scripts/tier-a-entries.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/tier-a-entries.sh"
CLASSIFICATION='(AGENTS.md, Assurance Tiers, Classification)'

usage() {
  echo "usage: $PROG [--root <repo-root>] <path>..." >&2
  echo "       $PROG [--root <repo-root>] --range <base> <head>" >&2
  exit 2
}

root=.
if [ "${1-}" = --root ]; then
  [ "$#" -ge 2 ] || usage
  root=$2
  shift 2
fi
range=''
if [ "${1-}" = --range ]; then
  [ "$#" -eq 3 ] || usage
  range="$2...$3"
elif [ "$#" -eq 0 ]; then
  usage
fi

if [ ! -d "$root" ]; then
  echo "$PROG: $root is not a directory" >&2
  exit 2
fi
agents=$root/AGENTS.md
if [ ! -f "$agents" ]; then
  echo "$PROG: $agents not found" >&2
  exit 2
fi
if ! grep -qxF "$TIER_A_HEADING" "$agents"; then
  echo "$PROG: $agents has no '$TIER_A_HEADING' heading" >&2
  exit 2
fi
entries=$(tier_a_entries "$agents")
if [ -z "$entries" ]; then
  echo "$PROG: '$TIER_A_HEADING' in $agents yields no path entries" >&2
  exit 2
fi

# lists <daemon-relative-path> <kind> <entry> succeeds when the entry lists
# the path.
lists() {
  case $2 in
    dir) case "$1/" in "$3"*) return 0 ;; esac ;;
    file) [ "$1" = "$3" ] && return 0 ;;
    glob)
      # The directory must equal the entry's and only the basename is a
      # pattern, so * stays within one segment, as a filesystem glob does.
      [ "${1%/*}" = "${3%/*}" ] || return 1
      # shellcheck disable=SC2254 # the entry's basename is the pattern
      case ${1##*/} in ${3##*/}) return 0 ;; esac
      ;;
    context|bare-unresolved) ;;
  esac
  return 1
}

classify() { # <repo-relative-path>
  local path=$1 rel test_only='' matched='' _bullet label kind entry
  case $path in
    *_test.go|*/testdata/*) test_only=1 ;;
  esac
  case $path in
    daemon/*)
      rel=${path#daemon/}
      while IFS=$'\t' read -r _bullet label kind entry; do
        lists "$rel" "$kind" "$entry" || continue
        matched=1
        if [ -n "$test_only" ]; then
          echo "$path: not Tier A by path: test-only file under entry $entry ($label); a gate test or live-credential change stays Tier A $CLASSIFICATION"
        else
          echo "$path: Tier A: entry $entry ($label)"
        fi
      done <<<"$entries"
      ;;
  esac
  [ -n "$matched" ] ||
    echo "$path: no Tier A entry matches; decide the tier by category $CLASSIFICATION"
}

if [ -z "$range" ]; then
  # Refuse every malformed path before printing anything: matching is
  # lexical, so `./daemon/x` would otherwise report as unlisted.
  for path in "$@"; do
    case /$path/ in
      //*|*//*|*/./*|*/../*)
        echo "$PROG: $path is not a repo-relative path" >&2
        exit 2
        ;;
    esac
  done
  for path in "$@"; do classify "$path"; done
  exit 0
fi

# Capture first, so a failed diff exits 2 instead of printing a partial
# report.
if ! changed=$(git -C "$root" -c core.quotePath=false diff --name-only --no-renames "$range" --); then
  echo "$PROG: git diff $range failed in $root" >&2
  exit 2
fi
if [ -z "$changed" ]; then
  echo "$PROG: no files differ in $range"
  exit 0
fi
while IFS= read -r path; do classify "$path"; done <<<"$changed"
