#!/usr/bin/env bash
# test-install-mac-app.sh — focused filesystem regressions for the Mac installer.
#
# Issues #464 and #458 pin the restart and ordinary-install state machines:
# crash recovery, swap-and-rollback fault injection, destination guards, and
# the server-URL validator. The ordinary suite uses command stand-ins for the
# build and signing boundaries, so it needs neither Xcode nor a signing
# identity and runs on Linux as well as macOS. URL cases pass through to a real
# Swift toolchain when one is available and report explicit skips otherwise.
# `FREESIDE_TEST_REAL_RENAME=true` uses macOS Swift for the production-exclusive
# rename while keeping the build and signing boundaries stubbed.
#
# Exit code: 0 when every assertion passes, 1 otherwise.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
INSTALLER=${FREESIDE_INSTALLER_UNDER_TEST:-$SCRIPT_DIR/../app/scripts/install-mac-app.sh}
REAL_SWIFT=$(command -v swift || true)
export STUB_REAL_SWIFT=$REAL_SWIFT

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
target=${!#}
[[ -f "$target" ]] || exit 1
if [[ "${1:-}" == -replace ]]; then
    key=$2
    value=$4
    case "$key" in
    ProgramArguments.2) token=__FREESIDE_DB_PATH__ ;;
    ProgramArguments.4) token=__FREESIDE_STATE_DIR__ ;;
    StandardErrorPath) token=__FREESIDE_STDERR_PATH__ ;;
    *) exit 1 ;;
    esac
    escaped=$(printf '%s' "$value" | sed 's/[\\&|]/\\&/g')
    sed "s|$token|$escaped|" "$target" >"$target.tmp"
    mv "$target.tmp" "$target"
    exit 0
fi
printf '%s\n' "${STUB_BUNDLE_ID:-ai.freeside.app.macos}"
STUB

cat >"$STUB_BIN/codesign" <<'STUB'
#!/usr/bin/env bash
target=${!#}
case " $* " in
*" --verify "*)
    count_file="${STUB_CASE_DIR:?}/codesign-verify-count"
    count=0
    [[ ! -f "$count_file" ]] || read -r count <"$count_file"
    count=$((count + 1))
    printf '%s\n' "$count" >"$count_file"
    if [[ -n "${STUB_INTERLOPER_DEST:-}" ]]; then
        mkdir -p "$STUB_INTERLOPER_DEST"
        printf 'foreign contents\n' >"$STUB_INTERLOPER_DEST/marker"
    fi
    if [[ -n "${STUB_INTERLOPER_ON_VERIFY_DEST:-}" && \
        "$target" == "$STUB_INTERLOPER_ON_VERIFY_DEST" ]]; then
        /bin/mv "$target" "$STUB_CASE_DIR/displaced-staged-app"
        mkdir -p "$target"
        printf 'rollback interloper\n' >"$target/marker"
        exit 1
    fi
    if [[ -n "${STUB_FAIL_VERIFY_CALL:-}" && \
        "$count" -eq "$STUB_FAIL_VERIFY_CALL" ]]; then
        exit 1
    fi
    [[ ! -e "$target/.invalid-signature" ]]
    ;;
*" --display "*)
    printf '%s\n' '# designated => identifier "ai.freeside.app.macos"' >&2
    ;;
*" --force --sign "*)
    printf 'called\n' >"${STUB_CASE_DIR:?}/codesign-sign-called"
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
    mkdir -p "$built_app/Contents/Library/LaunchAgents"
    cp "$STUB_AGENT_TEMPLATE" \
        "$built_app/Contents/Library/LaunchAgents/ai.freeside.daemon.plist"
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

cat >"$STUB_BIN/defaults" <<'STUB'
#!/usr/bin/env bash
phase=before-build
[[ -e "${STUB_CASE_DIR:?}/xcodebuild-called" ]] && phase=after-build
printf '%s %s\n' "$phase" "$*" >>"$STUB_CASE_DIR/defaults-calls"
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
if [[ -n "${STUB_INTERLOPER_AFTER_ASIDE:-}" && \
    "$destination" == "$source.install-superseded" ]]; then
    /bin/mv "$source" "$destination"
    case "$STUB_INTERLOPER_AFTER_ASIDE" in
    file)
        printf 'foreign file\n' >"$source"
        ;;
    directory)
        mkdir -p "$source"
        printf 'foreign directory\n' >"$source/marker"
        ;;
    *) exit 64 ;;
    esac
    exit 0
