#!/usr/bin/env bash
# install-mac-app.sh — install and update FreesideMac as the operator's
# real client (plan §10, issue #444).
#
# Usage: install-mac-app.sh --daemon-path <absolute-path> [--server-url <url>] [--launch]
#
# Builds the Release product, signs it with a stable identity, and
# installs or replaces Freeside.app at a fixed path. Re-running it after
# a source change updates the installed app in place without disturbing
# the Keychain-held device credential: the bundle identifier, install
# path, and signing identity stay fixed across runs, so the credential
# item's access control still names the same application and the
# operator never re-pairs. The script prints the designated requirement
# and warns loudly when an update changes it, because that change — not
# the rebuild — is what costs the pairing.
#
# --server-url persists the daemon URL into the installed app's
# preferences (`AppSession.fromEnvironment` reads `FreesideServerURL`
# from UserDefaults), so the app reaches the operator's daemon when it
# is launched from the Dock rather than with launch arguments.
#
# --daemon-path supplies the freesided binary copied into the app bundle
# before the app receives its final code-signature seal.
#
# Signing identity, in resolution order:
#   FREESIDE_MAC_SIGNING_IDENTITY  explicit codesign identity, by name or
#                                  SHA-1 hash; "-" selects ad-hoc signing
#   otherwise                      the sole "Apple Development:" identity
#                                  in the login keychain (Xcode's free
#                                  personal team mints one once an Apple
#                                  ID is added under Settings > Accounts)
#
# Ad-hoc signing stays opt-in rather than a silent fallback: its
# designated requirement is the code directory hash, which changes on
# every build, so each update presents as a different application and
# costs a Keychain prompt or a re-pair. Signing exists here for that
# stability, not for Gatekeeper.
#
# Environment:
#   FREESIDE_MAC_INSTALL_DIR  install root (default: ~/Applications)
#   FREESIDE_MAC_BUILD_DIR    derived data (default: app/DerivedData/mac-install)
#
# Requires: macOS, Xcode.
set -euo pipefail

app_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
bundle_id="ai.freeside.app.macos"
install_dir="${FREESIDE_MAC_INSTALL_DIR:-$HOME/Applications}"
build_dir="${FREESIDE_MAC_BUILD_DIR:-$app_dir/DerivedData/mac-install}"
destination="$install_dir/Freeside.app"
superseded="$destination.install-superseded"
rejected="$destination.install-rejected"
recovery_guard="$destination.install-recovery-guard"

server_url=""
server_url_given=false
daemon_path=""
daemon_path_given=false
launch_after_install=false

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
    *)
        die "unknown argument: $1"
        ;;
    esac
done

[[ "$daemon_path_given" == true ]] ||
    die "--daemon-path is required so the bundled LaunchAgent can run freesided"
