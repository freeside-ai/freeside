#!/usr/bin/env bash
# install-ios-app.sh — build, sign, and install FreesideIOS on the
# operator's iPhone under free provisioning (plan §10, issue #847).
#
# Usage: install-ios-app.sh --device <name-or-udid> [--server-url <url>] [--launch]
#        install-ios-app.sh --device <name-or-udid> [--server-url <url>] --launch-only
#
# Builds the Release product with automatic signing against a free
# personal team, then installs it on a physical device with
# `xcrun devicectl device install app`. Re-running it after a source
# change updates the installed app in place: the bundle identifier and
# team stay fixed across runs, so the on-device Keychain credential still
# names the same application and the operator never re-pairs (the same
# stability the Mac installer holds for its signing identity). What free
# provisioning cannot make durable is the profile itself: a personal-team
# provisioning profile expires seven days after it is minted, so the app
# stops launching until this script is re-run to re-sign and reinstall.
# app/README.md documents that cadence.
#
# --device is required. It accepts a device name, UDID, ECID, or serial;
# the script resolves it to the connected device's UDID (via
# `xcrun devicectl list devices`) and builds for that concrete destination,
# so `-allowProvisioningDeviceRegistration` registers this phone rather than
# a generic one. devicectl also accepts a DNS name, but this resolver does
# not (a DNS alias maps to no UDID in the fields it reads), so pass one of
# the four forms above. List the connected devices with
# `xcrun devicectl list devices`.
#
# --server-url points the app at the operator's daemon. Unlike the Mac
# installer, an iOS app's preferences cannot be written from the host, so
# the URL is passed as a launch argument (`-FreesideServerURL`) to the
# first pairing launch; it therefore requires --launch or --launch-only.
# --launch-only retries that launch without rebuilding or reinstalling, which
# is needed after trusting a personal-team developer certificate. AppSession then
# persists it on-device at pairing (issue #847 step 1), so later
# home-screen launches reach the same daemon with no argument. The URL is
# validated with the client's own parser, identically to the Mac
# installer, so a malformed one fails before the multi-minute build.
#
# Team ID, in resolution order:
#   FREESIDE_IOS_TEAM_ID  explicit 10-character Apple Developer team ID
#   otherwise             the organizational unit of the sole
#                         "Apple Development:" identity's certificate
#                         (Xcode's free personal team mints one once an
#                         Apple ID is added under Settings > Accounts)
#
# With more than one identity there is no sole certificate to read, so
# FREESIDE_IOS_TEAM_ID is required. Its value is the certificate's OU, NOT
# the 10-character tag `security find-identity` prints in the identity name;
# that parenthetical looks like a Team ID but is a different identifier. The
# multi-identity error below lists each identity's real Team ID to copy.
#
# Environment:
#   FREESIDE_IOS_BUILD_DIR  derived data (default: app/DerivedData/ios-install)
#
# Requires: macOS, Xcode, a physical iPhone (iOS 17+) with Developer Mode
# enabled, connected and trusted.
set -euo pipefail

app_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bundle_id="ai.freeside.app.ios"
build_dir="${FREESIDE_IOS_BUILD_DIR:-$app_dir/DerivedData/ios-install}"

device=""
device_given=false
server_url=""
server_url_given=false
launch_after_install=false
launch_only=false

die() {
    echo "install-ios-app: $1" >&2
    exit 1
}

[[ "$(uname)" == "Darwin" ]] || die "requires macOS"

