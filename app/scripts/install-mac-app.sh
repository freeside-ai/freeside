#!/usr/bin/env bash
# install-mac-app.sh — install and update FreesideMac as the operator's
# real client (plan §10, issue #444).
#
# Usage: install-mac-app.sh [dev|prod] [--prod] [--daemon-path <absolute-path>]
#                           [--server-url <url>] [--launch] [--force]
#
# The target is an environment tier (plan "Environments: Prod, Dev, and
# Ephemeral"; default prod). Each tier derives its own identity, so the two
# installs coexist and never replace each other:
#
#   tier  app                 bundle ID                  LaunchAgent label       port
#   prod  Freeside.app        ai.freeside.app.macos      ai.freeside.daemon      7331
#   dev   Freeside Dev.app    ai.freeside.app.macos.dev  ai.freeside.daemon.dev  7332
#
# The prod target replaces the operator's real client and daemon, so it runs
# only from an interactive terminal or with an explicit --prod; a script or
# agent with no TTY cannot reach it by omitting the tier.
#
# Builds the Release product with automatic provisioning, signs it with a
# stable Apple Development identity, and
# installs or replaces the tier's app at a fixed path. Re-running it after
# a source change updates the installed app in place without disturbing
# the Data Protection Keychain-held device credential: the provisioned App ID
# prefix and bundle identifier stay fixed across runs, while the Team ID is
# verified independently. A valid legacy file-based credential is migrated
# once by the app; that transition may present one final legacy ACL prompt,
# while later launches are silent.
#
# --server-url persists the daemon URL into the installed app's
# preferences (`AppSession.fromEnvironment` reads `FreesideServerURL`
# from UserDefaults), so the app reaches the operator's daemon when it
# is launched from the Dock rather than with launch arguments.
#
# The installer builds freesided from this checkout with
# `go build -trimpath -buildvcs=false`, so the same sources give the same
# bytes, and copies it into the app bundle before the app receives its final
# code-signature seal. --daemon-path supplies a binary to use instead. The
# bundle's Info.plist records the checkout's commit (FreesideBuildCommit,
# with -dirty for uncommitted changes) and the sha256 of the unsigned daemon
# (FreesideDaemonDigest).
#
# An update that leaves the daemon unchanged (same digest, LaunchAgent, and
# signing requirement, with the registration marker set) keeps it running and
# replaces the app only. An update that changes it drains the running daemon
# first: it prints the installed and new commits, asks the daemon to start no
# new work (`freesided drain`), names what it waits on, and swaps only once
# the daemon reports it is ready to restart; then it opens the new app, whose
# daemon starts on the new build and lifts the drain. --force drains with
# `freesided drain -force`, which cancels running implementations and
# re-runs them after the drain lifts. A failure or interrupt before the new
# app verifies cancels the drain, so the old install keeps serving. A killed
# install can leave the drain in force: open the new app if it is in place,
# otherwise re-run the installer or lift the drain with
# `freesided drain -cancel -db <tier database>`.
#
# Signing identity, in resolution order:
#   FREESIDE_MAC_SIGNING_IDENTITY  explicit Apple Development identity, by
#                                  name or SHA-1 hash
#   otherwise                      the sole "Apple Development:" identity
#                                  in the login keychain (Xcode's free
#                                  personal team mints one once an Apple
#                                  ID is added under Settings > Accounts)
#
# Ad-hoc signing is rejected: it cannot carry the profile-authorized private
# Keychain access group that the installed operator client requires.
#
# Environment:
#   FREESIDE_MAC_INSTALL_DIR  install root (default: ~/Applications)
#   FREESIDE_MAC_BUILD_DIR    derived data (default: app/DerivedData/mac-install-<tier>)
#   FREESIDE_MAC_STATE_ROOT   absolute tier state root (default:
#                             ~/Library/Application Support/<app name>); the
#                             daemon's state directory is <root>/daemon
#
# Requires: macOS, Xcode, jq, and Go unless --daemon-path is given.
set -euo pipefail

app_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
repo_root="$(dirname "$app_dir")"

target=""
prod_confirmed=false
server_url=""
server_url_given=false
daemon_path=""
daemon_path_given=false
launch_after_install=false
force_drain=false

die() {
    echo "install-mac-app: $1" >&2
    exit 1
}

# Encode a value as the contents of a JSON double-quoted string for `plutil
# -json`, which parses its argument as JSON. A raw path is otherwise decoded,
# not taken literally: a backslash before a JSON escape letter (a legal
# `…\troot`) would bind a different byte, a double quote could terminate the
# string early and inject arguments, and a control byte (a legal newline, tab,
# or carriage return in a pathname) is illegal raw in JSON, so it would abort
# the whole-array rewrite that the prior per-index `-string` op tolerated
# (#762). A complete encoder keeps every bound path byte-identical to the
# created directory. Iteration is byte-wise (LC_ALL=C) so multibyte UTF-8
# passes through unchanged, which is valid inside a JSON string; a NUL cannot
# occur (illegal in a pathname and unrepresentable in a shell variable), and a
# byte read as signed by printf falls through to raw passthrough.
json_string() {
    local s=$1 out='' i c ord hex bs=$'\\'
    local LC_ALL=C
    for ((i = 0; i < ${#s}; i++)); do
        c=${s:i:1}
        case "$c" in
        "$bs") out+="$bs$bs" ;;
        '"') out+="$bs"'"' ;;
        *)
            printf -v ord '%d' "'$c"
            if ((ord >= 0 && ord < 32)); then
                case "$ord" in
                8) out+='\b' ;;
                9) out+='\t' ;;
                10) out+='\n' ;;
                12) out+='\f' ;;
                13) out+='\r' ;;
                *)
                    printf -v hex '\\u%04x' "$ord"
                    out+=$hex
                    ;;
                esac
            else
                out+=$c
            fi
            ;;
        esac
    done
    printf '%s' "$out"
}

[[ "$(uname)" == "Darwin" ]] || die "requires macOS"

while [[ $# -gt 0 ]]; do
    case "$1" in
    --daemon-path)
        [[ $# -ge 2 ]] || die "--daemon-path needs an absolute executable path"
        daemon_path="$2"
        daemon_path_given=true
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
    --force)
        force_drain=true
        shift
        ;;
    --prod)
        prod_confirmed=true
        shift
        ;;
    dev | prod)
        [[ -z "$target" ]] || die "the target was given twice: $target and $1"
        target="$1"
        shift
        ;;
    *)
        die "unknown argument: $1"
        ;;
    esac
done

target="${target:-prod}"
[[ "$prod_confirmed" == false || "$target" == prod ]] ||
    die "--prod confirms only the prod target, not $target"
