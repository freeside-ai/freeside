#!/usr/bin/env bash
# test-record-screenshots.sh — hermetic regression suite for
# app/scripts/record-screenshots.sh.
#
# Uses synthetic git repositories and PATH stand-ins for uname and swift so
# the suite runs without Xcode, Swift, or network access on macOS and Linux.
# Exit code: 0 when every assertion passes, 1 otherwise.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
SCRIPT_UNDER_TEST=${FREESIDE_RECORD_SCREENSHOTS_UNDER_TEST:-$SCRIPT_DIR/../app/scripts/record-screenshots.sh}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.invalid
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.invalid
export GIT_AUTHOR_DATE='2026-01-01T00:00:00Z'
export GIT_COMMITTER_DATE='2026-01-01T00:00:00Z'
unset GIT_DIR GIT_WORK_TREE FREESIDE_RECORD_SCREENSHOTS FREESIDE_SCREENSHOT_FAMILY FREESIDE_SCREENSHOT_SHARD

STUB_BIN=$TMP/bin
mkdir -p "$STUB_BIN"

cat >"$STUB_BIN/uname" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "${STUB_UNAME:-Darwin}"
STUB
chmod +x "$STUB_BIN/uname"

# Stub `swift test` recorder/verifier. Computes deterministic digests from
# `app/Sources/surface-<family>.txt` when present so commits and rebases see
# real content-derived digests. Logs every invocation to STUB_SWIFT_LOG.
cat >"$STUB_BIN/swift" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
pkg=''
while [[ $# -gt 0 ]]; do
  case "$1" in
    --package-path) pkg=$2; shift 2 ;;
    *) shift ;;
  esac
done
mode=verify
if [[ ${FREESIDE_RECORD_SCREENSHOTS:-0} == 1 ]]; then
  mode=record
fi
if [[ -n ${STUB_SWIFT_LOG:-} ]]; then
  printf '%s\tfamily=%s\tshard=%s\tpkg=%s\n' \
    "$mode" "${FREESIDE_SCREENSHOT_FAMILY:-<all>}" "${FREESIDE_SCREENSHOT_SHARD:-<unset>}" "$pkg" \
    >>"$STUB_SWIFT_LOG"
fi
if [[ -n ${STUB_FAIL_MODE:-} && $STUB_FAIL_MODE == "$mode" ]]; then
  echo "stub swift failed in $mode mode" >&2
  exit 1
fi
if [[ -n ${STUB_FAIL_ON_CONTENT:-} && -f $pkg/Sources/surface-alpha.txt ]] \
    && grep -qF "$STUB_FAIL_ON_CONTENT" "$pkg/Sources/surface-alpha.txt"; then
  echo "stub swift failed on content $STUB_FAIL_ON_CONTENT" >&2
  exit 1
fi

res="$pkg/Tests/FreesideCoreTests/Resources"
fams=()
if [[ -n ${FREESIDE_SCREENSHOT_FAMILY:-} ]]; then
  IFS=, read -r -a fams <<< "$FREESIDE_SCREENSHOT_FAMILY"
