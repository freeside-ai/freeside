#!/usr/bin/env bash
# test-swift-build-times.sh — fixtures for scripts/swift-build-times.py, which
# app-ci.yml's `swift-build-cache` job runs to record each tracked app/ file's
# modification time and SHA-256 in the saved build (#3294). Each case builds a
# throwaway git repository under a temporary directory and records it. Needs
# git and python3; no network. Exit code: 0 when every assertion passes, 1
# otherwise.
set -euo pipefail

# The fixture repositories must not read the developer's git config or
# follow a GIT_DIR a hook exported.
export GIT_CONFIG_GLOBAL=/dev/null
export GIT_CONFIG_SYSTEM=/dev/null
unset GIT_DIR GIT_WORK_TREE GIT_INDEX_FILE

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
HELPER=$SCRIPT_DIR/swift-build-times.py

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
# Stop repository discovery at TMP, so a TMPDIR inside a work tree can't
# make the not-a-repository case find one.
export GIT_CEILING_DIRECTORIES=$TMP

pass=0
fail=0
case_number=0
CASE=''
OUT=''
RC=0
REPO=''
RECORD=''

begin_case() { # <name>; starts a case with an empty repository
  case_number=$((case_number + 1))
  CASE=$(printf '%02d %s' "$case_number" "$1")
  echo "case: $CASE"
  REPO=$TMP/case-$case_number/repo
  RECORD=$TMP/case-$case_number/out/source-times.tsv
  mkdir -p "$REPO" "$(dirname "$RECORD")"
  git -C "$REPO" init -q
}

report_failure() {
  fail=$((fail + 1))
  echo "FAIL [$CASE]: $*"
  printf '%s\n' "$OUT" | sed 's/^/    | /'
}

assert_rc() {
  if [ "$RC" -eq "$1" ]; then pass=$((pass + 1)); else report_failure "expected rc=$1, got rc=$RC"; fi
}

assert_contains() {
  case $OUT in
    *"$1"*) pass=$((pass + 1)) ;;
    *) report_failure "output does not contain: $1" ;;
  esac
}

assert_equal() { # <expected> <actual> <what>
  if [ "$1" = "$2" ]; then pass=$((pass + 1)); else report_failure "$3: expected [$1], got [$2]"; fi
}

assert_no_record() { # the record and any temporary file beside it are absent
  local left
  left=$(find "$(dirname "$RECORD")" -mindepth 1 -print)
  if [ -z "$left" ]; then pass=$((pass + 1)); else report_failure "left behind: $left"; fi
}

run() { # <args...>; runs the helper, capturing combined output and status
  RC=0
  OUT=$(python3 "$HELPER" "$@" 2>&1) || RC=$?
}

record() { run record "$REPO" "$RECORD"; }

# put <path> <content> [mtime-ns]; writes a file in REPO, stages it, and sets
# its modification time when one is given.
put() {
  mkdir -p "$(dirname "$REPO/$1")"
  printf '%s' "$2" >"$REPO/$1"
  git -C "$REPO" add -- "$1"
  if [ -n "${3:-}" ]; then
    python3 -c 'import os,sys; n=int(sys.argv[2]); os.utime(sys.argv[1], ns=(n, n))' "$REPO/$1" "$3"
  fi
}

sha256() { # <path>
  python3 -c 'import hashlib,sys; print(hashlib.sha256(open(sys.argv[1],"rb").read()).hexdigest())' "$1"
}

begin_case "records every tracked file once, with its time and hash"
put Package.swift 'let package = 1' 1700000000123456789
put 'Sources/Core/Deep/Nested File.swift' 'struct A {}' 1700000001000000007
put scripts/run.sh 'echo run' 1700000002999999999
chmod 755 "$REPO/scripts/run.sh"
git -C "$REPO" add -- scripts/run.sh
put .gitignore "$(printf '.build/\n*.log')" 1600000000000000000
mkdir -p "$REPO/.build"
printf 'built' >"$REPO/.build/artifact.o"
printf 'noise' >"$REPO/debug.log"
printf 'new' >"$REPO/Untracked.swift"
record
assert_rc 0
assert_contains 'recorded 4 tracked files'
expected=$(printf '%s\n' \
  '# freeside swift-build-times v1' \
  "1600000000000000000	$(sha256 "$REPO/.gitignore")	.gitignore" \
  "1700000000123456789	$(sha256 "$REPO/Package.swift")	Package.swift" \
  "1700000001000000007	$(sha256 "$REPO/Sources/Core/Deep/Nested File.swift")	Sources/Core/Deep/Nested File.swift" \
  "1700000002999999999	$(sha256 "$REPO/scripts/run.sh")	scripts/run.sh")
