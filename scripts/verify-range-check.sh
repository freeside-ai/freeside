#!/usr/bin/env bash
# verify-range-check.sh: run a range check over a verified candidate.
#
# Usage: bash scripts/verify-range-check.sh <check-script>
# Local verification:
#   bash scripts/test-verify-range-check.sh
#
# Freeside's verification recipe runs a command that asks for history
# ("history": true) in a repository holding the candidate's commits, with
# FREESIDE_VERIFY_BASE_SHA and FREESIDE_VERIFY_HEAD_SHA in its environment
# (daemon/README.md). The recipe can't hand those to a check script itself:
# its argv isn't shell-expanded, and verify.ParseRecipe refuses `bash -c`.
# This wrapper runs `bash <check-script> <base-sha> <head-sha>`, the
# two-argument form scripts/check-commit-messages.sh and
# scripts/check-migration-history.sh take in CI, so those scripts stay
# unchanged for CI and the commit-msg hook.
#
# Exit codes:
#   2  usage error: no check script named, or either variable unset or empty
#   otherwise the check script's own exit code
set -euo pipefail

PROG=verify-range-check

usage() {
  echo "usage: FREESIDE_VERIFY_BASE_SHA=<sha> FREESIDE_VERIFY_HEAD_SHA=<sha> $PROG <check-script>" >&2
  exit 2
}

[ "$#" -eq 1 ] && [ -n "$1" ] || usage
[ -n "${FREESIDE_VERIFY_BASE_SHA:-}" ] || usage
[ -n "${FREESIDE_VERIFY_HEAD_SHA:-}" ] || usage

exec bash "$1" "$FREESIDE_VERIFY_BASE_SHA" "$FREESIDE_VERIFY_HEAD_SHA"
