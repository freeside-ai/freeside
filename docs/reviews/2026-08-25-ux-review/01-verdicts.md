# Verdicts on all 27 findings

Verdict vocabulary:

- **Adopt** — the review's own solution is the one we would have chosen.
- **Adopt, modified** — the diagnosis holds, the prescription changes.
- **Later** — right call, wrong moment.

The `Ex.` column is the proposal id in `UX Review Response.dc.html` / `02-proposals.md`.

## Adopt as written (17)

| ID | Finding | Position | Ex. |
|---|---|---|---|
| P0-1 | Dynamic Type breaks core iOS layouts | Solution B reflow plus the Solution C accessibility composition. The screenshot regression matrix is part of the work, not a follow-up. | 1a |
| P1-1 | Action inventory ahead of understanding | Recommendation-led header. The action stack was never the problem; its position and equal weight were. | 1c |
| P1-2 | Unsupported actions shown as disabled | Hide what this client cannot perform; state a capability mismatch when the item's requested action is load-bearing. **See the open decision in `04-work-split.md` — hiding does not resolve this finding.** | 1d |
| P1-3 | iPhone inherits the sidebar model | Persistent tabs with independent stacks; adaptable tab/sidebar where the deployment target allows it. | 1f |
| P1-4 | Recommendation far from its action | Same shell as P1-1: recommendation, why, confidence, and the accepting action inside the block. | 1c |
| P1-6 | Successful decisions lose their context | Durable confirmation, deliberate advance to the next item, and a route back to what was just decided. | 1j |
| P1-7 | Evidence loading is visually silent | Five explicit states. A claim label with nothing under it is the worst outcome for evidence. | 1i |
| P1-8 | Selection is border colour only | Leading selection bar plus a fill step; survives Differentiate Without Color. | 1e |
| P1-10 | No manual or foreground refresh | Pull to refresh, toolbar refresh, ⌘R, refresh on activation, visible last-updated. | 1k |
| P2-1 | Titles duplicated | On iPhone the navigation title orients and the card opens on the ask. | 1c |
| P2-2 | Normal and open badges add noise | Show urgent and high; always show unusual lifecycle states; drop normal and open inside the Open scope. | 1e |
| P2-3 | Machine IDs compete with meaning | Human project and work-unit names in the scanning layer; identifiers to Details, tooltips, copy menus. | 1e |
| P2-4 | Absolute time less useful than urgency | Lead with waiting / due / blocked duration; exact timestamp one layer down. | 1e |
| P2-8 | Run rows squeeze one line | Stage and hold on two lines, watches in a wrapping chip row. | 1l |
| P2-11 | Menu bar has no route back | Open Freeside and Show Inbox at the top; daemon lifecycle in its own section. | 1k |
| P3-1 | Empty macOS pane is passive | A quiet operational summary is cheap and answers the question the operator opened the app with. | 1g |
| P3-2 | Primary path under-emphasised | One prominent action per card; everything else bordered or in an overflow menu. | 1d |

## Adopt, modified (9)

| ID | Finding | Modified position | Ex. |
|---|---|---|---|
| P0-2 | Semantic colours fail contrast | Split each semantic into `…Text` / `…Wash` / `…Border` and add Increased Contrast cuts. Keep wax and water as the palette; **refuse** the system-red substitution. | 1b |
| P1-5 | Card types too generic | Four compositions now — ready for final review, execution failure, dispute, review yield. System health and blocked are re-orderings of existing fact modules and can wait for the shell to settle. | 1h |
| P1-9 | No native destructive semantics | Destructive role and confirmation on system-owned surfaces only (alerts, confirmations, context menus); wax stays the content colour. | 1d |
| P2-5 | Filters lack counts | Quiet counts on every scope, but the urgent summary renders only when an urgent item exists — a permanent zero is badge inflation with extra steps. | 1e |
| P2-7 | macOS wastes its width | Two columns for the card, and facts/bindings/attachments in a **toggleable inspector** rather than a permanent third column. A column that is empty half the time is worse than a button. | 1g |
| P2-9 | Pairing code entry unoptimised | One-time-code semantics, grouped field, paste, prefilled device name, and the host being paired. **Numeric keypad does not apply** — the daemon's codes are not numeric. | 1l |
| P2-10 | macOS lacks keyboard efficiency | Take the command set, but Return fires only a validated primary action, and Space stays with the system rather than being bound to disclosure. No destructive shortcut. | 1k |
| P2-12 | Help is almost absent | Inline copy for load-bearing concepts, macOS tooltips for the mono state register. No TipKit until the card compositions settle — it would teach a layout we are about to replace. | 1c |
| P3-3 | Could use more SF Symbols | Icons only where the label alone is ambiguous, i.e. where the action changes *where you go* or *what is lost*. Never icon-only; never an icon on the recommended action. | 1d |

## Later (1)

| ID | Finding | Position | Ex. |
|---|---|---|---|
| P2-6 | Search is absent | Right call, wrong moment. A seven-item inbox does not earn an index, and search built before the row recomposition (`1e`) would index the strings we are about to demote. Reserve ⌘F now, build after `1e` ships. Server-wide historical search is new scope — Phase 2. | 1k |

## Four remedies refused

### P0-1 Solution A — cap Dynamic Type inside complex cards
The review itself calls this the wrong product decision, and it is right. Capping trades a layout bug for a permanent exclusion of the users who chose the larger text. Reflow instead (`1a`).

### P0-2 Solution A — replace semantic colours with system colours
This discards the thing the same review names a product advantage in order to inherit four variants that can be authored in a day. Author them (`1b`).

### System red for destructive controls
Wax is Freeside's failure and revocation colour everywhere else; a red Stop button beside a wax "failed" chip teaches two reds. Keep wax in content, and take the native destructive *role* only on system-owned surfaces where the system paints its own red. Fix wax's dusk contrast instead — its dusk value already exists as a legibility lift of one artifact colour, so strengthening it is in policy (`reference/design-system-guide.md`).

### Sticky action footer on macOS
A bar floating over a desktop detail pane is a web habit. The Mac has the width to keep the primary action in view without one: put it in the card's trailing action area (`1g`). Keep the sticky safe-area inset on iPhone only, and only after the recommendation block has scrolled out of view.

## Two things the review does not say, and should

1. **The run timeline is the standard.** Every graphic proposed in `1h` is that page's grammar applied to a smaller surface. If a module cannot state its fact in text beside the graphic, it does not belong in Freeside.
2. **Success must stay quiet.** Several proposals introduce checklists and confirmations; none of them uses green or celebration. Neutral tick, wax for the failing line, accent reserved for attention and the recommended path.