while [[ $# -gt 0 ]]; do
    case "$1" in
    --device)
        [[ $# -ge 2 ]] || die "--device needs a device name or UDID (see: xcrun devicectl list devices)"
        device="$2"
        device_given=true
        shift 2
        ;;
    --server-url)
        [[ $# -ge 2 ]] || die "--server-url needs a URL"
        server_url="$2"
        server_url_given=true
        shift 2
        ;;
    --launch)
        launch_after_install=true
        shift
        ;;
    --launch-only)
        launch_only=true
        shift
        ;;
    *)
        die "unknown argument: $1"
        ;;
    esac
done

[[ "$device_given" == true ]] ||
    die "--device is required (see: xcrun devicectl list devices)"

if [[ "$launch_after_install" == true && "$launch_only" == true ]]; then
    die "--launch and --launch-only cannot be combined"
fi

# The URL reaches the device only as a launch argument, so binding one
# without launching is a silent no-op. Refuse it rather than install an
# app that never learns the daemon URL the operator passed.
if [[ "$server_url_given" == true && "$launch_after_install" != true && "$launch_only" != true ]]; then
    die "--server-url only takes effect on the first launch and is passed as a
  launch argument, so it requires --launch or --launch-only. Re-run with one
  of those flags, or enter the daemon URL on the phone during pairing."
fi

# The daemon URL becomes a launch argument the app parses on first run, so
# a malformed one would strand that launch on the mock composition with no
# visible error: AppSession.launchMode falls through to the mock when
# URL(string:) returns nil. Validate with the client's own parser rather
# than a pattern, because a pattern drifts from Foundation (`http://[` and
# `http://%` look well-formed and parse to nil), and require the host
# AppSession.deploymentKey needs. The port is checked against the raw text
# rather than against `url.port`, because Foundation reports a nil port for
# both an omitted one and an unparseable one (`:18446744073709551616`
# overflows Int), so trusting nil-means-absent accepts exactly the values
# it should reject. Requiring the canonical rendering to match the raw
# digits also rejects `:0080`, and the 1–65535 bound matches what
# DeviceNtfySubscription enforces on the other URL the client stores. This
# is the same validator install-mac-app.sh applies to its --server-url.
if [[ "$server_url_given" == true ]] && ! swift -e 'import Foundation
guard CommandLine.arguments.count > 1 else { exit(1) }
let raw = CommandLine.arguments[1]
guard let url = URL(string: raw),
    let scheme = url.scheme?.lowercased(),
    scheme == "http" || scheme == "https",
    let host = url.host, !host.isEmpty,
    let schemeEnd = raw.range(of: "://")?.upperBound
else { exit(1) }
let authority = raw[schemeEnd...].prefix { ch in !"/?#".contains(ch) }
let afterHost =
    authority.hasPrefix("[")
    ? authority.lastIndex(of: "]").map { bracket in
        authority[authority.index(after: bracket)...]
    } ?? ""
    : authority.drop(while: { ch in ch != ":" })
if afterHost.hasPrefix(":") {
    let digits = String(afterHost.dropFirst())
    guard let port = url.port, (1...65535).contains(port), String(port) == digits
    else { exit(1) }
}' "$server_url" >/dev/null 2>&1; then
    die "--server-url is not an http(s) URL with a host: $server_url"
fi

# The Team ID is the certificate's organizational unit, read off the
# certificate rather than parsed from the identity name (whose parenthetical
# is a different 10-character identifier, not the Team ID).
team_id_of_identity() {
    security find-certificate -c "$1" -p 2>/dev/null |
        openssl x509 -noout -subject -nameopt multiline 2>/dev/null |
        sed -n 's/^[[:space:]]*organizationalUnitName[[:space:]]*=[[:space:]]*//p' |
        head -1
}

# Team ID (certificate OU) for the certificate with the given SHA-1
# fingerprint. The multi-identity listing must key on the fingerprint, not the
# common name: one Apple ID signed into several teams prints the same name for
# each, so a `-c <name>` lookup would return the first certificate's OU for
# every row and mislabel every team but one. `-a -Z` enumerates every
# certificate with its SHA-1 hash; select the PEM block whose hash matches this
# fingerprint (unique per certificate) and read its OU.
team_id_of_fingerprint() {
    security find-certificate -a -Z -p 2>/dev/null |
        awk -v want="$1" '
            /^SHA-1 hash:/ { sel = (toupper($NF) == toupper(want)); next }
            /^SHA-256 hash:/ { next }
            sel
        ' |
        openssl x509 -noout -subject -nameopt multiline 2>/dev/null |
        sed -n 's/^[[:space:]]*organizationalUnitName[[:space:]]*=[[:space:]]*//p' |
        head -1
}

# The sole "Apple Development" identity, resolved exactly as the Mac
# installer does. Xcode's free personal team mints one once an Apple ID is
# added under Settings > Accounts.
resolve_identity() {
    local candidates
    # "<SHA-1 fingerprint> <common name>" per Apple Development identity. The
    # fingerprint (unique per certificate) is kept so the multi-identity
    # listing can resolve each identity's own team even when several share a
    # common name; the single-identity path uses only the name.
    candidates="$(security find-identity -v -p codesigning 2>/dev/null |
        sed -n 's/^[[:space:]]*[0-9][0-9]*)[[:space:]]*\([0-9A-Fa-f][0-9A-Fa-f]*\)[[:space:]]*"\(Apple Development: .*\)"$/\1 \2/p')"
    local count=0
    if [[ -n "$candidates" ]]; then
        count="$(printf '%s\n' "$candidates" | wc -l | tr -d ' ')"
    fi
    case "$count" in
    1) printf '%s' "${candidates#* }" ;;
    0)
        die "no 'Apple Development' signing identity found. Add an Apple ID in
  Xcode > Settings > Accounts to mint one from the free personal team, or set
  FREESIDE_IOS_TEAM_ID to a Team ID whose signing assets Xcode already holds."
        ;;
    *)
        # List each identity's real Team ID (its certificate OU), the value
        # FREESIDE_IOS_TEAM_ID wants, not the tag shown in the identity name.
        # Resolve by fingerprint, not name, so identities that share a name
        # (one Apple ID, several teams) each show their own team.
        local listing="" fp cn tid
        while read -r fp cn; do
            [[ -n "$fp" ]] || continue
            tid="$(team_id_of_fingerprint "$fp")" || tid=""
            listing+="    ${tid:-??????????}  $cn"$'\n'
        done <<<"$candidates"
        die "several 'Apple Development' identities found; set FREESIDE_IOS_TEAM_ID
  to one of these Team IDs (the leading value, not the name's parenthetical):
${listing%$'\n'}"
        ;;
    esac
}

