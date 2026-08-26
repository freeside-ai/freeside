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
REAL_OPENSSL=$(command -v openssl || true)
export STUB_REAL_SWIFT=$REAL_SWIFT
export STUB_REAL_OPENSSL=$REAL_OPENSSL

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

STUB_BIN=$TMP/bin
mkdir -p "$STUB_BIN"

cat >"$STUB_BIN/uname" <<'STUB'
#!/usr/bin/env bash
printf 'Darwin\n'
STUB

# Faithful stand-in for the plutil operations install-mac-app.sh performs on
# the daemon plist. The real fix (#762) rewrites ProgramArguments in one
# `-replace … -json` call because `-replace ProgramArguments.<N>` inserts at
# the index instead of overwriting it. Real plutil would be more faithful, but
# the harness's synthetic Info.plist files are not valid plists, so its
# CFBundleIdentifier reads must be stubbed; this stub therefore models the
# whole-array replace directly and rejects the index form so a regression to
# it fails loudly here (installer `die`) rather than passing green-but-blind.
cat >"$STUB_BIN/plutil" <<'STUB'
#!/usr/bin/env bash
target=${!#}
[[ -f "$target" ]] || exit 1
case "${1:-}" in
-extract)
    case "$2" in
    CFBundleIdentifier) printf '%s\n' "${STUB_BUNDLE_ID:-ai.freeside.app.macos}" ;;
    'com\.apple\.application-identifier' | \
        'com\.apple\.developer\.team-identifier' | keychain-access-groups.* | \
        TeamIdentifier.0 | ApplicationIdentifierPrefix.0 | DeveloperCertificates.* | \
        'Entitlements.com\.apple\.application-identifier' | \
        'Entitlements.com\.apple\.developer\.team-identifier' | \
        Entitlements.keychain-access-groups.*)
        key=${2//\\./.}
        value=$(awk -F= -v key="$key" '
            index($0, key "=") == 1 {
                sub(/^[^=]*=/, "")
                print
                found = 1
                exit
            }
            END { exit(found ? 0 : 1) }
        ' "$target") || exit 1
        if [[ "${4:-}" == -o && "${5:-}" != - ]]; then
            printf '%s' "$value" >"$5"
        else
            printf '%s\n' "$value"
        fi
        exit 0
        ;;
    ProgramArguments)
        awk '
            /<key>ProgramArguments<\/key>/ { p=1; next }
            p && /<array>/ { a=1; next }
            a && /<\/array>/ { exit }
            a { print }
        ' "$target" ;;
    ProgramArguments.*)
        # Model `plutil -extract ProgramArguments.N raw`: print the Nth array
        # string (0-based) decoded to raw and exit non-zero when N is out of
        # range, so the installer's per-element guard and its trailing-index
        # bound behave against the stub as they do against real plutil.
        idx=${2#ProgramArguments.}
        if val=$(awk -v idx="$idx" '
            /<key>ProgramArguments<\/key>/ { p=1; next }
            p && /<array>/ { a=1; next }
            a && /<\/array>/ { a=0 }
            a && /<string>/ {
                if (n == idx+0) {
                    line = $0
                    sub(/^[[:space:]]*<string>/, "", line)
                    sub(/<\/string>[[:space:]]*$/, "", line)
                    print line
                    found = 1
                }
                n++
            }
            END { exit(found ? 0 : 1) }
        ' "$target"); then
            printf '%s\n' "$val"
            exit 0
        fi
        exit 1
        ;;
    StandardErrorPath)
        awk '
            /<key>StandardErrorPath<\/key>/ {
                getline
                sub(/^[[:space:]]*<string>/, "")
                sub(/<\/string>[[:space:]]*$/, "")
                print
                exit
            }
        ' "$target" ;;
    *) exit 1 ;;
    esac
    exit 0
    ;;
