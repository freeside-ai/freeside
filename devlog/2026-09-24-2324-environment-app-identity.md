# Derive Mac App Identity From the Environment Tier

Chose one `FreesideEnvironment` enum in the app, with `install-mac-app.sh` as
the only other writer of its strings, over per-tier constants spread across
the app, plist, and installer. The plan's tier table (`docs/plan.md`
"Environments: Prod, Dev, and Ephemeral") now has two mirrors that tests pin
from both sides: `FreesideEnvironmentTests` for the app and
`scripts/test-install-mac-app.sh` for the installer.

## Decisions

- **Bundle ID through a build setting.** Chose a `FREESIDE_MAC_BUNDLE_ID`
  build setting that `PRODUCT_BUNDLE_IDENTIFIER` reads over passing
  `PRODUCT_BUNDLE_IDENTIFIER` on the xcodebuild command line, because a
  command-line setting applies to every target and would also restamp
  FreesideCore's resource bundle. This touches `project.pbxproj`, which the
  issue's declared paths did not list; it stays inside `app/`.
- **Ephemeral never writes the persisted server URL.** The issue required
  ephemeral to skip reading `UserDefaults["FreesideServerURL"]`. A Debug build
  keeps the prod bundle ID and so shares prod's defaults domain, so writing
  there would also let an ephemeral run redirect the prod app's next launch.
  Chose to make ephemeral's persistence a no-op too.
- **Ephemeral keeps its device credential in memory.** Chose an in-memory
  credential store for every ephemeral session over the URL-keyed Keychain
  item. A run may reuse a fixed port (the real-run harness pins one) with a
  fresh credential database, so a Keychain credential from an earlier run
  would skip the new run's pairing code and sync with a stale token. The plan
  says `ephemeral` pairs afresh on each run; the cost is re-pairing after
  each ephemeral launch.
- **iOS stops reading a readiness file.** The parameterless
  `AppSession.fromEnvironment()` becomes the remote-client path. The Mac
  readiness path it used to probe never exists on iOS, so no working launch
  changes.
- **Readiness directory must be absolute and ephemeral-only.** Chose to refuse
  `-FreesideReadinessDir` in a supervised tier over letting it override the
  tier's own file, so a supervised app can only follow its own daemon.
- **Keychain group per bundle ID.** `$(AppIdentifierPrefix)$(PRODUCT_BUNDLE_IDENTIFIER)`
  resolves to the old literal for prod, so an upgraded prod install keeps its
  stored device credential; dev gets a separate group and credential.
- **Prod interlock at the installer, not in the app.** Replacing prod without
  a TTY requires `--prod`; dev needs no confirmation. The check runs after
  argument validation and before any recovery or replacement step, so an
  interrupted-install restore also sits behind it.

## Refute-First Findings

An adversarial review of the installer's replace and recovery paths and the
credential surface ran before the first push.

- **Confirmed and fixed:** an ephemeral app accepted a
  `-FreesideReadinessDir` inside a supervised state root and would consume
  that daemon's one-time pairing code. It is now refused for both supervised
  roots after symlink resolution, matching `freesided`'s ephemeral guard.
- **Confirmed and fixed:** the app's disk cache always lived under
  `Application Support/Freeside`, so dev and ephemeral wrote under the prod
  root. The cache root now follows the tier (ephemeral is in memory).
  Reading the same code showed `connect(serverURL:)` rebuilt its session
  with the default URL persistence, so an ephemeral app could still write
  prod's defaults domain after a typed connection; it now reuses the
  session's rules.
- **Allowed by decision:** `FREESIDE_ENV` overrides an installed app's
  `Info.plist` key, so `FREESIDE_ENV=prod` on the dev bundle attaches it to
  prod. The plan fixes that precedence; it is an explicit operator act.
- **Allowed by decision:** an explicit `-FreesideServerURL` or a typed
  `:7331` in an ephemeral Debug build reaches prod, because Debug keeps the
  prod bundle ID as before this change. It pairs as a new device held only in
  memory and never reads prod's stored credential. The `FreesideMacProd`
  scheme is the intended route; before this change every Debug launch did so
  by default.
- **Allowed by decision:** a non-default `FREESIDE_MAC_STATE_ROOT` produces a
  LaunchAgent that `freesided` refuses at start. The override is documented
  as test-only, and the failure is loud.
- **Disproved by reading and the harness:** a dev install moving, replacing,
  or recovering over prod's app; deleting prod's registration marker; the
  interlock running after a destructive step; `--prod` confirming dev; prod
  losing its stored credential on upgrade; dev reading prod's keychain item.

## Owner Decision

Merge this unit right after #1501 and don't re-run the prod installer from a
base that has one without the other (2026-09-23, recorded on #1500). The
installed plist passes `-environment <tier>`, which only #1501's `freesided`
accepts; #1501's daemon without this unit's flag starts supervised tiers as
ephemeral, which refuses their ports.

Revisit when a third supervised tier is added, or when the tier moves from
the installer's pre-signing edit of the built `Info.plist` into a build
setting.
