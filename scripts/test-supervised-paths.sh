#!/usr/bin/env bash
# test-supervised-paths.sh — regression suite for supervised-paths.sh.
#
# Points HOME at a synthetic home holding both supervised roots and checks
# that refuse_supervised_path refuses each root, a path inside either, a
# not-yet-created path under either (with the root present or absent), a
# symlink into one (to a directory, a file, or a missing target), a path
# through such a symlink, a differently cased spelling, a relative spelling,
# macOS's /System/Volumes/Data alias (on Darwin only), and an unresolvable
# `..`; that an unset HOME still checks the passwd home; and that it accepts an ordinary temp path and a
# sibling whose name only shares a root's prefix. It also checks that
# prod_app_directory_in accepts only prod's own daemon/ and credentials/
# directories (or a symlink to one), under homes that share one prod root, and
# that require_prod_app_directory includes the passwd home. The passwd home is
# the real account's and is only resolved, never written.
#
# Exit code: 0 when every assertion passes, 1 otherwise.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

pass=0
fail=0

HOME_DIR=$TMP/home
SUPPORT="$HOME_DIR/Library/Application Support"
PROD="$SUPPORT/Freeside"
DEV="$SUPPORT/Freeside Dev"
mkdir -p "$PROD/daemon" "$DEV/daemon" "$TMP/plain" "$SUPPORT/FreesideX" "$TMP/links"
: >"$PROD/daemon/freeside.db"
ln -s "$PROD" "$TMP/links/to-prod"
ln -s "$DEV/daemon" "$TMP/links/to-dev-daemon"
ln -s "$PROD/daemon/freeside.db" "$TMP/links/to-prod-db"
ln -s "$DEV/not-yet" "$TMP/links/dangling-to-dev"

# A second home whose roots do not exist yet, and whose Application Support
# is itself a symlink, as a relocated Library can be.
EMPTY_HOME=$TMP/empty-home
mkdir -p "$EMPTY_HOME/Library" "$TMP/elsewhere/Application Support"
ln -s "$TMP/elsewhere/Application Support" "$EMPTY_HOME/Library/Application Support"

# check <expect: refuse|accept> <home|UNSET> <path> [cwd]; the helper runs
# under set -u, as every caller does.
check() {
  local expect=$1 home=$2 path=$3 cwd=${4:-$TMP} out rc
  set +e
  out=$(
    cd "$cwd" || exit 99
    if [[ "$home" == UNSET ]]; then unset HOME; else export HOME=$home; fi
    bash -c '
      set -euo pipefail
      source "$1"
      refuse_supervised_path test-path "$2"
    ' _ "$SCRIPT_DIR/supervised-paths.sh" "$path" 2>&1
  )
  rc=$?
  set -e
  if [[ "$expect" == refuse && "$rc" != 0 && "$out" == *'refusing test-path'* ]] ||
    [[ "$expect" == accept && "$rc" == 0 && -z "$out" ]]; then
    pass=$((pass + 1))
  else
    fail=$((fail + 1))
    echo "FAIL: expected $expect for \"$path\" (HOME=$home, cwd=$cwd): rc=$rc"
    printf '%s\n' "$out" | sed 's/^/    | /'
  fi
}

echo "case: each supervised root is refused"
check refuse "$HOME_DIR" "$PROD"
check refuse "$HOME_DIR" "$DEV"
check refuse "$HOME_DIR" "$PROD/"

echo "case: a path inside either root is refused"
check refuse "$HOME_DIR" "$PROD/daemon"
check refuse "$HOME_DIR" "$PROD/daemon/freeside.db"
check refuse "$HOME_DIR" "$DEV/daemon"

echo "case: a not-yet-created path under either root is refused"
check refuse "$HOME_DIR" "$PROD/new/state"
check refuse "$HOME_DIR" "$DEV/new/state"
check refuse "$EMPTY_HOME" "$EMPTY_HOME/Library/Application Support/Freeside/state"
check refuse "$EMPTY_HOME" "$TMP/elsewhere/Application Support/Freeside Dev/state"

echo "case: a symlink into a root, and a path through it, are refused"
check refuse "$HOME_DIR" "$TMP/links/to-prod"
check refuse "$HOME_DIR" "$TMP/links/to-prod/daemon/new"
check refuse "$HOME_DIR" "$TMP/links/to-dev-daemon"
check refuse "$HOME_DIR" "$TMP/links/to-prod-db"
check refuse "$HOME_DIR" "$TMP/links/dangling-to-dev"
check refuse "$HOME_DIR" "$TMP/links/dangling-to-dev/state"

echo "case: other spellings of a root are refused"
check refuse "$HOME_DIR" "$HOME_DIR/library/application support/FREESIDE/state"
check refuse "$HOME_DIR" "Library/Application Support/Freeside Dev/state" "$HOME_DIR"
check refuse "$HOME_DIR" "$HOME_DIR/Library/Application Support/Freeside/./daemon"
check refuse "$HOME_DIR" "$TMP/plain/../home/Library/Application Support/Freeside"

