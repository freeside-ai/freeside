#!/usr/bin/env bash
# merge-result-audit.sh — deterministic prospective-merge audit.
#
# Usage: merge-result-audit.sh <base-ref> <head-ref> <allowed-path>...
#
# Constructs the prospective merge result of <head-ref> into <base-ref>
# with `git merge-tree --write-tree` (git >= 2.41; no checkout or index
# mutation — it does write objects into .git/objects), diffs that result
# against the exact resolved base SHA, prints the complete name-status
# change set, and fails when any changed path falls outside the explicit
# allowed-path list.
#
# The caller is responsible for fetching the current default branch
# first: this script performs no network I/O and audits exactly the refs
# it is given. Allowed paths are explicit arguments, never scraped from
# a PR body. An entry matches itself and everything under it as a
# directory ("api" matches "api" and "api/x", never "api2/x").
#
# Guarantees: merge-conflict detection, exact-base binding, complete
# prospective-diff visibility, and path-boundary enforcement. It does
# not infer semantic intent; an in-scope reversion still passes.
#
# Exit codes:
#   0  pass: clean merge, every changed path inside the allowed paths
#   1  out of scope: changed path(s) outside the allowed paths
#   2  prospective merge has conflicts
#   3  usage error / empty or malformed allowed-path list
#   4  git failure or unverifiable state (unresolvable or ambiguous
#      refs, corrupt objects, truncated plumbing output, not a repo)
set -euo pipefail

# User/system git config is ignored so the audit behaves identically
# across machines. Repository-local git state remains trusted input,
# except where it could make the audit disagree with what the forge
# would actually merge: submodule-ignore settings (countered with
# --ignore-submodules=none), ambiguous ref names (rejected outright),
# replace refs (a refs/replace/ entry substitutes another object during
# merge-tree/diff-tree reads while rev-parse still prints the original
# SHA, so the recorded SHA would not be the audited content), graft
# files (rejected below: .git/info/grafts fakes parents, moving the
# merge base), local merge configuration (rejected below: any merge.*
# knob or diff rename fallback can steer merge-tree to a result the
# forge would never produce, e.g. a driver erasing an out-of-scope
# change or merge.directoryRenames hiding a rename conflict), and
# shallow history (rejected below: a truncated history can yield a
# different merge base than the forge's).
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null
export GIT_NO_REPLACE_OBJECTS=1
export GIT_ATTR_NOSYSTEM=1
# Belt-and-suspenders with the partial-clone reject below: a promisor
# clone would otherwise lazily fetch missing objects over the network
# mid-audit (gits too old for this variable get the reject alone).
export GIT_NO_LAZY_FETCH=1

PROG=merge-result-audit

usage() {
  echo "usage: $PROG <base-ref> <head-ref> <allowed-path>..." >&2
}

fail_usage() { # exit 3
  echo "$PROG: $*" >&2
  usage
  exit 3
}

fail_git() { # exit 4
  echo "$PROG: $*" >&2
  exit 4
}

if [ "$#" -lt 3 ]; then
  fail_usage "need a base ref, a head ref, and at least one allowed path"
fi

BASE_REF=$1
HEAD_REF=$2
shift 2

