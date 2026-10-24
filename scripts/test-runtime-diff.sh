#!/usr/bin/env bash
# Hermetic runtime-diff fixtures: a synthetic repository and a go stand-in
# on PATH, with no network access and no real toolchain.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
CHECK=$SCRIPT_DIR/runtime-diff.sh
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null
export GIT_AUTHOR_NAME=test GIT_AUTHOR_EMAIL=test@example.invalid
export GIT_COMMITTER_NAME=test GIT_COMMITTER_EMAIL=test@example.invalid
export GIT_AUTHOR_DATE='2026-01-01T00:00:00Z'
export GIT_COMMITTER_DATE='2026-01-01T00:00:00Z'
unset GIT_DIR GIT_WORK_TREE

# The stand-in "binary" is a digest of go.mod and the non-test Go sources,
# so a test-only edit leaves it unchanged and a main.go edit changes it, as
# the real reproducible build does. A source containing BUILD_FAILS fails.
mkdir "$TMP/bin"
cat >"$TMP/bin/go" <<'GO'
#!/usr/bin/env bash
set -euo pipefail
[ "${1-}" = build ] || exit 2
out=''
while [ "$#" -gt 0 ]; do
  case $1 in
    -o) out=$2; shift 2 ;;
    *) shift ;;
  esac
done
[ -n "$out" ] || exit 2
sources=$(find . \( -name go.mod -o -name '*.go' \) ! -name '*_test.go' | LC_ALL=C sort)
if grep -q BUILD_FAILS $sources; then
  echo "stand-in go: build failed" >&2
  exit 1
fi
for f in $sources; do printf '%s\n' "$f"; cat "$f"; done | git hash-object --stdin >"$out"
GO
chmod +x "$TMP/bin/go"
export PATH="$TMP/bin:$PATH"

pass=0
fail=0
OUT=''
RC=0
CASE=''

assert_rc() {
  if [ "$RC" -eq "$1" ]; then
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

assert_not_contains() {
  if [[ $OUT != *"$1"* ]]; then
    pass=$((pass + 1))
  else
    echo "FAIL [$CASE]: unexpected '$1': $OUT"
    fail=$((fail + 1))
  fi
}

REPO=$TMP/repo
commit() { # <message>; prints the new commit SHA
  git -C "$REPO" add -A
  git -C "$REPO" commit -qm "$1"
  git -C "$REPO" rev-parse HEAD
}

git init -q -b main "$REPO"
mkdir -p "$REPO/daemon/cmd/freesided" "$REPO/docs" "$REPO/images/x"
printf 'module example.invalid/daemon\n\ngo 1.26\n' >"$REPO/daemon/go.mod"
printf 'package main\n\nvar version string\n\nfunc main() {}\n' >"$REPO/daemon/cmd/freesided/main.go"
printf 'package main\n' >"$REPO/daemon/cmd/freesided/main_test.go"
printf 'Docs\n' >"$REPO/docs/a.md"
printf 'FROM scratch\n' >"$REPO/images/x/Containerfile"
C0=$(commit base)
printf 'More docs\n' >>"$REPO/docs/a.md"
C1=$(commit docs)
printf '\nfunc TestNothing() {}\n' >>"$REPO/daemon/cmd/freesided/main_test.go"
C2=$(commit test)
printf '\nfunc helper() {}\n' >>"$REPO/daemon/cmd/freesided/main.go"
C3=$(commit main)
printf 'LABEL x=y\n' >>"$REPO/images/x/Containerfile"
C4=$(commit image)
mkdir -p "$REPO/prompts" "$REPO/policy" "$REPO/scripts" "$REPO/daemon/cmd/freeside-export"
printf 'prompt\n' >"$REPO/prompts/p.md"
printf 'policy\n' >"$REPO/policy/p.yaml"
printf '#!/bin/sh\n' >"$REPO/scripts/build-x-image.sh"
printf '#!/bin/sh\n' >"$REPO/scripts/build-x.sh"
printf 'package main\n' >"$REPO/daemon/cmd/freeside-export/main_test.go"
C5=$(commit flagged)
printf '\n// BUILD_FAILS\n' >>"$REPO/daemon/cmd/freesided/main.go"
C6=$(commit broken)

run_check() { # [runtime-diff arguments...]; runs from a subdirectory
  RC=0
  OUT=$(cd "$REPO/docs" && bash "$CHECK" "$@" 2>&1) || RC=$?
}

CASE=same-sha
run_check "$C0" "$C0"
assert_rc 0
assert_contains "runtime-diff: carries"
assert_not_contains "pass build:"

CASE=docs-only
run_check "$C0" "$C1"
assert_rc 0
assert_contains "$(printf 'M\tdocs/a.md')"
assert_contains "pass build: "
assert_contains "runtime-diff: carries"

CASE=test-only
run_check "$C1" "$C2"
assert_rc 0
assert_contains "runtime-diff: carries"

CASE=main-go
run_check "$C2" "$C3"
assert_rc 1
assert_contains "runtime-diff: changed: freesided differs"
assert_not_contains "path "

CASE=image-only
run_check "$C3" "$C4"
assert_rc 1
assert_contains "runtime-diff: changed: path images/x/Containerfile"
assert_not_contains "freesided differs"

CASE=both
run_check "$C2" "$C4"
assert_rc 1
assert_contains "runtime-diff: changed: freesided differs; path images/x/Containerfile"

CASE=every-flagged-path
run_check "$C4" "$C5"
assert_rc 1
assert_contains "runtime-diff: changed: path daemon/cmd/freeside-export/main_test.go; path policy/p.yaml; path prompts/p.md; path scripts/build-x-image.sh"
assert_not_contains "path scripts/build-x.sh"
assert_not_contains "freesided differs"

CASE=one-argument
run_check "$C0"
assert_rc 2
assert_contains "usage:"

CASE=unknown-sha
run_check "$C0" deadbeef
assert_rc 3
assert_contains "cannot resolve run SHA 'deadbeef'"

CASE=build-fails
run_check "$C5" "$C6"
assert_rc 3
assert_contains "go build failed at $C6"
assert_not_contains "runtime-diff: carries"

CASE=no-go
mkdir "$TMP/empty"
RC=0
OUT=$(cd "$REPO" && PATH="$TMP/empty" "$BASH" "$CHECK" "$C0" "$C1" 2>&1) || RC=$?
assert_rc 2
assert_contains "go is not on PATH"

CASE=not-a-repository
mkdir "$TMP/outside"
RC=0
OUT=$(cd "$TMP/outside" && GIT_CEILING_DIRECTORIES=$TMP bash "$CHECK" "$C0" "$C1" 2>&1) || RC=$?
assert_rc 2
assert_contains "not inside a git worktree"

CASE=checkout-unchanged
OUT="status=[$(git -C "$REPO" status --porcelain)] head=$(git -C "$REPO" rev-parse HEAD)"
OUT="$OUT worktrees=$(git -C "$REPO" worktree list --porcelain | grep -c '^worktree ')"
if [ "$OUT" = "status=[] head=$C6 worktrees=1" ]; then
  pass=$((pass + 1))
else
  echo "FAIL [$CASE]: checkout changed or a worktree appeared: $OUT"
  fail=$((fail + 1))
fi

echo "runtime-diff fixtures: $pass assertions passed, $fail failed"
[ "$fail" -eq 0 ]