echo "case: an unset HOME still checks the passwd home"
ACCOUNT_HOME=$(bash -c 'source "$1"; supervised_account_home' _ "$SCRIPT_DIR/supervised-paths.sh")
check refuse UNSET "$ACCOUNT_HOME/Library/Application Support/Freeside/new/state"
check accept UNSET "$TMP/plain"

PHYSICAL_TMP=$(cd -P "$TMP" && pwd -P)
if [[ "$(uname)" == Darwin && -d "/System/Volumes/Data$PHYSICAL_TMP" ]]; then
  echo "case: the macOS data-volume alias of a root is refused"
  check refuse "$HOME_DIR" "/System/Volumes/Data$PHYSICAL_TMP/home/Library/Application Support/Freeside/new"
  check refuse "$HOME_DIR" "/System/Volumes/Data$PHYSICAL_TMP/home/Library/Application Support/Freeside Dev/daemon"
fi

echo "case: an unresolvable path is refused"
check refuse "$HOME_DIR" "$TMP/plain/missing/../escape"

echo "case: ordinary paths are accepted"
check accept "$HOME_DIR" "$TMP/plain"
check accept "$HOME_DIR" "$TMP/plain/new/state"
check accept "$HOME_DIR" "$SUPPORT/FreesideX/state"
check accept "$HOME_DIR" "$SUPPORT"
check accept "$EMPTY_HOME" "$TMP/elsewhere/Application Support/Other"

# check_prod <expect: refuse|accept> <path> <daemon|credentials> <home>...
check_prod() {
  local expect=$1 path=$2 name=$3 out rc
  shift 3
  set +e
  out=$(bash -c '
    set -euo pipefail
    source "$1"
    shift
    prod_app_directory_in test-path "$@"
  ' _ "$SCRIPT_DIR/supervised-paths.sh" "$path" "$name" "$@" 2>&1)
  rc=$?
  set -e
  if [[ "$expect" == refuse && "$rc" != 0 && "$out" == *'refusing test-path'* ]] ||
    [[ "$expect" == accept && "$rc" == 0 && -z "$out" ]]; then
    pass=$((pass + 1))
  else
    fail=$((fail + 1))
    echo "FAIL: expected $expect for prod $name \"$path\" (homes: $*): rc=$rc"
    printf '%s\n' "$out" | sed 's/^/    | /'
  fi
}

mkdir -p "$PROD/credentials" "$PROD/daemon/nested" "$DEV/credentials" "$TMP/copy/daemon"
ln -s "$PROD/daemon" "$TMP/links/to-prod-daemon"
ln -s "$HOME_DIR" "$TMP/home-alias"
OTHER_HOME=$TMP/other-home
mkdir -p "$OTHER_HOME/Library/Application Support/Freeside/daemon"

echo "case: prod's own App directories are accepted"
check_prod accept "$PROD/daemon" daemon "$HOME_DIR"
check_prod accept "$PROD/credentials" credentials "$HOME_DIR"
check_prod accept "$TMP/links/to-prod-daemon" daemon "$HOME_DIR"
check_prod accept "$PROD/daemon" daemon "$HOME_DIR" "$TMP/home-alias"

echo "case: anything else is refused"
check_prod refuse "$PROD/credentials" daemon "$HOME_DIR"
check_prod refuse "$PROD/daemon/nested" daemon "$HOME_DIR"
check_prod refuse "$PROD" daemon "$HOME_DIR"
check_prod refuse "$DEV/daemon" daemon "$HOME_DIR"
check_prod refuse "$DEV/credentials" credentials "$HOME_DIR"
check_prod refuse "$TMP/copy/daemon" daemon "$HOME_DIR"
check_prod refuse "$PROD/daemon/missing" daemon "$HOME_DIR"
check_prod refuse "$EMPTY_HOME/Library/Application Support/Freeside/daemon" daemon "$EMPTY_HOME"
check_prod refuse "$PROD/daemon" daemon

echo "case: homes with different prod roots are refused"
check_prod refuse "$PROD/daemon" daemon "$HOME_DIR" "$OTHER_HOME"
check_prod refuse "$PROD/daemon" daemon "$HOME_DIR" "$EMPTY_HOME"

echo "case: the wrapper also checks the passwd home"
set +e
out=$(HOME=$HOME_DIR bash -c '
  set -euo pipefail
  source "$1"
  require_prod_app_directory test-path "$2" daemon
' _ "$SCRIPT_DIR/supervised-paths.sh" "$PROD/daemon" 2>&1)
rc=$?
set -e
if [[ "$rc" != 0 && "$out" == *'refusing test-path'* ]]; then
  pass=$((pass + 1))
else
  fail=$((fail + 1))
  echo "FAIL: require_prod_app_directory accepted a synthetic HOME the passwd home disagrees with: rc=$rc"
  printf '%s\n' "$out" | sed 's/^/    | /'
fi

echo
echo "passed $pass assertion(s), failed $fail"
if [ "$fail" -gt 0 ]; then
  exit 1
fi