# Validate the allowed-path list before touching git. Entries must be
# repo-relative, normalized paths; anything ambiguous is rejected rather
# than interpreted.
ALLOW=()
for raw in "$@"; do
  entry=$raw
  case $entry in
    '') fail_usage "empty allowed path" ;;
    *[[:cntrl:]]*) fail_usage "control character in allowed path: '$raw'" ;;
    *\\*) fail_usage "backslash in allowed path: '$raw'" ;;
    /*) fail_usage "absolute allowed path: '$raw'" ;;
  esac
  entry=${entry%/} # normalize one trailing slash ("api/" == "api")
  case $entry in
    '' | */) fail_usage "malformed allowed path: '$raw'" ;;
    *//*) fail_usage "double slash in allowed path: '$raw'" ;;
    . | ./* | */. | */./*) fail_usage "'.' component in allowed path: '$raw'" ;;
    .. | ../* | */.. | */../*) fail_usage "parent traversal in allowed path: '$raw'" ;;
  esac
  for seen in ${ALLOW[@]+"${ALLOW[@]}"}; do
    if [ "$entry" = "$seen" ]; then
      fail_usage "duplicate allowed path: '$raw'"
    fi
  done
  ALLOW+=("$entry")
done

git rev-parse --git-dir >/dev/null 2>&1 \
  || fail_git "not inside a git repository"
if [ "$(git rev-parse --is-shallow-repository)" != "false" ]; then
  fail_git "shallow repository: incomplete history cannot bind the merge base; unshallow first"
fi
# Grafts fake commit parents during ancestry walks (the deprecated
# precursor of replace refs, which GIT_NO_REPLACE_OBJECTS above cannot
# disable), so a graft can move the merge base off what the forge
# computes; reject the file outright.
if [ -e "$(git rev-parse --git-path info/grafts)" ]; then
  fail_git "graft file present ($(git rev-parse --git-path info/grafts)); grafts fake ancestry, remove it first"
fi
if [ -e "$(git rev-parse --git-path info/attributes)" ]; then
  fail_git "local attributes file present ($(git rev-parse --git-path info/attributes)); it can steer the merge, remove it first"
fi
# A partial (promisor) clone lazily fetches missing objects over the
# network during merge-tree/diff-tree, breaking the no-network
# guarantee and mutating the object store mid-audit.
if git config --get-regexp '\.(promisor|partialclonefilter)$' >/dev/null 2>&1; then
  fail_git "partial clone (promisor remote configured); the audit needs a full clone"
fi
# Global/system config is nulled above, so any merge-steering config
# still visible is repo-local state the forge's merge machinery will
# not have: drivers, rename detection, directory renames,
# renormalization. Reject the whole family rather than enumerating
# knobs one finding at a time (diff.renames/diff.renameLimit are the
# documented fallbacks merge-ort reads when merge.* is unset).
if git config --get-regexp '^merge\.' >/dev/null 2>&1 \
  || git config --get-regexp '^diff\.(renames|renamelimit)$' >/dev/null 2>&1; then
  fail_git "repo-local merge configuration present (merge.* or diff.renames/renameLimit); the audit cannot reproduce the forge merge"
fi

# Bind the audit to exact commits. --end-of-options guards against
# option-looking refs; ^{commit} forces the object to be read, so a
# missing/corrupt object fails here instead of inside merge-tree. An
# ambiguous name (a tag shadowing a branch or remote ref) would make
# the audit examine a different commit than the forge merges, so it is
# rejected instead of resolved; rev-parse's ambiguity warning is the
# authoritative signal, forced on in case repo config disabled it.
resolve_commit() { # <ref> <role> ; prints the commit SHA
  local ref=$1 role=$2 err sha
  if ! err=$(LC_ALL=C git -c core.warnAmbiguousRefs=true rev-parse --verify \
    --end-of-options "${ref}^{commit}" 2>&1 >/dev/null); then
    fail_git "cannot resolve $role ref '$ref' to a commit"
  fi
  case $err in
    *ambiguous*)
      fail_git "$role ref '$ref' is ambiguous (tag/branch/remote collision); pass a full ref like refs/remotes/origin/main" ;;
  esac
  sha=$(git rev-parse --verify --quiet --end-of-options "${ref}^{commit}") \
    || fail_git "cannot resolve $role ref '$ref' to a commit"
  printf '%s' "$sha"
}

BASE_SHA=$(resolve_commit "$BASE_REF" base)
HEAD_SHA=$(resolve_commit "$HEAD_REF" head)

# Attribute sources other than the base tree can steer the merge (e.g.
# merge=union turning a real conflict into a clean result): the system
# file is disabled above, the global/XDG default and any local
# core.attributesFile are overridden here, $GIT_DIR/info/attributes is
# rejected above, and --attr-source pins attribute reading to the base
# tree instead of the current checkout, so the audit is
# checkout-independent and a .gitattributes committed on the head
# branch cannot steer conflict detection before it is itself merged.
# (--attr-source needs git >= 2.41; an older git fails loud on the
# unknown option instead of silently reading checkout attributes.)
ATTR_OVERRIDE=("--attr-source=$BASE_SHA" -c core.attributesFile=/dev/null)

echo "base: $BASE_REF = $BASE_SHA"
echo "head: $HEAD_REF = $HEAD_SHA"

MERGE_BASE=$(git merge-base "$BASE_SHA" "$HEAD_SHA") \
  || fail_git "no merge base between $BASE_SHA and $HEAD_SHA (unrelated histories?)"
echo "merge base: $MERGE_BASE"
echo "allowed paths: ${ALLOW[*]}"

# Prospective merge. Exit 0 = clean, 1 = conflicts, anything else =
# error. A bogus argument also exits 1 but with no tree OID on stdout,
# so exit 1 counts as "conflict" only when the first line is an OID.
mt_rc=0
MT_OUT=$(git "${ATTR_OVERRIDE[@]}" merge-tree --write-tree --name-only \
  --no-messages "$BASE_SHA" "$HEAD_SHA") || mt_rc=$?
if [ "$mt_rc" -gt 1 ]; then
  fail_git "git merge-tree failed (exit $mt_rc)"
fi
TREE=${MT_OUT%%$'\n'*}
case $TREE in
  *[!0-9a-f]* | '') fail_git "git merge-tree produced no result tree (exit $mt_rc)" ;;
esac
if [ "${#TREE}" -ne 40 ] && [ "${#TREE}" -ne 64 ]; then
  fail_git "git merge-tree produced no result tree (exit $mt_rc)"
fi
if [ "$mt_rc" -eq 1 ]; then
  echo "$PROG: prospective merge of $HEAD_SHA into $BASE_SHA has conflicts:" >&2
  printf '%s\n' "${MT_OUT#*$'\n'}" >&2
  echo "FAIL: merge conflict"
  exit 2
fi
echo "prospective merge tree: $TREE"

path_allowed() {
  local path=$1 e
  for e in "${ALLOW[@]}"; do
    if [ "$path" = "$e" ] || [[ $path == "$e"/* ]]; then
      return 0
    fi
  done
  return 1
}

# Diff the prospective result against the exact base SHA. All parsing is
# NUL-delimited; the trailing sentinel proves diff-tree exited zero and
# the stream was not truncated (a failure inside process substitution is
# otherwise invisible to the reading loop).
CHANGES=0
VIOLATIONS=()
complete=0
echo "prospective changes vs base (name-status; tab-separated display):"
while IFS= read -r -d '' status; do
  if [ "$status" = "end-of-diff" ]; then
    complete=1
    continue
  fi
  if [ "$complete" -eq 1 ]; then
    fail_git "unexpected diff-tree output after sentinel"
  fi
  case $status in
    A | D | M | T)
      IFS= read -r -d '' p1 || fail_git "truncated diff-tree record"
      printf '  %s\t%s\n' "$status" "$p1"
      path_allowed "$p1" || VIOLATIONS+=("$status"$'\t'"$p1")
      ;;
    R[0-9][0-9][0-9] | C[0-9][0-9][0-9])
      IFS= read -r -d '' p1 || fail_git "truncated diff-tree record"
      IFS= read -r -d '' p2 || fail_git "truncated diff-tree record"
      printf '  %s\t%s\t%s\n' "$status" "$p1" "$p2"
      path_allowed "$p1" || VIOLATIONS+=("$status"$'\t'"$p1")
      path_allowed "$p2" || VIOLATIONS+=("$status"$'\t'"$p2")
      ;;
    *)
      fail_git "unexpected diff-tree status '$status'"
      ;;
  esac
  CHANGES=$((CHANGES + 1))
done < <(
  git "${ATTR_OVERRIDE[@]}" diff-tree -r -z -M -C \
    --ignore-submodules=none --name-status "$BASE_SHA" "$TREE" \
    && printf 'end-of-diff\0'
)
if [ "$complete" -ne 1 ]; then
  fail_git "git diff-tree failed or its output was truncated"
fi
if [ "$CHANGES" -eq 0 ]; then
  echo "  (empty change set)"
fi

if [ "${#VIOLATIONS[@]}" -gt 0 ]; then
  echo "out-of-scope changes:"
  printf '  %s\n' "${VIOLATIONS[@]}"
  echo "FAIL: ${#VIOLATIONS[@]} changed path(s) outside the allowed paths"
  exit 1
fi
echo "PASS: $CHANGES changed path(s), all within the allowed paths"
