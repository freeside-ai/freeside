#!/usr/bin/env bash
# test-pre-commit.sh — fixtures for the .githooks/pre-commit API-client
# regeneration hook, and for generate-api-client.sh's drift check in a tree
# without .git, as a verification room holds one.
#
# Each case builds an isolated synthetic repository with a fixed identity, a
# copy of the shipped hook and generate-api-client.sh, and a stand-in `swift`
# on PATH that records its arguments and regenerates Types.swift as a fixed
# transform of the schema mirror. A no-git case exports that repository's
# commit and runs the script there. No Swift toolchain and no network.
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
EXPORT=''
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
    "$REPO/app/scripts" "$REPO/scripts" \
    "$REPO/daemon/internal/signet" "$REPO/.githooks"
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
  # generate-api-client.sh regenerates the contract-digest constants through
  # this helper, so the fixture needs it and its output dirs (created above)
  # for a schema-changing case to reach the generator instead of aborting.
  cp "$ROOT/scripts/api-contract-digest.sh" \
    "$REPO/scripts/api-contract-digest.sh"
  chmod +x "$REPO/scripts/api-contract-digest.sh"
  # It also writes TolerantEnums.swift through this script. A stand-in
  # writes fixed output, as the swift stub stands in for the generator: the
  # real script needs a generated client with enums in it.
  cat >"$REPO/app/scripts/write-tolerant-enums.sh" <<'WRITER'
#!/usr/bin/env bash
printf 'tolerant: fixed\n' >"$(dirname "$0")/../Sources/FreesideAPI/TolerantEnums.swift"
WRITER
  chmod +x "$REPO/app/scripts/write-tolerant-enums.sh"
  printf 'tolerant: fixed\n' >"$REPO/app/Sources/FreesideAPI/TolerantEnums.swift"
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

# setup_export builds the in-sync fixture, exports its commit to a tree with
# no .git, and sets EXPORT. The fixture commits no contract-digest constants,
# so they are written here (into the directory the export drops for being
# empty) to make the export a tree regeneration leaves unchanged.
setup_export() {
  local digest_out
  setup_repo
  EXPORT=$TMP/export$case_number
  mkdir -p "$EXPORT"
  git -C "$REPO" archive HEAD | tar -x -C "$EXPORT"
  mkdir -p "$EXPORT/daemon/internal/signet"
  digest_out=$(bash "$EXPORT/scripts/api-contract-digest.sh" --write 2>&1) || {
    printf '%s\n' "$digest_out" >&2
    return 1
  }
}

generate_export() { # [<args...>]; runs the generator in EXPORT with the stub on PATH, sets OUT/RC
  set +e
  OUT=$(PATH="$BINDIR:$PATH" \
    bash "$EXPORT/app/scripts/generate-api-client.sh" "$@" 2>&1)
  RC=$?
  set -e
}

assert_trees_equal() { # <expected-dir> <actual-dir>
  local differences
  if differences=$(diff -r "$1" "$2" 2>&1); then
    pass=$((pass + 1))
  else
    report_failure "trees differ: $differences"
  fi
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

begin_case "default mode in a checkout reports a schema edit as drift"
setup_repo
printf 'schema: v2\n' >"$REPO/api/openapi.yaml"
set +e
OUT=$(PATH="$BINDIR:$PATH" \
  bash "$REPO/app/scripts/generate-api-client.sh" 2>&1)
RC=$?
set -e
assert_rc 1
assert_contains 'Generated API client changed'

begin_case "no-git: an up-to-date tree passes and stays unchanged"
setup_export
cp -R "$EXPORT" "$TMP/before$case_number"
generate_export
assert_rc 0
assert_file_exists "$ARGSFILE"
assert_trees_equal "$TMP/before$case_number" "$EXPORT"

begin_case "no-git: a schema edit without regeneration is drift"
setup_export
printf 'schema: v2\n' >"$EXPORT/api/openapi.yaml"
generate_export
assert_rc 1
assert_contains 'Generated API client changed'
assert_contains 'app/Sources/FreesideAPI/openapi.yaml'
assert_contains 'app/Sources/FreesideAPI/GeneratedSources/Types.swift'
assert_contains 'app/Sources/FreesideAPI/ContractDigest.swift'
assert_contains 'daemon/internal/signet/contract_digest.go'

begin_case "no-git: a stale schema mirror is drift"
setup_export
printf 'schema: stale\n' >"$EXPORT/app/Sources/FreesideAPI/openapi.yaml"
generate_export
assert_rc 1
assert_contains 'Generated API client changed'
assert_contains 'app/Sources/FreesideAPI/openapi.yaml'

begin_case "no-git: a hand-edited generated file is drift"
setup_export
printf 'edited by hand\n' \
  >>"$EXPORT/app/Sources/FreesideAPI/GeneratedSources/Types.swift"
generate_export
assert_rc 1
assert_contains 'Generated API client changed'
assert_contains 'app/Sources/FreesideAPI/GeneratedSources/Types.swift'

begin_case "no-git: a generated file regeneration creates is drift"
setup_export
rm "$EXPORT/app/Sources/FreesideAPI/GeneratedSources/Types.swift"
generate_export
assert_rc 1
assert_contains 'Generated API client changed'
assert_contains 'app/Sources/FreesideAPI/GeneratedSources/Types.swift'

begin_case "no-git: a tree nested in another repository compares itself"
setup_export
git init -q "$TMP/outer$case_number"
mv "$EXPORT" "$TMP/outer$case_number/tree"
EXPORT=$TMP/outer$case_number/tree
generate_export
assert_rc 0
printf 'schema: v2\n' >"$EXPORT/api/openapi.yaml"
generate_export
assert_rc 1
assert_contains 'app/Sources/FreesideAPI/openapi.yaml'

begin_case "no-git: --against-index refuses before generating"
setup_export
generate_export --against-index
assert_rc 2
assert_contains 'needs a git checkout'
assert_file_absent "$ARGSFILE"

echo "assertions: $pass passed, $fail failed"
if [ "$fail" -ne 0 ]; then
  exit 1
fi