# Automatic signing needs the 10-character Team ID. FREESIDE_IOS_TEAM_ID
# overrides for a multi-team login; otherwise it is the sole identity's
# certificate OU (see team_id_of_identity). resolve_identity runs at top
# level with an explicit `|| exit 1`: its `die` runs inside the
# command-substitution subshell, so only that propagation carries the failure
# out (a bare assignment under `set -e` would not).
mkdir -p "$build_dir"

if [[ "$launch_only" != true ]]; then
    if [[ -n "${FREESIDE_IOS_TEAM_ID:-}" ]]; then
        team_id="$FREESIDE_IOS_TEAM_ID"
    else
        identity=$(resolve_identity) || exit 1
        team_id=$(team_id_of_identity "$identity")
        [[ "$team_id" =~ ^[A-Z0-9]{10}$ ]] ||
            die "could not derive a 10-character Team ID from the '$identity'
  certificate; set FREESIDE_IOS_TEAM_ID explicitly."
    fi
    echo "install-ios-app: signing team: $team_id"
fi

# Resolve --device to the connected device's UDID and build for that concrete
# destination. `-allowProvisioningDeviceRegistration` registers the build
# *destination* device; a `generic/platform=iOS` build never resolves the
# selector to a physical device, so a first-seen phone can be left out of the
# minted free-provisioning profile and the later install then fails (#847).
# devicectl accepts a name/UDID/serial/ECID/DNS selector, but xcodebuild's
# `id=` destination wants the UDID, so map the selector here and reuse the one
# resolved identity for the build, install, and launch. Parsed with `swift -e`
# + JSONSerialization: the toolchain the installer already requires (no new
# dependency), the same mechanism as the --server-url validator above.
device_list="$build_dir/devices.json"
xcrun devicectl list devices --json-output "$device_list" >/dev/null 2>&1 ||
    die "could not list connected devices with 'xcrun devicectl list devices'.
  Check that the device is connected, unlocked, and trusted."