fi
/bin/mv "$@"
rc=$?
if [[ "$rc" -eq 0 && -n "${STUB_TERM_AFTER_MV_SOURCE:-}" && \
    "$source" == "$STUB_TERM_AFTER_MV_SOURCE" ]]; then
    kill -TERM "$PPID"
fi
exit "$rc"
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
*'import Foundation'*)
    [[ -n "${STUB_REAL_SWIFT:-}" ]] || exit 65
    exec "$STUB_REAL_SWIFT" "$@"
    ;;
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

STUB_AGENT_TEMPLATE=$TMP/ai.freeside.daemon.plist
export STUB_AGENT_TEMPLATE
cat >"$STUB_AGENT_TEMPLATE" <<'PLIST'
<plist><dict>
<key>BundleProgram</key><string>Contents/Resources/freesided</string>
<key>ProgramArguments</key><array>
<string>freesided</string><string>-db</string><string>__FREESIDE_DB_PATH__</string>
<string>-state-dir</string><string>__FREESIDE_STATE_DIR__</string>
<string>-listen</string><string>127.0.0.1:7331</string>
</array>
<key>StandardErrorPath</key><string>__FREESIDE_STDERR_PATH__</string>
</dict></plist>
PLIST

pass=0
fail=0
skip=0
CASE=''
CASE_DIR=''
OUT=''
RC=0

begin_case() {
    CASE=$1
    CASE_DIR=$TMP/case-$2
    mkdir -p "$CASE_DIR/Applications" "$CASE_DIR/home"
    printf '#!/usr/bin/env bash\n' >"$CASE_DIR/freesided"
    chmod +x "$CASE_DIR/freesided"
    export STUB_CASE_DIR=$CASE_DIR
    unset STUB_BUNDLE_ID
    unset STUB_INTERLOPER_DEST
    unset STUB_INTERLOPER_ON_VERIFY_DEST
    unset STUB_RENAME_INTERLOPER_DEST
    unset STUB_REPLACE_RENAME_SOURCE
    unset STUB_KILL_AFTER_RENAME
    unset STUB_INTERLOPER_AFTER_QUARANTINE
    unset STUB_BUILD_SUCCEEDS
    unset STUB_INTERLOPER_AFTER_BUILD
    unset STUB_REPLACE_BEFORE_ASIDE
    unset STUB_INTERLOPER_AFTER_ASIDE
    unset STUB_TERM_AFTER_MV_SOURCE
    unset STUB_FAIL_VERIFY_CALL
    unset STUB_SIGNING_IDENTITY
    echo "case: $CASE"
}

report_failure() {
    fail=$((fail + 1))
    echo "FAIL [$CASE]: $*"
    printf '%s\n' "$OUT" | sed 's/^/    | /'
}

run_installer() {
    run_installer_raw --daemon-path "$CASE_DIR/freesided" "$@"
}

