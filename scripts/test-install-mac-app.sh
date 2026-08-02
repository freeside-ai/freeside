#!/usr/bin/env bash
# test-install-mac-app.sh — focused filesystem regressions for the Mac installer.
#
# Issue #464 starts the broader matrix tracked by #458 with the restart states
# that change installer behaviour: SIGKILL after the old app is renamed aside
# leaves the valid app at `.install-superseded`, while the canonical path is
# either absent or holds the not-yet-verified replacement. The ordinary suite
# uses command stand-ins, so it needs neither Xcode nor a signing identity and
# runs on Linux as well as macOS.
# `FREESIDE_TEST_REAL_RENAME=true` uses macOS Swift for the production-exclusive
# rename while keeping the build and signing boundaries stubbed.
#
# Exit code: 0 when every assertion passes, 1 otherwise.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
INSTALLER=${FREESIDE_INSTALLER_UNDER_TEST:-$SCRIPT_DIR/../app/scripts/install-mac-app.sh}

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

STUB_BIN=$TMP/bin
mkdir -p "$STUB_BIN"

cat >"$STUB_BIN/uname" <<'STUB'
#!/usr/bin/env bash
printf 'Darwin\n'
STUB

cat >"$STUB_BIN/plutil" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "${STUB_BUNDLE_ID:-ai.freeside.app.macos}"
STUB

cat >"$STUB_BIN/codesign" <<'STUB'
#!/usr/bin/env bash
target=${!#}
case " $* " in
*" --verify "*)
    if [[ -n "${STUB_INTERLOPER_DEST:-}" ]]; then
        mkdir -p "$STUB_INTERLOPER_DEST"
        printf 'foreign contents\n' >"$STUB_INTERLOPER_DEST/marker"
    fi
    [[ ! -e "$target/.invalid-signature" ]]
    ;;
*" --display "*)
    printf '%s\n' '# designated => identifier "ai.freeside.app.macos"' >&2
    ;;
*) exit 1 ;;
esac
STUB

cat >"$STUB_BIN/xcodebuild" <<'STUB'
#!/usr/bin/env bash
printf 'called\n' >"${STUB_CASE_DIR:?}/xcodebuild-called"
if [[ -n "${STUB_BUILD_SUCCEEDS:-}" ]]; then
    built_app="$STUB_CASE_DIR/Build/Build/Products/Release/FreesideMac.app"
    mkdir -p "$built_app/Contents"
    printf 'new client\n' >"$built_app/Contents/marker"
    printf 'fixture\n' >"$built_app/Contents/Info.plist"
    if [[ -n "${STUB_INTERLOPER_AFTER_BUILD:-}" ]]; then
        mkdir -p "$STUB_INTERLOPER_AFTER_BUILD/Contents"
        printf 'interloper after build\n' >"$STUB_INTERLOPER_AFTER_BUILD/Contents/marker"
        printf 'fixture\n' >"$STUB_INTERLOPER_AFTER_BUILD/Contents/Info.plist"
        touch "$STUB_INTERLOPER_AFTER_BUILD/.invalid-signature"
        stat -f %i "$STUB_INTERLOPER_AFTER_BUILD" >"$STUB_CASE_DIR/after-build-interloper-inode"
    fi
    exit 0
fi
exit 42
STUB

cat >"$STUB_BIN/ditto" <<'STUB'
#!/usr/bin/env bash
cp -R "$1" "$2"
STUB

cat >"$STUB_BIN/stat" <<'STUB'
#!/usr/bin/env bash
if [[ "${1:-}" == -f && "${2:-}" == %i ]]; then
    if /usr/bin/stat -f %i "$3" >/dev/null 2>&1; then
        exec /usr/bin/stat -f %i "$3"
    fi
    exec /usr/bin/stat -c %i "$3"
fi
exec /usr/bin/stat "$@"
STUB

