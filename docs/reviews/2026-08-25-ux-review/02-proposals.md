# The twelve proposals

Each entry: what it changes, the build rules (normative), the source files it lands in, and its dependency class. `Client` = ships against today's API. `Wave 7` = needs the attention contract first (see `04-work-split.md`).

---

## 1a — The accessibility composition
**Mock · iPhone · AX5 · P0-1 · Client only**

Not a separate design: the `1c` shell with every horizontal pair broken vertical and everything below the recommendation collapsed. At AX5 a phone shows ~4 words per line, so the card must also decide what is seen first.

What changes: no repeated title (the nav bar names the type, the card opens on the ask); badges become a two-line mono text block instead of chip geometry; every label/value pair stacks; the three lower sections become collapsed disclosure rows.

Build rules:
- Switch composition on the **size category**, not on width: at `.accessibilityMedium` and above, header, facts, and action rows go vertical.
- Chips render single-line and non-compressible, or they render as text. Never a fixed-width chip with wrapping content.
- Buttons grow vertically; labels wrap to two lines rather than truncating.
- Disclosure state is composition-dependent: expanded at standard sizes, collapsed at accessibility sizes.
- VoiceOver order matches visual order exactly: type, urgency, ask, recommendation, action, facts, evidence, details.
- Screenshot regression at xSmall, Large, xxxLarge, AX1, AX3, AX5 across inbox, every card type, run list, run timeline, pairing.

Files: `DecisionDetailView.swift`, plus the regression harness in `app/Tests`.

---

## 1b — Text, wash, border: one semantic, three jobs
**Mock · tokens · both grounds · P0-2 · Client only**

The palette is not the problem; asking one hex to be a chip fill, a hairline, and 11pt text is. Values, ratios, and Increased Contrast cuts: `03-tokens-and-specs.md`.

Build rules:
- Ratios in `03` are computed sRGB values against the card ground; the build must assert them in a test, not trust the sheet.
- Text tokens are the only ones a `Text` view may use. A lint rule on `.foregroundStyle(.accent)` is cheaper than another audit.
- Mono chips at 9–10pt are small text: they take the text token, never the border token.
- Washes carry no threshold because they carry no meaning alone — every wash pairs with a text token and a glyph.
- Four cuts per semantic (day, dusk, day IC, dusk IC) declared in one place so Increase Contrast actually moves custom colours.
- `inkFaint` is **retired as a text colour** — it becomes rules and decoration only.

Files: `DesignLanguage.swift` (the `Color.freeside(day:dusk:)` factory needs a contrast-variant sibling).

---

## 1c — The recommendation-led card
**Mock · iPhone 393 · P1-1 P1-4 P2-1 P2-12 P3-2 · Client shell + Wave 7 recommendation authority**

One shell for all eleven card types: **ask → recommendation → why → confidence → accepting action → facts → claims → evidence → details**. The recommendation block owns the primary action so reasoning and button cannot drift apart. Out: repeated title, absolute timestamp, four equal buttons.

Build rules:
- The ask is one sentence, phrased as a question, generated **per card type** — not the item's free-text reason string.
- **No recommendation, no block.** If the daemon offers no recommended action, the card shows ask, facts, and equally weighted choices. Never invent a recommendation to fill the shell, and never treat the first `requested_decision` as recommended because it is first.
- Confidence renders only in words the daemon supplies; absent confidence renders nothing, not "unknown".
- One prominent action per card. The recommendation block holds it; the bordered row holds the reviewing action; everything else goes to More….
- The pinned iPhone footer appears only after the recommendation block leaves the viewport, and never on macOS.
- Help: inline copy for load-bearing concepts ("Written by the agent, not checked by the daemon" is permanent in the claims block); macOS tooltips for the mono register (`waiting 12 min` → exact timestamp); no TipKit yet. A consequence never hides in a tooltip.

Files: `DecisionDetailView.swift`, `DecisionModel.swift`, `AttentionDisplay.swift` (per-type ask strings).

---

## 1d — Actions: one primary, no roadmap, real consequences
**Mock · macOS · P1-2 P1-9 P3-2 P3-3 · Client ranking + Wave 7 transactions**

Five equal buttons and "Actions carrying discussion or parameters arrive with later units" is the app telling the operator about its own backlog. Ranked: recommended action (filled, in the block) → one reviewing action (bordered) → overflow menu (Snooze, Mark seen, Dismiss, separator, destructive).