run_installer_raw() {
    set +e
    OUT=$(PATH="$STUB_BIN:$PATH" \
        CLANG_MODULE_CACHE_PATH="$TMP/swift-module-cache" \
        FREESIDE_MAC_INSTALL_DIR="$CASE_DIR/Applications" \
        FREESIDE_MAC_BUILD_DIR="$CASE_DIR/Build" \
        FREESIDE_MAC_SIGNING_IDENTITY="${STUB_SIGNING_IDENTITY-Test Identity}" \
        HOME="$CASE_DIR/home" \
        bash "$INSTALLER" "$@" 2>&1)
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

assert_file_contains() {
    local path=$1
    local text=$2
    if [[ -f "$path" ]] && grep -Fq "$text" "$path"; then
        pass=$((pass + 1))
    else
        report_failure "expected $path to contain: $text"
    fi
}

assert_file_omits() {
    local path=$1
    local text=$2
    if [[ -f "$path" ]] && ! grep -Fq "$text" "$path"; then
        pass=$((pass + 1))
    else
        report_failure "expected $path to omit: $text"
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

assert_inode() {
    local path=$1
    local expected=$2
    local description=$3
    if [[ -e "$path" ]] && [[ "$(inode "$path")" == "$expected" ]]; then
        pass=$((pass + 1))
    else
        report_failure "$description"
    fi
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

begin_case "first install verification failure removes the replacement" 13
destination=$CASE_DIR/Applications/Freeside.app
export STUB_BUILD_SUCCEEDS=true
export STUB_FAIL_VERIFY_CALL=3
run_installer
assert_rc 1
assert_contains "the installed app failed signature verification"
assert_contains "removed the unverified install"
assert_absent "$destination"
assert_absent "$destination.install-superseded"
assert_absent "$destination.install-staging"

begin_case "first install signal after rename removes the replacement" 14
destination=$CASE_DIR/Applications/Freeside.app
export STUB_BUILD_SUCCEEDS=true
export STUB_TERM_AFTER_MV_SOURCE=$destination.install-staging
run_installer
assert_rc 130
assert_contains "removed the unverified install"
assert_absent "$destination"
assert_absent "$destination.install-superseded"
assert_absent "$destination.install-staging"

begin_case "update verification failure restores the previous inode" 15
destination=$CASE_DIR/Applications/Freeside.app
superseded=$destination.install-superseded
make_recovery_app "$destination"
old_inode=$(inode "$destination")
export STUB_BUILD_SUCCEEDS=true
export STUB_FAIL_VERIFY_CALL=3
run_installer
assert_rc 1
assert_contains "the installed app failed signature verification"
assert_contains "restored the previous install"
assert_exists "$destination/Contents/marker"
assert_absent "$superseded"
assert_inode "$destination" "$old_inode" \
    "verification rollback did not preserve the previous install inode"

begin_case "signal between update renames restores the previous inode" 16
destination=$CASE_DIR/Applications/Freeside.app
superseded=$destination.install-superseded
make_recovery_app "$destination"
old_inode=$(inode "$destination")
export STUB_BUILD_SUCCEEDS=true
export STUB_TERM_AFTER_MV_SOURCE=$destination
run_installer
assert_rc 130
assert_contains "restored the previous install"
assert_exists "$destination/Contents/marker"
assert_absent "$superseded"
assert_inode "$destination" "$old_inode" \
    "signal rollback did not preserve the previous install inode"

begin_case "foreign file appearing after aside survives rollback" 17
destination=$CASE_DIR/Applications/Freeside.app
superseded=$destination.install-superseded
make_recovery_app "$destination"
old_inode=$(inode "$destination")
export STUB_BUILD_SUCCEEDS=true
export STUB_INTERLOPER_AFTER_ASIDE=file
run_installer
assert_rc 1
assert_contains "$destination reappeared mid-swap"
assert_contains "something else now occupies $destination"
assert_exists "$destination"
assert_exists "$superseded/Contents/marker"
if [[ -f "$destination" && "$(<"$destination")" == "foreign file" ]]; then
    pass=$((pass + 1))
else
    report_failure "foreign file did not survive the rollback refusal"
fi
assert_inode "$superseded" "$old_inode" \
    "rollback refusal did not preserve the previous install inode"

begin_case "foreign directory appearing after aside survives rollback" 18
destination=$CASE_DIR/Applications/Freeside.app
superseded=$destination.install-superseded
make_recovery_app "$destination"
old_inode=$(inode "$destination")
export STUB_BUILD_SUCCEEDS=true
export STUB_INTERLOPER_AFTER_ASIDE=directory
run_installer
assert_rc 1
assert_contains "$destination reappeared mid-swap"
assert_contains "something else now occupies $destination"
assert_exists "$destination/marker"
assert_contains "previous install was left at $superseded"
if [[ "$(<"$destination/marker")" == "foreign directory" ]]; then
    pass=$((pass + 1))
else
    report_failure "foreign directory contents did not survive the rollback refusal"
fi
assert_inode "$superseded" "$old_inode" \
    "directory interloper displaced the previous install backup"

begin_case "interloper during rollback preserves the previous install aside" 19
destination=$CASE_DIR/Applications/Freeside.app
superseded=$destination.install-superseded
make_recovery_app "$destination"
old_inode=$(inode "$destination")
export STUB_BUILD_SUCCEEDS=true
export STUB_INTERLOPER_ON_VERIFY_DEST=$destination
run_installer
assert_rc 1
assert_contains "the installed app failed signature verification"
assert_contains "something else now occupies $destination"
assert_contains "previous install was left at $superseded"
assert_exists "$destination/marker"
assert_exists "$CASE_DIR/displaced-staged-app/Contents/marker"
assert_inode "$superseded" "$old_inode" \
    "rollback interloper displaced the previous install backup"

begin_case "dangling destination symlink is refused intact" 20
destination=$CASE_DIR/Applications/Freeside.app
target=$CASE_DIR/missing-target
ln -s "$target" "$destination"
run_installer
assert_rc 1
assert_contains "exists and is not an app bundle"
assert_absent "$CASE_DIR/xcodebuild-called"
if [[ -L "$destination" && "$(readlink "$destination")" == "$target" ]]; then
    pass=$((pass + 1))
else
    report_failure "dangling destination symlink changed"
fi
assert_absent "$target"

begin_case "live destination symlink is refused intact" 21
destination=$CASE_DIR/Applications/Freeside.app
target=$CASE_DIR/live-target
make_recovery_app "$target"
target_inode=$(inode "$target")
ln -s "$target" "$destination"
run_installer
assert_rc 1
assert_contains "exists and is not an app bundle"
assert_absent "$CASE_DIR/xcodebuild-called"
if [[ -L "$destination" && "$(readlink "$destination")" == "$target" ]]; then
    pass=$((pass + 1))
else
    report_failure "live destination symlink changed"
fi
assert_inode "$target" "$target_inode" "live symlink target changed"

begin_case "regular file destination is refused intact" 22
destination=$CASE_DIR/Applications/Freeside.app
printf 'foreign file\n' >"$destination"
entry_inode=$(inode "$destination")
run_installer
assert_rc 1
assert_contains "exists and is not an app bundle"
assert_absent "$CASE_DIR/xcodebuild-called"
assert_inode "$destination" "$entry_inode" "regular destination file changed"

begin_case "fifo destination is refused intact" 23
destination=$CASE_DIR/Applications/Freeside.app
mkfifo "$destination"
entry_inode=$(inode "$destination")
run_installer
assert_rc 1
assert_contains "exists and is not an app bundle"
assert_absent "$CASE_DIR/xcodebuild-called"
assert_inode "$destination" "$entry_inode" "destination fifo changed"

begin_case "directory without Info.plist is refused intact" 24
destination=$CASE_DIR/Applications/Freeside.app
mkdir -p "$destination/Contents"
printf 'foreign contents\n' >"$destination/marker"
entry_inode=$(inode "$destination")
run_installer
assert_rc 1
assert_contains "holds bundle id 'unknown'"
assert_absent "$CASE_DIR/xcodebuild-called"
assert_exists "$destination/marker"
assert_inode "$destination" "$entry_inode" \
    "directory without Info.plist changed"

begin_case "foreign bundle identifier is refused intact" 25
destination=$CASE_DIR/Applications/Freeside.app
make_recovery_app "$destination"
entry_inode=$(inode "$destination")
export STUB_BUNDLE_ID=example.foreign.app
run_installer
assert_rc 1
assert_contains "holds bundle id 'example.foreign.app'"
assert_absent "$CASE_DIR/xcodebuild-called"
assert_inode "$destination" "$entry_inode" \
    "foreign bundle destination changed"

begin_case "daemon path is required before building" 26
destination=$CASE_DIR/Applications/Freeside.app
run_installer_raw
assert_rc 1
assert_contains "--daemon-path is required"
assert_absent "$destination"
assert_absent "$CASE_DIR/xcodebuild-called"

begin_case "daemon path must be absolute" 27
run_installer_raw --daemon-path ./freesided
assert_rc 1
assert_contains "--daemon-path must be absolute"
assert_absent "$CASE_DIR/xcodebuild-called"

begin_case "daemon path must be executable" 28
daemon=$CASE_DIR/not-executable
printf 'not executable\n' >"$daemon"
run_installer_raw --daemon-path "$daemon"
assert_rc 1
assert_contains "--daemon-path is not an executable file"
assert_absent "$CASE_DIR/xcodebuild-called"

begin_case "daemon is bundled and state paths are templated before the final seal" 29
destination=$CASE_DIR/Applications/Freeside.app
export STUB_BUILD_SUCCEEDS=true
run_installer
assert_rc 0
agent=$destination/Contents/Library/LaunchAgents/ai.freeside.daemon.plist
assert_file_contains "$agent" "Contents/Resources/freesided"
assert_file_contains \
    "$agent" "$CASE_DIR/home/Library/Application Support/Freeside/daemon/freeside.db"
assert_file_contains \
    "$agent" "$CASE_DIR/home/Library/Application Support/Freeside/daemon"
assert_file_contains \
    "$agent" "$CASE_DIR/home/Library/Application Support/Freeside/daemon/freesided.log"
assert_file_omits "$agent" "__FREESIDE_"
assert_file_contains "$destination/Contents/Resources/freesided" "#!/usr/bin/env bash"
assert_exists "$CASE_DIR/codesign-sign-called"
assert_file_contains \
    "$SCRIPT_DIR/../app/Apps/macOS/LaunchAgents/ai.freeside.daemon.plist" \
    "Contents/Resources/freesided"
assert_file_contains \
    "$CASE_DIR/defaults-calls" \
    "before-build delete ai.freeside.app.macos FreesideLaunchAgentRegistrationCurrent"
assert_file_contains \
    "$CASE_DIR/defaults-calls" \
    "after-build delete ai.freeside.app.macos FreesideLaunchAgentRegistrationCurrent"

begin_case "registration invalidation precedes a later build failure" 30
run_installer
assert_rc 1
assert_contains "build failed"
assert_file_contains \
    "$CASE_DIR/defaults-calls" \
    "before-build delete ai.freeside.app.macos FreesideLaunchAgentRegistrationCurrent"
assert_file_omits \
    "$CASE_DIR/defaults-calls" \
    "after-build delete ai.freeside.app.macos FreesideLaunchAgentRegistrationCurrent"

has_real_swift=false
if [[ -n "$REAL_SWIFT" ]] && "$REAL_SWIFT" --version >/dev/null 2>&1; then
    has_real_swift=true
fi

run_rejected_url_case() {
    local name=$1
    local slug=$2
    local value=$3
    begin_case "$name" "$slug"
    if [[ "$has_real_swift" != true ]]; then
        skip=$((skip + 1))
        echo "SKIP [$CASE]: no real Swift toolchain; macOS CI runs this case"
        return
    fi
    destination=$CASE_DIR/Applications/Freeside.app
    run_installer --server-url "$value"
    assert_rc 1
    assert_contains "--server-url is not an http(s) URL with a host"
    assert_absent "$destination"
    assert_absent "$CASE_DIR/xcodebuild-called"
}

run_accepted_url_case() {
    local name=$1
    local slug=$2
    local value=$3
    begin_case "$name" "$slug"
    if [[ "$has_real_swift" != true ]]; then
        skip=$((skip + 1))
        echo "SKIP [$CASE]: no real Swift toolchain; macOS CI runs this case"
        return
    fi
    run_installer --server-url "$value"
    assert_rc 1
    assert_contains "build failed"
    assert_exists "$CASE_DIR/xcodebuild-called"
}

run_rejected_url_case "server URL rejects port zero" url-reject-0 \
    "http://localhost:0"
run_rejected_url_case "server URL rejects port 65536" url-reject-65536 \
    "http://localhost:65536"
run_rejected_url_case "server URL rejects port 99999" url-reject-99999 \
    "http://localhost:99999"
run_rejected_url_case "server URL rejects an overflowing port" url-reject-overflow \
    "http://localhost:18446744073709551616"
run_rejected_url_case "server URL rejects a leading-zero port" url-reject-leading-zero \
    "http://localhost:0080"
run_rejected_url_case "server URL rejects a trailing colon" url-reject-colon \
    "http://localhost:"
run_rejected_url_case "server URL rejects an unmatched IPv6 bracket" url-reject-bracket \
    "http://["
run_rejected_url_case "server URL rejects an invalid percent escape" url-reject-percent \
    "http://%"
run_rejected_url_case "server URL rejects a missing host" url-reject-host \
    "http://"
run_rejected_url_case "server URL rejects a non-http scheme" url-reject-scheme \
    "ftp://x"
run_rejected_url_case "server URL rejects an empty flag value" url-reject-empty ""

run_accepted_url_case "server URL accepts port 65535" url-accept-65535 \
    "http://localhost:65535"
run_accepted_url_case "server URL accepts an ordinary port" url-accept-port \
    "http://localhost:8080"
run_accepted_url_case "server URL accepts portless https with a path" url-accept-path \
    "https://example.com/api/v1"
run_accepted_url_case "server URL accepts a bracketed IPv6 host" url-accept-ipv6 \
    "http://[::1]:8080"

if [[ "${FREESIDE_CAN_GO_RED_CHILD:-false}" != true ]]; then
    begin_case "rollback mutation makes the suite fail" can-go-red-rollback
    mutant=$CASE_DIR/install-mac-app-no-rollback.sh
    sed "s/^        mv \"\\\$superseded\" \"\\\$destination\" &&$/        true \&\&/" \
        "$INSTALLER" >"$mutant"
    if cmp -s "$INSTALLER" "$mutant" ||
        ! grep -Fxq "        true &&" "$mutant" ||
        ! bash -n "$mutant"; then
        report_failure "rollback mutation anchor did not match the installer"
    else
        pass=$((pass + 1))
    fi
    set +e
    FREESIDE_CAN_GO_RED_CHILD=true \
        FREESIDE_INSTALLER_UNDER_TEST=$mutant \
        bash "$0" >"$CASE_DIR/mutant-suite.log" 2>&1
    mutant_rc=$?
    set -e
    if [[ "$mutant_rc" -ne 0 ]] &&
        grep -Fq "FAIL [update verification failure restores the previous inode]" \
            "$CASE_DIR/mutant-suite.log"; then
        pass=$((pass + 1))
    else
        OUT=$(tail -40 "$CASE_DIR/mutant-suite.log")
        report_failure "rollback mutant did not fail the targeted swap case"
    fi

    begin_case "URL-validator mutation makes the suite fail" can-go-red-url
    if [[ "$has_real_swift" != true ]]; then
        skip=$((skip + 1))
        echo "SKIP [$CASE]: no real Swift toolchain; macOS CI runs this case"
    else
        mutant=$CASE_DIR/install-mac-app-no-url-validation.sh
        sed "s/^if \\[\\[ \"\\\$server_url_given\" == true \\]\\] && ! swift -e /if false \&\& ! swift -e /" \
            "$INSTALLER" >"$mutant"
        if cmp -s "$INSTALLER" "$mutant" || ! bash -n "$mutant"; then
            report_failure "URL-validator mutation anchor did not match the installer"
        else
            pass=$((pass + 1))
        fi
        set +e
        FREESIDE_CAN_GO_RED_CHILD=true \
            FREESIDE_INSTALLER_UNDER_TEST=$mutant \
            bash "$0" >"$CASE_DIR/mutant-suite.log" 2>&1
        mutant_rc=$?
        set -e
        if [[ "$mutant_rc" -ne 0 ]] &&
            grep -Fq "FAIL [server URL rejects port zero]" \
                "$CASE_DIR/mutant-suite.log"; then
            pass=$((pass + 1))
        else
            OUT=$(tail -40 "$CASE_DIR/mutant-suite.log")
            report_failure "URL-validator mutant did not fail the targeted URL case"
        fi
    fi
fi

echo "$pass assertions passed, $fail failed, $skip skipped"
[[ "$fail" -eq 0 ]]
