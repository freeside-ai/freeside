#!/usr/bin/env bash
# pr-integrity-check.sh: the mechanical half of the "Monorepo scope
# discipline" and devlog append-only conventions (AGENTS.md), run as a
# PR gate. It exists because a parallel branch once silently reverted an
# unrelated component's files and deleted a foreign devlog entry, and a
# clean 3-way merge propagated that to main with nothing flagging it. This
# is the manual-convention-to-importer bridge AGENTS.md names: it enforces
# the component-scope *subset* a PR diff can prove, not the runtime
# control-plane restrictions the gauntlet importer will own (plan §5.6/§5.8).
#
# Two checks, both derived from existing convention (enforcing, never
# redefining it):
#   1. Scope: a changed path under a component dir whose component is not in
#      the PR's declared `Scope:` fails. Cross-cutting dirs (devlog/, docs/,
#      .github/) and root files are never scope violations: any work unit
#      legitimately touches them (session-bookend devlog, doc-gating, CI).
#   2. Devlog: a merged devlog entry is frozen; the one permitted edit is
#      appending a `->` marker (a modification). Deleting or renaming an
#      entry present on the base is never legitimate, so those fail.
#
# Inputs (NUL-delimited so non-ASCII / unusual paths aren't quoted or
# split-mangled: git quotes such paths in the default output, which would let
# a `daemon/é.go` read as top dir `"daemon` and slip the scope check):
#   stdin          git diff -z --name-status  (base...head): NUL-terminated
#                  status then path(s); rename/copy carry old then new.
#   $BODY          the PR description (for the `## Scope` declaration).
#   $NUMSTAT_FILE  path to a file holding git diff -z --numstat (base...head),
#                  used to tell a frozen-devlog rewrite (removed lines) from a
#                  permitted marker append (purely additive). A file, not an
#                  env value, because -z output contains NUL. Absent/empty =>
#                  that check is skipped (delete/rename still caught).
# Exit 0 clean, 1 on any violation, 64 on a malformed/absent scope.
set -euo pipefail

# Canonical component directories (AGENTS.md "Build, test, run" table). A
# change here is a deliberate edit, matching how the table itself changes.
COMPONENTS="api app daemon prompts policy images"

body="${BODY-}"

# --- extract the positive `Scope:` declaration -----------------------------
# The `## Scope` section runs from its header to the next `## ` header; drop
# the template's HTML-comment guidance, then take the text after the canonical
# `Scope:` marker (the section itself if none) and drop parenthetical asides.
# The declaration is a comma-separated list of `dir/` tokens; parse it
# positionally so that explanatory or negated prose can't flip the guard:
#   - opt-out fires only when the whole trimmed declaration IS
#     "repo-wide scaffold" (exact, not a substring buried in prose);
#   - a component is declared only when a comma-item *begins* with `dir/`, so
#     a negated dir ("daemon/ -- not api/", "daemon/ (not api/)") sits
#     mid-item and is never read as declared, while real trailing prose after
#     a dir token ("api/ (its CI workflow).") still declares it.
scope_section="$(
  printf '%s\n' "$body" \
    | awk '/^## +Scope/{f=1; next} /^## /{f=0} f' \
    | sed 's/<!--.*-->//; /<!--/,/-->/d'
)"
# Anchor to a line that *starts* with the `Scope:` marker and strip only that
# leading marker (not a greedy strip through a later "scope:" in prose, e.g.
# "Scope: daemon/ -- out of scope: api/", which would leave decl="api/").
# `|| true`: a no-match grep exits non-zero, which under `set -e` would abort
# the whole check (a bare list with no `Scope:` marker is a valid fallback).
decl="$(printf '%s\n' "$scope_section" | grep -iE '^[[:space:]]*scope[[:space:]]*:' | head -1 | sed -E 's/^[[:space:]]*[Ss]cope[[:space:]]*:[[:space:]]*//' || true)"
[ -n "$decl" ] || decl="$scope_section"
decl="$(printf '%s' "$decl" | sed 's/([^)]*)//g')"   # drop parenthetical asides

# Opt-out: exact match on the normalized (lowercased, single-spaced, trimmed)
# declaration, so only a genuine `Scope: repo-wide scaffold` opts out.
decl_norm="$(printf '%s' "$decl" | tr '[:upper:]' '[:lower:]' | tr '\t\n' '  ' | tr -s ' ' | sed 's/^ *//; s/ *$//')"
repo_wide=0
[ "$decl_norm" = "repo-wide scaffold" ] && repo_wide=1

