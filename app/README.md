# app

The SwiftUI multiplatform client: the macOS + iOS attention inbox, decision detail, and run timeline. Client databases are disposable read caches; the daemon is sole authority, and both platforms use the same sync API (see `docs/plan.md` §5.14).

**Bootstrap exemption** (plan §5.7): SwiftUI work in this directory does not flow through the Freeside pipeline until a macOS execution class exists (deferred, possibly forever). Go work joins the pipeline only once Freeside manages its own repo, the bootstrap test that follows the deliberately boring first repository (plan §11); this component may never join it.

- **Toolchain:** Xcode / Swift Package Manager.
- **Scope boundary:** client-side code only. The daemon/client contract is defined in `api/`; client code consuming it lives here, never in `api/`. No JS toolchain enters this component.
- **Status:** the installed client runs against the local daemon over the §5.14 sync API; the in-process mock backs previews, tests, and screenshot launches. The inbox and per-type decision cards are exercised against that stateful mock of the contract (idempotent commands, conflict-with-replacement, sync envelope, device pairing and revocation, digest-addressed attachment reads), rendering image attachments inline on the card by digest (plan §4; a missing or failed attachment shows a placeholder with the digest still visible, and bytes stay memory-only, never in the disk cache). Text discussions and specification change requests execute on both platforms: conversation snapshots bootstrap and persist with the disposable cache, the client refetches the thread after submit, and the awaiting-agent state converges on heartbeat. The §5.14 client cache keeps separate full-snapshot and observed cursors, bootstraps on revision gaps, discards on epoch changes, and carries the pending-command ledger so unresolved retry affordances survive relaunch. The app also includes coalesced manual/foreground/reachability refresh, Keychain-held device credentials, pairing, visible last-updated state, and durable post-decision receipts with delayed advance. The Mac app owns the local daemon's registered lifecycle, reports its health, version, observed restarts, and any client/daemon API contract mismatch from a menu-bar presence, and exposes the keyboard command set below. The client halves of §5.14 sync tests 1, 2, 8, 11–16, discuss, and request changes also converge against a real daemon process (`FreesideConvergenceTests`, env-gated): `bash scripts/run-convergence.sh` at the repo root builds and launches the `freeside-signet-dev` harness and runs the suite against it (#72, #693).

## Running

Launch arguments select the composition (`AppSession.fromEnvironment`):

- macOS default: the app's environment tier decides (`FREESIDE_ENV`, then the installed bundle's `FreesideEnvironment` key, then the build configuration: a Debug build is `ephemeral`, Release is `prod`; an unknown value fails at launch). `prod` and `dev` use their own supervised daemon (`http://127.0.0.1:7331` or `:7332`); that tier's readiness file selects the same deployment and prefills its pairing code, unless only the persisted deployment holds a device credential. A missing file leaves manual pairing entry available. `ephemeral` has no daemon of its own: it asks for an address, never reuses or records the persisted one, keeps its cache in memory, and never registers or controls a LaunchAgent. Each supervised tier keeps its cache under its own state root. A non-production window shows a `Dev` or `Ephemeral` badge.
- `-FreesideReadinessDir <absolute-path>` (`ephemeral` only; refused at launch in `prod` and `dev`, and when it resolves inside either supervised state root): follow the `readiness.json` that an `ephemeral` daemon run publishes in that `-state-dir`, for its URL and pairing code.
- iOS default: reconnect to the saved daemon, or ask for its address on a fresh installation. Continue to device pairing before the inbox opens. Missing or invalid configuration never selects sample data.
- `-FreesideMock YES`: the permissive in-process mock, used by unsigned development and screenshot launches.
- `-FreesidePairingDemo YES`: the full pairing flow against an enforcing mock; the code is `483911`.
- `-FreesideServerURL <url>`: a real daemon; the device credential lives in the Keychain and the cache on disk.

Mock and pairing-demo modes require explicit launch arguments. The installed
Mac daemon also defaults to execution disabled: it serves pairing, state, and
backups, with a setup notice in the inbox until a driver is configured.

`FreesideServerURL` is read from `UserDefaults`, so an installed app that is launched from the Dock (where nothing forwards launch arguments) takes it from its persisted preferences instead; `install-mac-app.sh --server-url` writes that preference. A launch argument still wins, because the argument domain outranks the persistent one.