[[ "$daemon_path" == /* ]] || die "--daemon-path must be absolute: $daemon_path"
[[ -f "$daemon_path" && -x "$daemon_path" ]] ||
    die "--daemon-path is not an executable file: $daemon_path"

daemon_state_dir="$HOME/Library/Application Support/Freeside/daemon"

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

restore_interrupted_install

resolve_identity() {
    if [[ -n "${FREESIDE_MAC_SIGNING_IDENTITY:-}" ]]; then
        printf '%s' "$FREESIDE_MAC_SIGNING_IDENTITY"
        return
    fi
    local candidates
    candidates="$(security find-identity -v -p codesigning 2>/dev/null |
        sed -n 's/.*"\(Apple Development: .*\)"$/\1/p')"
    local count=0
    if [[ -n "$candidates" ]]; then
        count="$(printf '%s\n' "$candidates" | wc -l | tr -d ' ')"
    fi
    case "$count" in
    1) printf '%s' "$candidates" ;;
    0)
        die "no 'Apple Development' signing identity found. Add an Apple ID in
  Xcode > Settings > Accounts to mint one from the free personal team, or set
  FREESIDE_MAC_SIGNING_IDENTITY (use '-' for ad-hoc signing, which costs the
  pairing on every update)."
        ;;
    *)
        die "several 'Apple Development' identities found; set
  FREESIDE_MAC_SIGNING_IDENTITY to the one to use:
$(printf '%s\n' "$candidates" | sed 's/^/    /')"
        ;;
    esac
}

# codesign prints the requirement on stderr, and marks an implicit one
# (the common case: nothing embeds an explicit requirement here) with a
# leading "# ".
designated_requirement() {
    codesign --display --requirements - "$1" 2>&1 |
        sed -n 's/^#* *designated => //p'
}

identity="$(resolve_identity)"
if [[ "$identity" == "-" ]]; then
    echo "install-mac-app: signing ad-hoc; every update will change the" >&2
    echo "  designated requirement, so the device must be re-paired." >&2
else
    echo "install-mac-app: signing identity: $identity"
fi

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

# Invalidate before any fallible build or swap step. A failed install may make
# the old app re-register its unchanged helper once, which is harmless; waiting
# until the new bundle is canonical creates a SIGKILL window where launchd can
# remain bound to the old generation indefinitely.
defaults delete "$bundle_id" FreesideLaunchAgentRegistrationCurrent \
    >/dev/null 2>&1 || true

mkdir -p "$daemon_state_dir"
[[ -d "$daemon_state_dir" && ! -L "$daemon_state_dir" ]] ||
    die "the daemon state path is not a regular directory: $daemon_state_dir"
chmod 700 "$daemon_state_dir"

mkdir -p "$build_dir"
build_log="$build_dir/xcodebuild.log"
echo "install-mac-app: building Release (log: $build_log)"
if ! xcodebuild \
    -project "$app_dir/Freeside.xcodeproj" \
    -scheme FreesideMac \
    -configuration Release \
    -destination 'platform=macOS' \
    -skipPackagePluginValidation \
    -derivedDataPath "$build_dir" \
    CODE_SIGN_STYLE=Manual \
    CODE_SIGN_IDENTITY="$identity" \
    PROVISIONING_PROFILE_SPECIFIER="" \
    build >"$build_log" 2>&1; then
    tail -40 "$build_log" >&2
    die "build failed; full log at $build_log"
fi

built_app="$build_dir/Build/Products/Release/FreesideMac.app"
[[ -d "$built_app" ]] || die "build produced no app at $built_app"
launch_agent_plist="$built_app/Contents/Library/LaunchAgents/ai.freeside.daemon.plist"
[[ -f "$launch_agent_plist" ]] ||
    die "the built app contains no bundled LaunchAgent at $launch_agent_plist"
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
listen_addr="127.0.0.1:7331"
stderr_path="$daemon_state_dir/freesided.log"
plutil -replace ProgramArguments -json \
    "[\"freesided\",\"-db\",\"$(json_string "$db_path")\",\"-state-dir\",\"$(json_string "$daemon_state_dir")\",\"-listen\",\"$(json_string "$listen_addr")\"]" \
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
expected_args=(freesided -db "$db_path" -state-dir "$daemon_state_dir" -listen "$listen_addr")
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
# after every host-local path is final. The helper gets the same identity first
# so Service Management can validate it, then the outer seal binds that exact
# executable before the artifact can replace the installed app.
codesign --force --sign "$identity" "$bundled_daemon" ||
    die "the bundled freesided executable could not be signed"
codesign --verify --strict "$bundled_daemon" ||
    die "the bundled freesided executable failed signature verification"
codesign --force --sign "$identity" "$built_app" ||
    die "the templated app could not be signed"
codesign --verify --strict "$built_app" ||
    die "the built app failed signature verification"

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

roll_back_install() {
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

# The old app can re-register its unchanged helper while the build runs and
# restore the marker deleted above. It has now been stopped; invalidate that
# stale generation again at the last possible point before the replacement
# becomes canonical.
defaults delete "$bundle_id" FreesideLaunchAgentRegistrationCurrent \
    >/dev/null 2>&1 || true

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
codesign --verify --strict "$destination" ||
    die "the installed app failed signature verification"
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

if [[ "$launch_after_install" == true ]]; then
    open "$destination"
fi