# Components: a comma-item declares C only when it begins with `C/`. Split the
# declaration on commas into positional params (globbing off), then restore
# IFS so the inner space-separated COMPONENTS loop splits normally.
set -f; _oifs="$IFS"; IFS=','
# shellcheck disable=SC2086  # deliberate word-split of decl on commas
set -- $decl
IFS="$_oifs"; set +f
declared=" "
for _item in "$@"; do
  _item="${_item#"${_item%%[![:space:]]*}"}"   # left-trim whitespace
  for c in $COMPONENTS; do
    case "$_item" in "$c"/*) declared="${declared}${c} " ;; esac
  done
done

# No upfront "scope missing" error: a PR that touches only cross-cutting
# dirs (this one included) has no component to scope, so an empty or
# non-component Scope is fine. Scope is enforced only when a *component*
# dir is actually changed (below), which is also what catches a missing
# declaration on a component-touching PR.

top_of() { printf '%s' "$1" | cut -d/ -f1; }
is_component() { case " $COMPONENTS " in *" $1 "*) return 0;; *) return 1;; esac; }
is_declared()  { case "$declared" in *" $1 "*) return 0;; *) return 1;; esac; }

# Deleted-line count for a path from NUMSTAT_FILE (git diff -z --numstat).
# Records are NUL-terminated `added<TAB>deleted<TAB>path` (path may itself
# contain tabs under -z, so path = everything after the 2nd tab). Rename
# records add empty-path + separate old/new tokens that never match a real
# query path, so no special-casing is needed. "-" (binary) => non-zero;
# missing file / no match => "0" so an absent NUMSTAT can't manufacture one.
TAB="$(printf '\t')"
numstat_deleted() {
  [ -n "${NUMSTAT_FILE-}" ] && [ -f "${NUMSTAT_FILE-}" ] || { printf 0; return; }
  local rest deleted path
  while IFS= read -r -d '' rec; do
    case "$rec" in
      *"$TAB"*"$TAB"*)
        rest="${rec#*"$TAB"}"            # drop added
        deleted="${rest%%"$TAB"*}"       # deleted = up to 2nd tab
        path="${rest#*"$TAB"}"           # path = remainder (may contain tabs)
        if [ -n "$path" ] && [ "$path" = "$1" ]; then
          [ "$deleted" = "-" ] && printf 1 || printf '%s' "$deleted"
          return
        fi ;;
    esac
  done < "$NUMSTAT_FILE"
  printf 0
}

scope_violations=""
devlog_violations=""

# --- walk the changed files (NUL-delimited -z name-status) ------------------
# Each record: a NUL-terminated status, then a NUL-terminated path; a
# rename/copy status is followed by two paths (old then new).
while IFS= read -r -d '' status; do
  IFS= read -r -d '' old || break
  case "$status" in
    R*|C*) IFS= read -r -d '' new || break ;;   # rename/copy: old then new
    *)     new="$old" ;;
  esac

  # Devlog: a merged session entry (present on base => appears as the old
  # path) is frozen. Match only the timestamped `YYYY-...md` entries, not
  # `devlog/README.md` (the protocol, meant to be edited) or other
  # non-entry files. Allowlist, not blocklist, so exotic statuses can't slip
  # through (e.g. T, a type change to a symlink): the ONLY permitted change
  # to a frozen entry is a purely additive `->` marker append (status M with
  # no removed lines). Everything else (D delete, R rename-away, T type
  # change, M that removes lines) fails. Brand-new entries (A) and copy
  # sources (C, original untouched) are not frozen-entry changes.
  case "$old" in
    devlog/[0-9][0-9][0-9][0-9]-*.md)
      case "$status" in
        A*|C*) : ;;
        M*) [ "$(numstat_deleted "$old")" = "0" ] \
              || devlog_violations="${devlog_violations}  ${status} (removes lines)  ${old}"$'\n' ;;
        *)  devlog_violations="${devlog_violations}  ${status}  ${old}"$'\n' ;;
      esac ;;
  esac

  # Scope: a rename touching a component in either direction counts, so check
  # both endpoints; other statuses have old==new.
  for path in "$old" "$new"; do
    top="$(top_of "$path")"
    if is_component "$top" && ! is_declared "$top"; then
      case "$repo_wide" in
        1) : ;;
        *) scope_violations="${scope_violations}  ${top}/ (${path})"$'\n' ;;
      esac
    fi
  done
done

status=0
if [ -n "$devlog_violations" ]; then
  status=1
  echo "pr-integrity: this PR deletes, renames, or rewrites merged devlog" >&2
  echo "  entries, which are frozen (append-only; only '->' marker lines" >&2
  echo "  may be added, never removed):" >&2
  printf '%s' "$devlog_violations" | sort -u >&2
fi
if [ -n "$scope_violations" ]; then
  status=1
  disp="$declared"; [ "$disp" = " " ] && disp=" (none) "
  echo "pr-integrity: this PR changes component dirs outside its declared" >&2
  echo "  Scope (declared:${disp}):" >&2
  printf '%s' "$scope_violations" | sort -u >&2
  echo "  Widen the PR's Scope, or move the out-of-scope change to its own" >&2
  echo "  work unit (Monorepo scope discipline, AGENTS.md)." >&2
fi
exit "$status"
