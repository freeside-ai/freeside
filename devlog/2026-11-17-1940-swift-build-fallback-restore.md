# Restore The Nearest Main Swift Build On App PRs

Chose a fallback restore of `app/.build` for app PRs, followed by giving
each unchanged source file the modification time the entry's build saw,
over keeping the exact-only key, because every app PR built cold (median
264 s on CI) and the cause of the October result was the file times, which
can be put back (#3295).

This reverses the "no fallback key" part of
`2026-10-24-1753-swift-build-cache.md`. That note kept the fallback out
because #2249's inexact restore still recompiled every project module and
saved under a minute. The 2026-11-16 app test audit found why: a fresh
checkout gives every file a new time, and SwiftPM treats a file whose time
differs from the one its build saw as changed. #3294 made `main`'s entries
carry each tracked `app/` file's time and SHA-256, and this unit restores
the time to every file whose hash still matches. The exact-hit rule is
unchanged: only an exact hit on a PR without the app component skips the
build.

The fallback is the exact key without its tree. Matching the PR's merge
base instead was rejected: that entry often doesn't exist, and finding it
needs the Actions API. The prefix still names the image OS, the Xcode
build, and the toolchain, so an entry built by another compiler never
matches. An entry without a usable record is deleted and the job builds
cold, because a restored build without the times recompiles everything
anyway and would only cost the download.

## The Risk Accepted

A stale object can now count as current if SwiftPM, llbuild, or the Swift
driver decides a file is unchanged from something other than its time and
content, such as a dependency the build graph doesn't track. The exact key
ruled that out by construction; this design relies on the build tools'
change detection. The stale-object cases recorded on the PR bound it: a
changed source, a deleted source, a changed resource or digest file, a
file set back to older content, and a changed `Package.swift` or
`Package.resolved` were each seen by the build or its tests after an
inexact restore on the runner image. The record keys on content, so a file
set back to an older version keeps its checkout time and recompiles.

The October trust gap now also covers a prefix match: a PR that once saved
a fake entry in its own cache scope could restore it first. Collaborators
are still trusted, the reader saves nothing, and the owner's review stays
the gate.

Every app PR now downloads and extracts the entry (about 460 MB), including
the half that change the API module and save nothing, because a change
there rebuilds every module downstream. The measured saving on the PR,
weighted by the mix of app merges since 2026-10-24, is net of that cost.

Revisit when a toolchain changes how it detects changed files, when GitHub
moves the runner workspace path the entry's absolute paths assume, when the
weighted saving falls under 30 s, or when the generated API client is split
so that API changes stop rebuilding every module.
