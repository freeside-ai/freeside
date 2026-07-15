# app

The SwiftUI multiplatform client: the macOS + iOS attention inbox, decision detail, and run timeline. Client databases are disposable read caches; the daemon is sole authority, and both platforms use the same sync API (see `docs/plan.md` §5.14).

**Bootstrap exemption** (plan §5.7): SwiftUI work in this directory does not flow through the Freeside pipeline until a macOS execution class exists (deferred, possibly forever). Go work joins the pipeline only once Freeside manages its own repo, the bootstrap test that follows the deliberately boring first repository (plan §11); this component may never join it.

- **Toolchain:** Xcode / Swift Package Manager.
- **Scope boundary:** client-side code only. The daemon/client contract is defined in `api/`; client code consuming it lives here, never in `api/`. No JS toolchain enters this component.
- **Status:** initialized for Phase 1A with walking-skeleton macOS and iOS apps, a schema-generated API client, and an in-process mock transport. Inbox and decision-detail behavior land in the next saddle unit.

## Structure

- `Freeside.xcodeproj` contains the two application targets. Both consume the local `FreesideCore` Swift package product.
- `Sources/FreesideAPI` owns the generated client surface and mock transport. Apple Swift OpenAPI Generator produces client and type source at build time from the schema mirror in that target.
- `Sources/FreesideCore` contains shared SwiftUI presentation code.
- `Tests/FreesideAPITests` exercises the generated client through the mock transport, with no network or daemon.

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
