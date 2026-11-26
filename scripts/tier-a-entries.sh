#!/usr/bin/env bash
# tier-a-entries.sh — read the Tier A path list out of AGENTS.md.
#
# Sourced, not executed. check-tier-a-paths.sh and tier-a-lookup.sh both
# read the `### Tier A Paths` section through tier_a_entries, so the check
# that every entry resolves and the lookup that says which entry lists a
# path can't disagree about what the list says (#3639).
#
#   tier_a_entries <agents-file>
#
# prints one "<bullet-number><TAB><label><TAB><kind><TAB><path>" line per
# path entry, in document order. <label> is the bullet's bold lead without
# its trailing colon, such as `Import and handoff`, or `bullet <n>` for a
# bullet with none; a bold lead that wraps onto a second line is not seen,
# so that bullet also falls back to `bullet <n>`. <path> is relative to
# daemon/, which the section says every entry sits under. <kind> comes from
# the backticked text:
#
#   dir              ends in /: lists the directory and everything under it
#   context          a dir that a later bare name in the same bullet
#                    resolves against, as in "in `cmd/freesided/` the files
#                    `auth*.go`": it names where those files are and lists
#                    nothing itself
#   glob             contains / and *: a pattern
#   file             contains / and no *: one path
#   bare-unresolved  a bare .go or .sql name with no directory before it in
#                    its bullet; <path> is the bare name
#
# A bare .go or .sql name after a directory in the same bullet resolves
# against the most recent one, then becomes a glob or file. Anything else
# backticked is a Go identifier such as a function or field name, and is
# dropped. Callers check the heading exists first: a missing heading and an
# empty section both print nothing.
#
# The section runs from its heading to the next heading of level 1 to 3; a
# deeper heading only closes the current item, so a subsection can't hide
# the items after it. It is read as Markdown reads it: an item starts with a
# `-`, `*`, `+`, or `1.` marker and takes in its indented lines (also after
# a blank line) and any unindented line that directly follows it (a lazy
# continuation). Other unindented text is prose and skipped. A code span may
# open with several backticks.

TIER_A_HEADING='### Tier A Paths'

tier_a_entries() { # <agents-file>
  awk -v heading="$TIER_A_HEADING" '
    function bare(span) { return span !~ /\// && span ~ /\.(go|sql)$/ }
    function out(kind, path) { print n "\t" label "\t" kind "\t" path }
    function emit(    text, spans, k, i, dir, context, span, path) {
      k = 0
      text = item
      while (match(text, /`+[^`]+`+/)) {
        span = substr(text, RSTART, RLENGTH)
        gsub(/^`+ ?| ?`+$/, "", span)
        spans[++k] = span
        text = substr(text, RSTART + RLENGTH)
      }
      # First pass: mark each directory a bare name resolves against.
      dir = 0
      for (i = 1; i <= k; i++) {
        if (spans[i] ~ /\/$/) dir = i
        else if (bare(spans[i]) && dir) context[dir] = 1
      }
      dir = ""
      for (i = 1; i <= k; i++) {
        span = spans[i]
        if (span ~ /\/$/) {
          dir = span
          out((i in context) ? "context" : "dir", span)
          continue
        }
        if (span ~ /\//) path = span
        else if (!bare(span)) continue
        else if (dir == "") { out("bare-unresolved", span); continue }
        else path = dir span
        out((path ~ /\*/) ? "glob" : "file", path)
      }
      item = ""
    }
    function close_item() { emit(); open = 0 }
    function bold_lead(line,    rest, end) {
      if (!match(line, /^([-*+]|[0-9]+[.)])[ \t]+\*\*/)) return ""
      rest = substr(line, RLENGTH + 1)
      end = index(rest, "**")
      if (end == 0) return ""
      rest = substr(rest, 1, end - 1)
      sub(/[ \t]*:$/, "", rest)
      return rest
    }
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
    /^([-*+]|[0-9]+[.)])[ \t]/ {
      emit(); n++; item = $0; open = 1; blank = 0
      label = bold_lead($0)
      if (label == "") label = "bullet " n
      next
    }
    /^[ \t]/ || (open && !blank) { if (open) item = item " " $0; blank = 0; next }
    { close_item(); blank = 0 }
    END { emit() }
  ' "$1"
}
