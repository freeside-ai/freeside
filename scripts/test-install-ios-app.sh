#!/usr/bin/env bash
# test-install-ios-app.sh — focused regressions for the iOS installer.
#
# Pins install-ios-app.sh's argument contract, team-ID resolution, build
# invocation, and devicectl install/launch dispatch. Command stand-ins
# replace the build, signing, and device boundaries (xcodebuild, xcrun
# devicectl, security, openssl), so the suite needs neither Xcode nor a
# device and runs on Linux as well as macOS. The two cases that exercise
# --server-url pass through to a real Swift toolchain (the installer's own
# Foundation URL validator) when one is present and report explicit skips
# otherwise.
#
# Exit code: 0 when every assertion passes, 1 otherwise.
set -euo pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
INSTALLER=${FREESIDE_INSTALLER_UNDER_TEST:-$SCRIPT_DIR/../app/scripts/install-ios-app.sh}
IOS_INFO_PLIST=$SCRIPT_DIR/../app/Apps/iOS/Info.plist
XCODE_PROJECT=$SCRIPT_DIR/../app/Freeside.xcodeproj/project.pbxproj
REAL_SWIFT=$(command -v swift || true)

# The UDID of the single device in the default fixture the xcrun stand-in
# writes for `devicectl list devices`. Keep it in sync with that JSON below;
# the installer resolves --device to it and threads it through build,
# install, and launch.
ASHPOOL_UDID="00008150-001E45A13E98401C"

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

STUB_BIN=$TMP/bin
mkdir -p "$STUB_BIN"

cat >"$STUB_BIN/uname" <<'STUB'
#!/usr/bin/env bash
printf 'Darwin\n'
STUB

# find-identity lists the sole Apple Development identity when STUB_HAS_IDENTITY
# is set (matching the name shape resolve_identity greps for); find-certificate
# prints a placeholder PEM the openssl stub turns into a subject. An empty
# find-identity query models a keychain with no signing identity, so the
# installer's own no-identity diagnostic owns that failure.
cat >"$STUB_BIN/security" <<'STUB'
#!/usr/bin/env bash
case "${1:-}" in
find-identity)
    if [[ -n "${STUB_TWO_IDENTITIES:-}" ]]; then
        printf '  1) AAAAAAAAAAAAAAAA "Apple Development: Operator One (TAGONE0001)"\n'
        printf '  2) BBBBBBBBBBBBBBBB "Apple Development: Operator Two (TAGTWO0002)"\n'
    elif [[ -n "${STUB_HAS_IDENTITY:-}" ]]; then
        printf '  1) DEADBEEFDEADBEEF "Apple Development: Operator (PERSONALXY)"\n'
    fi
    exit 0
    ;;
find-certificate)
    # `-a -Z` (multi-identity listing) enumerates every certificate with its
    # SHA-1 hash; tag each block's PEM with its fingerprint so the openssl
    # stand-in can map it to a distinct team OU. A plain `-c <name>` lookup
    # (single-identity path) prints one untagged PEM.
    if printf '%s\n' "$@" | grep -q -- '-Z'; then
        printf 'SHA-1 hash: AAAAAAAAAAAAAAAA\n'
        printf -- '-----BEGIN CERTIFICATE-----\nSTUBCERT AAAAAAAAAAAAAAAA\n-----END CERTIFICATE-----\n'
        printf 'SHA-1 hash: BBBBBBBBBBBBBBBB\n'
        printf -- '-----BEGIN CERTIFICATE-----\nSTUBCERT BBBBBBBBBBBBBBBB\n-----END CERTIFICATE-----\n'
    else
        printf -- '-----BEGIN CERTIFICATE-----\nSTUBCERT\n-----END CERTIFICATE-----\n'
    fi
    exit 0
    ;;
esac
exit 0
STUB

