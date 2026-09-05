#!/usr/bin/env bash
# test-pre-commit.sh — fixtures for the .githooks/pre-commit API-client
# regeneration hook.
#
# Each case builds an isolated synthetic repository with a fixed identity, a
# copy of the shipped hook and generate-api-client.sh, and a stand-in `swift`
# on PATH that records its arguments and regenerates Types.swift as a fixed
# transform of the schema mirror. No Swift toolchain and no network.
# Exit code: 0 when every assertion passes, 1 otherwise.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
ROOT=$(cd "$SCRIPT_DIR/.." && pwd)

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null

pass=0
fail=0
case_number=0
CASE=''
OUT=''
RC=0
REPO=''
BINDIR=''
ARGSFILE=''

begin_case() {
  case_number=$((case_number + 1))
  CASE=$(printf '%02d %s' "$case_number" "$1")
  echo "case: $CASE"
}

report_failure() {
  fail=$((fail + 1))
  echo "FAIL [$CASE]: $*"
  printf '%s\n' "$OUT" | sed 's/^/    | /'
}

assert_rc() {
  if [ "$RC" -eq "$1" ]; then
    pass=$((pass + 1))
  else
    report_failure "expected rc=$1, got rc=$RC"
  fi
}

assert_contains() {
  case $OUT in
    *"$1"*) pass=$((pass + 1)) ;;
    *) report_failure "output does not contain: $1" ;;
  esac
}

assert_file_exists() {
  if [ -e "$1" ]; then
    pass=$((pass + 1))
  else
    report_failure "expected file to exist: $1"
  fi
}

assert_file_absent() {
  if [ ! -e "$1" ]; then
    pass=$((pass + 1))
  else
    report_failure "expected file to be absent: $1"
  fi
}

# make_swift_stub writes a `swift` stand-in that records its arguments and
# regenerates Types.swift from the schema mirror it finds through
# --package-path. Client.swift is left untouched, mirroring a second generated
# file the schema transform does not rewrite, so a deletion of it surfaces as
# drift.
make_swift_stub() { # <bindir> <argsfile>
  local bindir=$1 argsfile=$2
  cat >"$bindir/swift" <<STUB
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "\$@" >>"$argsfile"
pkg=
prev=
for a in "\$@"; do
  if [ "\$prev" = --package-path ]; then pkg=\$a; fi
  prev=\$a
done
gen="\$pkg/Sources/FreesideAPI/GeneratedSources"
mkdir -p "\$gen"
{ printf 'generated-from:\n'; cat "\$pkg/Sources/FreesideAPI/openapi.yaml"; } \
  >"\$gen/Types.swift"
STUB
  chmod +x "$bindir/swift"
}

# setup_repo builds a fresh in-sync fixture and sets REPO, BINDIR, ARGSFILE.
# The initial commit predates core.hooksPath, so it bypasses the hook.
setup_repo() {
  REPO=$TMP/case$case_number
  BINDIR=$TMP/bin$case_number
  ARGSFILE=$TMP/case$case_number.swiftargs
  mkdir -p "$BINDIR"
  make_swift_stub "$BINDIR" "$ARGSFILE"

  git init -q -b main "$REPO"
  git -C "$REPO" config user.name test
  git -C "$REPO" config user.email test@example.invalid
  git -C "$REPO" config commit.gpgsign false

  mkdir -p "$REPO/api" \
    "$REPO/app/Sources/FreesideAPI/GeneratedSources" \
    "$REPO/app/scripts" "$REPO/.githooks"
  printf 'schema: v1\n' >"$REPO/api/openapi.yaml"
  cp "$REPO/api/openapi.yaml" "$REPO/app/Sources/FreesideAPI/openapi.yaml"
  printf 'generator: config\n' \
    >"$REPO/app/Sources/FreesideAPI/openapi-generator-config.yaml"
  printf 'resolved: pins\n' >"$REPO/app/Package.resolved"
  { printf 'generated-from:\n'; cat "$REPO/api/openapi.yaml"; } \
    >"$REPO/app/Sources/FreesideAPI/GeneratedSources/Types.swift"
  printf 'client: fixed\n' \
    >"$REPO/app/Sources/FreesideAPI/GeneratedSources/Client.swift"
  printf 'unrelated\n' >"$REPO/notes.txt"
  cp "$ROOT/app/scripts/generate-api-client.sh" \
    "$REPO/app/scripts/generate-api-client.sh"
  chmod +x "$REPO/app/scripts/generate-api-client.sh"
  cp "$ROOT/.githooks/pre-commit" "$REPO/.githooks/pre-commit"
  chmod +x "$REPO/.githooks/pre-commit"

  git -C "$REPO" add -A
  git -C "$REPO" commit -q -m 'Establish fixture'
  git -C "$REPO" config core.hooksPath .githooks
}

