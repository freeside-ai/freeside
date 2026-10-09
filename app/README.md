# app

The SwiftUI multiplatform client: the macOS + iOS attention inbox, decision detail, and run timeline. Client databases are disposable read caches; the daemon is sole authority, and both platforms use the same sync API (see `docs/plan.md` §5.14).

**Bootstrap exemption** (plan §5.7): SwiftUI work in this directory does not flow through the Freeside pipeline until a macOS execution class exists (deferred, possibly forever). Go work joins the pipeline only once Freeside manages its own repo, the bootstrap test that follows the deliberately boring first repository (plan §11); this component may never join it.

- **Toolchain:** Xcode / Swift Package Manager.
- **Scope boundary:** client-side code only. The daemon/client contract is defined in `api/`; client code consuming it lives here, never in `api/`. No JS toolchain enters this component.
- **Status:** the installed client runs against the local daemon over the §5.14 sync API; the in-process mock backs previews, tests, and screenshot launches. The inbox and per-type decision cards are exercised against that stateful mock of the contract (idempotent commands, conflict-with-replacement, sync envelope, device pairing and revocation, digest-addressed attachment reads), rendering image attachments inline on the card by digest (plan §4; a missing or failed attachment shows a placeholder with the digest still visible, and bytes stay memory-only, never in the disk cache). Text discussions and specification change requests execute on both platforms: conversation snapshots bootstrap and persist with the disposable cache, the client refetches the thread after submit, and the awaiting-agent state converges on heartbeat. The §5.14 client cache keeps separate full-snapshot and observed cursors, bootstraps on revision gaps, discards on epoch changes, and carries the pending-command ledger so unresolved retry affordances survive relaunch. The app also includes coalesced manual/foreground/reachability refresh, Keychain-held device credentials, pairing, visible last-updated state, and durable post-decision receipts with delayed advance. The Mac app owns the local daemon's registered lifecycle, reports its health, version, observed restarts, and any client/daemon API contract mismatch from a menu-bar presence, and exposes the keyboard command set below. The client halves of §5.14 sync tests 1, 2, 8, 11–16, discuss, and request changes also converge against a real daemon process (`FreesideConvergenceTests`, env-gated): `bash scripts/run-convergence.sh` at the repo root builds and launches the `freeside-signet-dev` harness and runs the suite against it (#72, #693).

## Running

Launch arguments select the composition (`AppSession.fromEnvironment`):