Build rules:
- The client publishes a capability set. Requested actions outside it are filtered out of the button row and recorded in Technical details, so an audit still shows what the daemon asked for.
- When **every** faithful response is outside the set, render the not-decidable-here state — never a card with no way to act and no explanation. Copy in the mock: *"This decision needs a written answer, and this build cannot carry one. Nothing is blocked by opening it — the item stays open until answered."*
- No roadmap language in shipping copy. "Later units" is a devlog phrase.
- Confirmation required for stop, decline, and dismiss-with-loss; never for approve or snooze. Cancel is always the default button. State the consequence in words: *"The current invocation is discarded. Work already exported stays; the round in flight does not."*
- Destructive role only where the system draws the surface; in-card destructive actions keep wax text and no fill.
- Icons only for actions that change where you go or what is lost (`↗` View PR, `⟳` Retry, `◷` Snooze, `■` Stop, `↺` Return to agent). Approve, Decline, Adjudicate get **no icon**.

Files: `DecisionDetailView.swift`, `ActionOutcome.swift`, `MockContractValidation.swift`.

---

## 1e — The scanning layer
**Mock · macOS sidebar · both grounds · P1-8 P2-2 P2-3 P2-4 P2-5 · Client rows + Wave 7 names**

Five findings, one fix: the row spends its three lines on things the operator already knows or cannot use. `proj-1 run-execution-failure` and a permanent `open` badge become the project name, the work-unit name, and how long this has waited.

Build rules:
- Line 1: type + badge (urgent/high, or an unusual lifecycle state). Line 2: written summary, sentence-cased, generated per card type — not the daemon's lowercase reason. Line 3: `project · work unit · relative time`; fall back to the identifier only if a name is missing.
- Relative time updates on a coarse timer (minute granularity under an hour) and reads "blocked 18h" / "due in 2h" where a deadline exists.
- Badges: urgent and high always; superseded, expired, revoked, degraded always; normal and open never inside the Open scope.
- Filter counts on every scope from the same query as the list, so a count and its scope cannot disagree. The urgent pill renders only when non-zero, and never for high.
- Selection = leading bar + wash step + border, on both platforms; on iOS the system selection stays underneath. Must survive Differentiate Without Color.
- Identifiers stay one gesture away: context menu (Copy item ID / run reference / evidence digest, Reveal in Technical details) and macOS tooltips.

Files: `InboxView.swift`, `InboxStore.swift`, `AttentionDisplay.swift`.

---

## 1f — iPhone: two tabs, two stacks
**Wireframe · iPhone · P1-3 · Client only**

Two `NavigationStack`s in a `TabView`; the Mac keeps `NavigationSplitView`. One shared store, two navigation paths.

Build rules:
- Scope and project move into the Inbox root — filters belong to a list, not to the app.
- Tab badge shows the open count only when non-zero; urgent is stated in text under the list header, not as a red dot.
- Deep links (notification taps) select the tab, then push — never present a decision outside its stack.
- Adopt the adaptable tab/sidebar configuration where the deployment target allows, so iPad regular width gets a sidebar from the same declaration.

Files: `FreesideRootView.swift`.

---

## 1g — macOS: spend the width, but only when there is width
**Wireframe + mock · macOS · P2-7 P3-1 · Client only**

Facts/bindings/attachments go in a native `.inspector`, not a permanent third column. Card goes two-column above ~1000pt of detail width, with recommendation and actions in the trailing column — which is why no sticky footer is needed.

Build rules:
- Inspector toggled from the toolbar, state remembered per section, never open by default.
- Below the two-column threshold the trailing column's content returns to the flow **in the same order**.
- The card keeps a reading measure (~72 characters); extra width goes to the trailing column and inspector, never to longer lines.
- Empty pane is a summary, not a dashboard: open count, highest priority, waiting longest, active runs, daemon state, one route in. No animation; no coloured number that is not exceptional.

Files: `FreesideRootView.swift`, `DecisionDetailView.swift`.

---

## 1h — Four compositions, one module set
**Mock ×2 + wireframe ×2 · P1-5 · Client modules + Wave 7 typed facts**

Not four new layouts: the `1c` shell with one graphic module inserted where the decisive fact lives.

- **Ready for final review** (mock): readiness checklist (4 gates, neutral ticks, wax for the failing line) + change summary bar (+412/−96 across 14 files) + yield line (R1 6 → R2 2 → R3 0).
- **Execution failure** (mock): horizontal stage rail with the failed stage marked, likely-cause line naming the failing assertion, attempt timings, recommendation = Return to agent.
- **Review dispute** (wireframe): equal two-column positions, a "what the daemon can verify" block, then adjudicate. **No recommendation block** — a card that preserves dissent cannot also pick a winner in accent colour.
- **Diminishing returns** (wireframe): stacked new-vs-recurring bars per round; the chart *is* the argument for finishing.