Build the daemon you point it at from the same commit as the client. The API schema moves with `api/openapi.yaml`, so a client from one commit and a daemon binary from another can connect, authenticate, and still fail every sync — visible only as the freshness banner, with no error naming the mismatch. `scripts/run-convergence.sh` avoids this by rebuilding the harness from source on every run; a hand-rolled daemon launch does not, and a rebase is enough to desynchronise the two.

Launch arguments also pin the presentation per launch (`LaunchInputs`), so screenshot and testing workflows drive the app without UI automation. These are launch arguments rather than environment variables because `open --args` forwards only arguments, and `xcrun simctl launch` forwards them too:

- `-FreesideColorScheme light|dark`: force an appearance without touching the system setting; unset follows the system.
- `-FreesideContrast standard|increased`: force accessibility contrast without touching the system setting; unset follows the system.
- `-FreesideDynamicType x-small|small|medium|large|x-large|xx-large|xxx-large|ax1|ax2|ax3|ax4|ax5`: force a Dynamic Type size without touching the system setting; unset follows the system.
- `-FreesideScreen inbox|tasks`: open the given screen at launch; unset opens the inbox.
- `-FreesideSelect <id>`: select the given inbox item at launch, or on the tasks screen open the given task or run. `AttentionFixtures.defaultInboxItemIDs()` is the source of truth for the inbox values, today the default mock inbox's ids: `item-spec_approval`, `item-execution_failure`, `item-agent_question`, `item-review_diminishing_returns`, `item-review_dispute`, `item-review_contradiction`, `item-review_configuration`, `item-finding_adjudication`, `item-ready_for_final_review`, `item-publish_blocked`, `item-task_proposal`, `item-effect_proposal`, `item-system_health`, `item-blocked`. With `-FreesideScreen tasks`, `TaskFixtures.defaultTaskIDs()` and `RunFixtures.defaultRunIDs()` are the accepted values: a task id opens the task's timeline, and a run id (for example `run-freeside-657`) opens the run's task with the run timeline pushed. An unknown id is ignored with a note on stderr.
- `-FreesideInboxScope open|resolved|all`: select the inbox scope for a screenshot or automation launch.
- `-FreesideProject <project-id>`: select the inbox project filter for a screenshot or automation launch.
- `-FreesideDetailsExpanded YES`: open the selected decision card's Details disclosure at launch.

## macOS Keyboard Commands

- ⌘1 shows Inbox; ⌘2 shows Tasks; ⌘R refreshes; ⌥⌘I toggles the inspector; ⌘N opens the New Task composer on the Tasks screen (File > New Task), enabled only while the sync is fresh.
- With the inbox list focused, J selects the next item and K the previous one; the arrow keys keep their native list behavior.
- Return takes only a validated authoritative recommendation. Esc dismisses pending action UI without resolving the item. Space is unbound.

## Installing the Operator Client

`scripts/install-mac-app.sh` makes FreesideMac the operator's actually-installed client rather than an Xcode-run artifact (plan §10). It builds Release, signs with a stable identity, and installs or replaces the tier's app. The first argument is the tier, `prod` (the default) or `dev`, and each tier derives its own identity, so the two installs coexist:

| Tier | App | Bundle ID | LaunchAgent label | Port | State root |
| --- | --- | --- | --- | --- | --- |
| `prod` | `~/Applications/Freeside.app` | `ai.freeside.app.macos` | `ai.freeside.daemon` | `7331` | `~/Library/Application Support/Freeside` |
| `dev` | `~/Applications/Freeside Dev.app` | `ai.freeside.app.macos.dev` | `ai.freeside.daemon.dev` | `7332` | `~/Library/Application Support/Freeside Dev` |

```sh
./scripts/install-mac-app.sh dev \
  --daemon-path /absolute/path/to/freesided \
  --launch

./scripts/install-mac-app.sh prod --prod \
  --daemon-path /absolute/path/to/freesided \
  --server-url http://127.0.0.1:7331 \
  --launch
```

The `prod` target replaces the operator's real client and daemon, so without a terminal on stdin (a script or an agent) it refuses unless `--prod` confirms it. The installed LaunchAgent passes `-environment <tier>` to `freesided`, which requires a daemon built with the `-environment` flag.

