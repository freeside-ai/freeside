# Design Language Restyle of the Built Client Surfaces

Direct owner assignment from the "Freeside Design Language Handoff"
bundle (Claude Design). A styling pass: the §15 palette, faces, and
semantic mapping applied to every Done surface in `app/SURFACES.md`,
with view structure, wording, and behavior unchanged except the
menu-bar item arrangement the handoff specifies.

## Decisions

- **Tokens as a `Color` extension, not an Asset Catalog.** Chose
  adaptive `NSColor`/`UIColor` providers in
  `app/Sources/FreesideCore/DesignLanguage.swift` over catalog colors
  because FreesideCore is a Swift package shared by both app targets,
  and a catalog would have to live in each Xcode target (or become a
  package resource with its own bundle lookup). The hex values mirror
  the handoff's `tokens.css` one-to-one. Owner-facing name: the `text`
  token is `Color.ink` (with `inkDim`, `inkFaint`) so it cannot be
  misread as the `Text` view.
- **Fonts bundled as a FreesideCore package resource and registered
  with CoreText at `FreesideRootView` init**, over `UIAppFonts`/
  `ATSApplicationFontsPath` entries in each target's Info.plist. One
  registration path serves both targets and needs no pbxproj edits;
  the Info.plist route would need per-target font lists that drift.
- **The serif ships as "Freeside Serif", a wght=500 / opsz=20 instance
  of Source Serif 4 (Adobe 4.005R).** Adobe publishes no static Medium,
  the spec wants Medium 500 and nothing heavier, and CoreText's
  descriptor matching on the variable font snaps to named instances
  (Semibold at opsz 8), so a fixed instance is the only way to get
  exactly 500. Instancing makes a Modified Version under the OFL, whose
  Reserved Font Name "Source" then cannot be used, hence the rename;
  `app/scripts/instance-serif-font.sh` regenerates it. Rejected:
  bundling the variable font and building `Font` from a `CTFont` with
  explicit variation axes, because that path loses `relativeTo:`
  Dynamic Type scaling for every title.
- **List rows become cards and the platform selection highlight is
  replaced by the accent-dim border** (plain list style, clear row
  background, `isSelected` passed into the row). The handoff's
  "selected row's border becomes accent-dim" cannot coexist with the
  system's filled highlight, which borrows the system accent.
- **Hold reason color keys on the run's outcome**, wax only when the
  run is failed or lost, accent otherwise. The handoff says "wax when
  the hold is a failure" without naming which `RunHoldReason` members
  are failures; the outcome already classifies that and avoids
  inventing a per-member taxonomy in the client.
- **Decision card primary action = the first offered action;
  Decline, Stop, and Stop unattended are destructive (wax).** The
  handoff names Decline and Stop; the daemon's action order already
  leads with the intended default.
- **Menu bar: no "since H:mm" gloss.** A standard menu item cannot
  carry a trailing gloss, and the Started row one line down already
  states the time, so the gloss was judged not expressible without a
  custom item, which the handoff rules out.
- **Success chips are quiet in hue, not in contrast (owner choice).**
  The first cut rendered "✓ ready" and "✓ completed" in text-dim; on
  the day ground a 10pt hairline chip in that tone all but vanished.
  They now use the full text color, still a neutral tick, never green
  or the accent, and all chips use Plex Mono Medium. The rule the
  handoff protects is hue, and a success state the eye skips is a
  different failure.
- **Segmented pickers keep platform appearance.** macOS's segmented
  control ignores SwiftUI tints for its track; only the iOS controls
  take the accent through `.tint`.

## Presentation Deltas Accepted as Styling

The handoff's chip and banner specs carry glyphs and casing the built
views did not: chip labels lowercase, a tick before "ready" and
"completed", a live dot before a running invocation (open when the
observation is not live, carrying the bit the old SF Symbol did), the
banner keywords UNREACHABLE / SYNC FAILING / REVOKED beside the
verbatim messages, the schedule pill's middle dot in place of its eye
and clock symbols, the dropped clock before the creation timestamp,
and the ellipsis on "Open Login Items…". Each is in the handoff; none
changes a message's words.

## Verification Findings

- The iOS pairs came from `simctl io screenshot`; the Mac pairs from
  the README's `screencapture` workflow once Screen Recording was
  granted to the session's terminal mid-unit.

Revisit when the app gains a surface the handoff marks "proposed / not
built" (devices list, rich expiry), or when a platform release lets a
`navigationTitle` on macOS take a custom face.
