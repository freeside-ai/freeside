#!/usr/bin/env bash
# test-mktemp-templates.sh — every mktemp template ends in its X run.
#
# BSD mktemp (macOS, the real-work reference platform) replaces only a
# trailing run of Xs, so a template such as name.XXXXXX.json creates that
# literal file and fails with "File exists" on its second use (#1186). This
# suite proves the scan on synthetic lines, then scans every tracked file
# except Markdown. The X run is assembled at runtime so this file passes its
# own scan. Reads the checkout only; no network. Exit code: 0 when every
# assertion passes, 1 otherwise.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$SCRIPT_DIR/.." && pwd)

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

xs="XX""X"
# A mktemp call whose X run is followed by more template text. Only what
# ends the shell word ends the template: a space, paren, or command
# separator. A slash continues it, and closing quotes do too, since the
# shell joins "name.XXX".json into one argument.
pattern="mktemp.*${xs}+[\"']*[^X\"'[:space:]);|&]"

pass=0
fail=0
CASE=''
OUT=''

begin_case() {
  CASE=$1
  echo "case: $CASE"
}

report_failure() {
  fail=$((fail + 1))
  echo "FAIL [$CASE]: $*"
  printf '%s\n' "$OUT" | sed 's/^/    | /'
}

begin_case "a suffixed template is reported"
printf '%s\n' \
  "path=\$(mktemp \"\$dir/name.${xs}${xs}.json\")" \
  "mktemp -t name.${xs}${xs}-log" \
  "path=\$(mktemp \"\$dir/name.${xs}${xs}\".json)" \
  "path=\$(mktemp '\$dir/name.${xs}${xs}'\"\$ext\")" \
  "mktemp -d \"\$dir/name.${xs}${xs}/sub\"" >"$TMP/suffixed.sh"
OUT=$(grep -nE "$pattern" "$TMP/suffixed.sh" || true)
if [[ $(printf '%s\n' "$OUT" | grep -c .) -eq 5 ]]; then
  pass=$((pass + 1))
else
  report_failure "expected all five suffixed templates to be reported"
fi

begin_case "trailing templates pass"
printf '%s\n' \
  "path=\$(mktemp \"\$dir/name.${xs}${xs}\")" \
  "TMP=\$(mktemp -d)" \
  "work=\$(mktemp -d \"\$TMP/session.${xs}${xs}\") || exit 1" \
  "mktemp -t name.${xs}${xs}" \
  "mktemp '\$dir/name.${xs}${xs}'; echo done" \
  "mktemp \"\$dir/a.${xs}${xs}\" \"\$dir/b.${xs}${xs}\"" >"$TMP/trailing.sh"
OUT=$(grep -nE "$pattern" "$TMP/trailing.sh" || true)
if [[ -z "$OUT" ]]; then
  pass=$((pass + 1))
else
  report_failure "a trailing template was reported"
fi

begin_case "no tracked file uses a suffixed template"
OUT=$(git -C "$ROOT" grep -nE "$pattern" -- ':!*.md' || true)
if [[ -z "$OUT" ]]; then
  pass=$((pass + 1))
else
  report_failure "suffixed mktemp templates (BSD mktemp keeps them literal)"
fi

echo
echo "passed $pass assertion(s), failed $fail"
if [ "$fail" -gt 0 ]; then
  exit 1
fi