assert_equal "$expected" "$(cat "$RECORD")" 'record'

begin_case "lists paths relative to the tree, sorted by bytes"
put app/b.swift 'b'
put app/Z.swift 'Z'
put app/a/z.swift 'z'
put app/a-z.swift 'dash'
put other/outside.swift 'outside'
run record "$REPO/app" "$RECORD"
assert_rc 0
# Byte order: 'Z' (0x5a) < 'a' (0x61), and '-' (0x2d) < '/' (0x2f), so
# neither a case-folding nor a directory-first order passes.
assert_equal "$(printf '%s\n' Z.swift a-z.swift a/z.swift b.swift)" "$(tail -n +2 "$RECORD" | cut -f3)" 'paths'

begin_case "gives byte-identical output for an unchanged tree"
put one.swift 'one' 1700000000000000001
put two/three.swift 'three' 1700000000000000003
record
first=$(cat "$RECORD")
record
assert_rc 0
assert_equal "$first" "$(cat "$RECORD")" 'second record'

begin_case "replaces an existing record whole"
put one.swift 'one'
printf 'stale\n' >"$RECORD"
record
assert_rc 0
assert_equal '# freeside swift-build-times v1' "$(head -n 1 "$RECORD")" 'header'
assert_equal 2 "$(wc -l <"$RECORD" | tr -d ' ')" 'line count'

begin_case "a tracked file missing from the work tree fails, naming it"
put kept.swift 'kept'
put gone.swift 'gone'
rm "$REPO/gone.swift"
record
assert_rc 1
assert_contains 'gone.swift'
assert_no_record

begin_case "a tracked symlink fails, naming it"
put target.swift 'target'
ln -s target.swift "$REPO/link.swift"
git -C "$REPO" add link.swift
record
assert_rc 1
assert_contains 'link.swift: not a regular file'
assert_no_record

begin_case "a tracked path that is now a directory fails, naming it"
put kept.swift 'kept'
put was-file 'file'
rm "$REPO/was-file"
mkdir "$REPO/was-file"
record
assert_rc 1
assert_contains 'was-file: not a regular file'
assert_no_record

begin_case "a path holding a tab fails, naming it"
put "$(printf 'tab\there.swift')" 'tab'
record
assert_rc 1
assert_contains 'a tab or newline in the path'
assert_no_record

# Root ignores file modes, so the case can't build an unreadable file there.
if [ "$(id -u)" != 0 ]; then
  begin_case "an unreadable tracked file fails, naming it"
  put kept.swift 'kept'
  put locked.swift 'locked'
  chmod 000 "$REPO/locked.swift"
  record
  chmod 644 "$REPO/locked.swift"
  assert_rc 1
  assert_contains 'locked.swift: Permission denied'
  assert_no_record
else
  echo "skip: an unreadable tracked file (running as root, which ignores file modes)"
fi

begin_case "a tree outside any git repository fails"
mkdir -p "$TMP/not-a-repo"
run record "$TMP/not-a-repo" "$RECORD"
assert_rc 1
assert_contains 'git ls-files'
assert_no_record

begin_case "a missing subcommand or argument is a usage error"
for args in '' 'record' "record $REPO" 'apply x y'; do
  # shellcheck disable=SC2086 # split the case's words on purpose
  run $args
  assert_rc 2
done
assert_no_record

echo "assertions: $pass passed, $fail failed"
if [ "$fail" -ne 0 ]; then
  exit 1
fi
