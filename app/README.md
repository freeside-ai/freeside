# app

The SwiftUI multiplatform client: the macOS + iOS attention inbox, decision detail, and run timeline. Client databases are disposable read caches; the daemon is sole authority, and both platforms use the same sync API (see `docs/plan.md` §5.14).

**Bootstrap exemption** (plan §5.7): SwiftUI work in this directory does not flow through the Freeside pipeline until a macOS execution class exists (deferred, possibly forever). Go work joins the pipeline only once Freeside manages its own repo, the bootstrap test that follows the deliberately boring first repository (plan §11); this component may never join it.

- **Toolchain:** Xcode / Swift Package Manager.
- **Scope boundary:** client-side code only. The daemon/client contract is defined in `api/`; client code consuming it lives here, never in `api/`. No JS toolchain enters this component.
- **Status:** the inbox and per-type decision cards run against an in-process stateful mock of the contract (idempotent commands, conflict-with-replacement, sync envelope, device pairing and revocation), with the §5.14 client cache semantics (separate full-snapshot and observed cursors, bootstrap on revision gap, epoch-change discard), a persisted disposable cache, Keychain-held device credentials, the pairing flow, and the freshness banner. The client halves of §5.14 sync tests 1, 2, 8, 11, and 13–16 also converge against a real daemon process (`FreesideConvergenceTests`, env-gated): `bash scripts/run-convergence.sh` at the repo root builds and launches the `freeside-signet-dev` harness and runs the suite against it (#72); the conversation-path tests join with #68.

## Running

Launch arguments select the composition (`AppSession.fromEnvironment`):

- default: the permissive in-process mock with a pre-paired identity — the inbox renders immediately.
- `-FreesidePairingDemo YES`: the full pairing flow against an enforcing mock; the code is `483911`.
- `-FreesideServerURL <url>`: a real daemon; the device credential lives in the Keychain and the cache on disk.

Launch arguments also pin the presentation per launch (`LaunchInputs`), so screenshot and testing workflows drive the app without UI automation. These are launch arguments rather than environment variables because `open --args` forwards only arguments, and `xcrun simctl launch` forwards them too:

- `-FreesideColorScheme light|dark`: force an appearance without touching the system setting; unset follows the system.
- `-FreesideSelect <item-id>`: select the given inbox item at launch. `AttentionFixtures.defaultInboxItemIDs()` is the source of truth for the accepted values, today the default mock inbox's ids: `item-spec_approval`, `item-execution_failure`, `item-agent_question`, `item-review_diminishing_returns`, `item-review_dispute`, `item-ready_for_final_review`, `item-publish_blocked`, `item-run_proposal`, `item-system_health`, `item-blocked`. An unknown id is ignored with a note on stderr.

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
  -FreesideColorScheme light -FreesideSelect item-blocked
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