# Model `openssl x509 -noout -subject -nameopt multiline`: emit the subject
# with the organizationalUnitName line resolve_team_id reads the Team ID from.
cat >"$STUB_BIN/openssl" <<'STUB'
#!/usr/bin/env bash
# Read the PEM piped in from the `security` stand-in before printing. The
# real `openssl x509` reads its stdin; a stand-in that exits without
# consuming it lets the upstream `security` stub take SIGPIPE under the
# installer's `pipefail`, which flaked the Team-ID pipeline with exit 141.
# A fingerprint-tagged PEM (the `-a -Z` multi-identity listing) maps to that
# identity's own team via STUB_TEAM_OU_A/_B, so the test can assert each row
# shows a distinct OU; an untagged PEM (single-identity path) uses STUB_TEAM_OU.
pem="$(cat)"
ou="${STUB_TEAM_OU:-ABCDE12345}"
case "$pem" in
*"STUBCERT AAAAAAAAAAAAAAAA"*) ou="${STUB_TEAM_OU_A:-$ou}" ;;
*"STUBCERT BBBBBBBBBBBBBBBB"*) ou="${STUB_TEAM_OU_B:-$ou}" ;;
esac
printf '    organizationalUnitName    = %s\n' "$ou"
exit 0
STUB

# Record the build invocation and synthesize the product the installer then
# checks for. Fails loudly (STUB_BUILD_FAILS) to exercise the build-failure
# path. The product path mirrors -derivedDataPath/Build/Products/Release-
# iphoneos/FreesideIOS.app.
cat >"$STUB_BIN/xcodebuild" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$*" >"${STUB_CASE_DIR:?}/xcodebuild-args"
if [[ -n "${STUB_BUILD_FAILS:-}" ]]; then
    echo "stub xcodebuild: forced failure" >&2
    exit 65
fi
built="${FREESIDE_IOS_BUILD_DIR:?}/Build/Products/Release-iphoneos/FreesideIOS.app"
mkdir -p "$built"
printf 'app\n' >"$built/marker"
exit 0
STUB

# Dispatch the two devicectl subcommands the installer uses, recording each
# full argument line so the assertions can pin what was installed/launched
# and with which arguments.
cat >"$STUB_BIN/xcrun" <<'STUB'
#!/usr/bin/env bash
if [[ "${1:-}" == devicectl ]]; then
    printf '%s\n' "$*" >>"${STUB_CASE_DIR:?}/devicectl-calls"
    case "$*" in
    *"list devices"*)
        # Write the device inventory to the --json-output path the installer
        # passes. STUB_DEVICES_FIXTURE overrides the default single device
        # for the zero- and multiple-match cases.
        out=""
        prev=""
        for arg in "$@"; do
            [[ "$prev" == "--json-output" ]] && out="$arg"
            prev="$arg"
        done
        [[ -n "$out" ]] || { echo "stub devicectl: no --json-output path" >&2; exit 1; }
        if [[ -n "${STUB_DEVICES_FIXTURE:-}" ]]; then
            cat "$STUB_DEVICES_FIXTURE" >"$out"
        else
            cat >"$out" <<'JSON'
{
  "info": { "outputType": "devices" },
  "result": {
    "devices": [
      {
        "identifier": "AAAA1111-2222-3333-4444-555566667777",
        "deviceProperties": { "name": "Ashpool" },
        "hardwareProperties": {
          "udid": "00008150-001E45A13E98401C",
          "serialNumber": "F4GABC123XYZ",
          "ecid": "12345678901234",
          "platform": "iOS"
        }
      }
    ]
  }
}
JSON
        fi
        ;;
    *"device install app"*)
        [[ -z "${STUB_INSTALL_FAILS:-}" ]] || { echo "stub devicectl: install failed" >&2; exit 7; }
        ;;
    *"device process launch"*)
        [[ -z "${STUB_LAUNCH_FAILS:-}" ]] || { echo "stub devicectl: launch failed" >&2; exit 8; }
        ;;
    esac
    exit 0
fi
exit 0
STUB

# The installer runs two `swift -e` programs: the device-list resolver
# (JSONSerialization) and the --server-url validator (Foundation URL).
# Dispatch on the program text like install-mac-app.sh's stub. The resolver
# has a bash fallback that returns the single fixture device's UDID, so the
# Linux job still exercises the installer's resolve -> build/install/launch
# plumbing without a Swift toolchain; field-matching and zero/multiple-match
# semantics are covered by the real-Swift cases. The URL validator has no
# fallback, so its cases skip when no real Swift is present.
export STUB_REAL_SWIFT=$REAL_SWIFT
cat >"$STUB_BIN/swift" <<'STUB'
#!/usr/bin/env bash
program=$2
case "$program" in
*JSONSerialization*)
    if [[ -n "${STUB_REAL_SWIFT:-}" ]]; then exec "$STUB_REAL_SWIFT" "$@"; fi
    udid=$(grep -o '"udid"[[:space:]]*:[[:space:]]*"[^"]*"' "$3" | head -1 |
        sed -E 's/.*"([^"]*)"$/\1/')
    [[ -n "$udid" ]] || { echo "stub resolver: no udid in $3" >&2; exit 1; }
    printf '%s\n' "$udid"
    exit 0
    ;;
