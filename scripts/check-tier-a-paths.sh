#!/usr/bin/env bash
# check-tier-a-paths.sh — fail when an AGENTS.md Tier A path names nothing.
#
# Usage: check-tier-a-paths.sh [<repo-root>]
#
# AGENTS.md's `### Tier A Paths` section lists the daemon files that carry
# the full assurance bar. A rename or deletion in daemon/ leaves a stale
# entry behind and silently drops the moved code from that bar, so this
# check resolves every entry against the tree (#2757). It reads the
# section through tier-a-entries.sh, which tier-a-lookup.sh shares: list
# items as Markdown reads them (prose between them is skipped), bare names
# resolved against their bullet's directory, Go identifiers skipped. Each
# entry is then checked under <repo-root>/daemon/:
#
#   directory        must be a directory, whether it lists files or only
#                    names where a bullet's bare names are
#   pattern with *   must match at least one file
#   other path       must exist
#   bare .go/.sql    with no directory before it in its bullet, it is
#                    unresolved
#
# It reads the filesystem, not git, so an untracked file can satisfy an
# entry locally; CI's clean checkout is authoritative. The script performs
# no network I/O and does not mutate the repository.
#
# Exit codes:
#   0  every entry resolves
#   1  one or more entries are unresolved, each listed
#   2  usage/environment error: <repo-root> is not a directory, AGENTS.md
#      or the section heading is missing, or the section yields no entries
#      (so a renamed heading or reshaped section can't pass vacuously)
set -euo pipefail

PROG=check-tier-a-paths
# Sourced from beside this script, not from <repo-root>, which need not
# hold a scripts/ directory.
# shellcheck source=scripts/tier-a-entries.sh
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/tier-a-entries.sh"
HEADING=$TIER_A_HEADING

usage() {
  echo "usage: $PROG [<repo-root>]" >&2
}

if [ "$#" -gt 1 ]; then
  usage
  exit 2
fi
root=${1:-.}
if [ ! -d "$root" ]; then
  echo "$PROG: $root is not a directory" >&2
  exit 2
fi
agents=$root/AGENTS.md
if [ ! -f "$agents" ]; then
  echo "$PROG: $agents not found" >&2
  exit 2
fi
if ! grep -qxF "$HEADING" "$agents"; then
  echo "$PROG: $agents has no '$HEADING' heading" >&2
  exit 2
fi
daemon=$root/daemon

# glob_has_file <pattern> succeeds when the pattern matches a regular file.
# An unmatched pattern expands to itself, which -f then rejects.
glob_has_file() {
  local IFS='' match
  for match in $1; do
    [ -f "$match" ] && return 0
  done
  return 1
}

entries=0
unresolved=0
report() { # <entry> <reason>
  unresolved=$((unresolved + 1))
  echo "$PROG: unresolved: $1 ($2)"
}

# Capture first: a failed awk then stops the script under set -e instead
# of feeding the loop a partial listing.
listing=$(tier_a_entries "$agents")
while IFS=$'\t' read -r _bullet _label kind path; do
  [ -n "$kind" ] || continue
  entries=$((entries + 1))
  case $kind in
    dir|context) [ -d "$daemon/$path" ] || report "$path" "no such directory" ;;
    glob) glob_has_file "$daemon/$path" || report "$path" "pattern matches no file" ;;
    file) [ -e "$daemon/$path" ] || report "$path" "no such file" ;;
    bare-unresolved) report "$path" "bare name with no directory before it in its bullet" ;;
  esac
done <<<"$listing"

if [ "$entries" -eq 0 ]; then
  echo "$PROG: '$HEADING' in $agents yields no path entries" >&2
  exit 2
fi
if [ "$unresolved" -gt 0 ]; then
  echo "$PROG: $unresolved of $entries Tier A path entries are unresolved" >&2
  exit 1
fi
echo "$PROG: all $entries Tier A path entries resolve"
