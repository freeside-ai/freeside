# app

The SwiftUI multiplatform client: the macOS + iOS attention inbox, decision detail, and run timeline. Client databases are disposable read caches; the daemon is sole authority, and both platforms use the same sync API (see `docs/plan.md` §5.14).

**Bootstrap exemption** (plan §5.7): SwiftUI work in this directory does not flow through the Freeside pipeline until a macOS execution class exists (deferred, possibly forever). Go work joins the pipeline only once Freeside manages its own repo, the bootstrap test that follows the deliberately boring first repository (plan §11); this component may never join it.

- **Toolchain:** Xcode / Swift Package Manager.
- **Scope boundary:** client-side code only. The daemon/client contract is defined in `api/`; client code consuming it lives here, never in `api/`. No JS toolchain enters this component.
- **Status:** the inbox and per-type decision cards run against an in-process stateful mock of the contract (idempotent commands, conflict-with-replacement, sync envelope, device pairing and revocation, digest-addressed attachment reads), rendering image attachments inline on the card by digest (plan §4; a missing or failed attachment shows a placeholder with the digest still visible, and bytes stay memory-only, never in the disk cache), with the §5.14 client cache semantics (separate full-snapshot and observed cursors, bootstrap on revision gap, epoch-change discard), a persisted disposable cache (carrying the pending-command ledger, so an unresolved decision's retry affordance survives a relaunch), Keychain-held device credentials, the pairing flow, and the freshness banner. The client halves of §5.14 sync tests 1, 2, 8, 11, and 13–16 also converge against a real daemon process (`FreesideConvergenceTests`, env-gated): `bash scripts/run-convergence.sh` at the repo root builds and launches the `freeside-signet-dev` harness and runs the suite against it (#72); the conversation-path tests join with #68.

## Running

Launch arguments select the composition (`AppSession.fromEnvironment`):

- macOS default: the supervised daemon at `http://127.0.0.1:7331`; a readable readiness file selects the same deployment and prefills its pairing code, while a missing file leaves the manual pairing entry available.
- iOS default: the permissive in-process mock with a pre-paired identity — the inbox renders immediately.
- `-FreesideMock YES`: the permissive in-process mock, used by unsigned development and screenshot launches.
- `-FreesidePairingDemo YES`: the full pairing flow against an enforcing mock; the code is `483911`.
- `-FreesideServerURL <url>`: a real daemon; the device credential lives in the Keychain and the cache on disk.

`FreesideServerURL` is read from `UserDefaults`, so an installed app that is launched from the Dock (where nothing forwards launch arguments) takes it from its persisted preferences instead; `install-mac-app.sh --server-url` writes that preference. A launch argument still wins, because the argument domain outranks the persistent one.

Build the daemon you point it at from the same commit as the client. The API schema moves with `api/openapi.yaml`, so a client from one commit and a daemon binary from another can connect, authenticate, and still fail every sync — visible only as the freshness banner, with no error naming the mismatch. `scripts/run-convergence.sh` avoids this by rebuilding the harness from source on every run; a hand-rolled daemon launch does not, and a rebase is enough to desynchronise the two.

Launch arguments also pin the presentation per launch (`LaunchInputs`), so screenshot and testing workflows drive the app without UI automation. These are launch arguments rather than environment variables because `open --args` forwards only arguments, and `xcrun simctl launch` forwards them too:

- `-FreesideColorScheme light|dark`: force an appearance without touching the system setting; unset follows the system.
- `-FreesideSelect <item-id>`: select the given inbox item at launch. `AttentionFixtures.defaultInboxItemIDs()` is the source of truth for the accepted values, today the default mock inbox's ids: `item-spec_approval`, `item-execution_failure`, `item-agent_question`, `item-review_diminishing_returns`, `item-review_dispute`, `item-review_contradiction`, `item-review_configuration`, `item-ready_for_final_review`, `item-publish_blocked`, `item-run_proposal`, `item-system_health`, `item-blocked`. An unknown id is ignored with a note on stderr.

## Installing the Operator Client

`scripts/install-mac-app.sh` makes FreesideMac the operator's actually-installed client rather than an Xcode-run artifact (plan §10). It builds Release, signs with a stable identity, and installs or replaces `~/Applications/Freeside.app`:

```sh
./scripts/install-mac-app.sh \
  --daemon-path /absolute/path/to/freesided \
  --server-url http://127.0.0.1:7331 \
  --launch
```

Re-run it after a source change and it updates the installed app in place. The supplied daemon is copied into `Contents/Resources` before the bundle is sealed; the app re-registers that bundled helper once after each install. The point of signing here is Keychain stability, not Gatekeeper: the device credential's access control names the application that created it, so as long as the bundle identifier, install path, and signing identity hold steady across updates, the paired device survives a rebuild. The script prints the designated requirement each run and warns loudly when an update changes it, because that change — not the rebuild — is what forces a re-pair.

Signing needs an `Apple Development` identity, which Xcode mints from the free personal team once an Apple ID is added under Settings > Accounts. `FREESIDE_MAC_SIGNING_IDENTITY` overrides the choice; `-` selects ad-hoc signing, whose designated requirement is the code directory hash and therefore changes on every build. Ad-hoc is opt-in for that reason, not a silent fallback. `FREESIDE_MAC_INSTALL_DIR` and `FREESIDE_MAC_BUILD_DIR` move the install root and derived-data path. The supervised daemon state directory is fixed at `~/Library/Application Support/Freeside/daemon`, matching the app's readiness reader; launchd captures its structured stderr at `freesided.log` in that protected directory.

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
  -FreesideMock YES -FreesideColorScheme light -FreesideSelect item-blocked
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

Repeat with `-FreesideColorScheme dark` into `dark.png`, then compare the two `sips` outputs: a light/dark pair must be dimension-identical, and a mismatch means a launch picked up stray window state — re-capture rather than shipping the pair.

## Structure

- `Freeside.xcodeproj` contains the two application targets. Both consume the local `FreesideCore` Swift package product.
- `Sources/FreesideAPI` owns the generated client surface, the stateful mock server and its transport, and the per-type attention fixtures. Apple Swift OpenAPI Generator produces client and type source at build time from the schema mirror in that target.
- `Sources/FreesideCore` contains shared SwiftUI presentation code.
- `Tests/FreesideAPITests` exercises the generated client through the mock server, with no network or daemon; `Tests/FreesideCoreTests` covers the inbox, decision, sync, pairing, and session models against the same mock, plus the cache and credential stores.
- `Apps/macOS/AppIcon.icon` holds the macOS app icon, the §15 signet mark with explicit default and dark appearance artwork. `Apps/macOS/Info.plist` names that adaptive resource without a static icon-file fallback. Only the macOS target carries either file; iOS gains no icon here.

The Icon Composer document lets the system select the appearance and own the platform mask. Its default artwork is the prior light export; its dark artwork keeps that geometry but replaces the treatment with the §15 umber (`#16120E`) ground and tawny (`#C2912E`) mark. Re-derive the dark asset from the 1024-pixel default master with

```sh
./scripts/generate-mac-icon.sh
```

The mask preserves the approved mark geometry and its cutouts; the appearance change is palette-only. Xcode compiles the one document into the installed bundle's platform and appearance renditions. Keep the document as a normal resource and `CFBundleIconName` in `Info.plist` authoritative: asking the asset compiler to emit a standalone primary icon adds `CFBundleIconFile`, and Finder then prefers that static fallback over the appearance-aware catalog.

`Sources/FreesideAPI/openapi.yaml` is a mechanical mirror of the repository contract at `../api/openapi.yaml`. Refreshing it and rebuilding the generated client is one reproducible command:

```sh
./scripts/generate-api-client.sh
```

The command leaves the checkout clean when the mirror and generated client agree with the merged schema. Do not edit the mirror or generated output to work around a schema gap; file a `kind:contract` issue instead.

## Build and test

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

## Style

Swift formatting and style analysis both come from the toolchain's
`swift-format` (swift-format 6.3.0, shipped with Xcode 26.6; CI pins the
matching Xcode and fails if the tool version drifts). The configuration is
`.swift-format` in this directory; the decision record is
`devlog/2026-07-20-1140-swift-style-tooling.md`.

From `app/`:

```sh
# Check formatting and style (what CI runs):
xcrun swift-format lint --strict --recursive Sources Tests Apps Package.swift
# Rewrite in place:
xcrun swift-format format --in-place --recursive Sources Tests Apps Package.swift
```

Generated OpenAPI client sources are build-plugin output under `.build/` and
never checked in, so the commands above gate only the hand-written roots.
Deliberate constant-input force unwraps on mock and fixture surfaces carry
`// swift-format-ignore: NeverForceUnwrap` annotations; the rule stays on
everywhere else.