# The tier table (plan "Derived identifiers"). FreesideEnvironment.swift is
# the app's copy; nothing else writes these strings.
case "$target" in
prod)
    bundle_id="ai.freeside.app.macos"
    display_name="Freeside"
    bundle_name="Freeside"
    app_icon_name="AppIcon"
    daemon_label="ai.freeside.daemon"
    listen_addr="127.0.0.1:7331"
    ;;
dev)
    bundle_id="ai.freeside.app.macos.dev"
    display_name="Freeside Dev"
    bundle_name="Freeside Dev"
    app_icon_name="AppIconDev"
    daemon_label="ai.freeside.daemon.dev"
    listen_addr="127.0.0.1:7332"
    ;;
esac
install_dir="${FREESIDE_MAC_INSTALL_DIR:-$HOME/Applications}"
build_dir="${FREESIDE_MAC_BUILD_DIR:-$app_dir/DerivedData/mac-install-$target}"
destination="$install_dir/$display_name.app"
superseded="$destination.install-superseded"
rejected="$destination.install-rejected"
recovery_guard="$destination.install-recovery-guard"
state_root="${FREESIDE_MAC_STATE_ROOT:-$HOME/Library/Application Support/$display_name}"
[[ "$state_root" == /* ]] ||
    die "FREESIDE_MAC_STATE_ROOT must be absolute: $state_root"
daemon_state_dir="$state_root/daemon"

if [[ "$daemon_path_given" == true ]]; then
    [[ "$daemon_path" == /* ]] || die "--daemon-path must be absolute: $daemon_path"
    [[ -f "$daemon_path" && -x "$daemon_path" ]] ||
        die "--daemon-path is not an executable file: $daemon_path"
else
    command -v go >/dev/null 2>&1 ||
        die "go is required to build freesided; install Go or pass --daemon-path"
fi
command -v jq >/dev/null 2>&1 ||
    die "jq is required to read the daemon's restart readiness; it ships with macOS 15 and later"

# The daemon URL becomes a durable preference of the installed app, so a
# malformed one would strand every later launch on the mock composition
# with no visible error: AppSession.fromEnvironment falls through to the
# mock when URL(string:) returns nil. Validate with the client's own
# parser rather than a pattern, because a pattern drifts from Foundation
# (`http://[` and `http://%` look well-formed and parse to nil), and
# require the host AppSession.deploymentKey needs. The port is checked
# against the raw text rather than against `url.port`, because Foundation
# reports a nil port for both an omitted one and an unparseable one
# (`:18446744073709551616` overflows Int), so trusting nil-means-absent
# accepts exactly the values it should reject. Requiring the canonical
# rendering to match the raw digits also rejects `:0080`, and the 1–65535
# bound matches what DeviceNtfySubscription enforces on the other URL the
# client stores.
# Keyed on whether the flag was passed, not on emptiness: an empty
# expansion (`--server-url "$UNSET_VAR"`) is a requested binding that
# cannot be honoured, and skipping both validation and the write would
# install successfully while silently ignoring it.
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

# SIGKILL cannot be trapped. If it lands during replacement, the superseded
# path is the only known-good client: the canonical path is either absent or
# contains the not-yet-verified replacement. Recover either state before
# identity resolution, the build, or generic stale-path cleanup can fail or
# destroy the recovery object. A wrong-ID or damaged object is never promoted:
# preserve it in place and tell the operator how to recover manually instead
# of guessing that it is a Freeside client.
rename_exclusively() {
    # `mv source existing-directory` nests the source and reports success. The
    # vacancy check cannot close the race by itself, so make nonexistence part
    # of the rename operation. renamex_np has provided RENAME_EXCL since macOS
    # 10.12; the installer already requires Swift/Xcode for its build.
    swift -e 'import Darwin
guard CommandLine.arguments.count == 3 else { exit(64) }
let result = CommandLine.arguments[1].withCString { source in
    CommandLine.arguments[2].withCString { target in
        renamex_np(source, target, UInt32(RENAME_EXCL))
    }
}
if result != 0 {
    perror("renamex_np")
    exit(1)
}' "$1" "$2"
}

recovery_validation_error=""
recovery_app_is_valid() {
    local candidate=$1
    recovery_validation_error=""
    if [[ ! -d "$candidate" || -L "$candidate" ]]; then
        recovery_validation_error="a non-bundle recovery object"
        return 1
    fi

    local recovered_id
    recovered_id="$(plutil -extract CFBundleIdentifier raw -o - \
        "$candidate/Contents/Info.plist" 2>/dev/null || true)"
    if [[ "$recovered_id" != "$bundle_id" ]]; then
        recovery_validation_error="bundle id '${recovered_id:-unknown}', not $bundle_id"
        return 1
    fi
    if ! codesign --verify --strict "$candidate"; then
        recovery_validation_error="an app that failed signature verification"
        return 1
    fi
}

restore_interrupted_install() {
    local guarded=false
    if [[ -e "$recovery_guard" || -L "$recovery_guard" ]]; then
        if [[ ! -d "$recovery_guard" || -L "$recovery_guard" ]]; then
            die "the recovery guard at $recovery_guard is not a directory.
  It was preserved; inspect it before retrying."
        fi
        guarded=true
    fi

    # A persistent guard is created before a recovery object is promoted. There
    # is no atomic validate-and-disarm filesystem primitive, so the guard is
    # deliberately never removed automatically: every later installer run
    # re-gates the canonical entry, including one after any possible SIGKILL.
    if [[ "$guarded" == true && ! -e "$superseded" && ! -L "$superseded" ]]; then
        if [[ ! -e "$destination" && ! -L "$destination" ]]; then
            echo "install-mac-app: recovery guard has no app to validate;" >&2
            echo "  proceeding with a fresh install and retaining $recovery_guard" >&2
            return 0
        fi
        if recovery_app_is_valid "$destination"; then
            echo "install-mac-app: validated the recovery-guarded app" >&2
            return 0
        fi

        local guarded_error
        guarded_error=$recovery_validation_error
        if [[ -e "$rejected" || -L "$rejected" ]]; then
            die "a guarded recovery left $guarded_error at $destination,
  but $rejected is already occupied. Both entries and $recovery_guard were
  preserved; inspect them before retrying."
        fi
        rename_exclusively "$destination" "$rejected" ||
            die "a guarded recovery left $guarded_error at $destination,
  and it could not be preserved at $rejected. The recovery guard remains;
  inspect both paths before retrying."
        die "a guarded recovery left $guarded_error at $destination.
  It was preserved at $rejected; no verified recovery app remains at the known
  path. The guard remains at $recovery_guard so any new canonical entry is
  re-gated; inspect the install directory before retrying."
    fi

    [[ -e "$superseded" || -L "$superseded" ]] || return 0

    if [[ -e "$destination" || -L "$destination" ]]; then
        if recovery_app_is_valid "$destination"; then
            return 0
        fi
        local destination_error
        destination_error=$recovery_validation_error

        if ! recovery_app_is_valid "$superseded"; then
            die "an interrupted install left $destination_error at
  $destination and $recovery_validation_error at $superseded. Both were
  preserved; inspect them and move one aside before retrying."
        fi
        if [[ -e "$rejected" || -L "$rejected" ]]; then
            die "an interrupted install left $destination_error at
  $destination, but $rejected is already occupied. The verified recovery app
  remains at $superseded; inspect all three paths before retrying."
        fi

        # Preserve the untrusted destination before freeing the canonical path.
        # An exclusive rename makes a concurrent occupant at the quarantine
        # path a hard failure rather than nesting or replacing either object.
        rename_exclusively "$destination" "$rejected" ||
            die "could not preserve the untrusted interrupted install at
  $rejected. The verified recovery app remains at $superseded; inspect both
  paths before retrying."
        echo "install-mac-app: preserved an untrusted interrupted install at" >&2
        echo "  $rejected" >&2
    fi

    if ! recovery_app_is_valid "$superseded"; then
        die "an interrupted install left $recovery_validation_error at
  $superseded. It was preserved; inspect it and move it aside before retrying."
    fi

    # Verification runs external processes. Recheck the directory entry after
    # they return for a precise diagnostic; the exclusive rename below is the
    # atomic guard that closes the remaining check-to-move race.
    [[ ! -e "$destination" && ! -L "$destination" ]] ||
        die "something else appeared at $destination while the recovery app was
  being verified. The recovery app remains at $superseded; move the other entry
  aside, then move the recovery app back manually."

    if [[ "$guarded" == false ]]; then
        mkdir "$recovery_guard" ||
            die "could not create the recovery guard at $recovery_guard.
  The recovery app remains at $superseded; inspect the guard path before
  retrying."
    fi
    rename_exclusively "$superseded" "$destination" ||
        die "could not restore the interrupted install. The recovery app remains
  at $superseded and the guard remains at $recovery_guard; retrying will
  re-gate both paths."

    # RENAME_EXCL binds destination vacancy, not source identity. Re-run the
    # whole trust gate against the moved object: if the source path was replaced
    # after the first verification, return that object to the recovery path
    # rather than leaving an unverified app at the canonical destination.
    if ! recovery_app_is_valid "$destination"; then
        local moved_error
        moved_error=$recovery_validation_error
        if rename_exclusively "$destination" "$superseded"; then
            die "the recovery app changed while it was being restored: $moved_error.
  The moved object was returned to $superseded; inspect it before retrying."
        fi
        die "the recovery app changed while it was being restored: $moved_error.
  It could not be returned because another entry occupies $superseded; inspect
  both $destination and $superseded before retrying."
    fi
    echo "install-mac-app: restored an install interrupted before replacement" >&2
}

# After every argument check and before anything touches the install or state
# paths: a caller with no terminal must say --prod to replace the operator's
# real client.
if [[ "$target" == prod && "$prod_confirmed" == false && ! -t 0 ]]; then
    die "refusing to replace the production install without a terminal;
  pass --prod to confirm, or install the dev target (install-mac-app.sh dev)"
fi

restore_interrupted_install

resolve_identity() {
    if [[ -n "${FREESIDE_MAC_SIGNING_IDENTITY:-}" ]]; then
        if [[ "$FREESIDE_MAC_SIGNING_IDENTITY" == - || \
            "$FREESIDE_MAC_SIGNING_IDENTITY" =~ ^[0-9A-Fa-f]{40}$ ]]; then
            printf '%s' "$FREESIDE_MAC_SIGNING_IDENTITY"
            return
        fi
        local matches match_count
        matches="$(security find-identity -v -p codesigning 2>/dev/null |
            awk -v want="$FREESIDE_MAC_SIGNING_IDENTITY" '
                {
                    hash = $2
                    name = $0
                    sub(/^[^"]*"/, "", name)
                    sub(/"[^"]*$/, "", name)
                    if (name == want && length(hash) == 40 &&
                        hash ~ /^[[:xdigit:]]+$/) print hash
                }
            ')"
        match_count=0
        if [[ -n "$matches" ]]; then
            match_count=$(printf '%s\n' "$matches" | wc -l | tr -d ' ')
        fi
        [[ "$match_count" -eq 1 ]] ||
            die "the requested signing identity '$FREESIDE_MAC_SIGNING_IDENTITY' did not resolve to exactly one certificate; use its SHA-1 fingerprint"
        printf '%s' "$matches"
        return
    fi
    local candidates
    candidates="$(security find-identity -v -p codesigning 2>/dev/null |
        awk '
            {
                hash = $2
                name = $0
                sub(/^[^"]*"/, "", name)
                sub(/"[^"]*$/, "", name)
                if (name ~ /^Apple Development: / && length(hash) == 40 &&
                    hash ~ /^[[:xdigit:]]+$/) print hash "\t" name
            }
        ')"
    local count=0
    if [[ -n "$candidates" ]]; then
        count="$(printf '%s\n' "$candidates" | wc -l | tr -d ' ')"
    fi
    case "$count" in
    1) printf '%s' "${candidates%%[[:space:]]*}" ;;
    0)
        die "no 'Apple Development' signing identity found. Add an Apple ID in
  Xcode > Settings > Accounts to mint one from the free personal team, or set
  FREESIDE_MAC_SIGNING_IDENTITY to an Apple Development identity."
        ;;
    *)
        die "several 'Apple Development' identities found; set
  FREESIDE_MAC_SIGNING_IDENTITY to the one to use:
$(printf '%s\n' "$candidates" | sed 's/^/    /')"
        ;;
    esac
}

identity_certificate() {
    if [[ "$1" =~ ^[0-9A-Fa-f]{40}$ ]]; then
        security find-certificate -a -Z -p 2>/dev/null |
            awk -v want="$1" '
                /^SHA-1 hash:/ { sel = (toupper($NF) == toupper(want)); next }
                /^SHA-256 hash:/ { next }
                sel
            '
        return
    fi
    security find-certificate -c "$1" -p 2>/dev/null
}

team_id_of_identity() {
    identity_certificate "$1" |
        openssl x509 -noout -subject -nameopt multiline 2>/dev/null |
        sed -n 's/^[[:space:]]*organizationalUnitName[[:space:]]*=[[:space:]]*//p' |
        head -1
}

# codesign prints the requirement on stderr, and marks an implicit one
# (the common case: nothing embeds an explicit requirement here) with a
# leading "# ".
designated_requirement() {
    codesign --display --requirements - "$1" 2>&1 |
        sed -n 's/^#* *designated => //p'
}

plist_value() {
    plutil -extract "$2" raw -o - "$1" 2>/dev/null
}

require_plist_value() {
    local plist=$1 key=$2 expected=$3 description=$4 actual
    actual=$(plist_value "$plist" "$key") ||
        die "$description is missing from $plist"
    [[ "$actual" == "$expected" ]] ||
        die "$description is '$actual', expected '$expected'"
}

profile_authorizes() {
    local authorization=$1 expected=$2 prefix
    [[ "$authorization" == "$expected" ]] && return 0
    [[ "$authorization" == *\* ]] || return 1
    prefix=${authorization%\*}
    [[ "$prefix" != *\* && "$expected" == "$prefix"* ]]
}

require_plist_authorization() {
    local plist=$1 key=$2 expected=$3 description=$4 actual
    actual=$(plist_value "$plist" "$key") ||
        die "$description is missing from $plist"
    profile_authorizes "$actual" "$expected" ||
        die "$description is '$actual', which does not authorize '$expected'"
}

require_plist_array_authorization() {
    local plist=$1 key=$2 expected=$3 description=$4 actual index=0
    while actual=$(plist_value "$plist" "$key.$index"); do
        profile_authorizes "$actual" "$expected" && return 0
        index=$((index + 1))
    done
    ((index > 0)) || die "$description is missing from $plist"
    die "$description does not authorize '$expected'"
}

require_profile_signer() {
    local profile_plist=$1
    local profile_certificate="$build_dir/FreesideMac.profile-signer.cer"
    local encoded_certificate index=0
    while encoded_certificate=$(plist_value \
        "$profile_plist" "DeveloperCertificates.$index"); do
        [[ -n "$encoded_certificate" && \
            "$encoded_certificate" =~ ^([A-Za-z0-9+/]{4})*([A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$ ]] ||
            die "the profile has a malformed developer certificate at index $index"
        printf '%s' "$encoded_certificate" |
            openssl base64 -d -A -out "$profile_certificate" 2>/dev/null ||
            die "the profile has a malformed developer certificate at index $index"
        cmp -s "$signing_certificate" "$profile_certificate" && return
        index=$((index + 1))
    done
    ((index > 0)) ||
        die "the profile has no authorized developer certificates: $profile_plist"
    die "the selected signing certificate is not authorized by $profile_plist"
}

verify_provisioned_app() {
    local candidate=$1 entitlements=$2
    local profile="$candidate/Contents/embedded.provisionprofile"
    local profile_plist="$build_dir/FreesideMac.verified-profile.plist"
    local application_prefix application_identifier

    codesign --verify --strict "$candidate" ||
        die "the app failed signature verification: $candidate"
    codesign --display --entitlements :- "$candidate" >"$entitlements" 2>/dev/null ||
        die "could not read the app's signed entitlements: $candidate"
    [[ -s "$entitlements" ]] ||
        die "the app has no signed entitlements: $candidate"
    [[ -f "$profile" ]] ||
        die "the app has no embedded provisioning profile: $candidate"
    security cms -D -i "$profile" >"$profile_plist" 2>/dev/null ||
        die "could not decode the app's embedded provisioning profile: $candidate"
    require_profile_signer "$profile_plist"

    application_prefix=$(plist_value "$profile_plist" ApplicationIdentifierPrefix.0) ||
        die "the profile application-identifier prefix is missing from $profile_plist"
    [[ "$application_prefix" =~ ^[A-Z0-9]{10}$ ]] ||
        die "the profile application-identifier prefix is '$application_prefix', expected a 10-character identifier"
    application_identifier="$application_prefix.$bundle_id"

    require_plist_value "$entitlements" 'com\.apple\.application-identifier' \
        "$application_identifier" "the signed application identifier"
    require_plist_value "$entitlements" 'com\.apple\.developer\.team-identifier' \
        "$team_id" "the signed team identifier"
    require_plist_value "$entitlements" keychain-access-groups.0 \
        "$application_identifier" "the signed Keychain access group"
    if plist_value "$entitlements" keychain-access-groups.1 >/dev/null; then
        die "the app has an unexpected additional Keychain access group"
    fi

    require_plist_value "$profile_plist" TeamIdentifier.0 \
        "$team_id" "the profile Team ID"
    require_plist_value \
        "$profile_plist" 'Entitlements.com\.apple\.developer\.team-identifier' \
        "$team_id" "the profile-authorized team identifier"
    require_plist_authorization \
        "$profile_plist" 'Entitlements.com\.apple\.application-identifier' \
        "$application_identifier" \
        "the profile-authorized application identifier"
    require_plist_array_authorization \
        "$profile_plist" Entitlements.keychain-access-groups \
        "$application_identifier" \
        "the profile-authorized Keychain access group"
}

identity="$(resolve_identity)"
if [[ "$identity" == "-" ]]; then
    die "ad-hoc signing cannot authorize the Data Protection Keychain access group.
  Add an Apple ID in Xcode > Settings > Accounts and use its Apple Development identity."
fi
team_id=$(team_id_of_identity "$identity") || team_id=""
[[ "$team_id" =~ ^[A-Z0-9]{10}$ ]] ||
    die "could not derive a 10-character Team ID from the '$identity' certificate"
echo "install-mac-app: signing identity: $identity"
echo "install-mac-app: signing team: $team_id"

# Never replace a bundle that is not a Freeside client: the install root
# is the operator's own ~/Applications, and a name collision there is
# someone else's app. For an ordinary install, the bundle identifier is the
# whole test on purpose. It does not prove this script installed the bundle,
# and it is not meant to: a hand-copied or differently signed Freeside build at
# this path is the client, and replacing it is an update, which the changed-
# requirement warning below already surfaces. A persistent recovery guard is
# not an ownership marker; it tightens this boundary to strict signature
# validation after a crash-recovery promotion. Checked before the build so the
# failure is immediate, and again immediately before the swap, since the build
# takes minutes and a guarded interloper must not reach the deletable backup.
# The vacancy test is on the directory entry, not on what it resolves to.
# `-e` follows symlinks, so a dangling one reads as vacant while still
# occupying the path: the staged rename then fails against it (a
# directory cannot replace a non-directory) and the rollback would
# delete the operator's link, which is precisely what this guard exists
# to prevent. Anything else occupying the path is refused below, so the
# enumeration is closed over every file type rather than the two that
# happened to come up.
assert_destination_replaceable() {
    [[ -e "$destination" || -L "$destination" ]] || return 0
    [[ -d "$destination" && ! -L "$destination" ]] ||
        die "$destination exists and is not an app bundle; move it aside"
    local installed_id
    installed_id="$(plutil -extract CFBundleIdentifier raw -o - \
        "$destination/Contents/Info.plist" 2>/dev/null || true)"
    [[ "$installed_id" == "$bundle_id" ]] ||
        die "$destination holds bundle id '${installed_id:-unknown}', not $bundle_id; move it aside"
    if [[ -e "$recovery_guard" || -L "$recovery_guard" ]]; then
        [[ -d "$recovery_guard" && ! -L "$recovery_guard" ]] ||
            die "the recovery guard at $recovery_guard is not a directory; inspect it before retrying"
        codesign --verify --strict "$destination" ||
            die "the recovery guard refused to replace an app that failed signature verification at
  $destination. It was preserved; retry to quarantine it before rebuilding."
    fi
}

assert_destination_replaceable
previous_requirement=""
if [[ -e "$destination" ]]; then
    previous_requirement="$(designated_requirement "$destination")"
fi

mkdir -p "$daemon_state_dir"
[[ -d "$daemon_state_dir" && ! -L "$daemon_state_dir" ]] ||
    die "the daemon state path is not a regular directory: $daemon_state_dir"
chmod 700 "$daemon_state_dir"

mkdir -p "$build_dir"
signing_certificate="$build_dir/FreesideMac.selected-signer.cer"
identity_certificate "$identity" |
    openssl x509 -outform DER -out "$signing_certificate" 2>/dev/null ||
    die "could not encode the '$identity' signing certificate"
[[ -s "$signing_certificate" ]] ||
    die "the '$identity' signing certificate is empty"

build_commit=$(git -C "$repo_root" rev-parse HEAD) ||
    die "could not read the commit of the checkout at $repo_root"
checkout_status=$(git -C "$repo_root" status --porcelain) ||
    die "could not read the status of the checkout at $repo_root"
[[ -z "$checkout_status" ]] || build_commit+="-dirty"
echo "install-mac-app: building commit $build_commit"

# -trimpath and -buildvcs=false keep the checkout's path and VCS state out
# of the binary, so the same sources give the same bytes from any checkout,
# and an unchanged daemon can be recognized by its digest below. GOFLAGS is
# cleared so an operator's setting cannot change the bytes either.
if [[ "$daemon_path_given" == false ]]; then
    daemon_path="$build_dir/freesided"
    daemon_build_log="$build_dir/go-build.log"
    echo "install-mac-app: building freesided (log: $daemon_build_log)"
    if ! GOFLAGS='' go build -C "$repo_root/daemon" -trimpath -buildvcs=false \
        -o "$daemon_path" ./cmd/freesided >"$daemon_build_log" 2>&1; then
        tail -40 "$daemon_build_log" >&2
        die "the freesided build failed; full log at $daemon_build_log"
    fi
fi
# The digest of the unsigned binary: signing changes the bytes, so only this
# one is comparable across installs.
new_daemon_digest=$(shasum -a 256 "$daemon_path" | awk '{ print $1 }') ||
    die "could not compute the sha256 of $daemon_path"

build_log="$build_dir/xcodebuild.log"
echo "install-mac-app: building Release (log: $build_log)"
if ! xcodebuild \
    -project "$app_dir/Freeside.xcodeproj" \
    -scheme FreesideMac \
    -configuration Release \
    -destination 'platform=macOS' \
    -skipPackagePluginValidation \
    -derivedDataPath "$build_dir" \
    -allowProvisioningUpdates \
    CODE_SIGN_STYLE=Automatic \
    CODE_SIGN_IDENTITY="Apple Development" \
    DEVELOPMENT_TEAM="$team_id" \
    FREESIDE_MAC_BUNDLE_ID="$bundle_id" \
    FREESIDE_MAC_APP_ICON_NAME="$app_icon_name" \
    build >"$build_log" 2>&1; then
    tail -40 "$build_log" >&2
    die "build failed; full log at $build_log"
fi

built_app="$build_dir/Build/Products/Release/FreesideMac.app"
[[ -d "$built_app" ]] || die "build produced no app at $built_app"
preserved_entitlements="$build_dir/FreesideMac.xcode.entitlements"
verify_provisioned_app "$built_app" "$preserved_entitlements"
# The tier the app resolves at launch, and the names Finder, the Dock, and
# the menu bar show. The provisioning profile binds none of them, and the
# re-sign below seals them all. The Release build already reads "Freeside"
# (FREESIDE_MAC_DISPLAY_NAME), but the tier table still fixes the installed
# names, and dev overrides them. The icon and the Debug build's names are
# build settings instead, the icon checked here, because the Debug build has
# no installer step to patch them.
built_info="$built_app/Contents/Info.plist"
plutil -replace CFBundleDisplayName -string "$display_name" "$built_info" ||
    die "could not set the app display name"
plutil -replace CFBundleName -string "$bundle_name" "$built_info" ||
    die "could not set the app bundle name"
plutil -replace FreesideEnvironment -string "$target" "$built_info" ||
    die "could not set the app environment"
# The build identity, which the next install compares against.
plutil -replace FreesideBuildCommit -string "$build_commit" "$built_info" ||
    die "could not record the build commit"
plutil -replace FreesideDaemonDigest -string "$new_daemon_digest" "$built_info" ||
    die "could not record the daemon digest"
require_plist_value "$built_info" CFBundleDisplayName "$display_name" "the app display name"
require_plist_value "$built_info" CFBundleName "$bundle_name" "the app bundle name"
require_plist_value "$built_info" CFBundleIconName "$app_icon_name" "the app icon name"
require_plist_value "$built_info" FreesideEnvironment "$target" "the app environment"
require_plist_value "$built_info" FreesideBuildCommit "$build_commit" "the build commit"
require_plist_value "$built_info" FreesideDaemonDigest "$new_daemon_digest" "the daemon digest"
# The app registers the plist its FreesideEnvironment names, so a dev bundle
# ships its own file and label rather than a second ai.freeside.daemon.
launch_agent_dir="$built_app/Contents/Library/LaunchAgents"
launch_agent_plist="$launch_agent_dir/$daemon_label.plist"
[[ -f "$launch_agent_dir/ai.freeside.daemon.plist" ]] ||
    die "the built app contains no bundled LaunchAgent at $launch_agent_dir/ai.freeside.daemon.plist"
if [[ "$launch_agent_dir/ai.freeside.daemon.plist" != "$launch_agent_plist" ]]; then
    mv "$launch_agent_dir/ai.freeside.daemon.plist" "$launch_agent_plist" ||
        die "could not rename the bundled LaunchAgent for $daemon_label"
fi
plutil -replace Label -string "$daemon_label" "$launch_agent_plist" ||
    die "could not bind the LaunchAgent label"
require_plist_value "$launch_agent_plist" Label "$daemon_label" "the LaunchAgent label"
bundled_daemon="$built_app/Contents/Resources/freesided"
mkdir -p "$(dirname "$bundled_daemon")"
ditto "$daemon_path" "$bundled_daemon" ||
    die "could not copy freesided into the app bundle"
chmod 755 "$bundled_daemon"
[[ -f "$bundled_daemon" && -x "$bundled_daemon" ]] ||
    die "the bundled freesided executable is missing or not executable"
# `plutil -replace ProgramArguments.<N>` inserts at the index instead of
# overwriting it, shifting each placeholder down rather than replacing it, so
# per-index replacement shipped a doubled arg vector with the template tokens
# still present (#762). Rewrite the whole array in one position-independent
# call; the arg structure now mirrors the bundled plist template, and the
# guard below re-reads and verifies it. The dynamic state paths are
# JSON-encoded (json_string) before they enter the fragment so a backslash or
# double quote in the pathname binds literally instead of being decoded or
# injecting arguments; see json_string for why a raw interpolation does not
# fail closed on those bytes.
db_path="$daemon_state_dir/freeside.db"
stderr_path="$daemon_state_dir/freesided.log"
plutil -replace ProgramArguments -json \
    "[\"freesided\",\"-environment\",\"$target\",\"-db\",\"$(json_string "$db_path")\",\"-state-dir\",\"$(json_string "$daemon_state_dir")\",\"-listen\",\"$(json_string "$listen_addr")\"]" \
    "$launch_agent_plist" || die "could not bind the daemon program arguments"
plutil -replace StandardErrorPath -string "$stderr_path" \
    "$launch_agent_plist" || die "could not bind the daemon stderr log path"

# Re-read the templated values and require them to equal exactly what was just
# written, not merely that no "__FREESIDE_" prefix survived: an operator's
# legal home path can itself contain that substring (e.g. /Users/__FREESIDE_x),
# so a prefix check would abort a correct install. Extract each argument in raw
# form and compare byte-for-byte: `raw` yields the unescaped scalar, so any
# metacharacter in the path (a backslash, an XML `&`/`<`/`>`, a double quote)
# compares literally, where an xml1/json re-extraction would re-encode it and
# spuriously mismatch. The entry past the last expected index must be absent,
# so a doubled vector (the #762 defect) is caught, not just per-element drift.
# Fails closed on every real install (#762).
expected_args=(freesided -environment "$target" -db "$db_path" -state-dir "$daemon_state_dir" -listen "$listen_addr")
for i in "${!expected_args[@]}"; do
    got=$(plutil -extract "ProgramArguments.$i" raw -o - "$launch_agent_plist") ||
        die "the templated daemon program arguments are shorter than expected (see #762)"
    [[ "$got" == "${expected_args[$i]}" ]] ||
        die "templated daemon argument $i does not match the expected value (see #762)"
done
if plutil -extract "ProgramArguments.${#expected_args[@]}" raw -o - "$launch_agent_plist" \
    >/dev/null 2>&1; then
    die "the templated daemon program arguments have more entries than expected (see #762)"
fi
templated_stderr=$(plutil -extract StandardErrorPath raw -o - "$launch_agent_plist") ||
    die "could not read the templated daemon stderr log path"
[[ "$templated_stderr" == "$stderr_path" ]] ||
    die "the templated daemon stderr path does not match the expected value (see #762)"

# Templating changes the outer bundle seal produced by Xcode. Re-sign only
# after every host-local path is final, explicitly reusing Xcode's provisioned
# entitlements. The helper gets the same identity first so Service Management
# can validate it, then the outer seal binds that exact executable before the
# artifact can replace the installed app.
codesign --force --sign "$identity" "$bundled_daemon" ||
    die "the bundled freesided executable could not be signed"
codesign --verify --strict "$bundled_daemon" ||
    die "the bundled freesided executable failed signature verification"

# The registration marker tells the app its LaunchAgent registration is
# current. Deleting it makes the next app launch re-register the LaunchAgent,
# which restarts the daemon from the bundle then installed. Its prior value is
# recorded the first time, so an undo can put it back.
registration_marker=FreesideLaunchAgentRegistrationCurrent

# The daemon is unchanged when the installed bundle recorded the same
# unsigned digest, its LaunchAgent plist is byte-identical to the new one,
# its helper has the same designated requirement as the one just signed, and
# the registration marker is set. An install that predates the digest key
# counts as changed. An unchanged daemon keeps running: the marker stays, so
# the new app leaves the LaunchAgent alone, and nothing restarts it. With the
# marker absent, the next app launch restarts the daemon anyway, and the
# running daemon may still be the old build: an install killed after a
# drained swap leaves exactly that, drained, so it must take the drain path
# again to finish.
daemon_unchanged=false
installed_daemon="$destination/Contents/Resources/freesided"
installed_launch_agent="$destination/Contents/Library/LaunchAgents/$daemon_label.plist"
if [[ -d "$destination" && -f "$installed_daemon" ]] &&
    installed_digest=$(plist_value "$destination/Contents/Info.plist" FreesideDaemonDigest) &&
    [[ "$installed_digest" == "$new_daemon_digest" ]] &&
    cmp -s "$installed_launch_agent" "$launch_agent_plist" &&
    installed_helper_requirement=$(designated_requirement "$installed_daemon") &&
    new_helper_requirement=$(designated_requirement "$bundled_daemon") &&
    [[ -n "$installed_helper_requirement" &&
        "$installed_helper_requirement" == "$new_helper_requirement" ]] &&
    [[ "$(defaults read "$bundle_id" "$registration_marker" 2>/dev/null)" == 1 ]]; then
    daemon_unchanged=true
fi
if [[ "$daemon_unchanged" == true ]]; then
    echo "install-mac-app: freesided is unchanged; leaving the daemon and its registration as they are"
    # Ship the installed, already-signed bytes, not the copy just signed. The
    # daemon's build identity is the sha256 of its executable (ExecutableBuild
    # in daemon/internal/daemoncompose/update_drain.go), and a re-signed copy
    # hashes differently because its signature differs. A crash restart in
    # the middle of a later drain would then read as a new build and lift the
    # drain.
    ditto "$installed_daemon" "$bundled_daemon" ||
        die "could not copy the installed freesided into the app bundle"
    cmp -s "$installed_daemon" "$bundled_daemon" ||
        die "the bundled freesided differs from the installed one it was copied from"
    codesign --verify --strict "$bundled_daemon" ||
        die "the installed freesided failed signature verification"
else
    echo "install-mac-app: freesided changed; the daemon restarts on the new build"
fi

codesign --force --sign "$identity" --entitlements "$preserved_entitlements" "$built_app" ||
    die "the templated app could not be signed"
final_entitlements="$build_dir/FreesideMac.final.entitlements"
verify_provisioned_app "$built_app" "$final_entitlements"

# Check the new daemon against a copy of this tier's database before
# anything running is touched: a build that cannot migrate or decode the
# operator's data would otherwise fail only after the swap, with the old
# app gone. The check copies, migrates, and decodes under the state
# directory, never writes the live database, and removes its copy; it
# works with the old daemon running or stopped. Nothing above has
# changed the installed app or the running daemon, so a failure here
# leaves both as they were. There is no skip flag (#3107).
if [[ -e "$db_path" ]]; then
    echo "install-mac-app: checking the new freesided against a copy of the $target database"
    "$bundled_daemon" check-database -environment "$target" -db "$db_path" \
        -state-dir "$daemon_state_dir" ||
        die "the new freesided failed its database check; the installed app and daemon are unchanged"
else
    echo "install-mac-app: skipped the database check: the $target tier has no database yet"
fi

marker_recorded=false
marker_was_set=false
invalidate_registration() {
    if [[ "$marker_recorded" == false ]]; then
        [[ "$(defaults read "$bundle_id" "$registration_marker" 2>/dev/null)" != 1 ]] ||
            marker_was_set=true
        marker_recorded=true
    fi
    defaults delete "$bundle_id" "$registration_marker" >/dev/null 2>&1 || true
}

# The new bundle's freesided sends every drain request: it is the binary known
# to have the drain command, and it reaches the running daemon through that
# daemon's control socket.
drain_requested=false
drain_error="$build_dir/drain.stderr"
drain_daemon() {
    "$bundled_daemon" drain -db "$db_path" "$@" >/dev/null 2>"$drain_error"
}

# Undo an update that has not verified, so the old install keeps serving
# without a surprise restart: lift the drain this run requested, and put back
# a marker it deleted. Neither step restarts the daemon. The drain flag clears
# only once the cancel has returned: a second interrupt kills the cancel, and
# bash 5 re-enters the signal handler, which must then try again.
undo_update() {
    if [[ "$drain_requested" == true ]]; then
        if drain_daemon -cancel; then
            drain_requested=false
            echo "install-mac-app: canceled the drain; the installed daemon keeps serving" >&2
        else
            drain_requested=false
            cat "$drain_error" >&2
            echo "install-mac-app: could not cancel the drain request. If a drain is in" >&2
            echo "  force, lift it once the daemon runs: \"$bundled_daemon\" drain -cancel -db \"$db_path\"" >&2
        fi
    fi
    if [[ "$marker_was_set" == true ]]; then
        marker_was_set=false
        defaults write "$bundle_id" "$registration_marker" -bool true ||
            echo "install-mac-app: could not restore $registration_marker for $bundle_id" >&2
    fi
}

# A signal trap must terminate, not just clean up: bash resumes the
# interrupted script once the handler returns.
undo_update_and_abort() {
    undo_update
    trap - EXIT
    exit 130
}

wait_for_drain() {
    local readiness ready blockers waiting_on=""
    while true; do
        if ! readiness=$("$bundled_daemon" drain -db "$db_path" -status 2>"$drain_error"); then
            cat "$drain_error" >&2
            die "could not read the daemon's restart readiness"
        fi
        ready=$(jq -r '.ready | if type == "boolean" then . else error("no ready flag") end' \
            <<<"$readiness") || die "unreadable restart readiness: $readiness"
        [[ "$ready" == false ]] || return 0
        blockers=$(jq -r '[.blockers[]? | "\(.count) \(.kind)"] | join(", ")' \
            <<<"$readiness") || die "unreadable restart readiness: $readiness"
        if [[ "$blockers" != "$waiting_on" ]]; then
            echo "install-mac-app: waiting for the daemon to drain: ${blockers:-the drain to take effect}"
            waiting_on=$blockers
        fi
        sleep 5
    done
}

# An unchanged daemon keeps running, so it has nothing to drain. A changed one
# must stop launching work and finish (or, forced, cancel) what runs before
# the swap, because the new app restarts it on the new build. The daemon is
# asked, not stopped: it waits as long as its work takes, and the operator
# interrupts to give up, which cancels the drain. There is no timeout.
if [[ "$daemon_unchanged" == true ]]; then
    [[ "$force_drain" == false ]] ||
        echo "install-mac-app: --force has nothing to do: the daemon is unchanged and keeps running"
else
    installed_commit="none, a first install"
    if [[ -d "$destination" ]]; then
        installed_commit=$(plist_value "$destination/Contents/Info.plist" FreesideBuildCommit) ||
            installed_commit="unknown, installed before the commit was recorded"
    fi
    echo "install-mac-app: installed commit: $installed_commit"
    echo "install-mac-app: new commit: $build_commit"
    drain_flag=""
    drain_kind=drain
    if [[ "$force_drain" == true ]]; then
        drain_flag=-force
        drain_kind="forced drain"
    fi
    # Armed before the request: an interrupt while it is in flight, or a
    # failure after the daemon recorded it, may leave a drain in force.
    drain_requested=true
    trap undo_update EXIT
    trap undo_update_and_abort INT TERM
    if drain_daemon ${drain_flag:+"$drain_flag"}; then
        echo "install-mac-app: requested a $drain_kind; waiting until the daemon is ready to restart"
        wait_for_drain
        echo "install-mac-app: the daemon is drained and ready to restart"
        invalidate_registration
    elif grep -Fq "no daemon is running" "$drain_error"; then
        drain_requested=false
        trap - EXIT INT TERM
        echo "install-mac-app: no $target daemon is running, so there is nothing to drain"
    else
        cat "$drain_error" >&2
        die "the running daemon could not drain, so nothing was replaced. Stop the
  daemon from the Freeside menu, then re-run the installer."
    fi
fi

# A running instance holds the bundle open and would keep serving stale
# code after the swap. The client's disk cache is written atomically
# (CacheStore), so terminating it cannot tear durable state. The pattern
# matches only the installed executable, so an Xcode-run build of the
# same app is left alone; the path is escaped because pgrep matches an
# expression, not a literal. It ends at a space or the end of the command
# line rather than anchoring at the end, because pgrep -f matches the
# whole command line and the documented launch arguments would otherwise
# hide a running client.
running_pattern="^$(printf '%s' "$destination" |
    sed 's/[][\\.^$*+?(){}|]/\\&/g')/Contents/MacOS/FreesideMac( |\$)"
if pgrep -f "$running_pattern" >/dev/null 2>&1; then
    echo "install-mac-app: quitting the running installed app"
    pkill -f "$running_pattern" || true
    for _ in $(seq 20); do
        pgrep -f "$running_pattern" >/dev/null 2>&1 || break
        sleep 0.5
    done
    if pgrep -f "$running_pattern" >/dev/null 2>&1; then
        die "the installed app is still running; quit it and re-run"
    fi
fi

# Stage beside the destination and swap by rename, so a failed copy
# leaves the working installed app in place rather than a half-written
# bundle, and the previous install is only deleted once its replacement
# is already at the destination.
mkdir -p "$install_dir"
staged="$destination.install-staging"
staged_inode=""
rm -rf "$staged" "$superseded"
ditto "$built_app" "$staged"
staged_inode="$(stat -f %i "$staged")"
assert_destination_replaceable

# The two renames are not one atomic exchange, so an interruption or a
# failed verification between them must not leave the operator with a
# half-installed client. The traps below cover every reachable abnormal
# exit short of SIGKILL. Enumerated rather than patched case by case,
# because each earlier attempt closed one branch and missed the next:
#
#   prior app  progress                     handler must
#   ---------  ---------------------------  ---------------------------
#   none       before the staged rename     nothing (no destination yet)
#   none       after it                     remove the unverified app
#   exists     before the aside rename      nothing (original in place)
#   exists     after aside, before staged   restore the original
#   exists     after the staged rename      discard the new, restore
#
# A destination is only ever removed when it *is* the staged bundle, and
# that is decided by inode identity rather than by a flag recording that
# the rename was reached. A flag cannot be right in both directions: set
# after the rename, a signal arriving during it runs the handler while
# the flag still reads false; set before, an entry appearing between the
# vacancy check and the rename is mistaken for ours and deleted. A rename
# preserves the inode, so comparing it answers the real question at the
# moment the answer is needed, whatever the timing. If something else has
# taken the path, the rollback refuses and says where the app went: a
# stranded install the operator can move back beats destroying someone
# else's bundle, which is the invariant this whole guard exists to hold.
destination_is_staged_bundle() {
    [[ -n "$staged_inode" ]] || return 1
    local current
    current="$(stat -f %i "$destination" 2>/dev/null)" || return 1
    [[ "$current" == "$staged_inode" ]]
}

# The undo goes first: errexit still applies in a trap handler, so a failed
# restore would end it before a later undo ran, leaving the drain in force.
# The undo touches neither bundle, so its order against the restore is free.
roll_back_install() {
    undo_update
    restore_previous_install
}

restore_previous_install() {
    if [[ -d "$superseded" ]]; then
        if destination_is_staged_bundle; then
            rm -rf "$destination"
        elif [[ -e "$destination" || -L "$destination" ]]; then
            echo "install-mac-app: something else now occupies $destination," >&2
            echo "  so the previous install was left at $superseded; move it back" >&2
            return 0
        fi
        mv "$superseded" "$destination" &&
            echo "install-mac-app: restored the previous install" >&2
    elif destination_is_staged_bundle; then
        rm -rf "$destination"
        echo "install-mac-app: removed the unverified install; there was no" >&2
        echo "  previous one to restore" >&2
    fi
}

# A signal trap must terminate, not just clean up: bash resumes the
# interrupted script once the handler returns, so a restore-only handler
# would put the app back and then let the staging rename land *inside*
# the restored bundle, corrupting its signed contents.
roll_back_and_abort() {
    roll_back_install
    trap - EXIT
    exit 130
}
trap roll_back_install EXIT
trap roll_back_and_abort INT TERM

# A changed daemon needs the new app to re-register the LaunchAgent, which
# restarts the daemon from the new bundle; deleting the registration marker
# asks for that. It is deleted only now, with the old app stopped: deleted
# earlier, an app launch during the build would re-register the old helper
# and restart the daemon, canceling its work. It is deleted before the swap,
# so a SIGKILL after the swap cannot leave launchd bound to the old
# generation. After a drain it was deleted once already; the old app could
# have re-registered and restored it before it quit.
if [[ "$daemon_unchanged" == false ]]; then
    invalidate_registration
fi

if [[ -e "$destination" ]]; then
    mv "$destination" "$superseded"
    if [[ -e "$recovery_guard" || -L "$recovery_guard" ]] &&
        ! recovery_app_is_valid "$superseded"; then
        aside_error=$recovery_validation_error
        if rename_exclusively "$superseded" "$destination"; then
            die "the recovery-guarded app changed before replacement: $aside_error.
  The moved object was returned to $destination and preserved."
        fi
        die "the recovery-guarded app changed before replacement: $aside_error.
  It remains at $superseded because $destination is occupied; inspect both
  paths before retrying."
    fi
fi
# Renaming onto an existing directory would nest the staged bundle inside
# it rather than replace it, so require the path to be free. Same
# directory-entry test as the guard above, for the same reason.
[[ ! -e "$destination" && ! -L "$destination" ]] ||
    die "$destination reappeared mid-swap; nothing was replaced"
mv "$staged" "$destination"

# Verify before discarding the rollback copy: a bundle damaged in the
# copy or the rename fails here, and dropping the backup first would
# leave the operator an unusable app and nothing to fall back to.
new_requirement="$(designated_requirement "$destination")"
installed_entitlements="$build_dir/FreesideMac.installed.entitlements"
verify_provisioned_app "$destination" "$installed_entitlements"
trap - EXIT INT TERM
rm -rf "$superseded"

if [[ "$server_url_given" == true ]]; then
    defaults write "$bundle_id" FreesideServerURL -string "$server_url"
    echo "install-mac-app: daemon URL set to $server_url"
fi

echo "install-mac-app: installed $destination"
echo "install-mac-app: designated requirement: ${new_requirement:-unknown}"
if [[ -n "$previous_requirement" && "$previous_requirement" != "$new_requirement" ]]; then
    echo "install-mac-app: WARNING: the designated requirement changed since the" >&2
    echo "  previous install, so the Keychain no longer recognizes this app and" >&2
    echo "  the device must be re-paired. Previous: $previous_requirement" >&2
fi

# The drained daemon holds all work until the new app re-registers the
# LaunchAgent and the new build starts and lifts the drain, so a drained
# install opens the app even without --launch.
if [[ "$drain_requested" == true ]]; then
    echo "install-mac-app: opening the new app, which restarts the drained daemon on the new build"
fi
if [[ "$launch_after_install" == true || "$drain_requested" == true ]]; then
    open "$destination" || die "could not open $destination"
fi
