#!/usr/bin/env bash
# check-vocabulary.sh — refuse the pre-rename stage vocabulary (#986).
#
# Usage: check-vocabulary.sh [<repo-root>]
#
# The specification stage was renamed from its earlier name in #986. This
# check greps for the retired word stem (case-insensitive) in the surfaces
# the rename covers and fails when it reappears. In a git checkout it reads
# tracked files only, so untracked scratch files never fail it. In a tree
# without git (a verification room, which holds a commit's exact tree and
# no .git, or a `git archive` export) it reads every regular file, which is
# the same set. Both modes search with git grep, so they print the same
# lines and apply the same binary rule. The scope:
#
#   daemon/**/*.go   non-test Go code, except one legacy_vocabulary.go per
#                    package, which is the only place pre-rename literals
#                    may live (they canonicalize stored rows on decode)
#   daemon/migrations/*.sql   files numbered above 0064; earlier migrations
#                    predate the rename and 0064 must name the old column
#                    it renames
#   api/ app/ prompts/ scripts/   every file
#
# devlog/ and docs/history/ are frozen records; docs/ prose is reviewed by
# hand because the word also has an ordinary English sense. The retired stem
# is assembled at runtime so this file passes its own check. The script
# performs no network I/O and does not mutate the repository.
#
# Exit codes:
#   0  no file in scope spells the retired vocabulary
#   1  one or more hits, listed as path:line:text
#   2  usage/environment error: <repo-root> is not a directory, or listing
#      or searching a file failed
set -euo pipefail

PROG=check-vocabulary

usage() {
  echo "usage: $PROG [<repo-root>]" >&2
}

if [ "$#" -gt 1 ]; then
  usage
  exit 2
fi
root=${1:-.}
if git -C "$root" rev-parse --show-toplevel >/dev/null 2>&1; then
  in_git=1
elif [ -d "$root" ]; then
  in_git=0
else
  echo "$PROG: $root is not a directory" >&2
  exit 2
fi

stem="elab""orat"
last_prerename_migration=64

in_scope() {
  case "$1" in
    daemon/migrations/*.sql)
      number=$(basename "$1" | cut -c1-4)
      case "$number" in
        [0-9][0-9][0-9][0-9]) [ "$((10#$number))" -gt "$last_prerename_migration" ] ;;
        *) return 0 ;;
      esac
      ;;
    daemon/*_test.go) return 1 ;;
    daemon/*/legacy_vocabulary.go) return 1 ;;
    daemon/*.go) return 0 ;;
    daemon/*) return 1 ;;
    api/*|app/*|prompts/*|scripts/*) return 0 ;;
    *) return 1 ;;
  esac
}

surfaces=(daemon api app prompts scripts)

# list_files prints NUL-delimited repo-relative paths in byte order, the
# order git ls-files uses, without quoting unusual filenames.
list_files() {
  if [ "$in_git" -eq 1 ]; then
    git -C "$root" ls-files -z -- "${surfaces[@]}"
    return
  fi
  (
    cd "$root"
    for dir in "${surfaces[@]}"; do
      if [ -d "$dir" ]; then
        find "$dir" -type f -print0 || exit
      fi
    done
  ) | LC_ALL=C sort -z
}

# search exits 0 on a hit and 1 on none, as git grep does; any other
# status is a failure that must not pass as a clean file.
search() {
  if [ "$in_git" -eq 1 ]; then
    git -C "$root" grep -I -i -n -e "$stem" -- "$1"
  else
    (cd "$root" && git grep --no-index -I -i -n -e "$stem" -- "$1")
  fi
}

# A temporary list preserves NUL separators and lets listing errors stop
# the check before any file is searched.
if ! files=$(mktemp); then
  echo "$PROG: cannot create file list" >&2
  exit 2
fi
trap 'rm -f "$files"' EXIT
if ! list_files > "$files"; then
  echo "$PROG: cannot list files under $root" >&2
  exit 2
fi

hits=0
while IFS= read -r -d '' path; do
  in_scope "$path" || continue
  rc=0
  out=$(search "$path") || rc=$?
  case "$rc" in
    0)
      printf '%s\n' "$out"
      hits=$((hits + 1))
      ;;
    1) ;;
    *)
      echo "$PROG: git grep failed on $path (exit $rc)" >&2
      exit 2
      ;;
  esac
done < "$files"

if [ "$hits" -gt 0 ]; then
  echo "$PROG: retired vocabulary in $hits file(s) in scope; see AGENTS.md build table" >&2
  exit 1
fi
echo "$PROG: no retired vocabulary in scope"