else
  for f in "$res"/ScreenshotDigests-*.json; do
    [[ -e $f ]] || continue
    base=$(basename "$f")
    base=${base#ScreenshotDigests-}
    fams+=("${base%.json}")
  done
fi

expected_json() {
  local fam=$1 src="$pkg/Sources/surface-$fam.txt" extra="$pkg/Sources/surface-$fam-extra.txt" content="default-$fam"
  if [[ -f $src ]]; then
    content=$(cat "$src")
  fi
  if [[ -f $extra ]]; then
    content="${content}+$(cat "$extra")"
  fi
  printf '{\n  "%s-large" : "digest-%s"\n}\n' "$fam" "$content"
}

for fam in "${fams[@]}"; do
  target="$res/ScreenshotDigests-$fam.json"
  want=$(expected_json "$fam")
  if [[ $mode == record ]]; then
    printf '%s\n' "$want" >"$target"
  else
    if [[ ! -f $target ]]; then
      echo "stub verify: missing $target" >&2
      exit 1
    fi
    got=$(cat "$target")
    if [[ $got != "$want" ]]; then
      echo "stub verify mismatch for $fam: got '$got', want '$want'" >&2
      exit 1
    fi
  fi
done
STUB
chmod +x "$STUB_BIN/swift"

pass=0
fail=0
CASE=''
OUT=''
RC=0
REPO=''
LOG=''

assert_rc() {
  if [[ $RC -eq $1 ]]; then
    pass=$((pass + 1))
  else
    echo "FAIL [$CASE]: expected rc=$1, got rc=$RC: $OUT"
    fail=$((fail + 1))
  fi
}

assert_contains() {
  if [[ $OUT == *"$1"* ]]; then
    pass=$((pass + 1))
  else
    echo "FAIL [$CASE]: missing '$1': $OUT"
    fail=$((fail + 1))
  fi
}

assert_eq() {
  if [[ $1 == "$2" ]]; then
    pass=$((pass + 1))
  else
    echo "FAIL [$CASE]: expected '$2', got '$1'"
    fail=$((fail + 1))
  fi
}

commit_all() {
  git -C "$REPO" add -A
  git -C "$REPO" commit -qm "$1"
}

new_repo() {
  CASE=$1
  REPO=$TMP/$CASE
  LOG=$TMP/$CASE.swift.log
  : >"$LOG"
  git init -q -b main "$REPO"
  mkdir -p "$REPO/app/scripts" \
    "$REPO/app/Sources" \
    "$REPO/app/Tests/FreesideCoreTests/Resources"
  cp "$SCRIPT_UNDER_TEST" "$REPO/app/scripts/record-screenshots.sh"
  chmod +x "$REPO/app/scripts/record-screenshots.sh"
  printf 'v1\n' >"$REPO/app/Sources/surface-alpha.txt"
  printf 'v1\n' >"$REPO/app/Sources/surface-beta.txt"
  printf '{\n  "alpha-large" : "digest-v1"\n}\n' \
    >"$REPO/app/Tests/FreesideCoreTests/Resources/ScreenshotDigests-alpha.json"
  printf '{\n  "beta-large" : "digest-v1"\n}\n' \
    >"$REPO/app/Tests/FreesideCoreTests/Resources/ScreenshotDigests-beta.json"
  printf '{}\n' >"$REPO/app/Tests/FreesideCoreTests/Resources/ScreenshotDigestOverrides.json"
  commit_all 'Initial base'
}

run_script() {
  RC=0
  OUT=$(PATH="$STUB_BIN:$PATH" STUB_SWIFT_LOG="$LOG" \
    bash "$REPO/app/scripts/record-screenshots.sh" "$@" 2>&1) || RC=$?
}

# 1. Argument validation.
new_repo usage
run_script
assert_rc 2
assert_contains 'usage:'
run_script --all alpha
assert_rc 2
run_script --resolve alpha
assert_rc 2
run_script --each-commit main
assert_rc 2
run_script 'bad/name'
assert_rc 2

# 2. Direct family recording records then verifies only the requested family,
#    preserving unrelated working-tree edits and leaving other families untouched.
new_repo direct-record
printf 'v2\n' >"$REPO/app/Sources/surface-alpha.txt"
printf 'unrelated\n' >"$REPO/notes.txt"
run_script alpha
assert_rc 0
assert_eq "$(grep -c '^record	family=alpha	' "$LOG")" "1"
assert_eq "$(grep -c '^verify	family=alpha	' "$LOG")" "1"
assert_eq "$(cat "$REPO/app/Tests/FreesideCoreTests/Resources/ScreenshotDigests-alpha.json")" \
  $'{\n  "alpha-large" : "digest-v2"\n}'
assert_eq "$(cat "$REPO/app/Tests/FreesideCoreTests/Resources/ScreenshotDigests-beta.json")" \
  $'{\n  "beta-large" : "digest-v1"\n}'
assert_eq "$(cat "$REPO/notes.txt")" "unrelated"

# 3. Stop on recorder failure; verification never runs after failed record.
new_repo record-failure
RC=0
OUT=$(PATH="$STUB_BIN:$PATH" STUB_SWIFT_LOG="$LOG" STUB_FAIL_MODE=record \
  bash "$REPO/app/scripts/record-screenshots.sh" alpha 2>&1) || RC=$?
assert_rc 1
assert_eq "$(wc -l <"$LOG" | tr -d ' ')" "1"
assert_eq "$(cat "$REPO/app/Tests/FreesideCoreTests/Resources/ScreenshotDigests-alpha.json")" \
  $'{\n  "alpha-large" : "digest-v1"\n}'

# 4. --all unsets any ambient FREESIDE_SCREENSHOT_FAMILY and FREESIDE_SCREENSHOT_SHARD.
new_repo all-unsets-env
RC=0
OUT=$(PATH="$STUB_BIN:$PATH" STUB_SWIFT_LOG="$LOG" \
  FREESIDE_SCREENSHOT_FAMILY=alpha FREESIDE_SCREENSHOT_SHARD=0/2 \
  bash "$REPO/app/scripts/record-screenshots.sh" --all 2>&1) || RC=$?
assert_rc 0
assert_eq "$(grep -c '^record	family=<all>	shard=<unset>	' "$LOG")" "1"
assert_eq "$(grep -c '^verify	family=<all>	shard=<unset>	' "$LOG")" "1"

# 5. --resolve refuses when not in a rebase.
new_repo resolve-outside-rebase
run_script --resolve
assert_rc 1
assert_contains 'not at a stopped rebase'

# 6. --resolve re-records and stages ONLY conflicted digest files, preserving
#    unrelated untracked files and not continuing the rebase.
new_repo resolve-digest-only
git -C "$REPO" checkout -qb branch-a
printf 'vA\n' >"$REPO/app/Sources/surface-alpha.txt"
printf '{\n  "alpha-large" : "digest-vA"\n}\n' \
  >"$REPO/app/Tests/FreesideCoreTests/Resources/ScreenshotDigests-alpha.json"
commit_all 'Branch A updates alpha'

git -C "$REPO" checkout -q main
git -C "$REPO" checkout -qb branch-b
printf 'vB\n' >"$REPO/app/Sources/surface-alpha-extra.txt"
printf '{\n  "alpha-large" : "digest-v1+vB"\n}\n' \
  >"$REPO/app/Tests/FreesideCoreTests/Resources/ScreenshotDigests-alpha.json"
commit_all 'Branch B touches another surface in alpha'

if git -C "$REPO" rebase branch-a >/dev/null 2>&1; then
  echo "FAIL [$CASE]: expected rebase conflict in alpha digest file"
  fail=$((fail + 1))
fi
printf 'keep me\n' >"$REPO/scratch.txt"
run_script --resolve
assert_rc 0
assert_contains 're-recorded and staged conflicted families: alpha'
# Only alpha was recorded and staged; rebase is still paused, not continued.
assert_eq "$(grep -c '^record	family=alpha	' "$LOG")" "1"
assert_eq "$(git -C "$REPO" diff --name-only --diff-filter=U)" ""
assert_eq "$(git -C "$REPO" diff --cached --name-only -- app/Tests/FreesideCoreTests/Resources/ScreenshotDigests-alpha.json)" \
  "app/Tests/FreesideCoreTests/Resources/ScreenshotDigests-alpha.json"
assert_eq "$(cat "$REPO/app/Tests/FreesideCoreTests/Resources/ScreenshotDigests-alpha.json")" \
  $'{\n  "alpha-large" : "digest-vA+vB"\n}'
assert_eq "$(cat "$REPO/scratch.txt")" "keep me"
if [[ ! -d "$REPO/.git/rebase-merge" && ! -d "$REPO/.git/rebase-apply" ]]; then
  echo "FAIL [$CASE]: --resolve must not continue the rebase"
  fail=$((fail + 1))
else
  pass=$((pass + 1))
fi
GIT_EDITOR=true git -C "$REPO" rebase --continue >/dev/null

# 7. --resolve refuses when any source file (or non-family path) is unmerged,
#    runs no recorder, and stages nothing.
new_repo resolve-refuses-source-conflict
git -C "$REPO" checkout -qb src-a
printf 'side-a\n' >"$REPO/app/Sources/surface-alpha.txt"
printf '{\n  "alpha-large" : "digest-side-a"\n}\n' \
  >"$REPO/app/Tests/FreesideCoreTests/Resources/ScreenshotDigests-alpha.json"
commit_all 'Side A changes source and digest'

git -C "$REPO" checkout -q main
git -C "$REPO" checkout -qb src-b
printf 'side-b\n' >"$REPO/app/Sources/surface-alpha.txt"
printf '{\n  "alpha-large" : "digest-side-b"\n}\n' \
  >"$REPO/app/Tests/FreesideCoreTests/Resources/ScreenshotDigests-alpha.json"
commit_all 'Side B changes source and digest'

if git -C "$REPO" rebase src-a >/dev/null 2>&1; then
  echo "FAIL [$CASE]: expected source + digest rebase conflict"
  fail=$((fail + 1))
fi
run_script --resolve
assert_rc 1
assert_contains 'unmerged path(s) outside screenshot digest families'
assert_contains 'app/Sources/surface-alpha.txt'
assert_eq "$(wc -l <"$LOG" | tr -d ' ')" "0"
assert_eq "$(git -C "$REPO" diff --name-only --diff-filter=U | grep -c 'surface-alpha.txt')" "1"
git -C "$REPO" rebase --abort

# 8. --resolve stops without staging if recording fails.
new_repo resolve-record-failure
git -C "$REPO" checkout -qb rf-a
printf '{\n  "alpha-large" : "digest-rf-a"\n}\n' \
  >"$REPO/app/Tests/FreesideCoreTests/Resources/ScreenshotDigests-alpha.json"
commit_all 'RF A'
git -C "$REPO" checkout -q main
git -C "$REPO" checkout -qb rf-b
printf '{\n  "alpha-large" : "digest-rf-b"\n}\n' \
  >"$REPO/app/Tests/FreesideCoreTests/Resources/ScreenshotDigests-alpha.json"
commit_all 'RF B'
git -C "$REPO" rebase rf-a >/dev/null 2>&1 || true
RC=0
OUT=$(PATH="$STUB_BIN:$PATH" STUB_SWIFT_LOG="$LOG" STUB_FAIL_MODE=record \
  bash "$REPO/app/scripts/record-screenshots.sh" --resolve 2>&1) || RC=$?
assert_rc 1
assert_eq "$(git -C "$REPO" diff --name-only --diff-filter=U)" \
  "app/Tests/FreesideCoreTests/Resources/ScreenshotDigests-alpha.json"
git -C "$REPO" rebase --abort

# 9. --each-commit replays recording across commits and amends only the target
#    family file, while refusing dirty working trees.
new_repo each-commit
BASE=$(git -C "$REPO" rev-parse HEAD)
git -C "$REPO" checkout -qb feature
printf 'v2\n' >"$REPO/app/Sources/surface-alpha.txt"
commit_all 'First alpha change'
printf 'v3\n' >"$REPO/app/Sources/surface-alpha.txt"
commit_all 'Second alpha change'

# Dirty tracked file must be refused before rebase starts.
printf 'dirty\n' >"$REPO/app/Sources/surface-beta.txt"
run_script --each-commit "$BASE" alpha
assert_rc 1
assert_contains 'uncommitted tracked changes'
assert_eq "$(cat "$REPO/app/Sources/surface-beta.txt")" "dirty"
printf 'v1\n' >"$REPO/app/Sources/surface-beta.txt"

# Untracked file is preserved while each commit is amended with its digest.
printf 'untracked\n' >"$REPO/unrelated.txt"
run_script --each-commit "$BASE" alpha
assert_rc 0
assert_eq "$(cat "$REPO/unrelated.txt")" "untracked"
c1_digest=$(git -C "$REPO" show HEAD~1:app/Tests/FreesideCoreTests/Resources/ScreenshotDigests-alpha.json)
c2_digest=$(git -C "$REPO" show HEAD:app/Tests/FreesideCoreTests/Resources/ScreenshotDigests-alpha.json)
assert_eq "$c1_digest" $'{\n  "alpha-large" : "digest-v2"\n}'
assert_eq "$c2_digest" $'{\n  "alpha-large" : "digest-v3"\n}'

# 10. --each-commit stops at the first failing commit without advancing further.
new_repo each-commit-stops-on-failure
BASE=$(git -C "$REPO" rev-parse HEAD)
git -C "$REPO" checkout -qb feature
printf 'bad-commit\n' >"$REPO/app/Sources/surface-alpha.txt"
commit_all 'Failing commit'
printf 'later-commit\n' >"$REPO/app/Sources/surface-alpha.txt"
commit_all 'Later commit'
RC=0
OUT=$(PATH="$STUB_BIN:$PATH" STUB_SWIFT_LOG="$LOG" STUB_FAIL_ON_CONTENT=bad-commit \
  bash "$REPO/app/scripts/record-screenshots.sh" --each-commit "$BASE" alpha 2>&1) || RC=$?
assert_rc 1
assert_eq "$(git -C "$REPO" log -1 --format=%s)" "Failing commit"
git -C "$REPO" rebase --abort

# 11. --amend-step refuses outside a rebase and leaves HEAD unchanged.
new_repo amend-step-outside-rebase
printf 'v2\n' >"$REPO/app/Sources/surface-alpha.txt"
head_before=$(git -C "$REPO" rev-parse HEAD)
run_script --amend-step alpha
assert_rc 1
assert_contains 'refusing --amend-step outside a rebase'
assert_eq "$(git -C "$REPO" rev-parse HEAD)" "$head_before"
assert_eq "$(wc -l <"$LOG" | tr -d ' ')" "0"

echo "record-screenshots fixtures: $pass assertions passed, $fail failed"
[[ $fail -eq 0 ]]