commit_git() { # <git-args...>; runs in REPO with the stub on PATH, sets OUT/RC
  set +e
  OUT=$(PATH="$BINDIR:$PATH" git -C "$REPO" "$@" 2>&1)
  RC=$?
  set -e
}

begin_case "schema edit staged without regeneration is refused, then succeeds"
setup_repo
printf 'schema: v2\n' >"$REPO/api/openapi.yaml"
git -C "$REPO" add api/openapi.yaml
commit_git commit -q -m 'Change the schema'
assert_rc 1
assert_contains 'app/Sources/FreesideAPI/openapi.yaml'
assert_contains 'app/Sources/FreesideAPI/GeneratedSources/Types.swift'
assert_file_exists "$ARGSFILE"
git -C "$REPO" add app/Sources/FreesideAPI/openapi.yaml \
  app/Sources/FreesideAPI/GeneratedSources
commit_git commit -q -m 'Change the schema and regenerate the client'
assert_rc 0

begin_case "schema edit with regenerated output staged succeeds"
setup_repo
printf 'schema: v2\n' >"$REPO/api/openapi.yaml"
cp "$REPO/api/openapi.yaml" "$REPO/app/Sources/FreesideAPI/openapi.yaml"
{ printf 'generated-from:\n'; cat "$REPO/api/openapi.yaml"; } \
  >"$REPO/app/Sources/FreesideAPI/GeneratedSources/Types.swift"
git -C "$REPO" add -A
commit_git commit -q -m 'Change the schema and regenerate the client'
assert_rc 0

begin_case "commit staging no input skips generation"
setup_repo
printf 'more notes\n' >>"$REPO/notes.txt"
git -C "$REPO" add notes.txt
commit_git commit -q -m 'Edit unrelated notes'
assert_rc 0
assert_file_absent "$ARGSFILE"

begin_case "commit staging no input from a subdirectory skips generation"
setup_repo
printf 'more notes\n' >>"$REPO/notes.txt"
git -C "$REPO" add notes.txt
set +e
OUT=$( (cd "$REPO/app" && PATH="$BINDIR:$PATH" \
  git commit -q -m 'Edit unrelated notes from a subdirectory') 2>&1 )
RC=$?
set -e
assert_rc 0
assert_file_absent "$ARGSFILE"

begin_case "amend of an input-free commit runs no generation"
setup_repo
printf 'more notes\n' >>"$REPO/notes.txt"
git -C "$REPO" add notes.txt
commit_git commit -q -m 'Edit unrelated notes'
assert_rc 0
commit_git commit -q --amend --no-edit
assert_rc 0
assert_file_absent "$ARGSFILE"

begin_case "merge commit runs no generation"
setup_repo
git -C "$REPO" switch -q -c side
printf 'side\n' >>"$REPO/notes.txt"
git -C "$REPO" add notes.txt
git -C "$REPO" commit -q -m 'Add side work'
git -C "$REPO" switch -q main
printf 'main\n' >"$REPO/other.txt"
git -C "$REPO" add other.txt
git -C "$REPO" commit -q -m 'Add main work'
set +e
OUT=$( (cd "$REPO" && PATH="$BINDIR:$PATH" \
  git merge --no-ff --no-commit side) 2>&1 )
set -e
commit_git commit -q --no-edit
assert_rc 0
assert_file_absent "$ARGSFILE"

begin_case "unstaged input edits are refused before generation"
setup_repo
printf 'schema: v2\n' >"$REPO/api/openapi.yaml"
git -C "$REPO" add api/openapi.yaml
printf 'schema: v3-unstaged\n' >"$REPO/api/openapi.yaml"
commit_git commit -q -m 'Change the schema'
assert_rc 1
assert_contains 'stage or stash'
assert_contains 'api/openapi.yaml'
assert_file_absent "$ARGSFILE"

begin_case "a deleted generated file is reported as drift"
setup_repo
printf 'schema: v2\n' >"$REPO/api/openapi.yaml"
git -C "$REPO" add api/openapi.yaml
rm "$REPO/app/Sources/FreesideAPI/GeneratedSources/Client.swift"
commit_git commit -q -m 'Change the schema'
assert_rc 1
assert_contains 'app/Sources/FreesideAPI/GeneratedSources/Client.swift'

echo "assertions: $pass passed, $fail failed"
if [ "$fail" -ne 0 ]; then
  exit 1
fi