Build rules:
- Module set: `FactBlock`, `Recommendation`, `Checklist`, `StageRail`, `Comparison`, `YieldChart`, `Claims`, `Evidence`, `Details`. A card type is an ordering of modules, never a new view.
- Success stays quiet: checklist tick is neutral ink, never green. Only the failing line takes wax.
- Every graphic carries adjacent text stating the same fact plus a VoiceOver summary ("verify failed, stage 3 of 4"). Decorative rails hidden from VoiceOver.
- The stage rail reflows vertical at accessibility sizes, reusing the run timeline's rail rather than a second implementation.
- System health and blocked come later; they are re-orderings of existing fact modules.

Files: new module views under `FreesideCore`, `RunTimelineView.swift` (rail reuse), `AttentionFixtures.swift`.

---

## 1i — Evidence has five states, and none is silence
**Mock · both platforms · P1-7 · Client states + Wave 7 metadata**

States: **loading** (spinner + "fetching by digest…"), **image available** (preview + expand), **not an image** (media type, size, first-line preview, Quick Look), **unavailable** (wax wash: "The daemon holds the digest but not the bytes" — the claim is still a claim), **too large here** (size + "Open on the Mac").

Build rules:
- The digest is a caption, never the headline. The headline is the artifact's kind.
- Unavailable is a state, not an error banner: the card stays usable and the claim stays labelled a claim.
- Images open in a native preview (Quick Look / zoomable sheet) and remain memory-only.
- A loading state that outlives its timeout becomes unavailable — it never spins forever, which is what the rendered fixture did for seven seconds.
- Every state keeps a copy affordance for the digest.

Files: `AttachmentLoader.swift`, `DecisionDetailView.swift`.

---

## 1j — After the decision
**Mock · macOS detail · P1-6 · Client only**

Three states: applied (neutral tick banner + "View", next highest-priority item loaded beneath), inbox clear (a result, not a blank pane, and it never says "well done"), uncertain (accent RETRY banner, card and selection both stay).

Build rules:
- Confirmation lives **above** the detail pane, outside the card's lifetime, so resolving the item cannot unmount its own receipt. Persists ~6s and through the advance.
- Announced to VoiceOver before focus moves.
- Advance is opt-out in settings but never instant. Success stays neutral — no green, no chime.
- "View" reopens the resolved item in the Resolved scope without changing the current filter.
- Uncertain submissions never advance and never clear the action row; Retry is idempotent at the daemon.

Files: `DecisionModel.swift`, `FreesideRootView.swift`, `ActionOutcome.swift`.

---

## 1k — Freshness, keyboard, and the way back in
**Mock · macOS chrome + iOS · P1-10 P2-6 P2-10 P2-11 · Client only (+ Phase 2 search)**

Toolbar: sidebar toggle, refresh, "Updated 12s ago", inspector toggle. View menu: ⌘1 Inbox, ⌘2 Runs, ⌘R Refresh, ⌥⌘I Inspector, J/↓ next, K/↑ previous, ↩ take recommended. Menu-bar extra: Open Freeside, Show Inbox · 7, urgent line when it exists, daemon section, Quit.

Build rules:
- Last-updated is relative and always visible; it turns wax once past the staleness threshold, at which point the freshness banner takes over.
- Manual refresh never blocks the UI and never clears a pending decision.
- Refresh on foreground activation and on reachability regained, coalesced so a fast app-switch does not thrash the daemon.
- ↩ takes the recommendation **only** when revalidation has passed and one exists; otherwise it does nothing and says so.
- Esc dismisses sheets and clears the pending action; it never resolves an item. **Space is not bound** — it belongs to the system and Quick Look. **No destructive shortcut.**
- Every shortcut has a menu item.
- ⌘F registered and disabled with no visible affordance until search ships.
- The urgent count is one source, three surfaces (tab badge, sidebar pill, menu-bar line).

Files: `SyncCoordinator.swift`, `FreshnessBanner.swift`, `DaemonMenuModel.swift`, `FreesideRootView.swift`.

---

## 1l — First run, and the run list
**Mock · iPhone + macOS · P2-8 P2-9 · Client run rows + Wave 7 pairing identity**

Pairing names what is being joined (`freesided · studio.local`), groups the code with a Paste affordance, states the expiry, prefills the device name, and says where that name will appear ("in Devices on the host and in the audit record of every decision you make from here"). Run rows go two lines plus a wrapping chip row: what it is / where it is / what is holding it.

Build rules:
- Code field uses one-time-code semantics and the ASCII-uppercase keyboard the daemon's alphabet needs; grouping is display-only, never validation. **No numeric keypad** — the codes are not numeric.
- Paste accepts the code with or without separator and trims terminal cruft.
- Device name prefills from the system device name, stays editable; helper states both places it appears.
- Pairing never shows a credential, token, or key — only host, code, name.
- Watch chips are single-line and wrap. The hold line is the only run-row line permitted emphasis, because it is why the run is on screen.
- The run ID moves to the timeline header and the copy menu.

Files: `PairingView.swift`, `PairingModel.swift`, `RunsListView.swift`.