*'import Foundation'*)
    if [[ -n "${STUB_REAL_SWIFT:-}" ]]; then exec "$STUB_REAL_SWIFT" "$@"; fi
    exit 65
    ;;
*) exit 65 ;;
esac
STUB

chmod +x "$STUB_BIN"/*

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
    mkdir -p "$CASE_DIR/Build" "$CASE_DIR/home"
    export STUB_CASE_DIR=$CASE_DIR
    unset STUB_HAS_IDENTITY
    unset STUB_TWO_IDENTITIES
    unset STUB_TEAM_OU
    unset STUB_TEAM_OU_A
    unset STUB_TEAM_OU_B
    unset STUB_BUILD_FAILS
    unset STUB_INSTALL_FAILS
    unset STUB_LAUNCH_FAILS
    unset STUB_DEVICES_FIXTURE
    unset FREESIDE_IOS_TEAM_ID
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
        CLANG_MODULE_CACHE_PATH="$TMP/swift-module-cache" \
        FREESIDE_IOS_BUILD_DIR="$CASE_DIR/Build" \
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

assert_file_contains() {
    local path=$1 text=$2
    if [[ -f "$path" ]] && grep -Fq -- "$text" "$path"; then
        pass=$((pass + 1))
    else
        report_failure "expected $path to contain: $text"
    fi
}

assert_file_omits() {
    local path=$1 text=$2
    if [[ ! -f "$path" ]] || ! grep -Fq -- "$text" "$path"; then
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

# --- Argument contract -----------------------------------------------------

begin_case "missing --device is refused before any build" args-device
run_installer
assert_rc 1
assert_contains "--device is required"
assert_absent "$CASE_DIR/xcodebuild-args"

begin_case "an unknown argument is refused" args-unknown
run_installer --device Phone --frobnicate
assert_rc 1
assert_contains "unknown argument: --frobnicate"

begin_case "--server-url without --launch is refused as a no-op" args-url-no-launch
export FREESIDE_IOS_TEAM_ID=ABCDE12345
run_installer --device Phone --server-url http://100.64.0.1:7331
assert_rc 1
assert_contains "requires --launch"
assert_absent "$CASE_DIR/xcodebuild-args"

begin_case "the iOS target scopes cleartext HTTP to the tailnet" ats-tailnet
assert_file_contains "$IOS_INFO_PLIST" "<key>NSAppTransportSecurity</key>"
assert_file_contains "$IOS_INFO_PLIST" "<key>100.64.0.0/10</key>"
assert_file_contains "$IOS_INFO_PLIST" "<key>NSExceptionAllowsInsecureHTTPLoads</key>"
assert_file_omits "$IOS_INFO_PLIST" "NSAllowsArbitraryLoads"
assert_file_contains "$XCODE_PROJECT" "INFOPLIST_FILE = Apps/iOS/Info.plist"

# --- Team-ID resolution ----------------------------------------------------

begin_case "no identity and no override fails with the mint-one diagnostic" team-none
run_installer --device Phone
assert_rc 1
assert_contains "no 'Apple Development' signing identity found"
assert_absent "$CASE_DIR/xcodebuild-args"

begin_case "the Team ID is derived from the certificate OU" team-from-cert
export STUB_HAS_IDENTITY=1
export STUB_TEAM_OU=PERSONALXY
run_installer --device Ashpool
assert_rc 0
assert_contains "signing team: PERSONALXY"
assert_file_contains "$CASE_DIR/xcodebuild-args" "DEVELOPMENT_TEAM=PERSONALXY"

begin_case "several identities list each identity's own Team ID" team-multi
export STUB_TWO_IDENTITIES=1
# Distinct OUs keyed by fingerprint: the two identities share a resolution
# path but must each surface their own team, which a common-name lookup
# (both names differ here, but one Apple ID across teams prints the same
# name) could not guarantee. Keying on the fingerprint does.
export STUB_TEAM_OU_A=TEAMONE0001
export STUB_TEAM_OU_B=TEAMTWO0002
run_installer --device Ashpool
assert_rc 1
assert_contains "several 'Apple Development' identities found"
assert_contains "not the name's parenthetical"
assert_contains "TEAMONE0001  Apple Development: Operator One (TAGONE0001)"
assert_contains "TEAMTWO0002  Apple Development: Operator Two (TAGTWO0002)"
assert_absent "$CASE_DIR/xcodebuild-args"

begin_case "FREESIDE_IOS_TEAM_ID overrides certificate discovery" team-override
export FREESIDE_IOS_TEAM_ID=OVERRIDE12
run_installer --device Ashpool
assert_rc 0
assert_contains "signing team: OVERRIDE12"
assert_file_contains "$CASE_DIR/xcodebuild-args" "DEVELOPMENT_TEAM=OVERRIDE12"

# --- Build invocation ------------------------------------------------------

begin_case "the build targets the resolved device with automatic signing" build-invocation
export FREESIDE_IOS_TEAM_ID=ABCDE12345
run_installer --device Ashpool
assert_rc 0
assert_file_contains "$CASE_DIR/xcodebuild-args" "-scheme FreesideIOS"
# Concrete destination, not generic: -allowProvisioningDeviceRegistration
# registers the destination device, so a first-seen phone must be named.
assert_file_contains "$CASE_DIR/xcodebuild-args" "-destination platform=iOS,id=$ASHPOOL_UDID"
assert_file_omits "$CASE_DIR/xcodebuild-args" "generic/platform=iOS"
assert_file_contains "$CASE_DIR/xcodebuild-args" "CODE_SIGN_STYLE=Automatic"
assert_file_contains "$CASE_DIR/xcodebuild-args" "-allowProvisioningUpdates"
assert_file_contains "$CASE_DIR/xcodebuild-args" "-allowProvisioningDeviceRegistration"

begin_case "a build failure aborts before install" build-failure
export FREESIDE_IOS_TEAM_ID=ABCDE12345
export STUB_BUILD_FAILS=1
run_installer --device Ashpool
assert_rc 1
assert_contains "build failed"
# Resolution ran (list devices), but the build failed before any install.
assert_file_omits "$CASE_DIR/devicectl-calls" "device install app"

# --- Install and launch dispatch -------------------------------------------

begin_case "a successful run installs the built app on the resolved device" install-ok
export FREESIDE_IOS_TEAM_ID=ABCDE12345
run_installer --device Ashpool
assert_rc 0
assert_contains "installed ai.freeside.app.ios on $ASHPOOL_UDID"
assert_file_contains "$CASE_DIR/devicectl-calls" "device install app --device $ASHPOOL_UDID"
assert_file_contains "$CASE_DIR/devicectl-calls" "Release-iphoneos/FreesideIOS.app"
assert_file_omits "$CASE_DIR/devicectl-calls" "process launch"

begin_case "an install failure surfaces and does not launch" install-failure
export FREESIDE_IOS_TEAM_ID=ABCDE12345
export STUB_INSTALL_FAILS=1
run_installer --device Ashpool --launch
assert_rc 1
assert_contains "could not install the app"
assert_file_omits "$CASE_DIR/devicectl-calls" "process launch"

begin_case "--launch without a URL launches with no server argument" launch-plain
export FREESIDE_IOS_TEAM_ID=ABCDE12345
run_installer --device Ashpool --launch
assert_rc 0
assert_file_contains "$CASE_DIR/devicectl-calls" "device process launch --device $ASHPOOL_UDID ai.freeside.app.ios"
assert_file_omits "$CASE_DIR/devicectl-calls" "FreesideServerURL"

begin_case "--launch-only retries an installed app without signing or building" launch-only
run_installer --device Ashpool --launch-only
assert_rc 0
assert_file_contains "$CASE_DIR/devicectl-calls" "device process launch --device $ASHPOOL_UDID ai.freeside.app.ios"
assert_absent "$CASE_DIR/xcodebuild-args"
assert_file_omits "$CASE_DIR/devicectl-calls" "device install app"

# --- Cases that need a real Swift toolchain --------------------------------
# The device-list resolver and the --server-url validator both run under
# `swift -e`; these cases exercise the real parsers.

if [[ -n "$REAL_SWIFT" ]]; then
    begin_case "a device name resolves to its UDID for build, install, and launch" resolve-by-name
    export FREESIDE_IOS_TEAM_ID=ABCDE12345
    run_installer --device Ashpool --launch
    assert_rc 0
    assert_file_contains "$CASE_DIR/xcodebuild-args" "-destination platform=iOS,id=$ASHPOOL_UDID"
    assert_file_contains "$CASE_DIR/devicectl-calls" "device install app --device $ASHPOOL_UDID"
    assert_file_contains "$CASE_DIR/devicectl-calls" "device process launch --device $ASHPOOL_UDID"

    begin_case "a UDID selector resolves to itself" resolve-by-udid
    export FREESIDE_IOS_TEAM_ID=ABCDE12345
    run_installer --device "$ASHPOOL_UDID"
    assert_rc 0
    assert_file_contains "$CASE_DIR/xcodebuild-args" "-destination platform=iOS,id=$ASHPOOL_UDID"

    begin_case "a serial-number selector resolves to the UDID" resolve-by-serial
    export FREESIDE_IOS_TEAM_ID=ABCDE12345
    run_installer --device F4GABC123XYZ
    assert_rc 0
    assert_file_contains "$CASE_DIR/xcodebuild-args" "-destination platform=iOS,id=$ASHPOOL_UDID"

    begin_case "an unmatched --device is refused before the build" resolve-zero
    export FREESIDE_IOS_TEAM_ID=ABCDE12345
    run_installer --device Neuromancer
    assert_rc 1
    assert_contains "no connected device matched 'Neuromancer'"
    assert_contains "Ashpool"
    assert_absent "$CASE_DIR/xcodebuild-args"

    begin_case "an ambiguous --device is refused before the build" resolve-multi
    export FREESIDE_IOS_TEAM_ID=ABCDE12345
    cat >"$CASE_DIR/fixture.json" <<'JSON'
{ "result": { "devices": [
  { "identifier": "D1", "deviceProperties": { "name": "Twins" },
    "hardwareProperties": { "udid": "00008150-AAAA0001", "serialNumber": "S1", "ecid": "1", "platform": "iOS" } },
  { "identifier": "D2", "deviceProperties": { "name": "Twins" },
    "hardwareProperties": { "udid": "00008150-BBBB0002", "serialNumber": "S2", "ecid": "2", "platform": "iOS" } }
] } }
JSON
    export STUB_DEVICES_FIXTURE="$CASE_DIR/fixture.json"
    run_installer --device Twins
    assert_rc 1
    assert_contains "'Twins' matched 2 connected devices"
    assert_absent "$CASE_DIR/xcodebuild-args"

    begin_case "--launch with a valid URL passes it as a launch argument" launch-url
    export FREESIDE_IOS_TEAM_ID=ABCDE12345
    run_installer --device Ashpool --server-url http://100.64.0.1:7331 --launch
    assert_rc 0
    assert_file_contains "$CASE_DIR/devicectl-calls" \
        "ai.freeside.app.ios -- -FreesideServerURL http://100.64.0.1:7331"

    begin_case "--launch-only passes a URL without rebuilding" launch-only-url
    run_installer --device Ashpool --server-url http://100.64.0.1:7331 --launch-only
    assert_rc 0
    assert_file_contains "$CASE_DIR/devicectl-calls" \
        "ai.freeside.app.ios -- -FreesideServerURL http://100.64.0.1:7331"
    assert_absent "$CASE_DIR/xcodebuild-args"

    begin_case "a malformed --server-url is rejected before the build" url-malformed
    export FREESIDE_IOS_TEAM_ID=ABCDE12345
    run_installer --device Ashpool --server-url 'http://[' --launch
    assert_rc 1
    assert_contains "not an http(s) URL"
    assert_absent "$CASE_DIR/xcodebuild-args"
else
    skip=$((skip + 7))
    echo "skip: resolver and --server-url cases (no swift toolchain for the swift -e parsers)"
fi

echo
echo "install-ios-app tests: $pass passed, $fail failed, $skip skipped"
[[ "$fail" -eq 0 ]]
