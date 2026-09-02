#!/usr/bin/env bash
# check-vocabulary.sh — refuse the pre-rename stage vocabulary (#986).
#
# Usage: check-vocabulary.sh [<repo-root>]
#
# The specification stage was renamed from its earlier name in #986. This
# check greps tracked files for the retired word stem (case-insensitive) in
# the surfaces the rename covers and fails when it reappears:
#
#   daemon/**/*.go   non-test Go code, except one legacy_vocabulary.go per
#                    package, which is the only place pre-rename literals
#                    may live (they canonicalize stored rows on decode)
#   daemon/migrations/*.sql   files numbered above 0064; earlier migrations
#                    predate the rename and 0064 must name the old column
#                    it renames
#   api/ app/ prompts/ scripts/   every tracked file
#
# devlog/ and docs/history/ are frozen records; docs/ prose is reviewed by
# hand because the word also has an ordinary English sense. The retired stem
# is assembled at runtime so this file passes its own check. The script
# performs no network I/O and does not mutate the repository.
#
# Exit codes:
#   0  no tracked file in scope spells the retired vocabulary
#   1  one or more hits, listed as path:line:text
#   2  usage/environment error
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
if ! git -C "$root" rev-parse --show-toplevel >/dev/null 2>&1; then
  echo "$PROG: $root is not inside a git repository" >&2
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

hits=0
while IFS= read -r path; do
  in_scope "$path" || continue
  if out=$(git -C "$root" grep -I -i -n -e "$stem" -- "$path"); then
    printf '%s\n' "$out"
    hits=$((hits + 1))
  fi
done < <(git -C "$root" ls-files -- daemon api app prompts scripts)

if [ "$hits" -gt 0 ]; then
  echo "$PROG: retired vocabulary in $hits tracked file(s); see AGENTS.md build table" >&2
  exit 1
fi
echo "$PROG: no retired vocabulary in scope"
