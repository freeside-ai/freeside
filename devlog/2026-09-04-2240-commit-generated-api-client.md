# Track the Generated Swift API Client

Chose the pinned generator package's command plugin and tracked Swift output
over regenerating during each consumer build. This overturns the July 20
[style decision](2026-07-20-1140-swift-style-tooling.md) that generated sources
remain untracked build-plugin output. The owner approved this change in #1159.

The changed condition is measured clean-build cost: September 4/5 CI logs
attribute 42–85 seconds per Swift consumer build to the generator toolchain,
including about 63 seconds on the app build's critical path. The July 15
[runtime decision](2026-07-15-2040-app-ci-runtime.md) anticipated revisiting
the approach when clean builds exceeded the acceptable feedback budget.

The command plugin produces byte-identical `Types.swift` and `Client.swift`
for the existing schema, configuration, and 1.13.0 pin. Its fixed output
directory is `GeneratedSources/`; the build plugin's empty `Server.swift`
is unnecessary. Generation runs in a separate CI job so consumer builds can
compile the tracked output immediately.

Rejected `.build` and DerivedData caching because they change the provenance
of clean-build evidence and require cache invalidation policy. Rejected an
external generator CLI in a gitignored directory because the existing pinned
package already supplies the command plugin and dependency resolution.

The format gate moves from exclusion by construction under `.build/` to an
explicit path exclusion in both lint and format-drift checks. Generated code
keeps the generator's formatting and is marked `linguist-generated`.

The schema and generator configuration are copy resources so SwiftPM keeps
them visible to the command plugin without unhandled-file warnings. Excluding
them from the target would prevent the plugin from discovering its inputs.

Revisit when the toolchain supplies a prebuilt plugin binary, or a generator
upgrade produces a diff too large to review meaningfully.
