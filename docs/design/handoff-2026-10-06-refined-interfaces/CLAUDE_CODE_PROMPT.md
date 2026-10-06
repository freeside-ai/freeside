# Brief for Claude Code — Freeside refined interfaces (presentation-only sweep)

Paste this into Claude Code at the root of the `freeside` repository with this handoff folder alongside.

---

You are implementing a presentation-only refinement of the Freeside Mac and iOS clients (`app/`, SwiftUI, `FreesideCore`). The design is finished and lives in `design_handoff_freeside_refined/`. Read, in this order, before planning:

1. `design_handoff_freeside_refined/README.md` — the scope, the scale, the rule → code ledger, the sweep order and the gate on open decisions.
2. `design_handoff_freeside_refined/design-direction.md` — the eight principles P1–P8.
3. `design_handoff_freeside_refined/Design Language Survey - 6 Oct 2026.dc.html` — open in a browser. Part 1b is the emphasis ladder; Part 2 card **4b** is the reference build; Parts 3, 4, 5 are the rules **R0–R33** (each row says what changes on main and names the component); Parts 5–7 draw each surface as built beside refined; **Part 8** is the ledger of open decisions.
4. `design_handoff_freeside_refined/Interfaces - Refined - 6 Oct 2026.dc.html` — the target. Every surface at real size, day and dusk, Mac and iPhone. Each frame’s caption names the rules it applies and how many decisions are open on it.
5. `app/SURFACES.md` and the views it names — the truth for what each surface contains. Never add a fact, action or module the source does not have; never remove one.

Constraints:
- **Presentation only.** No daemon, API, contract, schema or command change. Relabeling names an existing command; it never changes what it does. `Approve` on the review-dispute card keeps its label.
- Work through the existing vocabulary in `DesignLanguage.swift` (`KeywordLabel`, `KeywordDisclosure`, `FactRow`, `StateChip`, `FreesideLink`, `FreesideActionButtonStyle`, `FreesideSheetHeader`, `ChronologyMarker`, `UnverifiedLabel`, `FreesidePalette`) — extend it (quote block, system callout, notice, sentence disclosure, diff tokens, dusk lifted cuts) rather than styling ad hoc in views.
- Everything under *System Chrome* in SURFACES.md stays native.
- Dusk follows the token mirror plus the lifted cuts in the README. Increased Contrast keeps its `-ic` cuts.
- Accessibility sizes stack; nothing shown at the default size is hidden at a larger one.

Plan first. Produce:
1. A sweep plan following the README’s order (shared → inbox/tasks → question/conversation → remaining cards → standing surfaces/sheets/readers), one PR per sweep, each listing the views touched, the rules applied (by number) and the frames it must match (by caption).
2. For sweep 0, the exact API of the new/changed `DesignLanguage.swift` pieces before writing views against them.
3. A list of the ◆ rows (survey Part 8) each sweep depends on. **Stop and ask before sweep 0** if R2, R3, R5 or R7 is not yet decided; for per-surface ◆, implement the drawn default and note the row in the PR.
4. Fixture updates: day/dusk screenshot digests for Mac and iPhone per sweep, and the SURFACES.md lines rewritten to name the rules applied.

Then implement sweep 0 and stop for review before sweep 1.
