# Freeside · Task Page Density, 4 Nov 2026 · Client Handoff

Date: 2026-11-04 · The task page and the decision cards it seats, redrawn so the facts an operator reads stop drawing at the same weight as the facts they would only copy: the decision's inputs above its actions, one reading fold under them, the card's identifiers in the page's Identifiers footer, each finding's route and reason in reach, an open Current Run and Timeline, and the task's prompt, specification, and pull request one click from its head.
Scope: **presentation, both clients, plus one synced-task field.** ◆D's Prompt link needs the daemon to carry the work-item artifact's digest on the synced task, a field in the API schema and its generated client too (#2610, `kind:contract`). Nothing else touches the daemon or the API, and no token, face, or ladder changes.

## Contents

| File | What it is |
|---|---|
| `README.md` | This brief: the ◆ gate, what is not built, the accepted defaults, what was rejected, and the units that build each part |
| [`Task Page Density - 4 Nov 2026.html`](Task%20Page%20Density%20-%204%20Nov%202026.html) | The original proposal. Five paired frames, each the task page or a seated card as built (left) beside the same surface with the forensic layer demoted (right), with ◆1 to ◆4 of its own. Not a target: the counter-proposal keeps parts of it and rejects others (below) |
| [`Task Page Density - Counter-Proposal - 4 Nov 2026.html`](Task%20Page%20Density%20-%20Counter-Proposal%20-%204%20Nov%202026.html) | **The canvas.** The counter-proposal panel, revision 2 (after owner feedback), with its renders inlined. §1 the seated card; §2 finding adjudication; §3 the task page body and documents line; §4 the debugging case; §5 and §6 the original frames scored and the counts; §7 the ◆ decisions, the order of work, and what it did not verify. Its "Now" frames are renders from `main` at `df720455` (`app/scripts/render-surfaces.sh`, Mac, day, large text), per its Method paragraph; the other frames are mocks, and blue pins mark each change |

Both files open directly in a browser and load only Google Fonts. The original's own ◆1 to ◆4 are not this handoff's gate: the owner answered the counter-proposal's ◆A to ◆D instead.

## ◆ Gate

Nothing behind a ◆ lands until the owner marks it decided here. All four decided on 4 Nov, as the counter-proposal recommends. The reasons are in [`devlog/2026-11-04-2032-task-page-density-decisions.md`](../../../devlog/2026-11-04-2032-task-page-density-decisions.md).

- **◆A Identifiers.** A decision card seated on a task page adds its Technical Bindings to the page's one Identifiers footer as a "Decision" group, extending #2117 ([`devlog/2026-10-21-1238-identifiers-footer.md`](../../../devlog/2026-10-21-1238-identifiers-footer.md)) to the last surface it missed. The card eyebrow gains a Copy menu (item ID, evidence digest). A scoped card with no task page keeps one Identifiers fold of its own. **Decided 4 Nov (owner):** yes.
- **◆B Task actions.** The task's More Actions menu moves to the outline row's empty trailing slot ([30 Oct handoff](../handoff-2026-10-30-unified-list/README.md) ◆5) as `Task Actions ▾`, and to the navigation bar's trailing slot on iPhone. **Decided 4 Nov (owner):** yes.
- **◆C Open Timeline.** The task page body splits into an always-open **Current Run** section and an always-open **Timeline**. Only earlier attempts and earlier campaigns fold. The Task fold built from the inspector on 30 Oct ([`devlog/2026-10-30-1800-task-page-inspector-fold.md`](../../../devlog/2026-10-30-1800-task-page-inspector-fold.md)) dissolves, every row moving to the eyebrow, Current Run, the documents line, or Identifiers. This reverses "every task-page fold starts closed" for these two sections only. **Decided 4 Nov (owner):** yes.
- **◆D Documents line.** A quiet line under the status links Prompt, Specification, and Pull Request (Issue in place of Prompt for an issue-sourced task). The Prompt link needs the daemon to carry the work-item artifact's digest on the synced task, a `kind:contract` unit (#2610). **Decided 4 Nov (owner):** yes, and yes to the daemon change.

## Not Built

The owner chose on 4 Nov not to build these, though the canvas draws or mentions them:

- **⌥-click recursive open** (§4), its optional one-shot `View ▸ Expand All Folds` command, and the §2 sentence "⌥-click opens every reason on the card".
- **Stage bands.** Already removed from revision 2.
- **The prompt excerpt variation** (§3). The documents line's Prompt link stands in for it.
- **A cap on long Timeline rails** (§3 pin 7), unless measurement later shows tasks routinely past about 15 rail rows.

## Accepted Defaults

The owner accepted these without change:

- The name stays **Timeline**, not History.
- A finding's reason clamps to two lines, one line past three findings on a card, and stays expanded once opened, remembered per finding.
- Every finding label keeps "(unverified)", so plan §9 needs no revision.
- Whether the body unit splits is decided when that unit is planned.

## Rejected From the Original Proposal

The counter-proposal rejects these with reasons (§5):

- **The per-card Details fold** (frame 1). It re-adds the per-card technical fold #2117 removed.
- **The ⌥⌘D technical-detail mode** (frame 2). A persistent mode on top of fold memory that changes rendering.
- **The plan §9 provenance revision** (frame 5, its ◆4). The long producer phrase, not the word "(unverified)", is what makes the labels hard to read.
- **Frame 5's pick sentence.** It says Accept All Dispositions keeps a held pick; Accept All sends the proposed routes, and only Choose Another Route sends a held pick. The app's current wording is correct.

## Units

Tracker #2613 builds the canvas in these units, as planned on 4 Nov. Each reads its slice of the canvas here; start order lives on the tracker.

- **#2607:** counter-proposal §1, the card foot, and ◆A.
- **#2608:** §2, the finding route and reason, and the accepted clamp defaults.
- **#2609:** the head half of §3 and §1 pins 1 and 2: ◆B, and ◆D's Specification, Pull Request, and Issue links.
- **#2610** (`kind:contract`): ◆D's work-item artifact digest on the synced task.
- **#2611:** the body half of §3, ◆C.
- **#2612:** ◆D's Prompt link.

The counter-proposal's §7 lists what it did not verify, such as whether the client holds per-finding confidence for every producer and how it reads the prompt and specification text.