-replace)
    case "$2" in
    ProgramArguments)
        [[ "$3" == -json ]] || exit 1
        inner=${4#\[}
        inner=${inner%\]}
        inner=${inner#\"}
        inner=${inner%\"}
        sep=$'\x1f'
        inner=${inner//'","'/$sep}
        # Model plutil's JSON string decoding for the escapes json_string
        # emits: \\ and \" (default: the char after the backslash), plus the
        # control-character shortcuts \n \t \r \b \f. A single left-to-right
        # pass, so an escaped backslash binds one backslash, not two. The rare
        # \uXXXX form (only for control bytes a realistic path never carries)
        # is not modelled. Without this the stub would not detect a regression
        # that dropped json_string's encoding.
        unescape_json() {
            local s=$1 out='' i c e
            for ((i = 0; i < ${#s}; i++)); do
                c=${s:i:1}
                if [[ "$c" == '\' && $((i + 1)) -lt ${#s} ]]; then
                    ((i++))
                    e=${s:i:1}
                    case "$e" in
                    n) out+=$'\n' ;;
                    t) out+=$'\t' ;;
                    r) out+=$'\r' ;;
                    b) out+=$'\b' ;;
                    f) out+=$'\f' ;;
                    *) out+=$e ;;
                    esac
                else
                    out+=$c
                fi
            done
            printf '%s' "$out"
        }
        new=""
        old_ifs=$IFS
        IFS=$sep
        for el in $inner; do
            new+="		<string>$(unescape_json "$el")</string>"$'\n'
        done
        IFS=$old_ifs
        STUB_NEW="$new" awk '
            /<key>ProgramArguments<\/key>/ { print; p=1; next }
            p && /<array>/ { print; printf "%s", ENVIRON["STUB_NEW"]; s=1; p=0; next }
            s && /<\/array>/ { print; s=0; next }
            s { next }
            { print }
        ' "$target" >"$target.tmp" && mv "$target.tmp" "$target"
        exit 0
        ;;
    StandardErrorPath)
        escaped=$(printf '%s' "$4" | sed 's/[\\&|]/\\&/g')
        sed "s|__FREESIDE_STDERR_PATH__|$escaped|" "$target" >"$target.tmp"
        mv "$target.tmp" "$target"
        exit 0
        ;;
    *) exit 1 ;;
    esac
    ;;
esac
exit 1
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
*" --display --entitlements :- "*)
    cat "$target/Contents/entitlements.fixture"
    ;;
*" --display "*)
    printf '%s\n' '# designated => identifier "ai.freeside.app.macos"' >&2
    ;;
*" --force --sign "*)
    printf 'called\n' >"${STUB_CASE_DIR:?}/codesign-sign-called"
    printf '%s\n' "$*" >>"$STUB_CASE_DIR/codesign-sign-calls"
    if [[ -d "$target/Contents" ]]; then
        previous=''
        entitlements=''
        for argument in "$@"; do
            if [[ "$previous" == --entitlements ]]; then
                entitlements=$argument
                break
            fi
            previous=$argument
        done
        [[ -n "$entitlements" ]] || exit 1
        cp "$entitlements" "$target/Contents/entitlements.fixture"
        if [[ -n "${STUB_CODESIGN_DROP_ENTITLEMENTS:-}" ]]; then
            sed 's/^keychain-access-groups.0=.*/keychain-access-groups.0=WRONGTEAM0.ai.freeside.app.macos/' \
                "$target/Contents/entitlements.fixture" \
                >"$target/Contents/entitlements.fixture.tmp"
            mv "$target/Contents/entitlements.fixture.tmp" \
                "$target/Contents/entitlements.fixture"
        fi
    fi
    ;;
*) exit 1 ;;
esac
STUB

