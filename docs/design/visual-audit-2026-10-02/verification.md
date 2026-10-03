# Verification Matrix And Review Ledger

This is the acceptance checklist for future implementation, not a report of
tests already run. The audit's successful native builds and export checks
established prototype fidelity and report usability only.

## Evidence Required For Each Decision

| ID | Visual And Information Checks | Interaction Or Regression Checks |
| --- | --- | --- |
| D01 | Match accepted serif summary, original heading hierarchy and right-aligned urgency/time; preserve two-line limit, selected/unselected and empty states | Open/Resolved/All, project filtering, counts, ordering and selection produce the same results |
| D02 | Hold callout follows title and precedes history; task-list geometry remains the current readable layout | Capacity hold versus human-attention hold, no hold, historical hold, stop pending/error and current guidance keep their meanings; Stop remains reachable |
| D03 | Unverified remains visible beside every affected agent claim, including recommendation and question; explanation is discoverable | Mouse, keyboard, touch and VoiceOver can open/dismiss it without activating a decision; no hover-only requirement |
| D04 | Short message unchanged; long plain text, paragraphs and code-like content expand without loss; author/time/attachments remain available | Expansion is reversible and keyed by message, preserves selection/copy and composer draft, survives unrelated rerender, and does not collapse the conversation backlog |
| D05 | Empty, focused, filled, invalid and recovery states are distinguishable; external labels remain visible | Placeholder is not the field value; project/name/source, validation, cancel, keyboard submission and retry/recovery retain current behavior |
| D06 | Question, blocker and complete options read together; long tradeoffs wrap; context/source/run disclosures have clear destinations | Options remain descriptive panels; recommendation attribution, answer controls and submitted answer are unchanged; question without enumerated options still works |
| D07 | Clean and degraded verdicts, all passed names, failed/waived/informational rows, concerns and excerpt warnings are preserved; View PR is prominent only when supported | Opening PR does not resolve; Return to agent, full report, review yield and evidence remain reachable; stale/base-advanced and missing-summary/PR cases remain truthful |
| D08 | Supplied claim leads metadata; missing inline text retains original attachment state; no invented opposing argument | Full claim, digest and bindings reachable; actions have the same command and consequence; missing data stays visibly missing rather than replaced by convincing prose |
| D09 | One card per finding, message/route/source visible, correct detail inside each card; bulk scope clear at count 1 and at multiple findings | Mixed model/daemon producers, missing location, long messages, alternative selection, collapse/reopen and replacement item; exact command action, bound version and route-pair payload preserved |

## Required State And Platform Coverage

Use risk-based fixture selection rather than an uncontrolled cross-product.
Every changed surface needs light and dark reference captures on Mac and
iPhone. The existing screenshot suite's six sizes are `xsmall`, `large`,
`xxxlarge`, `ax1`, `ax3`, and `ax5`; keep that matrix and inspect changed
captures at ordinary and accessibility sizes. Screenshot-rendered phone
layouts do not replace a native iPhone simulator interaction pass.

| Dimension | Required Evidence |
| --- | --- |
| Ordinary native layout | Mac normal and narrow detail column, inspector open/closed; iPhone portrait with its sticky actions and safe areas |
| Long/large content | Long questions and finding messages, many findings, many checks, long messages, large text; scrolling must not clip controls or trap content under the footer |
| Accessibility | Keyboard focus order, visible focus, Escape/Return where applicable, VoiceOver labels/read order, Dynamic Type, Increased Contrast and Differentiate Without Color on affected surfaces |
| Decision restrictions | Current, loading, stale/replaced, offline and unsupported-action states; existing restrictions remain in force and understandable |
| Readiness exceptions | Clean and degraded fixtures, failed required check with waiver, informational/not-run rows, excerpt/concern warning, commit-plan notice and advanced base |
| Trust/content variants | Model-backed and daemon-authenticated findings, missing inline text, attachment loading/unavailable/error, absent location, full source/provenance access |
| Shared-component sentinels | Spec approval, execution failure, publish blocked, blocked, system health, task/effect proposal, diminishing returns/drift, task/run history, pairing, recovery and confirmation sheet |

Sentinels should remain visually and behaviorally unchanged unless a specific
approved decision, such as the Unverified explanation, applies there. Inspect
all changed golden images, including those outside the intended card types.
An unexpected change is a regression to explain and fix, not a baseline to
record automatically.

## Behavioral Proof

