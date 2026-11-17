#!/usr/bin/env bash
# check-credential-literals.sh — fail when a tracked file carries a line the
# daemon's credential rules match.
#
# Usage: check-credential-literals.sh [<repo-root>]
#
# Freeside runs agents on this repository. An agent that reads a tracked
# file echoes it into its transcript, which the export scan reads, and may
# quote it in a specification or reply, which the importer screens. Both
# apply internal/credentialpattern's rules, so one credential-shaped fixture
# in the tree can refuse a handoff that leaked nothing (#3305). This check
# applies the same rules, through daemon/cmd/freeside-credential-literals,
# to every file `git ls-files` lists, so the gates and the check can't drift.
#
# Each match prints as `path:line: rule-id`, never the matched text, so a
# real credential committed by mistake doesn't reach a CI log.
#
# Environment:
#   CREDENTIAL_LITERALS  command to run instead of building the tool (the
#                        regression suite's stand-in); it reads the
#                        NUL-separated paths on stdin.
#
# It needs Go for the default tool build, performs no network I/O beyond
# what that build needs, and does not mutate the repository.
#
# Exit codes:
#   0  no tracked file matches
#   1  one or more matches, each listed
#   2  usage/environment error: <repo-root> is not a git work tree, the tool
#      failed to build, or a file couldn't be read
set -euo pipefail

PROG=check-credential-literals

if [ "$#" -gt 1 ]; then
  echo "usage: $PROG [<repo-root>]" >&2
  exit 2
fi
root=${1:-.}
if ! git -C "$root" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  echo "$PROG: $root is not a git work tree" >&2
  exit 2
fi
root=$(git -C "$root" rev-parse --show-toplevel)

if [ -n "${CREDENTIAL_LITERALS:-}" ]; then
  # shellcheck disable=SC2206 # the override may carry arguments
  tool=($CREDENTIAL_LITERALS)
else
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  if ! go -C "$root/daemon" build -o "$tmp/freeside-credential-literals" ./cmd/freeside-credential-literals; then
    echo "$PROG: building the scanner failed" >&2
    exit 2
  fi
  tool=("$tmp/freeside-credential-literals")
fi

status=0
(cd "$root" && git ls-files -z | "${tool[@]}") || status=$?
case "$status" in
  0) ;;
  1)
    cat >&2 <<'EOF'
check-credential-literals: tracked files carry credential-shaped text.
  An agent working on this repository can quote it into an export the
  credential scan refuses (#3305). Build fixtures at runtime instead:
  split a Go literal after its prefix ("sk-ant-" + "..."), escape one
  character in JSON ("ghs_..."), or keep a key's body without its
  armor and add the armor in the test.
EOF
    ;;
  *)
    echo "$PROG: the scanner failed (exit $status)" >&2
    exit 2
    ;;
esac
exit "$status"
