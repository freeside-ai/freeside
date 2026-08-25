# Handoff: Freeside UX review response — planning package

## What this is

An external UX review of the Freeside clients (macOS + iOS, `main` @ `0e81ee2e`, reviewed 2026-08-25) produced 27 findings. This bundle is the **design response**: a verdict on every finding, twelve worked design proposals, and a build-order split separating what the clients can do alone from what needs daemon/contract work first.

**The immediate goal is a planning session, not implementation.** Expected output of that session: a set of contract-first work units allocated to Wave 7 / Wave 8 / Phase 2, plus a client-side work item that can start immediately — not code.

## About the design files

`UX Review Response.dc.html` is a **design reference created in HTML**. It is a prototype showing intended composition, hierarchy, colour, and copy — it is not production code and nothing in it should be ported literally. The target environment is the existing SwiftUI app in `app/Sources/FreesideCore/`, using its established patterns (`NavigationSplitView`, `NavigationStack`, `List`, `Form`, `ContentUnavailableView`, native sheets and toolbars) and its own `DesignLanguage.swift` colour/type vocabulary.

Open it by opening the HTML file in a browser; `support.js` must stay beside it. It is a single scrollable document, ~14 sections. Every proposal carries a stable id (`1a` … `1l`) used throughout the written docs in this bundle.

## Fidelity

Mixed, deliberately, and stated per proposal in the document itself:

- **Mocks (hifi intent)** — `1a` `1b` `1c` `1d` `1e` `1h` (two of four) `1i` `1j` `1k` `1l`. Final colours, type register, hierarchy, and copy. Exact hex values and type roles are in `03-tokens-and-specs.md`. These are *intent-accurate, not pixel-final*: several frames are drawn at reduced scale (`1a` states its 0.62× text scale explicitly) and the geometry is illustrative. Treat the colour values, ratios, content order, and copy as normative; treat pixel measurements as indicative.
- **Wireframes (lofi)** — `1f` `1g` (structure frames) `1h` (dispute, review yield). These argue structure only. Style them from `DesignLanguage.swift`; do not read the greys as a palette.

## Read in this order

| File | What it carries |
|---|---|
| `01-verdicts.md` | All 27 findings with verdict and position; the four remedies refused and why |
| `02-proposals.md` | The twelve proposals: what each changes, build rules, target source files |
| `03-tokens-and-specs.md` | Contrast-safe colour variants with computed ratios; type roles; component specs |
| `04-work-split.md` | Client-only vs. Wave 7 contract vs. Wave 8 vs. Phase 2/3; contract shapes; the one open decision |
| `reference/Freeside-UX-Review.md` | The original review, verbatim, with its rendered-state matrices |
| `reference/design-system-guide.md` | Freeside design language (identity policy, two grounds, three faces) |
| `reference/tokens.css` | Current token values as shipped, for diffing against `03` |

## Headline positions

1. **All 27 findings are actionable.** 17 adopted as written, 9 adopted with a changed remedy, 1 (search) right but premature.
2. **Four of the review's own remedies are refused**: capping Dynamic Type, replacing the palette with system colours, system red for destructive controls, and a sticky action footer on macOS. Rationale in `01-verdicts.md`.
3. **Sequencing change vs. the review's roadmap**: the recommendation-led card shell (`1c`) lands *before* the Dynamic Type reflow (`1a`), because the accessibility composition is a property of the new shell rather than a patch on the one being replaced.
4. **The split matters more than the designs.** Roughly two-thirds of the review is client recomposition needing no contract change and can start now. The remainder is not a UI problem: a recommendation the client infers, a fact the client parses out of prose, or a button whose transaction does not exist would each be worse than the current honest mess. See `04-work-split.md`.
5. **One decision cannot be made in the client**: unsupported actions (P1-2). Either the Wave 7 transactions land, or the affected workflows come out of the Phase 1B exit claim. Hiding the buttons is correct either way and settles nothing.

## Target source map

Client work touches, roughly:

| Area | Files |
|---|---|
| Colour/type tokens (`1b`) | `app/Sources/FreesideCore/DesignLanguage.swift` |
| Card shell, actions, evidence, confirmation (`1a` `1c` `1d` `1i` `1j`) | `DecisionDetailView.swift`, `DecisionModel.swift`, `AttachmentLoader.swift`, `ActionOutcome.swift` |
| Rows, filters, selection, counts (`1e`) | `InboxView.swift`, `InboxStore.swift`, `AttentionDisplay.swift` |
| Navigation, tabs, inspector, empty pane (`1f` `1g`) | `FreesideRootView.swift` |
| Refresh, freshness, keyboard, menu bar (`1k`) | `SyncCoordinator.swift`, `FreshnessBanner.swift`, `DaemonMenuModel.swift`, `DaemonService.swift` |
| Pairing (`1l`) | `PairingView.swift`, `PairingModel.swift` |
| Run rows and timeline (`1l` `1h` stage rail) | `RunsListView.swift`, `RunTimelineView.swift` |
| Fixtures for every new state | `app/Sources/FreesideAPI/AttentionFixtures.swift`, `RunFixtures.swift` |

Contract work touches `api/openapi.yaml` and the daemon projections behind it; plan sections named in `04-work-split.md`.

## Suggested planning-session prompt

> Read `design_handoff_freeside_ux_review/` in full, then read `docs/plan.md` §4, §5.13, §5.14, §5.15, §9, and the Wave 7/8 definitions. Produce: (1) a Wave 7 attention-contract work unit specified contract-first, with the OpenAPI surface it changes; (2) the client-only work item as a sequence of PR-sized units with no contract dependency; (3) allocation for everything else across Wave 8 / Phase 2 / Phase 3; (4) a recommendation on the open P1-2 decision in `04-work-split.md`. Flag anything in the design proposals that conflicts with the plan's existing contracts rather than designing around it.

Screenshots of the proposals are not included — say the word and I'll add them.