Re-run it after a source change and it updates the installed app in place. The supplied daemon is copied into `Contents/Resources` before the bundle is sealed; the app re-registers that bundled helper once after each install. Xcode automatically provisions the private Keychain access group, and the installer preserves those entitlements while sealing the modified outer bundle. Before replacement it verifies the signature, the exact signing certificate against the embedded profile, native macOS `com.apple.application-identifier`, Team ID, and access group; profile values may authorize the exact identifier through a trailing wildcard, while the signed values must remain exact. The stable credential identity is the profile's application-identifier prefix plus the tier's bundle ID (the Keychain access group is `$(AppIdentifierPrefix)$(PRODUCT_BUNDLE_IDENTIFIER)`, so a `dev` install pairs as its own client); that prefix normally equals the Team ID, but older accounts can retain a distinct bundle-seed prefix, which the installer verifies separately from the signing team. Rebuilds and updates under the same prefix and bundle ID retain the pairing. A build under another application-identifier prefix or bundle ID has a different Keychain identity and appears unpaired; pair it as a new client and revoke the old device if that identity change was intentional.

On the first launch after this change, an existing valid credential under the exact current deployment service is copied from the legacy file-based Keychain into the Data Protection Keychain, read back in full, and only then removed from the legacy store. That one-time legacy read or cleanup may present one final Keychain ACL password prompt. Once migration completes, ordinary launches and same-identity updates use the provisioned Data Protection Keychain silently. A separate `codesign wants to sign using key` prompt concerns access to the Apple Development private key during installation; it is not a Freeside credential prompt and this installer does not alter that key's ACL.

Signing needs an `Apple Development` identity, which Xcode mints from the free personal team once an Apple ID is added under Settings > Accounts. `FREESIDE_MAC_SIGNING_IDENTITY` overrides the choice by certificate name or SHA-1 fingerprint. Ad-hoc signing (`-`) is rejected because it cannot carry the profile-authorized private Keychain access group. `FREESIDE_MAC_INSTALL_DIR` and `FREESIDE_MAC_BUILD_DIR` move the install root and derived-data path (default `DerivedData/mac-install-<tier>`). The daemon state directory is `<state root>/daemon`, matching the app's readiness reader; launchd captures its structured stderr at `freesided.log` in that protected directory. `FREESIDE_MAC_STATE_ROOT` replaces the derived state root with another absolute path; the app still reads the derived one, so the override is for the installer's tests, not for a daemon the app should find. The first `dev` install registers a new App ID on the personal team, which counts against its weekly App ID quota.

## Installing on an iOS Device

`scripts/install-ios-app.sh` builds, signs, and installs FreesideIOS on the operator's physical iPhone under free provisioning (plan §10). It uses the same free personal team as the Mac client, so no paid Apple Developer Program membership is required; APNs and push delivery stay deferred to Phase 2, and client correctness never depends on them.

```sh
./scripts/install-ios-app.sh \
  --device 'My iPhone' \
  --server-url http://100.64.0.1:7331 \
  --launch
```

`--device` accepts a device name, UDID, ECID, or serial; list the connected devices with `xcrun devicectl list devices`. The script resolves that selector to the connected device's UDID and builds for that concrete destination (`platform=iOS,id=<udid>`), reusing the one UDID for the build, install, and launch. (`devicectl` also takes a DNS name, but this script resolves only the four forms above, so pass one of them rather than a hostname.) The build uses automatic signing with `-allowProvisioningUpdates` and `-allowProvisioningDeviceRegistration` so Xcode mints or renews the free-provisioning profile and registers this device without the paid program; because registration targets the concrete build destination rather than a generic one, that a first-seen phone is actually added to the profile is confirmed on the operator's on-device run, not by this repo's command stand-in tests. The Team ID is read from the sole `Apple Development` certificate's organizational unit; `FREESIDE_IOS_TEAM_ID` overrides it for a multi-team login, and `FREESIDE_IOS_BUILD_DIR` moves the derived-data path. With more than one signing identity the override is required, and its value is the certificate's `OU`, not the 10-character tag `security find-identity` prints in the identity name (that parenthetical looks like a Team ID but is a different identifier, and building against it fails with `No Account for Team`). The installer's multi-identity error lists each identity's real Team ID, resolved by each certificate's SHA-1 fingerprint so identities that share a name still show their own team; copy the value from there. With a single identity you can also read it directly: `security find-certificate -c '<identity-name>' -p | openssl x509 -noout -subject` (the `OU=`). That `-c` form shows only the first matching certificate, so it misreads the team when several identities share a name (one Apple ID signed into different teams prints the same name); in that case use the installer's list or read it from a minted profile, which is unambiguous per profile: `security cms -D -i <profile>.mobileprovision | plutil -extract TeamIdentifier.0 raw -`.