For moved or relabeled actions, compare the generated command before and
after using the same bound snapshot: action enum, item/version, answer or
route payload, and existing retry/idempotency path. Use current production
handlers and existing tests to establish the contract; mock behavior alone
is not proof that a new interpretation is safe.

For D09, separately exercise accepting every proposed disposition and
choosing an alternative for a named finding. Bulk acceptance remains the
existing `accept_recommended_route`; changing which cards are open cannot
alter its scope. Alternative selections remain attached to finding IDs even
if the visible order changes. A stale replacement cannot submit a selection
against the wrong item version.

Retain existing capability filtering, destructive confirmations, stale-state
disabling, receipts and recovery behavior. Do not add a new confirmation
merely because a button changed position; do not remove an existing one.

## Visual Baselines And Test Changes

Capture a reproducible current baseline at each unit's fresh base before
editing. Use stable fixtures, time, appearance, viewport, scale, scroll
position and disclosure state. Include expanded states, not only the clean
first viewport. Compare:

1. Current versus implementation, to detect everything the unit changed.
2. Implementation versus the approved reference, to detect design drift.
3. Unchanged sentinels versus their baseline, to detect shared-style effects.

Use `FREESIDE_DUMP_SCREENSHOTS=1` and the repository's existing screenshot
output mechanism for inspectable failures. Re-record through
`FREESIDE_RECORD_SCREENSHOTS=1` only after reviewing each intended visual
change. Never hand-edit digests, weaken comparisons, remove cases, or update
all baselines merely to obtain a passing result.

Existing issue #1698 records load-dependent screenshot mismatches. If a
mismatch occurs, preserve its dump and reproduce on unchanged base under
comparable conditions. Report a demonstrated baseline failure separately;
do not call an unexplained mismatch harmless or use it to skip verification.

Classify assertion changes before editing them:

- **Behavioral invariant:** Preserve the assertion and fix the implementation.
- **Approved presentation rule:** Cite the decision and merged specification
  change, then update the assertion and its fixture deliberately.
- **Numeric budget conflict:** Measure and escalate through #1141. The
  520pt threshold at 560pt/`.large` stays until explicitly reconsidered;
  arbitrary-length text must still scroll and remain readable.

Run `bash scripts/check.sh app` for the standard generation, formatting,
tests, Mac build and iOS build steps. Run `bash scripts/check.sh docs` when
the presentation specification changes. Add targeted tests only for behavior
or information-preservation risks introduced by the change; avoid tests that
merely duplicate CSS-like spacing choices already covered by screenshots.

## Owner Walkthrough Before Merge

Supply a runnable, separately named native mock preview and a short review
sheet. For each decision, offer **Matches / Adjust / Regression** and a note.
Record the reviewed head/base, build, platform, theme, fixture and screenshots.
Keep these results on the issue/PR, with no duplicate live status in this plan.

Ask the owner to complete the relevant tasks:

1. Find an Inbox item and explain what needs attention; filter and switch
   projects without losing the familiar controls.
2. Read a question, compare its options and find how to answer.
3. Identify readiness, spot a waiver or concern and open the PR; then find
   supporting evidence and the original summary.
4. Read a dispute and identify what the available claim actually says,
   without assuming missing arguments or new approval semantics.
5. Explain each finding's proposed outcome and exactly what bulk acceptance
   will do; inspect and choose a permitted alternative in the mock build.
6. Find a task's hold reason, expand/copy a long message and enter a new task
   into a genuinely empty form.

Owner acceptance requires the intended reading path to feel clearer while
the preserved information and actions remain easy to reach. A passing test
suite cannot substitute for this judgment. A specific Adjust or Regression
blocks the affected unit until fixed or explicitly accepted as a deviation.

## PR Evidence Ledger

Each implementation PR includes this compact mapping, filled with links:

| Decision | Current / Implemented / Approved Images | Preservation And Interaction Evidence | Owner Result | Open Deviation |
| --- | --- | --- | --- | --- |
| Dxx | Matched captures and approved reference | Test names plus native walkthrough result | Pending, Matches, Adjust or Regression | Issue or explicit accepted decision |

Before handoff, every applicable decision has evidence; no command or trust
regression remains; standard required checks are green; automated review is
handled; the full diff is self-reviewed; and the merge-result audit is current
for the base SHA. Mark anything not exercised as unverified. Do not describe
the implementation as fully accepted while the owner walkthrough is pending.
