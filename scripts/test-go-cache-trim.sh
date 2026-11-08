#!/usr/bin/env bash
# test-go-cache-trim.sh — fixtures for scripts/go-cache-trim.py, the Go build
# cache size cap check.sh runs before daemon steps. Each case builds a fake
# cache under a temporary directory, with modification times set relative to
# now, and calls the helper with byte-level limits. Needs python3; no Go and
# no network. Exit code: 0 when every assertion passes, 1 otherwise.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
HELPER=$SCRIPT_DIR/go-cache-trim.py

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0
case_number=0
CASE=''
OUT=''
RC=0
CACHE=''
STATE=''

begin_case() { # <name>; starts a case with an empty cache and state
  case_number=$((case_number + 1))
  CASE=$(printf '%02d %s' "$case_number" "$1")
  echo "case: $CASE"
  CACHE=$TMP/case-$case_number/cache
  STATE=$TMP/case-$case_number/state
  mkdir -p "$CACHE" "$STATE"
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

assert_silent() {
  if [ -z "$OUT" ]; then pass=$((pass + 1)); else report_failure "expected no output"; fi
}

assert_present() { # <path...>
  local p
  for p in "$@"; do
    if [ -e "$p" ] || [ -L "$p" ]; then pass=$((pass + 1)); else report_failure "deleted: $p"; fi
  done
}

assert_absent() { # <path...>
  local p
  for p in "$@"; do
    if [ -e "$p" ] || [ -L "$p" ]; then report_failure "still present: $p"; else pass=$((pass + 1)); fi
  done
}

age() { # <path> <seconds>; sets the modification time that far in the past
  python3 -c 'import os,sys,time; t=time.time()-float(sys.argv[2]); os.utime(sys.argv[1],(t,t),follow_symlinks=False)' "$1" "$2"
}

# entry <n> <bytes> <age-seconds> [dir]; creates entry number n in CACHE,
# named like Go's (64 hex digits, -a or -d) under its two-hex subdirectory,
# and prints its path. With "dir" the entry is a directory holding two files
# that share the bytes, as Go 1.26 caches executables.
entry() {
  local name sub path
  name=$(printf '%02x%062x' "$1" "$1")
  sub=${name:0:2}
  mkdir -p "$CACHE/$sub"
  if [ "${4:-}" = dir ]; then
    path=$CACHE/$sub/$name-d
    mkdir "$path"
    head -c $(($2 / 2)) /dev/zero >"$path/a.out"
    head -c $(($2 - $2 / 2)) /dev/zero >"$path/b.out"
  else
    path=$CACHE/$sub/$name-a
    head -c "$2" /dev/zero >"$path"
  fi
  age "$path" "$3"
  printf '%s' "$path"
}

trim() { # <limit-bytes>; runs the helper on CACHE and STATE, sets OUT and RC
  set +e
  OUT=$(python3 "$HELPER" --cache "$CACHE" --limit-bytes "$1" --state "$STATE" 2>&1)
  RC=$?
  set -e
}

stamp_mtime() { python3 -c 'import os,sys; print(os.stat(sys.argv[1]).st_mtime_ns)' "$STATE/last-walk"; }

hour=3600

begin_case "under the cap it deletes nothing"
a=$(entry 1 100 $((72 * hour)))
b=$(entry 2 100 $((48 * hour)))
c=$(entry 3 100 $((24 * hour)))
trim 1000
assert_rc 0
assert_silent
assert_present "$a" "$b" "$c" "$STATE/last-walk"

begin_case "over the cap it deletes oldest first and stops at 75% of the cap"
paths=()
for i in 1 2 3 4 5 6 7 8 9 10; do
  # Entry 1 is the newest, at 3 hours; entry 10 the oldest.
  paths+=("$(entry "$i" 100 $(((2 + i) * hour)))")
done
trim 800
assert_rc 0
assert_contains 'was 0.0 GB, now 0.0 GB'
assert_contains 'removed 4 entries'
assert_absent "${paths[@]:6:4}"
assert_present "${paths[@]:0:6}"

begin_case "it never deletes an entry modified in the last 2 hours, and warns"
old=$(entry 1 100 $((5 * hour)))
recent=(
  "$(entry 2 100 1800)"
  "$(entry 3 100 $((90 * 60)))"
  "$(entry 4 100 $((110 * 60)))"
)
trim 250
assert_rc 0
assert_contains 'removed 1 entries'
assert_contains 'warning:'
assert_contains 'over the 0.0 GB cap'
assert_absent "$old"
assert_present "${recent[@]}"

begin_case "a directory -d entry counts by its contents and goes whole"
exe=$(entry 1 300 $((10 * hour)) dir)
files=(
  "$(entry 2 100 $((9 * hour)))"
  "$(entry 3 100 $((8 * hour)))"
  "$(entry 4 100 $((7 * hour)))"
)
# 600 bytes over a 500-byte cap trims to 375 by removing the directory.
# Counted by its own st_size instead, the cache would read as under the cap.
trim 500
assert_rc 0
assert_contains 'removed 1 entries'
assert_absent "$exe"
assert_present "${files[@]}"

begin_case "a stamp under an hour old skips the walk; an older one doesn't"
a=$(entry 1 100 $((10 * hour)))
b=$(entry 2 100 $((9 * hour)))
touch "$STATE/last-walk"
age "$STATE/last-walk" $((50 * 60))
before=$(stamp_mtime)
trim 150
assert_rc 0
assert_silent
assert_present "$a" "$b"
if [ "$(stamp_mtime)" = "$before" ]; then pass=$((pass + 1)); else report_failure "a throttled run refreshed the stamp"; fi
age "$STATE/last-walk" $((70 * 60))
trim 150
assert_rc 0
assert_contains 'removed 1 entries'
assert_absent "$a"
assert_present "$b"
if [ "$(stamp_mtime)" != "$before" ]; then pass=$((pass + 1)); else report_failure "a walk left the stamp unchanged"; fi

begin_case "names that aren't entries are neither counted nor touched"
a=$(entry 1 100 $((10 * hour)))
b=$(entry 2 100 $((9 * hour)))
sub=$(dirname "$a")
outside=$TMP/outside
mkdir -p "$outside/00"
head -c 100 /dev/zero >"$outside/target"
head -c 100 /dev/zero >"$outside/00/$(basename "$a")"
others=("$CACHE/trim.txt" "$CACHE/README" "$CACHE/testexpire.txt" "$CACHE/log"
  "$sub/$(printf 'A%063x' 1)-a" "$sub/$(printf '%063x' 1)-a" "$sub/$(printf '%064x' 1)-x"
  "$sub/$(printf '%064x' 1)-a.tmp" "$sub/foo")
for p in "${others[@]}"; do
  head -c 5000 /dev/zero >"$p"
  age "$p" $((30 * 24 * hour))
done
link=$sub/$(printf '%064x' 9)-a
ln -s "$outside/target" "$link"
age "$link" $((30 * 24 * hour))
others+=("$link")
rmdir "$CACHE/ff" 2>/dev/null || :
ln -s "$outside/00" "$CACHE/ff"
others+=("$CACHE/ff")
# Counted, the 45 kB of non-entries would put the cache over the cap.
trim 1000
assert_rc 0
assert_silent
assert_present "$a" "$b" "${others[@]}"
rm "$STATE/last-walk"
trim 150
assert_rc 0
assert_contains 'removed 1 entries'
assert_absent "$a"
assert_present "$b" "${others[@]}" "$outside/target" "$outside/00/$(basename "$a")"

begin_case "a second trimmer neither walks nor deletes while another holds the lock"
a=$(entry 1 100 $((10 * hour)))
b=$(entry 2 100 $((9 * hour)))
python3 - "$STATE/lock" "$TMP/holder" <<'PY' &
import fcntl, os, sys, time
lock = open(sys.argv[1], "a")
fcntl.flock(lock, fcntl.LOCK_EX)
open(sys.argv[2] + ".ready", "w").close()
deadline = time.time() + 30
while not os.path.exists(sys.argv[2] + ".release") and time.time() < deadline:
    time.sleep(0.1)
PY
holder=$!
end=$((SECONDS + 30))
until [ -e "$TMP/holder.ready" ] || ((SECONDS >= end)); do sleep .1; done
trim 150
assert_rc 0
assert_contains 'another trim holds'
assert_present "$a" "$b"
assert_absent "$STATE/last-walk"
touch "$TMP/holder.release"
wait "$holder"
trim 150
assert_rc 0
assert_contains 'removed 1 entries'
assert_absent "$a"

# Root ignores file modes, so the case can't build an undeletable entry there.
if [ "$(id -u)" != 0 ]; then
  begin_case "an entry it can't delete or a subdirectory it can't read doesn't stop the rest"
  stuck=$(entry 1 100 $((10 * hour)))
  hidden=$(entry 5 100 $((11 * hour)))
  rest=(
    "$(entry 2 100 $((9 * hour)))"
    "$(entry 3 100 $((8 * hour)))"
    "$(entry 4 100 $((7 * hour)))"
  )
  chmod 555 "$(dirname "$stuck")"
  chmod 000 "$(dirname "$hidden")"
  # 400 counted bytes over a 300-byte cap: the stuck entry stays, so entries
  # 2 and 3 go to reach 225.
  trim 300
  chmod 755 "$(dirname "$stuck")" "$(dirname "$hidden")"
  assert_rc 0
  assert_contains 'removed 2 entries; could not remove 1'
  assert_present "$stuck" "$hidden" "${rest[2]}"
  assert_absent "${rest[@]:0:2}"
fi

begin_case "a limit that isn't a positive integer is a usage error"
a=$(entry 1 100 $((10 * hour)))
for limit in 0 -1 abc ''; do
  trim "$limit"
  assert_rc 2
done
assert_present "$a"
assert_absent "$STATE/last-walk"

echo "assertions: $pass passed, $fail failed"
if [ "$fail" -ne 0 ]; then
  exit 1
fi