cat >"$STUB_BIN/mv" <<'STUB'
#!/usr/bin/env bash
source=$1
destination=$2
if [[ -n "${STUB_REPLACE_BEFORE_ASIDE:-}" && \
    "$source" == "$STUB_REPLACE_BEFORE_ASIDE" && \
    "$destination" == "$source.install-superseded" ]]; then
    /bin/mv "$source" "$STUB_CASE_DIR/displaced-valid-before-aside"
    mkdir -p "$source/Contents"
    printf 'interloper before aside\n' >"$source/Contents/marker"
    printf 'fixture\n' >"$source/Contents/Info.plist"
    touch "$source/.invalid-signature"
    "$(dirname "$0")/stat" -f %i "$source" >"$STUB_CASE_DIR/pre-aside-interloper-inode"
fi
exec /bin/mv "$@"
STUB

cat >"$STUB_BIN/security" <<'STUB'
#!/usr/bin/env bash
# No signing identities. Exit successfully, like an empty keychain query, so
# the installer's explicit no-identity diagnostic owns the failure.
exit 0
STUB

if [[ "${FREESIDE_TEST_REAL_RENAME:-false}" != true ]]; then
    cat >"$STUB_BIN/swift" <<'STUB'
#!/usr/bin/env bash
# The recovery invocation is: swift -e <program> <source> <destination>.
program=$2
case "$program" in
*renamex_np*RENAME_EXCL*) ;;
*) exit 65 ;;
esac
source=$3
destination=$4
if [[ -n "${STUB_REPLACE_RENAME_SOURCE:-}" && \
    ! -e "${STUB_CASE_DIR:?}/source-replaced" ]]; then
    touch "$STUB_CASE_DIR/source-replaced"
    mv "$source" "$STUB_CASE_DIR/displaced-valid-recovery"
    mkdir -p "$source/Contents"
    printf 'foreign replacement\n' >"$source/Contents/marker"
    printf 'fixture\n' >"$source/Contents/Info.plist"
    touch "$source/.invalid-signature"
fi
if [[ -n "${STUB_RENAME_INTERLOPER_DEST:-}" ]]; then
    mkdir -p "$STUB_RENAME_INTERLOPER_DEST"
    printf 'foreign at rename\n' >"$STUB_RENAME_INTERLOPER_DEST/marker"
fi
[[ ! -e "$destination" && ! -L "$destination" ]] || exit 1
mv "$source" "$destination"
if [[ -n "${STUB_INTERLOPER_AFTER_QUARANTINE:-}" && \
    "$destination" == *.install-rejected ]]; then
    mkdir -p "$STUB_INTERLOPER_AFTER_QUARANTINE/Contents"
    printf 'interloper after quarantine\n' >"$STUB_INTERLOPER_AFTER_QUARANTINE/Contents/marker"
    printf 'fixture\n' >"$STUB_INTERLOPER_AFTER_QUARANTINE/Contents/Info.plist"
    touch "$STUB_INTERLOPER_AFTER_QUARANTINE/.invalid-signature"
fi
if [[ -n "${STUB_KILL_AFTER_RENAME:-}" ]]; then
    kill -KILL "$PPID"
fi
STUB
fi