cat >"$STUB_BIN/xcodebuild" <<'STUB'
#!/usr/bin/env bash
printf 'called\n' >"${STUB_CASE_DIR:?}/xcodebuild-called"
printf '%s\n' "$@" >"$STUB_CASE_DIR/xcodebuild-args"
if [[ -n "${STUB_BUILD_SUCCEEDS:-}" ]]; then
    built_app="$STUB_CASE_DIR/Build/Build/Products/Release/FreesideMac.app"
    mkdir -p "$built_app/Contents"
    printf 'new client\n' >"$built_app/Contents/marker"
    printf 'fixture\n' >"$built_app/Contents/Info.plist"
    mkdir -p "$built_app/Contents/Library/LaunchAgents"
    cp "$STUB_AGENT_TEMPLATE" \
        "$built_app/Contents/Library/LaunchAgents/ai.freeside.daemon.plist"
    team_id=''
    for argument in "$@"; do
        case "$argument" in
        DEVELOPMENT_TEAM=*) team_id=${argument#DEVELOPMENT_TEAM=} ;;
        esac
    done
    signed_team=${STUB_SIGNED_TEAM_ID:-$team_id}
    signed_application=${STUB_SIGNED_APPLICATION_ID:-$signed_team.ai.freeside.app.macos}
    signed_group=${STUB_SIGNED_KEYCHAIN_GROUP:-$signed_application}
    cat >"$built_app/Contents/entitlements.fixture" <<EOF
com.apple.application-identifier=$signed_application
com.apple.developer.team-identifier=$signed_team
keychain-access-groups.0=$signed_group
EOF
    profile_team=${STUB_PROFILE_TEAM_ID:-$team_id}
    profile_prefix=${STUB_PROFILE_APPLICATION_PREFIX:-$profile_team}
    profile_application=${STUB_PROFILE_APPLICATION_ID:-$profile_prefix.ai.freeside.app.macos}
    profile_group=${STUB_PROFILE_KEYCHAIN_GROUP:-$profile_application}
    profile_team_entitlement=${STUB_PROFILE_TEAM_ENTITLEMENT:-$profile_team}
    profile_certificate=${STUB_PROFILE_CERTIFICATE:-Zml4dHVyZSBjZXJ0aWZpY2F0ZQ==}
    cat >"$built_app/Contents/embedded.provisionprofile" <<EOF
TeamIdentifier.0=$profile_team
ApplicationIdentifierPrefix.0=$profile_prefix
DeveloperCertificates.0=$profile_certificate
Entitlements.com.apple.application-identifier=$profile_application
Entitlements.com.apple.developer.team-identifier=$profile_team_entitlement
Entitlements.keychain-access-groups.0=$profile_group
EOF
    if [[ -n "${STUB_PROFILE_KEYCHAIN_GROUP_1:-}" ]]; then
        printf 'Entitlements.keychain-access-groups.1=%s\n' \
            "$STUB_PROFILE_KEYCHAIN_GROUP_1" >>"$built_app/Contents/embedded.provisionprofile"
    fi
    if [[ -n "${STUB_PROFILE_CERTIFICATE_1:-}" ]]; then
        printf 'DeveloperCertificates.1=%s\n' \
            "$STUB_PROFILE_CERTIFICATE_1" >>"$built_app/Contents/embedded.provisionprofile"
    fi
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
case "${1:-}" in
find-identity)
    if [[ -n "${FREESIDE_MAC_SIGNING_IDENTITY:-}" && \
        "$FREESIDE_MAC_SIGNING_IDENTITY" != - && \
        ! "$FREESIDE_MAC_SIGNING_IDENTITY" =~ ^[0-9A-Fa-f]{40}$ ]]; then
        printf '  1) 1111111111111111111111111111111111111111 "%s"\n' \
            "$FREESIDE_MAC_SIGNING_IDENTITY"
    fi
    exit 0
    ;;
find-certificate)
    if [[ " $* " == *" -a "* && " $* " == *" -Z "* ]]; then
        printf 'SHA-1 hash: 1111111111111111111111111111111111111111\n'
    fi
    printf '%s\n' "${STUB_SIGNER_CERTIFICATE:-fixture certificate}"
    ;;