- macOS default: the app's environment tier decides (`FREESIDE_ENV`, then the installed bundle's `FreesideEnvironment` key, then the build configuration: a Debug build is `ephemeral`, Release is `prod`; an unknown value fails at launch). `prod` and `dev` use their own supervised daemon (`http://127.0.0.1:7331` or `:7332`); that tier's readiness file selects the same deployment and prefills its pairing code, unless only the persisted deployment holds a device credential. A missing file leaves manual pairing entry available. `ephemeral` has no daemon of its own: it asks for an address, never reuses or records the persisted one, keeps its cache in memory, and never registers or controls a LaunchAgent. Each supervised tier keeps its cache under its own state root. The Mac window's title-bar subtitle names the instance: none for `prod` on port `7331` or for `dev`; "Real-work run on :N" for a `prod` app connected to any other port; and for `ephemeral`, "Ephemeral · <worktree>" when launched by `scripts/dev-instance.sh`, otherwise the connected port (":N"), or "Ephemeral" before it connects.
- `-FreesideReadinessDir <absolute-path>` (`ephemeral` only; refused at launch in `prod` and `dev`, and when it resolves inside either supervised state root): follow the `readiness.json` that an `ephemeral` daemon run publishes in that `-state-dir`, for its URL and pairing code.
- Readiness stamp check (#1504): `freesided` stamps its `environment` and a per-start `run_id` into `readiness.json`. Every tier refuses a file stamped for another environment, and a file with no stamp (a daemon older than the stamp), however old the file is. A refusal stops at the connect screen, which names both environments or asks the operator to upgrade `freesided` (re-run the installer with `--daemon-path`); it never falls back to the persisted URL or the tier's fixed port, and the Mac menu never prefills a pairing code from it. An explicit launch argument or a typed address still connects without consulting the file. Upgrade the app and daemon together: an app-only install refuses the older daemon's file.
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
- `-FreesideDevices YES`: open the Devices sheet at launch. The mock lists three devices (this one, another active one, and a revoked one), so one capture shows every state.
- `-FreesideComposer discuss|request_changes|return_to_agent|answer_and_retry|answer_without_retry`: present that action's message composer on the item `-FreesideSelect` names, once per launch. The values are the API's action names. In the default mock inbox, `discuss` opens on `item-spec_approval`, `item-execution_failure`, `item-review_dispute`, `item-review_configuration`, and `item-finding_adjudication`; `request_changes` on `item-spec_approval`; `return_to_agent` on `item-ready_for_final_review`; and both answer composers on `item-agent_question`. The composer opens as soon as the action's button is enabled, the way one click on it would, so it follows the card's validation. A value that names no composer, a launch with no inbox item selected, and an item that doesn't offer the action are each ignored with a note on stderr. Don't combine it with `-FreesideDevices YES`: that asks for two sheets at once.

## Running a Dev Daemon

Agents and scripts use the `ephemeral` tier only (see the [environment rules](../docs/plan.md#environments-prod-dev-and-ephemeral)).

- **Screenshots: use `-FreesideMock YES`.** It needs no daemon and no pairing, and the `-FreesideSelect` ids come from the mock's own fixtures, so the same launch gives the same capture every time. See [Capturing screenshots](#capturing-screenshots).
- **A live daemon: run `scripts/dev-instance.sh` from the repo root.** It starts a seeded `ephemeral` daemon in a throwaway root inside the worktree, launches the Debug app against it, and removes the root when you stop it. To attach a Debug app to an `ephemeral` daemon started some other way, pass `-FreesideReadinessDir` with that daemon's `-state-dir`.
- **The Debug app has its own bundle ID.** The `FreesideMac` scheme's Debug build, which `scripts/dev-instance.sh` and the screenshot recipe use, is `ai.freeside.app.macos.ephemeral`, so its preferences, LaunchServices registration, and Keychain access group are its own, not the installed app's. It also has the dev look, the yellow icon and menu bar tile described under [Structure](#structure), because it is dev work, and is named "Freeside Ephemeral" in the app menu, Dock, and Cmd-Tab.
- **Never run `scripts/install-mac-app.sh` from an agent or script.** Omitting the tier installs `prod`, and its `--prod` flag is for the operator's explicit instruction only. The `dev` install is the operator's own too; its tiers are under [Installing the Operator Client](#installing-the-operator-client).
- **Attaching to production is an operator act.** The `FreesideMacProd` scheme runs a Debug build with `FREESIDE_ENV=prod`, so it connects to the `prod` daemon on port `7331`, and it keeps the prod icon and menu bar mark. It is not a passive attachment: the build uses the `DebugProd` configuration, which keeps prod's bundle ID and defaults, and its daemon menu is live. Launching it can re-register the `prod` LaunchAgent against the Debug bundle whenever prod's registration marker is unset, as after an install before the installed app relaunches, and Stop and Start there act on the `prod` LaunchAgent.

The daemon README lists [the production identifiers and persistent stores](../daemon/README.md#running-a-dev-daemon).

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

## New Task Prompt History

On Mac, press **Up** in **Work to do** on the first visual line with no
selection to recall the selected project's most recent confirmed prompt.
Further Up/Down presses browse older/newer prompts; Down past the newest
restores your exact draft. Editing a recalled prompt makes it the current
draft. Switching projects restores the pre-browse draft. Modified arrows,
selections, and input-method composition keep their native behavior.

The live app retains up to 50 prompt entries locally across restarts, scoped
to the deployment and paired device. Consecutive identical prompts for a
project are coalesced; the oldest entries are evicted first. Mock and ephemeral
sessions keep history only in memory. Recall changes neither the project nor
the optional name and sends nothing. Only a confirmed submission (including
a successful recovery) enters history. A history-save failure reports a
notice without changing task success.

## New Task Recovery

Each Submit creates separate work, even with identical
text. Mac and iPhone save the exact command before sending. After dismissal or
restart, open **Unconfirmed submissions** from the Tasks toolbar to review
saved requests and choose **Retry**. Recovery is a separate read-only screen;
New task always opens a fresh form. Opening or reconnecting never sends a
saved request automatically. Entries belong to the paired device and daemon;
switching either does not replay another connection's commands. A failed local
save prevents sending.

## Devices

The **Devices** toolbar button opens the list of devices paired with the
daemon: this device first, other active devices next, revoked ones apart, each
with its paired and last-seen times. Last-seen is coarse: the daemon refreshes
it at most every few minutes, and "Never" means it has recorded no request
from that device. **Revoke** asks for confirmation. Revoking this device signs
the app out: once the daemon confirms, the stored credential is deleted and
the app returns to pairing, which takes a new code from the daemon's host.

## Capturing screenshots

The launch inputs above make a capture run deterministic end to end: no System Settings mutation, no accessibility scripting, no clicking. The only host permission involved is Screen Recording for the invoking terminal (a one-time grant `screencapture` prompts for). `-ApplePersistenceIgnoreState YES` skips AppKit saved-state restoration so the window opens at the scene default (960×640) regardless of how it was last resized.

```sh
# Build once; any derived-data path works.
xcodebuild -project Freeside.xcodeproj -scheme FreesideMac \
  -destination 'platform=macOS' -skipPackagePluginValidation \
  CODE_SIGNING_ALLOWED=NO -derivedDataPath /tmp/freeside-dd build
APP=/tmp/freeside-dd/Build/Products/Debug/FreesideMac.app

# One pass per appearance: launch pinned, find the window by owner
# name (the Debug app's name is "Freeside Ephemeral"), capture it by id, quit.
open -n "$APP" --args -ApplePersistenceIgnoreState YES \
  -FreesideMock YES -FreesideColorScheme light -FreesideContrast standard \
  -FreesideSelect item-blocked
sleep 3
WID=$(swift -e 'import CoreGraphics
let windows = CGWindowListCopyWindowInfo(
    [.optionOnScreenOnly, .excludeDesktopElements], kCGNullWindowID
) as? [[String: Any]] ?? []
for w in windows where w[kCGWindowOwnerName as String] as? String == "Freeside Ephemeral" {
    if let id = w[kCGWindowNumber as String] as? Int { print(id) }
}')
screencapture -l "$WID" -o light.png
sips -g pixelWidth -g pixelHeight light.png
pkill -x FreesideMac
```

Repeat for `light|dark` crossed with `standard|increased` to capture all four cuts, then compare the `sips` outputs: the set must be dimension-identical, and a mismatch means a launch picked up stray window state — re-capture rather than shipping it.

A composer sheet takes one more argument and one change to the lookup. On the Mac a sheet is a second window the app owns, so the lookup prints two ids. Either id captures the main window with the sheet over it, so take the first:

```sh
open -n "$APP" --args -ApplePersistenceIgnoreState YES \
  -FreesideMock YES -FreesideColorScheme light -FreesideContrast standard \
  -FreesideSelect item-agent_question -FreesideComposer answer_and_retry
sleep 3
WID=$(swift -e 'import CoreGraphics
let windows = CGWindowListCopyWindowInfo(
    [.optionOnScreenOnly, .excludeDesktopElements], kCGNullWindowID
) as? [[String: Any]] ?? []
for w in windows where w[kCGWindowOwnerName as String] as? String == "Freeside Ephemeral" {
    if let id = w[kCGWindowNumber as String] as? Int { print(id) }
}' | head -1)
screencapture -l "$WID" -o composer-light.png
pkill -x FreesideMac
```

### Iterating on a Visible Change

Look at a change through selected renders, and run the whole screenshot
matrix once before push, not once per step. The full
`surfacesMatchRecordedPixels` pass draws 2,466 images (411 manifest keys at
each of six text sizes) one at a time on the main actor: 90 to 180 s on the
development Mac (2026-10-08), depending on what else was building, before
the build and the rest of the suite. One card through the selected-key
probe took 16 to 19 s including an incremental build, and 3.5 s with
nothing to rebuild.

1. **While iterating, render only the surfaces the step touches,** and look
   at those images:

   ```sh
   # From the repository root. Prints the path of each PNG it wrote.
   bash app/scripts/render-surfaces.sh /tmp/renders decision-blocked
   FREESIDE_RENDER_SIZES=large,ax5 \
     bash app/scripts/render-surfaces.sh /tmp/renders decision-blocked message-composer
   ```

   A surface is a primary-surface key from
   `Tests/FreesideCoreTests/Resources/ScreenshotDigests.json` without its
   text-size suffix; `FREESIDE_RENDER_SIZES` defaults to `large`. The script
   wraps the probe described under
   [Screenshot Regression Determinism](#screenshot-regression-determinism),
   which rejects the manifest's supplemental fixtures. It names each key the
   probe rejected and fails when any image is missing, refuses
   `FREESIDE_RECORD_SCREENSHOTS=1`, and never compares or records a digest.
   Run only the affected test suites next to it, from `app/`:
   `swift test --only-use-versions-from-resolved-file --filter '<SuiteA>|<SuiteB>'`.
2. **Record digests at the end, once per render-changing commit.** Every
   commit stays green, so each commit that changes pixels carries its own
   manifest update. Review the changed images first, then replay the branch
   with the recording command instead of re-recording during iteration. For
   example, from the repository root (the `-x` payload stays on one line:
   Git rejects an exec command that contains a newline):

   ```sh
   git rebase -x 'FREESIDE_RECORD_SCREENSHOTS=1 swift test --package-path app --only-use-versions-from-resolved-file --filter ScreenshotRegressionTests/surfacesMatchRecordedPixels && git add app/Tests/FreesideCoreTests/Resources/ScreenshotDigests.json && git commit --amend --no-edit' origin/main
   ```

   The recording rules at the end of the next section still apply.
3. **Then run the full suite once, before push.**
   `bash scripts/check.sh app test` compares every digest and runs the rest
   of the app tests; a selected render compares none, and the recording
   command runs one test. It comes after recording because a change that
   moves pixels fails the comparison until its digests are recorded.

### Screenshot Regression Determinism

The macOS package tests compare exact dimensions-plus-RGBA digests at six
Dynamic Type sizes. Capture draws through `ImageRenderer.render` into an
explicit sRGB bitmap; comparison and recording use that same path. Explicit
contrast is scoped in memory, and screenshot disclosure preferences use
separate temporary suites. Ordinary launch inputs and host contrast behavior
remain unchanged.

To reproduce order and process-isolation checks without changing a baseline:

```sh
# From the repository root; OUTPUT must be a new directory.
bash app/scripts/check-screenshot-determinism.sh --probes-only /tmp/screenshot-probes
```

For the fixed-commit stress experiment, commit the implementation first and
provide another checkout for concurrent builds:

```sh
bash app/scripts/check-screenshot-determinism.sh /tmp/screenshot-stress /path/to/load-checkout
```

Use the same implementation for every simultaneous screenshot process.
Older versions write shared `FreesideContrast` preferences and can change the
fallback input while this version draws a surface with no explicit override.
Keep those legacy screenshot runs stopped during the experiment. Ordinary
builds are the intended concurrent load.

The script keeps commands, environment metadata, timestamps, every exit status,
and complete logs. It checks selected keys individually, in reversed order,
after extra fixture construction, and after extra rendering. It runs opposite
contrast captures simultaneously with separate scratch builds and output paths.
The full experiment also runs `bash scripts/check.sh app test` five times
without background builds and five times alongside recorded release builds.
The script checks for external builds before each quiet run and samples the
process tree once per second during it, excluding that test command's own
build children. An observed external build invalidates the quiet run. Keep
that checkout and the host free of other builds for the entire quiet phase.
Opposite-contrast rendering intervals must overlap. During each loaded run,
at least one capture must overlap an observed compiler from its load build;
the script samples those build processes every 100 ms. Command startup alone
does not establish either overlap. Any failure makes the experiment fail. It never
retries failures or records baselines. `FREESIDE_DETERMINISM_RUNS=1` is useful
for a smoke check but does not satisfy the five-plus-five requirement.

For a narrow diagnostic, set `FREESIDE_SCREENSHOT_PROBE_KEYS` to comma-separated
primary-surface manifest keys (supplemental fixtures are not supported by the
probe), set `FREESIDE_SCREENSHOT_OUTPUT` to a separate directory,
and run `swift test --package-path app --filter
ScreenshotRegressionTests/probeScreenshotDeterminism`. The optional
`FREESIDE_SCREENSHOT_PROBE_MODE` is `selected` (the default), `reverse`,
`construct`, `render`, `prefix`, or `contrast`. Every mode constructs the
ordinary fixture set;
`construct` adds another unrendered set, while `render` also draws that extra
set before the selected keys. `prefix` renders the ordinary fixtures preceding
the first selected key. `FREESIDE_SCREENSHOT_PROBE_REPETITIONS` repeats selected
captures and checks that their digests agree. Sample diagnostics include the key, dimensions,
and digest. A diagnostic subset refuses `FREESIDE_RECORD_SCREENSHOTS=1` and
cannot replace the full manifest. `FREESIDE_SCREENSHOT_TRACE=1` adds the same
sample diagnostics to a complete matrix run. `scripts/render-surfaces.sh`
wraps the `selected` mode for everyday use
([Iterating on a Visible Change](#iterating-on-a-visible-change)).

`FREESIDE_PAIRING_PROBE_REPETITIONS` repeats the four mounted countdown pairs
after the complete matrix in the same process. Its default is one; a diagnostic
value such as `500` stops at the first mismatch and retains both images in
`FREESIDE_SCREENSHOT_OUTPUT`, along with clock bounds and pixel digests.
It does not retry a failed pair or reduce the ordinary matrix.

The `contrast` mode precedes each selected capture with an Increased Contrast
capture of the same surface. `FREESIDE_SCREENSHOT_PROBE_CONTRAST` can force
`standard` or `increased` for selected diagnostic captures. Trace output also
records capture start/end timestamps, the explicit contrast, current launch/defaults fallback, and host
contrast at each sample boundary; it does not establish their values during
an earlier capture.

Baseline recording still requires the designated local macOS version and
reviewed images. Hosted-runner overrides still require hosted image evidence;
local pixels cannot establish an override.

## Structure

- `Freeside.xcodeproj` contains the two application targets. Both consume the local `FreesideCore` Swift package product.
- `Sources/FreesideAPI` owns the generated client surface, the stateful mock server and its transport, and the per-type attention fixtures. Apple Swift OpenAPI Generator produces tracked client and type source in `GeneratedSources/` from the schema mirror in that target.
- `Sources/FreesideCore` contains shared SwiftUI presentation code. `DesignLanguage.swift` holds the §15 tokens, faces, and shared chip, banner, and button styles; `Fonts/` carries the bundled OFL faces (see its README; `scripts/instance-serif-font.sh` regenerates the serif instance).
- `Apps/macOS/FreesideMenuMark.png` is the menu-bar template source, rendered from the one-color key `Apps/macOS/FreesideKeyMono.svg` by `scripts/generate-menu-mark.sh` with the dot retired for the sub-24px size. The prod look tints it with the label color; the dev look (`FreesideEnvironment.look`, which maps `dev` and `ephemeral` to dev) draws it black on a safety-yellow tile and cuts a clear ring around the status dot, so the dot stays visible against the tile over a translucent menu bar. The status item's VoiceOver label names the tier: "Freeside", "Freeside Dev", or "Freeside Ephemeral".
- `SURFACES.md` tracks what the app has to show and how far along each piece is: screens, cards, the rules every card follows, behind-the-scenes state, and the open placement questions. When a PR adds or changes something the app shows, update its line there in the same PR. It holds no design decisions.
- `Tests/FreesideAPITests` exercises the generated client through the mock server, with no network or daemon; `Tests/FreesideCoreTests` covers the inbox, decision, sync, pairing, session, and daemon-menu models against the same mock, plus the cache and credential stores.
- `Apps/macOS/AppIcon.icon` is the single app-icon source for both application targets: the §15 signet mark with explicit default and dark appearance artwork. `Apps/macOS/Info.plist` names that adaptive resource without a static icon-file fallback. FreesideIOS references the same document from its own Resources phase (no copy), names `AppIcon` in `Apps/iOS/Info.plist`, and sets `ASSETCATALOG_COMPILER_APPICON_NAME = AppIcon` on its configurations because iOS shows the home-screen placeholder until `actool` runs with `--app-icon` and emits the `CFBundleIcons` entry SpringBoard reads.

The Icon Composer document lets the system select the appearance and own the platform mask. Its default artwork is the prior light export; its dark artwork keeps that geometry but replaces the treatment with the §15 umber (`#16120E`) ground and tawny (`#C2912E`) mark. Re-derive the dark asset from the 1024-pixel default master with

```sh
./scripts/generate-mac-icon.sh
```

The mask preserves the approved mark geometry and its cutouts; the appearance change is palette-only. Xcode compiles the one document into each installed bundle's platform and appearance renditions. On macOS, keep the document a normal resource with `CFBundleIconName` authoritative and leave `ASSETCATALOG_COMPILER_APPICON_NAME` unset: asking the asset compiler to emit a standalone primary icon adds `CFBundleIconFile`, and Finder then prefers that static fallback over the appearance-aware catalog. That caution is macOS-only; FreesideIOS sets the setting deliberately (above), because SpringBoard needs the `actool`-generated icon.

Dev work gets its own icon, so it can't pass for prod in the Dock, Cmd-Tab, or Finder. `Apps/macOS/AppIconDev.icon` is built the same way as `AppIcon.icon`, with light and dark layers: the black key on a safety-yellow plate inside a hazard-stripe frame. Both documents compile into the Mac app as normal resources, and `Info.plist` names the one to use through the `FREESIDE_MAC_APP_ICON_NAME` build setting: the `FreesideMac` target's `Debug` configuration (the ephemeral app) sets `AppIconDev`, and `DebugProd` and `Release` set `AppIcon`. `scripts/install-mac-app.sh dev` overrides it to `AppIconDev` and names the bundle "Freeside Dev"; the prod install keeps `AppIcon`. The `FREESIDE_MAC_DISPLAY_NAME` build setting names the build the same way, binding both `CFBundleDisplayName` and `CFBundleName`: `Debug` sets "Freeside Ephemeral", and `DebugProd` and `Release` set "Freeside", the name the prod install keeps. Re-render the dev layers from `Apps/macOS/FreesideKeyMono.svg` with

```sh
./scripts/generate-dev-icon.sh
```

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

`swift test` renders the whole screenshot matrix. While iterating on a
visible change, follow
[Iterating on a Visible Change](#iterating-on-a-visible-change) instead of
re-running it after every step.

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

Open a task from Tasks on Mac or iPhone, open **More Actions**, and choose
**Stop Task…**. Confirm the task and project to stop any remaining owned work
and prevent further work.
History and existing PRs remain available. Finished or administratively
abandoned tasks can still have owned work, so their labels alone do not hide
Stop. A task with a cancellation fence shows its daemon status instead.

The client saves the exact command before sending. If delivery is uncertain,
**Pending Stops** in the Tasks toolbar keeps recovery available across navigation,
relaunch, and read-cache eviction. **Retry** on an unconfirmed Stop replays that saved
command; it never refreshes its version or epoch. Opening, syncing, or
reconnecting never sends it automatically. A stale rejection requires fresh
confirmation. A failed disk save sends nothing. Requests belong to the paired
device and daemon, independently of the existing submission and decision ledgers.

An accepted Stop awaits the runtime's bound quiescence evidence. **Failed to
stop** means execution may continue; **Refresh Task Status** only reads state.
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