**Free-provisioning cadence.** A personal-team provisioning profile expires seven days after it is minted, so the installed app stops launching after a week until you re-run the script to re-sign and reinstall. A personal team is also capped at a small number of distinct app IDs registered per week; reinstalling the same `ai.freeside.app.ios` bundle does not consume that quota, but experimenting with new bundle IDs can exhaust it. The bundle identifier and team stay fixed across runs, so the on-device Keychain credential still names the same application and a reinstall (or a weekly re-sign) does not force a re-pair, the same stability the Mac installer holds.

**Device preconditions.** The iPhone must run iOS 17 or later with Developer Mode enabled (Settings > Privacy & Security > Developer Mode), be connected and unlocked, and trust this Mac. On the first install of a personal-team build you must also trust the developer certificate on the device (Settings > General > VPN & Device Management), which is a manual step Apple does not let the host script automate.

**Daemon reachability.** The daemon listens only on loopback or a Tailscale-owned address, so the phone reaches it over the operator's tailnet: install the Tailscale app on the iPhone, join the same tailnet, and point `--server-url` at the Mac's Tailscale IP (the `100.64.0.0/10` range) and the daemon's port. The iOS target permits cleartext HTTP only for that tailnet CIDR through its committed Info.plist; App Transport Security continues to protect every other destination. Prefer HTTPS once the daemon has a first-class HTTPS endpoint, but do not substitute a MagicDNS hostname today: the scoped exception deliberately covers only the numeric tailnet range. The Mac app running on the daemon's own host is a special case: when its paired address is a Tailscale IP assigned to this Mac, it sends requests to `127.0.0.1` at the same port, because the daemon also serves the API there and a host VPN can drop the machine's traffic to its own Tailscale address; the paired URL stays the app's identity, so this does not re-pair.

**Pairing.** `--server-url` cannot be written to an iOS app's preferences from the host, so it is passed after `devicectl`'s `--` application-argument separator as `-FreesideServerURL` on the first launch; that is why it requires `--launch` or `--launch-only`. If a first personal-team launch is rejected before the certificate trust step, trust the certificate, then retry only the launch, without another build, as `./scripts/install-ios-app.sh --device '<name>' --server-url '<url>' --launch-only`. The launch enters live mode and shows the pairing screen: run the pairing command on the daemon host and enter the code it displays. The app persists the deployment URL on-device when pairing succeeds, so later launches from the home screen (which forward no arguments) reach the same daemon with no `--server-url`. Routine updates are therefore just `./scripts/install-ios-app.sh --device '<name>'`.

