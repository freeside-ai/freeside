#!/usr/bin/env bash
# check-tier-a-paths.sh — fail when an AGENTS.md Tier A path names nothing.
#
# Usage: check-tier-a-paths.sh [<repo-root>]
#
# AGENTS.md's `### Tier A Paths` section lists the daemon files that carry
# the full assurance bar. A rename or deletion in daemon/ leaves a stale
# entry behind and silently drops the moved code from that bar, so this
# check resolves every entry against the tree (#2757). It reads the
# section's list items as Markdown does (section_entries below; prose
# between them is skipped) and resolves each backticked entry under
# <repo-root>/daemon/:
#
#   ends in /        must be a directory; it also becomes the directory
#                    that later bare names in the same bullet resolve
#                    against
#   contains /       a pattern with * must match at least one file; any
#                    other entry must exist
#   bare .go/.sql    resolves against the most recent directory entry in
#                    the same bullet, then as above; with no directory
#                    before it in the bullet, it is unresolved
#   anything else    a Go identifier such as a function or field name;
#                    skipped
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
HEADING='### Tier A Paths'

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

# section_entries prints one "<bullet-number><TAB><entry>" line per
# backticked entry in the section's list items, in document order, read as
# Markdown reads them. The section runs from its heading to the next
# heading of level 1 to 3; a deeper heading only closes the current item,
# so a subsection can't hide the items after it. An item starts with a
# `-`, `*`, `+`, or `1.` marker and takes in its indented lines (also after
# a blank line) and any unindented line that directly follows it (a lazy
# continuation). Other unindented text is prose and skipped. A code span
# may open with several backticks.
section_entries() {
  awk -v heading="$HEADING" '
    function emit(    text, span) {
      text = item
      while (match(text, /`+[^`]+`+/)) {
        span = substr(text, RSTART, RLENGTH)
        gsub(/^`+ ?| ?`+$/, "", span)
        print n "\t" span
        text = substr(text, RSTART + RLENGTH)
      }
      item = ""
    }
    function close_item() { emit(); open = 0 }
    /^#+([ \t]|$)/ {
      level = $0
      sub(/[^#].*$/, "", level)
      if (in_section && length(level) <= 3) { close_item(); exit }
      if ($0 == heading) in_section = 1
      if (in_section) close_item()
      blank = 0
      next
    }
    !in_section { next }
    /^[ \t]*$/ { blank = 1; next }
    /^([-*+]|[0-9]+[.)])[ \t]/ { emit(); n++; item = $0; open = 1; blank = 0; next }
    /^[ \t]/ || (open && !blank) { if (open) item = item " " $0; blank = 0; next }
    { close_item(); blank = 0 }
    END { emit() }
  ' "$agents"
}

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
listing=$(section_entries)
current_bullet=''
dir=''
while IFS=$'\t' read -r bullet entry; do
  if [ "$bullet" != "$current_bullet" ]; then
    current_bullet=$bullet
    dir=''
  fi
  case $entry in
    */|*/*|*.go|*.sql) entries=$((entries + 1)) ;;
    *) continue ;;
  esac
  case $entry in
    */)
      dir=$entry
      [ -d "$daemon/$entry" ] || report "$entry" "no such directory"
      continue
      ;;
    */*) path=$entry ;;
    *)
      if [ -z "$dir" ]; then
        report "$entry" "bare name with no directory before it in its bullet"
        continue
      fi
      path=$dir$entry
      ;;
  esac
  case $path in
    *'*'*) glob_has_file "$daemon/$path" || report "$path" "pattern matches no file" ;;
    *) [ -e "$daemon/$path" ] || report "$path" "no such file" ;;
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