cms)
    while [[ $# -gt 0 ]]; do
        if [[ "$1" == -i && $# -ge 2 ]]; then
            cat "$2"
            exit
        fi
        shift
    done
    exit 1
    ;;
*) exit 1 ;;
esac
STUB

cat >"$STUB_BIN/openssl" <<'STUB'
#!/usr/bin/env bash
if [[ "${1:-}" == base64 ]]; then
    [[ -n "${STUB_REAL_OPENSSL:-}" ]] || exit 1
    exec "$STUB_REAL_OPENSSL" "$@"
fi
certificate=$(cat)
case " $* " in
*" -outform DER "*)
    previous=''
    output=''
    for argument in "$@"; do
        if [[ "$previous" == -out ]]; then
            output=$argument
            break
        fi
        previous=$argument
    done
    [[ -n "$output" ]] || exit 1
    printf '%s' "$certificate" >"$output"
    ;;
*) printf '    organizationalUnitName = ABCDE12345\n' ;;
esac
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
# Mirror the shipped bundle plist (app/Apps/macOS/LaunchAgents/…): the same
# reused `__FREESIDE_STATE_DIR__/freeside.db` db token and one string per line,
# so the fixture exercises the real template shape rather than a divergent one.
cat >"$STUB_AGENT_TEMPLATE" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>BundleProgram</key>
	<string>Contents/Resources/freesided</string>
	<key>ProgramArguments</key>
	<array>
		<string>freesided</string>
		<string>-db</string>
		<string>__FREESIDE_STATE_DIR__/freeside.db</string>
		<string>-state-dir</string>
		<string>__FREESIDE_STATE_DIR__</string>
		<string>-listen</string>
		<string>127.0.0.1:7331</string>
	</array>
	<key>StandardErrorPath</key>
	<string>__FREESIDE_STDERR_PATH__</string>
</dict>
</plist>
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
    unset STUB_SIGNED_TEAM_ID
    unset STUB_SIGNED_APPLICATION_ID
    unset STUB_SIGNED_KEYCHAIN_GROUP
    unset STUB_PROFILE_TEAM_ID
    unset STUB_PROFILE_APPLICATION_PREFIX
    unset STUB_PROFILE_APPLICATION_ID
    unset STUB_PROFILE_KEYCHAIN_GROUP
    unset STUB_PROFILE_KEYCHAIN_GROUP_1
    unset STUB_PROFILE_TEAM_ENTITLEMENT
    unset STUB_PROFILE_CERTIFICATE
    unset STUB_PROFILE_CERTIFICATE_1
    unset STUB_SIGNER_CERTIFICATE
    unset STUB_CODESIGN_DROP_ENTITLEMENTS
    unset RUN_HOME
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
        HOME="${RUN_HOME:-$CASE_DIR/home}" \
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
    if [[ -f "$path" ]] && grep -Fq -- "$text" "$path"; then
        pass=$((pass + 1))
    else
        report_failure "expected $path to contain: $text"
    fi
}

