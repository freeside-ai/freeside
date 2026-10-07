# Separate Screenshot Raster State From Contrast Inputs

Chose `ImageRenderer.render` into an explicit sRGB bitmap over the renderer's
`cgImage` property for #1698. Identical `cgImage` captures returned different
glyph-edge pixels before digest conversion. Constructing extra fixtures did
not change the affected app images, but rendering those fixtures did. The
explicit-context path removed that order-sensitive difference in controlled
probes. Its bitmap must be top-aligned when a fractional layout height rounds
up; the spare fraction belongs below the content. The framework's internal
raster-cache implementation is not established by these observations.

Chose a scoped in-memory contrast input over saving and restoring shared
defaults. The real AppKit color callback reads that input. Nested scopes and
failures restore it without persistent writes. Screenshot disclosure stores
likewise use unique suites and remove only their own temporary state.
Production launch/defaults and host fallback behavior remain unchanged.
The former macOS 26.5 hosted override was removed after the current macOS
26.6 runner passed the universal baseline without an override; hosted
recording remains refused.

These are distinct boundaries. A candidate still failed its three-sample
settling check while a concurrent legacy screenshot process was running. The
trace caught the fallback changing from host to Increased Contrast between
samples while host contrast stayed off. The legacy source still wrote that
shared preference. Another changed candidate image exactly matched its
explicit Increased Contrast digest. This establishes a reachable interference
mechanism even when the candidate itself never writes the preference.

The first candidate task-history failure lacked input traces and saved
samples. Attributing that earlier failure to the same interference remains an
inference, not retrospective proof. Later controlled validation must retain
all outcomes and stop if drift recurs without the legacy writer. Replaying
each drawing callback twice produced equal raw pixels through the newly
traced failure; renderer and image retention experiments did not establish a
lifetime fix.

Rejected more retries, sleeps, fixture ordering, and selecting a sample by
its expected digest. Two consecutive samples can agree on the wrong pixels.
Comparison and recording retain exact dimensions-plus-RGBA SHA equality, and
recording writes only after the whole capture completes. Fresh-process
isolation adds startup and fixture costs without addressing an external
shared-defaults writer, so the evidence does not justify it here.

Mounted countdown checks belong to the same recording boundary. Their exact
PNG comparison must throw on failure: a non-throwing test assertion can mark
the test red yet let capture return and replace the manifest. Preserve the
failed images, clock bounds, and pixel digests without changing successful
capture order or weakening the comparison.

The mounted pairing helper also needs the explicit bitmap renderer. After a
full matrix, its old `cgImage` path produced 543 differing pixels, each by one
channel value, for equal-size images with identical countdown text and urgency.
The pair took 15 milliseconds under fixed Standard Contrast. Decoding the
failed PNGs only after comparison established a pixel difference without
changing the successful rendering sequence. Reuse the same bitmap boundary
there and keep the exact PNG comparison.

Revisit when an unchanged surface differs across order, fixture-set, or
process probes under controlled inputs, or when a macOS/SwiftUI update changes
rasterization. Finite repetitions are evidence for one host and commit, not
proof against every possible load. Host contrast pinning remains #945,
automatic dump-directory isolation remains #1150, and baseline families and
recording speed remain #1834.