chmod +x "$STUB_BIN"/*

pass=0
fail=0
CASE=''
CASE_DIR=''
OUT=''
RC=0

begin_case() {
    CASE=$1
    CASE_DIR=$TMP/case-$2
    mkdir -p "$CASE_DIR/Applications"
    export STUB_CASE_DIR=$CASE_DIR
    unset STUB_BUNDLE_ID
    unset STUB_INTERLOPER_DEST
    unset STUB_RENAME_INTERLOPER_DEST
    unset STUB_REPLACE_RENAME_SOURCE
    unset STUB_KILL_AFTER_RENAME
    unset STUB_INTERLOPER_AFTER_QUARANTINE
    unset STUB_BUILD_SUCCEEDS
    unset STUB_INTERLOPER_AFTER_BUILD
    unset STUB_REPLACE_BEFORE_ASIDE
    unset STUB_SIGNING_IDENTITY
    echo "case: $CASE"
}

report_failure() {
    fail=$((fail + 1))
    echo "FAIL [$CASE]: $*"
    printf '%s\n' "$OUT" | sed 's/^/    | /'
}

run_installer() {
    set +e
    OUT=$(PATH="$STUB_BIN:$PATH" \
        FREESIDE_MAC_INSTALL_DIR="$CASE_DIR/Applications" \
        FREESIDE_MAC_BUILD_DIR="$CASE_DIR/Build" \
        FREESIDE_MAC_SIGNING_IDENTITY="${STUB_SIGNING_IDENTITY-Test Identity}" \
        bash "$INSTALLER" 2>&1)
    RC=$?
    set -e
}

assert_rc() {
    if [[ "$RC" -eq "$1" ]]; then
        pass=$((pass + 1))
    else
        report_failure "expected rc=$1, got rc=$RC"
    fi
}

assert_contains() {
    case "$OUT" in
    *"$1"*) pass=$((pass + 1)) ;;
    *) report_failure "output does not contain: $1" ;;
    esac
}

assert_exists() {
    if [[ -e "$1" || -L "$1" ]]; then
        pass=$((pass + 1))
    else
        report_failure "expected path to exist: $1"
    fi
}

assert_absent() {
    if [[ -e "$1" || -L "$1" ]]; then
        report_failure "expected path to be absent: $1"
    else
        pass=$((pass + 1))
    fi
}

inode() {
    if stat -f %i "$1" >/dev/null 2>&1; then
        stat -f %i "$1"
    else
        stat -c %i "$1"
    fi
}

make_recovery_app() {
    local path=$1
    mkdir -p "$path/Contents"
    printf 'old client\n' >"$path/Contents/marker"
    printf 'fixture\n' >"$path/Contents/Info.plist"
}

# The pre-fix installer fails this case: the later build failure leaves the
# valid app stranded under `.install-superseded` instead of restoring it.
begin_case "valid interrupted install is restored before a later build failure" 1
destination=$CASE_DIR/Applications/Freeside.app
superseded=$destination.install-superseded
make_recovery_app "$superseded"
old_inode=$(inode "$superseded")
run_installer
assert_rc 1
assert_contains "restored an install interrupted before replacement"
assert_contains "build failed"
assert_exists "$destination/Contents/marker"
assert_absent "$superseded"
if [[ -e "$destination" ]] && [[ "$(inode "$destination")" == "$old_inode" ]]; then
    pass=$((pass + 1))
else
    report_failure "restored app did not retain the recovery inode"
fi
guard=$destination.install-recovery-guard
rejected=$destination.install-rejected
assert_exists "$guard"
guard_inode=$(inode "$guard")
guarded_inode=$(inode "$destination")
rm "$CASE_DIR/xcodebuild-called"
run_installer
assert_rc 1
assert_contains "validated the recovery-guarded app"
assert_contains "build failed"
assert_exists "$guard"
if [[ -d "$guard" && ! -L "$guard" && "$(inode "$guard")" == "$guard_inode" ]]; then
    pass=$((pass + 1))
else
    report_failure "valid guarded startup replaced or weakened the recovery guard"
fi
if [[ "$(inode "$destination")" == "$guarded_inode" ]]; then
    pass=$((pass + 1))
else
    report_failure "valid guarded startup replaced the canonical inode"
fi
rm "$CASE_DIR/xcodebuild-called"
touch "$destination/.invalid-signature"
run_installer
assert_rc 1
assert_contains "guarded recovery left an app that failed signature verification"
assert_absent "$destination"
assert_exists "$guard"
if [[ -d "$guard" && ! -L "$guard" && "$(inode "$guard")" == "$guard_inode" ]]; then
    pass=$((pass + 1))
else
    report_failure "invalid quarantine replaced or weakened the recovery guard"
fi
assert_absent "$CASE_DIR/xcodebuild-called"
if [[ "$(inode "$rejected")" == "$guarded_inode" ]]; then
    pass=$((pass + 1))
else
    report_failure "persistent guard did not preserve the later-invalidated inode"
fi
rm -f "$CASE_DIR/xcodebuild-called"
run_installer
assert_rc 1
assert_contains "recovery guard has no app to validate"
assert_contains "proceeding with a fresh install"
assert_contains "build failed"
assert_absent "$destination"
assert_exists "$guard"
if [[ -d "$guard" && ! -L "$guard" && "$(inode "$guard")" == "$guard_inode" ]]; then
    pass=$((pass + 1))
else
    report_failure "fresh guarded install replaced or weakened the recovery guard"
fi
assert_exists "$rejected/.invalid-signature"
assert_exists "$CASE_DIR/xcodebuild-called"
if [[ "$(inode "$rejected")" == "$guarded_inode" ]]; then
    pass=$((pass + 1))
else
    report_failure "fresh guarded install replaced the quarantined inode"
fi

begin_case "guard signature-gates an interloper immediately before swap" 11
destination=$CASE_DIR/Applications/Freeside.app
guard=$destination.install-recovery-guard
mkdir "$guard"
guard_inode=$(inode "$guard")
export STUB_BUILD_SUCCEEDS=true
export STUB_INTERLOPER_AFTER_BUILD=$destination
run_installer
assert_rc 1
assert_contains "recovery guard refused to replace an app that failed signature verification"
assert_contains "It was preserved"
assert_exists "$destination/.invalid-signature"
interloper_inode=$(cat "$CASE_DIR/after-build-interloper-inode")
assert_absent "$destination.install-superseded"
assert_exists "$CASE_DIR/xcodebuild-called"
if [[ -d "$guard" && ! -L "$guard" && "$(inode "$guard")" == "$guard_inode" ]]; then
    pass=$((pass + 1))
else
    report_failure "pre-swap guard check replaced or weakened the recovery guard"
fi
if [[ "$(inode "$destination")" == "$interloper_inode" ]]; then
    pass=$((pass + 1))
else
    report_failure "pre-swap guard check replaced the interloper inode"
fi

begin_case "guard revalidates the exact app moved aside" 12
destination=$CASE_DIR/Applications/Freeside.app
superseded=$destination.install-superseded
guard=$destination.install-recovery-guard
make_recovery_app "$destination"
valid_inode=$(inode "$destination")
mkdir "$guard"
guard_inode=$(inode "$guard")
export STUB_BUILD_SUCCEEDS=true
export STUB_REPLACE_BEFORE_ASIDE=$destination
run_installer
assert_rc 1
assert_contains "recovery-guarded app changed before replacement"
assert_contains "moved object was returned to $destination and preserved"
assert_exists "$destination/.invalid-signature"
interloper_inode=$(cat "$CASE_DIR/pre-aside-interloper-inode")
assert_absent "$superseded"
assert_exists "$CASE_DIR/displaced-valid-before-aside/Contents/marker"
if [[ "$(inode "$destination")" == "$interloper_inode" ]]; then
    pass=$((pass + 1))
else
    report_failure "post-aside validation replaced the interloper inode"
fi
if [[ "$(inode "$CASE_DIR/displaced-valid-before-aside")" == "$valid_inode" ]]; then
    pass=$((pass + 1))
else
    report_failure "pre-aside injection lost the originally validated inode"
fi
if [[ -d "$guard" && ! -L "$guard" && "$(inode "$guard")" == "$guard_inode" ]]; then
    pass=$((pass + 1))
else
    report_failure "post-aside validation replaced or weakened the recovery guard"
fi

begin_case "invalid recovery app fails closed and remains untouched" 2
destination=$CASE_DIR/Applications/Freeside.app
superseded=$destination.install-superseded
make_recovery_app "$superseded"
touch "$superseded/.invalid-signature"
old_inode=$(inode "$superseded")
run_installer
assert_rc 1
assert_contains "failed signature verification"
assert_contains "$superseded. It was preserved"
assert_absent "$destination"
assert_exists "$superseded/Contents/marker"
assert_absent "$CASE_DIR/xcodebuild-called"
if [[ "$(inode "$superseded")" == "$old_inode" ]]; then
    pass=$((pass + 1))
else
    report_failure "invalid recovery object was replaced"
fi

begin_case "symlink recovery object fails closed and preserves its target" 3
destination=$CASE_DIR/Applications/Freeside.app
superseded=$destination.install-superseded
target=$CASE_DIR/foreign-app
make_recovery_app "$target"
ln -s "$target" "$superseded"
run_installer
assert_rc 1
assert_contains "non-bundle recovery object"
assert_contains "preserved"
assert_absent "$destination"
assert_exists "$superseded"
assert_exists "$target/Contents/marker"
assert_absent "$CASE_DIR/xcodebuild-called"
if [[ -L "$superseded" ]] && [[ "$(readlink "$superseded")" == "$target" ]]; then
    pass=$((pass + 1))
else
    report_failure "recovery symlink or its target changed"
fi

begin_case "foreign recovery bundle fails closed and remains untouched" 4
destination=$CASE_DIR/Applications/Freeside.app
superseded=$destination.install-superseded
make_recovery_app "$superseded"
old_inode=$(inode "$superseded")
export STUB_BUNDLE_ID=example.foreign.app
run_installer
assert_rc 1
assert_contains "bundle id 'example.foreign.app'"
assert_contains "It was preserved"
assert_absent "$destination"
assert_exists "$superseded/Contents/marker"
assert_absent "$CASE_DIR/xcodebuild-called"
if [[ "$(inode "$superseded")" == "$old_inode" ]]; then
    pass=$((pass + 1))
else
    report_failure "foreign recovery object was replaced"
fi

begin_case "interloper during verification preserves both directory entries" 5
destination=$CASE_DIR/Applications/Freeside.app
superseded=$destination.install-superseded
make_recovery_app "$superseded"
old_inode=$(inode "$superseded")
export STUB_INTERLOPER_DEST=$destination
run_installer
assert_rc 1
assert_contains "something else appeared at $destination"
assert_contains "recovery app remains at $superseded"
assert_exists "$destination/marker"
assert_absent "$destination/Contents/marker"
assert_exists "$superseded/Contents/marker"
assert_absent "$CASE_DIR/xcodebuild-called"
if [[ "$(inode "$superseded")" == "$old_inode" ]]; then
    pass=$((pass + 1))
else
    report_failure "interloper check replaced the recovery object"
fi

begin_case "recovery precedes signing identity failure" 6
destination=$CASE_DIR/Applications/Freeside.app
superseded=$destination.install-superseded
make_recovery_app "$superseded"
old_inode=$(inode "$superseded")
export STUB_SIGNING_IDENTITY=''
run_installer
assert_rc 1
assert_contains "restored an install interrupted before replacement"
assert_contains "no 'Apple Development' signing identity found"
assert_exists "$destination/Contents/marker"
assert_absent "$superseded"
assert_absent "$CASE_DIR/xcodebuild-called"
if [[ "$(inode "$destination")" == "$old_inode" ]]; then
    pass=$((pass + 1))
else
    report_failure "identity failure did not preserve the restored app inode"
fi

if [[ "${FREESIDE_TEST_REAL_RENAME:-false}" == true ]]; then
    echo "case: rename race injections are covered by the stand-in suite"
else
    begin_case "exclusive rename refuses a last-moment interloper" 7
    destination=$CASE_DIR/Applications/Freeside.app
    superseded=$destination.install-superseded
    make_recovery_app "$superseded"
    old_inode=$(inode "$superseded")
    export STUB_RENAME_INTERLOPER_DEST=$destination
    run_installer
    assert_rc 1
    assert_contains "could not restore the interrupted install"
    assert_contains "The recovery app remains"
    assert_contains "at $superseded and the guard remains"
    assert_exists "$destination.install-recovery-guard"
    assert_exists "$destination/marker"
    assert_absent "$destination/Contents/marker"
    assert_exists "$superseded/Contents/marker"
    assert_absent "$CASE_DIR/xcodebuild-called"
    if [[ "$(inode "$superseded")" == "$old_inode" ]]; then
        pass=$((pass + 1))
    else
        report_failure "exclusive rename replaced the recovery object"
    fi

    begin_case "source replacement is revalidated and rolled back" 8
    destination=$CASE_DIR/Applications/Freeside.app
    superseded=$destination.install-superseded
    make_recovery_app "$superseded"
    old_inode=$(inode "$superseded")
    export STUB_REPLACE_RENAME_SOURCE=true
    run_installer
    assert_rc 1
    assert_contains "changed while it was being restored"
    assert_contains "moved object was returned to $superseded"
    assert_absent "$destination"
    assert_exists "$superseded/Contents/marker"
    assert_exists "$CASE_DIR/displaced-valid-recovery/Contents/marker"
    assert_absent "$CASE_DIR/xcodebuild-called"
    if [[ "$(inode "$CASE_DIR/displaced-valid-recovery")" == "$old_inode" ]]; then
        pass=$((pass + 1))
    else
        report_failure "source replacement deleted or replaced the verified inode"
    fi

    begin_case "persistent guard re-gates a source swap after SIGKILL" 9
    destination=$CASE_DIR/Applications/Freeside.app
    superseded=$destination.install-superseded
    rejected=$destination.install-rejected
    guard=$destination.install-recovery-guard
    make_recovery_app "$superseded"
    old_inode=$(inode "$superseded")
    export STUB_REPLACE_RENAME_SOURCE=true
    export STUB_KILL_AFTER_RENAME=true
    run_installer
    assert_rc 137
    assert_exists "$destination/.invalid-signature"
    untrusted_inode=$(inode "$destination")
    assert_absent "$superseded"
    assert_exists "$guard"
    assert_exists "$CASE_DIR/displaced-valid-recovery/Contents/marker"
    unset STUB_REPLACE_RENAME_SOURCE
    unset STUB_KILL_AFTER_RENAME
    export STUB_INTERLOPER_AFTER_QUARANTINE=$destination
    run_installer
    assert_rc 1
    assert_contains "guarded recovery left an app that failed signature verification"
    assert_contains "preserved at $rejected"
    assert_exists "$destination/.invalid-signature"
    assert_absent "$superseded"
    assert_exists "$guard"
    assert_exists "$rejected/.invalid-signature"
    assert_exists "$CASE_DIR/displaced-valid-recovery/Contents/marker"
    assert_absent "$CASE_DIR/xcodebuild-called"
    if [[ "$(inode "$CASE_DIR/displaced-valid-recovery")" == "$old_inode" ]]; then
        pass=$((pass + 1))
    else
        report_failure "combined source replacement and SIGKILL lost the verified inode"
    fi
    if [[ "$(inode "$rejected")" == "$untrusted_inode" ]]; then
        pass=$((pass + 1))
    else
        report_failure "guarded recovery did not preserve the untrusted inode"
    fi
    interloper_inode=$(inode "$destination")
    unset STUB_INTERLOPER_AFTER_QUARANTINE
    run_installer
    assert_rc 1
    assert_contains "guarded recovery left an app that failed signature verification"
    assert_contains "$rejected is already occupied"
    assert_exists "$guard"
    assert_absent "$CASE_DIR/xcodebuild-called"
    if [[ "$(inode "$destination")" == "$interloper_inode" ]]; then
        pass=$((pass + 1))
    else
        report_failure "re-gating replaced the canonical interloper"
    fi
    if [[ "$(inode "$rejected")" == "$untrusted_inode" ]]; then
        pass=$((pass + 1))
    else
        report_failure "re-gating replaced the first quarantined inode"
    fi
fi

begin_case "untrusted replacement is quarantined and backup restored" 10
destination=$CASE_DIR/Applications/Freeside.app
superseded=$destination.install-superseded
rejected=$destination.install-rejected
make_recovery_app "$destination"
printf 'untrusted replacement\n' >"$destination/Contents/marker"
touch "$destination/.invalid-signature"
make_recovery_app "$superseded"
old_inode=$(inode "$superseded")
untrusted_inode=$(inode "$destination")
run_installer
assert_rc 1
assert_contains "preserved an untrusted interrupted install"
assert_contains "restored an install interrupted before replacement"
assert_contains "build failed"
assert_exists "$destination/Contents/marker"
assert_absent "$superseded"
assert_exists "$rejected/Contents/marker"
if [[ "$(inode "$destination")" == "$old_inode" ]]; then
    pass=$((pass + 1))
else
    report_failure "known-good backup was not restored to the canonical path"
fi
if [[ "$(inode "$rejected")" == "$untrusted_inode" ]]; then
    pass=$((pass + 1))
else
    report_failure "untrusted replacement was not preserved in quarantine"
fi

echo "$pass assertions passed, $fail failed"
[[ "$fail" -eq 0 ]]
