#!/usr/bin/env bash
# api-compatibility-check.sh — keep api/openapi.yaml's compatibility level
# honest (#3109).
#
# The spec's `x-compatibility-level` counts changes that break an app built
# before them, and `x-compatibility-floor` names the lowest app level a
# daemon built from the spec still serves (api/README.md, Compatibility
# Level). This check compares api/openapi.yaml with the same file at a base
# commit and fails when:
#   - oasdiff finds a breaking change (ERR level, under the project's
#     severity file api/oasdiff-severity.txt) and the level did not rise;
#   - the level went down;
#   - the floor went down;
#   - the floor exceeds the level, or either line is missing or malformed
#     (scripts/api-contract-digest.sh --compatibility rejects these).
# A base spec with neither line reads as level 0 and floor 0, the values
# every build before #3109 implicitly had.
#
# Usage:
#   api-compatibility-check.sh [--base <ref>]
# With no --base, the base is `git merge-base HEAD origin/main`; the check
# fails, rather than passing silently, when origin/main is absent, so a
# shallow CI checkout must fetch it first.
#
# Environment:
#   OASDIFF  command for oasdiff (default: `go run` of the pinned module
#            below; CI installs a checksum-verified binary of the same
#            version in .github/workflows/api-ci.yml). Bump the version
#            here, there, and in api/README.md together.
#
# Requires git and jq. oasdiff runs with --format json and no --fail-on,
# so a non-zero exit always means oasdiff itself failed (`go run` turns
# every non-zero exit into 1, which would hide the difference), and jq
# counts the ERR-level changes.
#
# Exit codes: 0 the level is honest; 1 a rule failed or a tool failed;
# 2 usage error.
set -euo pipefail

PROG=$(basename "$0")
ROOT=$(cd "$(dirname "$0")/.." && pwd)
OASDIFF=${OASDIFF:-go run github.com/oasdiff/oasdiff@v1.35.0}
SPEC=api/openapi.yaml
SEVERITY=api/oasdiff-severity.txt

usage() {
  echo "usage: $PROG [--base <ref>]" >&2
  exit 2
}

base=
while [ $# -gt 0 ]; do
  case $1 in
    --base)
      [ $# -ge 2 ] && [ -n "$2" ] || usage
      base=$2
      shift 2
      ;;
    *) usage ;;
  esac
done

cd "$ROOT"

if [ -z "$base" ]; then
  if ! git rev-parse --verify --quiet 'origin/main^{commit}' >/dev/null; then
    echo "$PROG: origin/main is not available to compare against; run \`git fetch origin main\` or pass --base <ref>" >&2
    exit 1
  fi
  base=$(git merge-base HEAD origin/main)
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
if ! git show "$base:$SPEC" >"$TMP/base.yaml"; then
  echo "$PROG: cannot read $SPEC at $base" >&2
  exit 1
fi

if grep -q '^  x-compatibility-' "$TMP/base.yaml"; then
  base_levels=$(bash scripts/api-contract-digest.sh --compatibility "$TMP/base.yaml")
else
  base_levels='0 0'
fi
head_levels=$(bash scripts/api-contract-digest.sh --compatibility "$SPEC")
read -r base_level base_floor <<<"$base_levels"
read -r head_level head_floor <<<"$head_levels"

# shellcheck disable=SC2086 # OASDIFF may carry arguments (the go run form)
if ! report=$($OASDIFF breaking "$TMP/base.yaml" "$SPEC" --flatten-allof \
  --severity-levels "$SEVERITY" --format json); then
  echo "$PROG: oasdiff failed comparing $SPEC with $base" >&2
  exit 1
fi
# Every reported change is an object with an id and a numeric level (3 is
# ERR); fail loudly if the report's shape is not what this filter expects.
if ! breaking=$(printf '%s' "$report" | jq -r '
  [.. | objects | select(has("id") and has("level"))]
  | map(if (.level | type) == "number" then . else error("oasdiff level is not a number: \(.level)") end)
  | map(select(.level >= 3))
  | .[] | "  \(.id): \(.text // "")"'); then
  echo "$PROG: cannot read oasdiff's report" >&2
  exit 1
fi

status=0
if [ "$head_level" -lt "$base_level" ]; then
  echo "x-compatibility-level went down from $base_level to $head_level; it never goes down. Set it back to $base_level or higher." >&2
  status=1
fi
if [ "$head_floor" -lt "$base_floor" ]; then
  echo "x-compatibility-floor went down from $base_floor to $head_floor; it never goes down. Set it back to $base_floor or higher." >&2
  status=1
fi
if [ -n "$breaking" ]; then
  echo "oasdiff found changes that break an app built before them:"
  printf '%s\n' "$breaking"
  if [ "$head_level" -le "$base_level" ]; then
    echo "these need x-compatibility-level raised from $base_level to $((base_level + 1)) in $SPEC, and x-compatibility-floor with it unless the daemon keeps serving the old shape (api/README.md, Compatibility Level)" >&2
    status=1
  fi
fi
if [ "$status" -ne 0 ]; then
  exit "$status"
fi
echo "compatibility level $head_level (base $base_level), floor $head_floor (base $base_floor): honest against $base"