# On a unique match the resolver prints only the UDID; on zero or several it
# prints the connected-device inventory and exits non-zero, which becomes the
# diagnostic here (a bare assignment under `set -e` would swallow the status,
# so branch on the substitution directly).
if ! device_udid=$(swift -e 'import Foundation
let args = CommandLine.arguments
guard args.count > 2,
    let data = FileManager.default.contents(atPath: args[1]),
    let root = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
    let result = root["result"] as? [String: Any],
    let devices = result["devices"] as? [[String: Any]]
else {
    print("could not read the connected-device list")
    exit(1)
}
let selector = args[2]
func scalar(_ value: Any?) -> String? {
    if let text = value as? String { return text }
    if let number = value as? NSNumber { return number.stringValue }
    return nil
}
var matches: [String] = []
var inventory: [String] = []
for device in devices {
    let props = device["deviceProperties"] as? [String: Any]
    let hw = device["hardwareProperties"] as? [String: Any]
    let name = scalar(props?["name"]) ?? "(unnamed)"
    let udid = scalar(hw?["udid"]) ?? ""
    inventory.append("  \(name) (\(udid.isEmpty ? "no udid" : udid))")
    // The selector forms the header advertises. devicectl DNS aliases live
    // elsewhere (connectionProperties host arrays) and are deliberately not
    // matched, so --device does not accept a DNS name (see the header).
    let fields = [
        scalar(props?["name"]), scalar(hw?["udid"]),
        scalar(hw?["serialNumber"]), scalar(hw?["ecid"]),
        scalar(device["identifier"]),
    ]
    if !udid.isEmpty, fields.contains(where: { field in field == selector }) {
        matches.append(udid)
    }
}
if matches.count == 1 {
    print(matches[0])
    exit(0)
}
let listing = inventory.isEmpty ? "  (none)" : inventory.joined(separator: "\n")
if matches.isEmpty {
    print("no connected device matched \u{27}\(selector)\u{27}. Connected devices:\n\(listing)")
} else {
    print("\u{27}\(selector)\u{27} matched \(matches.count) connected devices; pass its UDID. Connected devices:\n\(listing)")
}
exit(1)' "$device_list" "$device"); then
    die "$device_udid"
fi
echo "install-ios-app: resolved '$device' to device $device_udid"

launch_app() {
    echo "install-ios-app: launching $bundle_id"
    if [[ "$server_url_given" == true ]]; then
        xcrun devicectl device process launch --device "$device_udid" "$bundle_id" \
            -- -FreesideServerURL "$server_url" ||
            die "could not launch the app on '$device_udid'. On a first personal-team
  install, trust the developer certificate in Settings > General > VPN & Device
  Management, then retry this launch without rebuilding: $0 --device '$device' --server-url '$server_url' --launch-only"
    else
        xcrun devicectl device process launch --device "$device_udid" "$bundle_id" ||
            die "could not launch the app on '$device_udid'. On a first personal-team
  install, trust the developer certificate in Settings > General > VPN & Device
  Management, then retry this launch without rebuilding: $0 --device '$device' --launch-only"
    fi
}

if [[ "$launch_only" == true ]]; then
    launch_app
    exit 0
fi

build_log="$build_dir/xcodebuild.log"
echo "install-ios-app: building Release for device (log: $build_log)"
# Automatic signing with -allowProvisioningUpdates lets Xcode mint or renew
# the free-provisioning profile and register the device without the paid
# program; -allowProvisioningDeviceRegistration adds a first-seen device to
# the team, which needs the concrete `id=<udid>` destination resolved above
# (a generic destination registers no specific device). No signing assets
# are committed: the project already carries CODE_SIGN_STYLE = Automatic and
# the ai.freeside.app.ios bundle id.
if ! xcodebuild \
    -project "$app_dir/Freeside.xcodeproj" \
    -scheme FreesideIOS \
    -configuration Release \
    -destination "platform=iOS,id=$device_udid" \
    -skipPackagePluginValidation \
    -derivedDataPath "$build_dir" \
    -allowProvisioningUpdates \
    -allowProvisioningDeviceRegistration \
    CODE_SIGN_STYLE=Automatic \
    DEVELOPMENT_TEAM="$team_id" \
    build >"$build_log" 2>&1; then
    tail -40 "$build_log" >&2
    die "build failed; full log at $build_log"
fi

built_app="$build_dir/Build/Products/Release-iphoneos/FreesideIOS.app"
[[ -d "$built_app" ]] || die "build produced no app at $built_app"

echo "install-ios-app: installing on $device_udid"
xcrun devicectl device install app --device "$device_udid" "$built_app" ||
    die "could not install the app on '$device_udid'. Check that the device is
  connected, unlocked, trusted, and has Developer Mode enabled."
echo "install-ios-app: installed $bundle_id on $device_udid"

# The first launch carries the daemon URL as a process argument so the app
# enters live mode and pairs; AppSession persists it on-device from there.
# The empty-argument case is spelled out rather than expanded from an array
# so it stays correct under the system bash (3.2) `set -u`.
if [[ "$launch_after_install" == true ]]; then
    echo "install-ios-app: on a first personal-team install, trust the developer certificate
  in Settings > General > VPN & Device Management before this launch. If launch fails,
  retry without rebuilding with --launch-only."
    launch_app
fi