assert_file_omits() {
    local path=$1
    local text=$2
    if [[ -f "$path" ]] && ! grep -Fq -- "$text" "$path"; then
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

# Extract the ProgramArguments string vector, one element per line, from a
# templated LaunchAgent plist. Works uniformly on the stubbed and (were the
# harness ever run against it) real plutil output, since both emit standard
# plist XML with one <string> per line inside the array.
program_arguments_of() {
    awk '
        /<key>ProgramArguments<\/key>/ { p = 1; next }
        p && /<array>/ { a = 1; next }
        a && /<\/array>/ { exit }
        a {
            line = $0
            sub(/^[[:space:]]*<string>/, "", line)
            sub(/<\/string>[[:space:]]*$/, "", line)
            print line
        }
    ' "$1"
}

# Pin the whole templated argument vector, not just placeholder absence: the
# #762 defect shipped a doubled 9-element vector, which an omits-only check
# would miss if a placeholder happened to resolve. $1 is the plist; the rest
# are the expected arguments in order.
assert_program_arguments() {
    local path=$1
    shift
    local expected
    expected=$(printf '%s\n' "$@")
    local actual
    actual=$(program_arguments_of "$path")
    if [[ "$actual" == "$expected" ]]; then
        pass=$((pass + 1))
    else
        report_failure "ProgramArguments mismatch in $path"$'\n'"expected:"$'\n'"$expected"$'\n'"actual:"$'\n'"$actual"
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
export STUB_FAIL_VERIFY_CALL=4
run_installer
assert_rc 1
assert_contains "the app failed signature verification: $destination"
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
export STUB_FAIL_VERIFY_CALL=4
run_installer
assert_rc 1
assert_contains "the app failed signature verification: $destination"
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
assert_contains "the app failed signature verification: $destination"
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

begin_case "ad-hoc signing is rejected before building" ad-hoc
export STUB_SIGNING_IDENTITY=-
run_installer
assert_rc 1
assert_contains "ad-hoc signing cannot authorize the Data Protection Keychain access group"
assert_absent "$CASE_DIR/xcodebuild-called"

begin_case "profile authorization mismatch fails before replacement" profile-mismatch
destination=$CASE_DIR/Applications/Freeside.app
make_recovery_app "$destination"
old_inode=$(inode "$destination")
export STUB_BUILD_SUCCEEDS=true
export STUB_PROFILE_KEYCHAIN_GROUP=ABCDE12345.ai.freeside.other
run_installer
assert_rc 1
assert_contains "the profile-authorized Keychain access group"
assert_inode "$destination" "$old_inode" \
    "profile mismatch replaced the existing app"

begin_case "signed macOS application identifier mismatch fails before replacement" signed-app-id-mismatch
destination=$CASE_DIR/Applications/Freeside.app
make_recovery_app "$destination"
old_inode=$(inode "$destination")
export STUB_BUILD_SUCCEEDS=true
export STUB_SIGNED_APPLICATION_ID=ABCDE12345.ai.freeside.other
run_installer
assert_rc 1
assert_contains "the signed application identifier"
assert_inode "$destination" "$old_inode" \
    "signed application identifier mismatch replaced the existing app"

begin_case "profile application identifier mismatch fails before replacement" profile-app-id-mismatch
destination=$CASE_DIR/Applications/Freeside.app
make_recovery_app "$destination"
old_inode=$(inode "$destination")
export STUB_BUILD_SUCCEEDS=true
export STUB_PROFILE_APPLICATION_ID=ABCDE12345.ai.freeside.other
run_installer
assert_rc 1
assert_contains "the profile-authorized application identifier"
assert_inode "$destination" "$old_inode" \
    "profile application identifier mismatch replaced the existing app"

begin_case "profile team entitlement mismatch fails before replacement" profile-team-mismatch
destination=$CASE_DIR/Applications/Freeside.app
make_recovery_app "$destination"
old_inode=$(inode "$destination")
export STUB_BUILD_SUCCEEDS=true
export STUB_PROFILE_TEAM_ENTITLEMENT=ZXCVB67890
run_installer
assert_rc 1
assert_contains "the profile-authorized team identifier"
assert_inode "$destination" "$old_inode" \
    "profile team entitlement mismatch replaced the existing app"

begin_case "profile signer mismatch fails before replacement" profile-signer-mismatch
destination=$CASE_DIR/Applications/Freeside.app
make_recovery_app "$destination"
old_inode=$(inode "$destination")
export STUB_BUILD_SUCCEEDS=true
export STUB_PROFILE_CERTIFICATE=b3RoZXIgY2VydGlmaWNhdGU=
run_installer
assert_rc 1
assert_contains "the selected signing certificate is not authorized"
assert_inode "$destination" "$old_inode" \
    "profile signer mismatch replaced the existing app"

begin_case "profile signer allowlist may match after another certificate" profile-signer-second-entry
destination=$CASE_DIR/Applications/Freeside.app
export STUB_BUILD_SUCCEEDS=true
export STUB_PROFILE_CERTIFICATE=b3RoZXIgY2VydGlmaWNhdGU=
export STUB_PROFILE_CERTIFICATE_1=Zml4dHVyZSBjZXJ0aWZpY2F0ZQ==
run_installer
assert_rc 0
assert_exists "$destination"
assert_file_contains \
    "$destination/Contents/embedded.provisionprofile" \
    'DeveloperCertificates.1=Zml4dHVyZSBjZXJ0aWZpY2F0ZQ=='

begin_case "malformed profile signer fails before replacement" malformed-profile-signer
destination=$CASE_DIR/Applications/Freeside.app
make_recovery_app "$destination"
old_inode=$(inode "$destination")
export STUB_BUILD_SUCCEEDS=true
export STUB_PROFILE_CERTIFICATE='not base64!'
run_installer
assert_rc 1
assert_contains "the profile has a malformed developer certificate at index 0"
assert_inode "$destination" "$old_inode" \
    "malformed profile signer replaced the existing app"

begin_case "team wildcard profile authorizes the exact signed group" profile-wildcard
destination=$CASE_DIR/Applications/Freeside.app
export STUB_BUILD_SUCCEEDS=true
export STUB_PROFILE_APPLICATION_ID='ABCDE12345.*'
export STUB_PROFILE_KEYCHAIN_GROUP='ABCDE12345.*'
run_installer
assert_rc 0
assert_exists "$destination"
assert_file_contains \
    "$destination/Contents/embedded.provisionprofile" \
    'Entitlements.keychain-access-groups.0=ABCDE12345.*'

begin_case "scoped profile wildcards authorize exact signed identifiers" scoped-profile-wildcard
destination=$CASE_DIR/Applications/Freeside.app
export STUB_BUILD_SUCCEEDS=true
export STUB_PROFILE_APPLICATION_ID='ABCDE12345.ai.freeside.*'
export STUB_PROFILE_KEYCHAIN_GROUP='ABCDE12345.ai.freeside.*'
run_installer
assert_rc 0
assert_exists "$destination"
assert_file_contains \
    "$destination/Contents/entitlements.fixture" \
    'com.apple.application-identifier=ABCDE12345.ai.freeside.app.macos'

begin_case "profile group allowlist may match after an unrelated entry" profile-group-second-entry
destination=$CASE_DIR/Applications/Freeside.app
export STUB_BUILD_SUCCEEDS=true
export STUB_PROFILE_KEYCHAIN_GROUP=com.apple.token
export STUB_PROFILE_KEYCHAIN_GROUP_1='ABCDE12345.ai.freeside.*'
run_installer
assert_rc 0
assert_exists "$destination"
assert_file_contains \
    "$destination/Contents/embedded.provisionprofile" \
    'Entitlements.keychain-access-groups.1=ABCDE12345.ai.freeside.*'

begin_case "legacy App ID prefix is verified independently of team" legacy-prefix
destination=$CASE_DIR/Applications/Freeside.app
export STUB_BUILD_SUCCEEDS=true
export STUB_PROFILE_APPLICATION_PREFIX=ZXCVB67890
export STUB_PROFILE_APPLICATION_ID=ZXCVB67890.ai.freeside.app.macos
export STUB_PROFILE_KEYCHAIN_GROUP='ZXCVB67890.*'
export STUB_SIGNED_APPLICATION_ID=ZXCVB67890.ai.freeside.app.macos
export STUB_SIGNED_KEYCHAIN_GROUP=ZXCVB67890.ai.freeside.app.macos
run_installer
assert_rc 0
assert_exists "$destination"
assert_file_contains \
    "$destination/Contents/embedded.provisionprofile" \
    'TeamIdentifier.0=ABCDE12345'
assert_file_contains \
    "$destination/Contents/embedded.provisionprofile" \
    'ApplicationIdentifierPrefix.0=ZXCVB67890'
assert_file_contains \
    "$destination/Contents/entitlements.fixture" \
    'keychain-access-groups.0=ZXCVB67890.ai.freeside.app.macos'

begin_case "post-sign entitlement mismatch fails before replacement" entitlement-mismatch
destination=$CASE_DIR/Applications/Freeside.app
make_recovery_app "$destination"
old_inode=$(inode "$destination")
export STUB_BUILD_SUCCEEDS=true
export STUB_CODESIGN_DROP_ENTITLEMENTS=true
run_installer
assert_rc 1
assert_contains "the signed Keychain access group"
assert_inode "$destination" "$old_inode" \
    "post-sign entitlement mismatch replaced the existing app"

begin_case "daemon is bundled and state paths are templated before the final seal" 29
destination=$CASE_DIR/Applications/Freeside.app
export STUB_BUILD_SUCCEEDS=true
run_installer
assert_rc 0
agent=$destination/Contents/Library/LaunchAgents/ai.freeside.daemon.plist
daemon_dir=$CASE_DIR/home/Library/Application\ Support/Freeside/daemon
assert_file_contains "$agent" "Contents/Resources/freesided"
assert_file_contains \
    "$agent" "$CASE_DIR/home/Library/Application Support/Freeside/daemon/freesided.log"
assert_file_omits "$agent" "__FREESIDE_"
# Exact vector: freesided binds -db, -state-dir, and the fixed listener with
# no surviving placeholder and no doubled positional args (#762).
assert_program_arguments "$agent" \
    freesided \
    -db "$daemon_dir/freeside.db" \
    -state-dir "$daemon_dir" \
    -listen 127.0.0.1:7331
assert_file_contains "$destination/Contents/Resources/freesided" "#!/usr/bin/env bash"
assert_exists "$CASE_DIR/codesign-sign-called"
assert_file_contains "$CASE_DIR/xcodebuild-args" "-allowProvisioningUpdates"
assert_file_contains "$CASE_DIR/xcodebuild-args" "CODE_SIGN_STYLE=Automatic"
assert_file_contains "$CASE_DIR/xcodebuild-args" "CODE_SIGN_IDENTITY=Apple Development"
assert_file_contains "$CASE_DIR/xcodebuild-args" "DEVELOPMENT_TEAM=ABCDE12345"
assert_file_omits "$CASE_DIR/xcodebuild-args" "CODE_SIGN_STYLE=Manual"
assert_file_contains "$CASE_DIR/codesign-sign-calls" "--entitlements"
assert_file_contains \
    "$destination/Contents/entitlements.fixture" \
    "keychain-access-groups.0=ABCDE12345.ai.freeside.app.macos"
assert_exists "$destination/Contents/embedded.provisionprofile"
assert_file_contains \
    "$SCRIPT_DIR/../app/Apps/macOS/LaunchAgents/ai.freeside.daemon.plist" \
    "Contents/Resources/freesided"
assert_file_contains \
    "$CASE_DIR/defaults-calls" \
    "before-build delete ai.freeside.app.macos FreesideLaunchAgentRegistrationCurrent"
assert_file_contains \
    "$CASE_DIR/defaults-calls" \
    "after-build delete ai.freeside.app.macos FreesideLaunchAgentRegistrationCurrent"

# A state path bearing a JSON metacharacter must bind byte-for-byte. Here HOME
# carries a literal backslash before `t`; a raw interpolation into the `-json`
# fragment would let plutil decode `\t` into a tab and bind a path that is not
# the created directory. The exact-vector assertion pins the literal backslash,
# so dropping json_string's encoding reddens this case.
begin_case "state paths with a JSON metacharacter bind literally" json-metachar
export STUB_BUILD_SUCCEEDS=true
bs=$'\\'
meta_home="$CASE_DIR/home${bs}tstate"
RUN_HOME=$meta_home run_installer
assert_rc 0
agent=$CASE_DIR/Applications/Freeside.app/Contents/Library/LaunchAgents/ai.freeside.daemon.plist
meta_dir=$meta_home/Library/Application\ Support/Freeside/daemon
assert_program_arguments "$agent" \
    freesided \
    -db "$meta_dir/freeside.db" \
    -state-dir "$meta_dir" \
    -listen 127.0.0.1:7331

# A legal home path may itself contain "__FREESIDE_"; the guard must verify the
# bound values exactly, not reject that substring, or it would abort every
# install for such an operator. The old prefix guard reddens this case.
begin_case "state paths containing __FREESIDE_ install cleanly" freeside-substr
export STUB_BUILD_SUCCEEDS=true
sub_home="$CASE_DIR/__FREESIDE_home"
RUN_HOME=$sub_home run_installer
assert_rc 0
agent=$CASE_DIR/Applications/Freeside.app/Contents/Library/LaunchAgents/ai.freeside.daemon.plist
sub_dir=$sub_home/Library/Application\ Support/Freeside/daemon
assert_program_arguments "$agent" \
    freesided \
    -db "$sub_dir/freeside.db" \
    -state-dir "$sub_dir" \
    -listen 127.0.0.1:7331

# XML metacharacters in a legal home path must round-trip literally through the
# raw per-element guard: an xml1 re-extraction would hand them back escaped
# (&amp; etc.) and abort a valid install. Enumerate &, <, and > together.
begin_case "state paths with XML metacharacters install cleanly" xml-metachar
export STUB_BUILD_SUCCEEDS=true
xml_home="$CASE_DIR/home&a<b>c"
RUN_HOME=$xml_home run_installer
assert_rc 0
agent=$CASE_DIR/Applications/Freeside.app/Contents/Library/LaunchAgents/ai.freeside.daemon.plist
xml_dir=$xml_home/Library/Application\ Support/Freeside/daemon
assert_program_arguments "$agent" \
    freesided \
    -db "$xml_dir/freeside.db" \
    -state-dir "$xml_dir" \
    -listen 127.0.0.1:7331

# A control character (here a tab) is a legal pathname byte that `-json`
# rejects raw; json_string must encode it (\t) so the whole-array rewrite
# still installs, preserving the prior per-index `-string` behavior. The tab
# is mid-path so the line-based assertions read it intact. (Real plutil's JSON
# strictness is proven against json_string directly in the PR verification; the
# stub is lenient, so this case exercises the encode/decode plumbing.)
begin_case "state paths with a control character install cleanly" ctrl-char
export STUB_BUILD_SUCCEEDS=true
tab_home="$CASE_DIR/home"$'\t'"state"
RUN_HOME=$tab_home run_installer
assert_rc 0
agent=$CASE_DIR/Applications/Freeside.app/Contents/Library/LaunchAgents/ai.freeside.daemon.plist
tab_dir=$tab_home/Library/Application\ Support/Freeside/daemon
assert_program_arguments "$agent" \
    freesided \
    -db "$tab_dir/freeside.db" \
    -state-dir "$tab_dir" \
    -listen 127.0.0.1:7331

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