**Sync-contract churn.** When a sync-contract change lands (the app's generated API client changes shape), the installed build is stale against the daemon and must be reinstalled; the weekly re-sign cadence makes that reinstall routine rather than a special step.

## New Task Recovery

Each Submit creates separate work, even with identical
text. Mac and iPhone save the exact command before sending. After dismissal or
restart, open **Unconfirmed submissions** from the Tasks toolbar to review
saved requests and choose **Retry**. Recovery is a separate read-only screen;
New task always opens a fresh form. Opening or reconnecting never sends a
saved request automatically. Entries belong to the paired device and daemon;
switching either does not replay another connection's commands. A failed local
save prevents sending.

## Capturing screenshots

The launch inputs above make a capture run deterministic end to end: no System Settings mutation, no accessibility scripting, no clicking. The only host permission involved is Screen Recording for the invoking terminal (a one-time grant `screencapture` prompts for). `-ApplePersistenceIgnoreState YES` skips AppKit saved-state restoration so the window opens at the scene default (960×640) regardless of how it was last resized.

```sh
# Build once; any derived-data path works.
xcodebuild -project Freeside.xcodeproj -scheme FreesideMac \
  -destination 'platform=macOS' -skipPackagePluginValidation \
  CODE_SIGNING_ALLOWED=NO -derivedDataPath /tmp/freeside-dd build
APP=/tmp/freeside-dd/Build/Products/Debug/FreesideMac.app

# One pass per appearance: launch pinned, find the window by owner
# name (the app's display name is "Freeside"), capture it by id, quit.
open -n "$APP" --args -ApplePersistenceIgnoreState YES \
  -FreesideMock YES -FreesideColorScheme light -FreesideContrast standard \
  -FreesideSelect item-blocked
sleep 3
WID=$(swift -e 'import CoreGraphics
let windows = CGWindowListCopyWindowInfo(
    [.optionOnScreenOnly, .excludeDesktopElements], kCGNullWindowID
) as? [[String: Any]] ?? []
for w in windows where w[kCGWindowOwnerName as String] as? String == "Freeside" {
    if let id = w[kCGWindowNumber as String] as? Int { print(id) }
}')
screencapture -l "$WID" -o light.png
sips -g pixelWidth -g pixelHeight light.png
pkill -x FreesideMac
```

Repeat for `light|dark` crossed with `standard|increased` to capture all four cuts, then compare the `sips` outputs: the set must be dimension-identical, and a mismatch means a launch picked up stray window state — re-capture rather than shipping it.

## Structure

- `Freeside.xcodeproj` contains the two application targets. Both consume the local `FreesideCore` Swift package product.
- `Sources/FreesideAPI` owns the generated client surface, the stateful mock server and its transport, and the per-type attention fixtures. Apple Swift OpenAPI Generator produces tracked client and type source in `GeneratedSources/` from the schema mirror in that target.
- `Sources/FreesideCore` contains shared SwiftUI presentation code. `DesignLanguage.swift` holds the §15 tokens, faces, and shared chip, banner, and button styles; `Fonts/` carries the bundled OFL faces (see its README; `scripts/instance-serif-font.sh` regenerates the serif instance).
- `Apps/macOS/FreesideMenuMark.png` is the menu-bar template source, rendered from the one-color key `Apps/macOS/FreesideKeyMono.svg` by `scripts/generate-menu-mark.sh` with the dot retired for the sub-24px size.
- `SURFACES.md` tracks what the app has to show and how far along each piece is: screens, cards, the rules every card follows, behind-the-scenes state, and the open placement questions. When a PR adds or changes something the app shows, update its line there in the same PR. It holds no design decisions.
- `Tests/FreesideAPITests` exercises the generated client through the mock server, with no network or daemon; `Tests/FreesideCoreTests` covers the inbox, decision, sync, pairing, session, and daemon-menu models against the same mock, plus the cache and credential stores.
- `Apps/macOS/AppIcon.icon` is the single app-icon source for both application targets: the §15 signet mark with explicit default and dark appearance artwork. `Apps/macOS/Info.plist` names that adaptive resource without a static icon-file fallback. FreesideIOS references the same document from its own Resources phase (no copy), names `AppIcon` in `Apps/iOS/Info.plist`, and sets `ASSETCATALOG_COMPILER_APPICON_NAME = AppIcon` on its configurations because iOS shows the home-screen placeholder until `actool` runs with `--app-icon` and emits the `CFBundleIcons` entry SpringBoard reads.

The Icon Composer document lets the system select the appearance and own the platform mask. Its default artwork is the prior light export; its dark artwork keeps that geometry but replaces the treatment with the §15 umber (`#16120E`) ground and tawny (`#C2912E`) mark. Re-derive the dark asset from the 1024-pixel default master with

```sh
./scripts/generate-mac-icon.sh
```

The mask preserves the approved mark geometry and its cutouts; the appearance change is palette-only. Xcode compiles the one document into each installed bundle's platform and appearance renditions. On macOS, keep the document a normal resource with `CFBundleIconName` authoritative and leave `ASSETCATALOG_COMPILER_APPICON_NAME` unset: asking the asset compiler to emit a standalone primary icon adds `CFBundleIconFile`, and Finder then prefers that static fallback over the appearance-aware catalog. That caution is macOS-only; FreesideIOS sets the setting deliberately (above), because SpringBoard needs the `actool`-generated icon.

`Sources/FreesideAPI/openapi.yaml` is a mechanical mirror of the repository contract at `../api/openapi.yaml`. Refreshing it and regenerating the tracked client through the pinned command plugin is one reproducible command:

```sh
./scripts/generate-api-client.sh
```

The command leaves the checkout clean when the mirror and generated client agree with the schema. Otherwise it refreshes them and exits with an error until their changes are committed. Schema changes commit the mirror and regenerated client in the same PR. Do not edit the mirror or generated output to work around a schema gap; file a `kind:contract` issue instead. With `git config core.hooksPath .githooks` set, the `pre-commit` hook runs this regeneration for any commit that stages the schema, `openapi-generator-config.yaml`, or `Package.resolved`, and refuses the commit until the regenerated output is staged.

## Build and test

Package dependencies are pinned in `Package.resolved`.

From `app/`:

```sh
./scripts/generate-api-client.sh
swift test
xcodebuild -project Freeside.xcodeproj -scheme FreesideMac \
  -destination 'platform=macOS' -skipPackagePluginValidation \
  CODE_SIGNING_ALLOWED=NO build
xcodebuild -project Freeside.xcodeproj -scheme FreesideIOS \
  -destination 'generic/platform=iOS Simulator' -skipPackagePluginValidation \
  CODE_SIGNING_ALLOWED=NO build
```

## Restore After A Production Rig

After a production rig, restore the bundled daemon through the installed app's
**Stop**/**Start** controls. `scripts/restore-supervised-daemon.sh` clears the
launchd disable override, prints the needed app actions and waits for both service
registration and `http://127.0.0.1:7331/health`. Login Items approval or a timeout
leaves restoration incomplete. See the
[production walkthrough runbook](../docs/production-walkthrough.md) for the exact
sequence and retained-session recovery command.

## Style

Swift formatting and style analysis both come from the toolchain's
`swift-format` (swift-format 6.3.0, shipped with Xcode 26.6; CI pins the
matching Xcode and fails if the tool version drifts). The configuration is
`.swift-format` in this directory; the decision record is
`devlog/2026-07-20-1140-swift-style-tooling.md`.

From `app/`:

```sh
# Check formatting and style (what CI runs):
bash ../scripts/check.sh app format
# Rewrite in place:
find Sources Tests Apps -name '*.swift' \
  -not -path 'Sources/FreesideAPI/GeneratedSources/*' -print0 \
  | xargs -0 xcrun swift-format format --in-place
xcrun swift-format format --in-place Package.swift
```

Generated OpenAPI client sources are tracked under `Sources/FreesideAPI/GeneratedSources/`.
The lint and formatting commands explicitly exclude that directory; the
generation check verifies its exact output instead of reformatting it.
Deliberate constant-input force unwraps on mock and fixture surfaces carry
`// swift-format-ignore: NeverForceUnwrap` annotations; the rule stays on
everywhere else.

## Task Cancellation Contract

`stop_task` on `/commands` accepts a paired device's task ID, project ID,
expected sync epoch, and observed task snapshot version. It persists an
immutable receipt and task fence. The task's `cancellation` field is null or
carries `requested`, `failed_to_stop`, or `confirmed` independently of lifecycle
and WIP. Exact command replay returns the original receipt and revision.
A new command is accepted when its version is between 1 and the current
revision, so unrelated revision movement no longer rejects it; a stale epoch
or a greater version returns the current task snapshot and epoch.

Open a task from Tasks on Mac or iPhone and choose **Stop task…**. Confirm the
task and project to stop any remaining owned work and prevent further work.
History and existing PRs remain available. Finished or administratively
abandoned tasks can still have owned work, so their labels alone do not hide
Stop. A task with a cancellation fence shows its daemon status instead.

The client saves the exact command before sending. If delivery is uncertain,
**Pending Stops** in the Tasks toolbar keeps recovery available across navigation,
relaunch, and read-cache eviction. **Retry sending Stop** replays that saved
command; it never refreshes its version or epoch. Opening, syncing, or
reconnecting never sends it automatically. A stale rejection requires fresh
confirmation. A failed disk save sends nothing. Requests belong to the paired
device and daemon, independently of the existing submission and decision ledgers.

An accepted Stop awaits the runtime's bound quiescence evidence. **Failed to
stop** means execution may continue; **Refresh task status** only reads state.
Repeating Stop does not restart provider cancellation. A late receipt cannot
replace newer synced failure or confirmation. New Stop requests require fresh,
authenticated task state; offline and unvalidated views explain the restriction.

A bound confirmed acknowledgement atomically releases
any held episode and projects `stopped`; an already completed episode stays
`finished`. Explicit abandonment projects `abandoned` without claiming that
execution stopped. Stopped and abandoned tasks leave Active filters and remain
in Finished/All history. A confirmed queued task needs no run or position.
Ordinary submission cannot reopen a terminal episode; an uncancelled explicit
retry needs atomic cap-checked admission, and cancellation forbids it. No client may set
cancellation state. The mock also stays pending unless a test explicitly seeds
acknowledgement evidence. See [the plan](../docs/plan.md#59-durability-effectively-once)
for admission/publication ordering, late results, and restart reconciliation.
