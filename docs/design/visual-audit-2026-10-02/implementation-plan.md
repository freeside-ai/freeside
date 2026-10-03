# Implementation Plan

Deliver the accepted treatments in three client work units after a separate
specification prerequisite. Keep Mac and iPhone together because they share
FreesideCore. Preserve each unit's commit boundary so an isolated visual
regression can be reverted without undoing unrelated improvements.

## Prerequisite: Reconcile The Presentation Contract

Scope: `docs/plan.md` §9, corresponding comprehension references, and one
decision note. This is a material document change and requires its own
reviewed PR under the existing contract-coordination rules. The present
handoff does not modify those rules or the active specification.

| Current Requirement | Approved Delta To Specify | Invariant To Retain |
| --- | --- | --- |
| §9 final-review table places the PR link last | Put supported View PR after verdict and concise summary, ahead of secondary evidence/history and technical bindings | Navigation does not resolve the item; capability restrictions still apply |
| §9 recommendation layer leads with route and reason; finding table leads with binding facts in a separate register | On finding cards, show each exact message, route and source label; put its rationale and binding coordinates in its own disclosure, followed by explicit batch approval scope | Recommendation provenance is revalidated, source classes stay distinct, and every rationale/fact remains reachable |
| §9 summaries are labeled with producer invocation ID | Permit the invocation ID and original rendering in accessible source details while keeping the producer/Unverified label next to the prose | Identity, provenance validation, uncertainty, dissent and links to evidence survive |
| §9 dispute table requires both positions side by side | Document the existing-data fallback used by D08 when the snapshot supplies only one claim; show both positions when actually available | Never imply absent arguments exist or manufacture the semantics of Approve |
| Global layering and composition tests assume facts precede actions | Distinguish verdicts, exceptions and decision restrictions from routine technical coordinates for D06–D09 | No stale, degraded, waived, capability, consequence or commit-plan warning disappears into a generic details disclosure |

Write the changes narrowly for these card types. Do not weaken the producer
trust rules, move all evidence behind an opaque link, or change the daemon
contract to make the UI easier to render. Preserve evidence-before-long-form
text where the existing rule applies; the leading claim/summary is concise,
with its original material available below.

Review #1141 against D09 and its exact 520pt/560pt constraint. The approved
design answers the message-visibility preference: show the finding message.
It does not establish a new numeric budget. Retain the budget unless the
owner explicitly approves a measured alternative. Carry the realistic-text
fixture requirement into the finding work unit.

**Exit:** The presentation specification and approved design agree, existing
layout assertions are classified as preserved invariants or intentional
presentation updates, and any unresolved conflict has a specific owner
decision. Code that depends on a new rule cannot start before this PR merges.

## Unit A: Inbox, Task Hold, Messages And Empty Forms

Decisions D01, D02, D04 and D05. Scope: the named views below, focused tests,
fixtures, screenshot baselines and the affected `app/SURFACES.md` rows.

| Surface | Current Implementation Entry Point | Implementation Boundary |
| --- | --- | --- |
| Inbox | `InboxView.swift`: scope controls and `InboxRowView`; `FreesideRootView.swift`: Inbox tab count | Use the existing serif face for summaries, keep right-aligned urgency/time and current filters/order; reconcile the tab/header count on each platform without changing store logic |
| Task hold | `TaskTimelineView.swift`: task facts/header; `TasksListView.swift`: row composition | Add the approved detail callout using current hold data; keep list geometry, progress and existing state/guidance derivation |
| Messages | `ConversationView.swift`: message-body rendering | Local expansion keyed by message identity; preserve body, attachments and input state; choose the expansion threshold from the native reference and validate real long content |
| New task | `NewTaskSheet.swift`: project/name fields and source editor | Add external labels and a placeholder that is never entered text; preserve both interactive and screenshot rendering paths |

**Acceptance:** D01/D02/D04/D05 pass the corresponding verification rows;
store-level sorting/filtering and submission behavior remain unchanged.
Run the existing InboxStore, TasksListView and HoldPresentation tests; add
focused message/form interaction coverage for the newly introduced states.
Capture untouched task-list and run-timeline sentinels as well as the changes.

## Unit B: Questions, Final Review And Disputes

Decisions D03, D06, D07 and D08. Scope: `DesignLanguage.swift` only for the
small explanation affordance or reusable presentation needed by these views;
`DecisionCardComposition.swift`, `DecisionDetailView.swift`, focused tests,
fixtures, baselines and `app/SURFACES.md`.

- Keep `KeywordLabel` as the original heading style. Make the Unverified
  explanation discoverable on both platforms without repeating its sentence
  beside every claim. Prefer explicit source semantics over matching the
  word “unverified” in an arbitrary title string.
- Group question options with their full tradeoffs. Leave answer collection
  and option semantics unchanged. Move only the recorded generic context and
  technical source details to their named destinations.
- Adjust the ready-card composition and the existing `reviewingAction`
  presentation to put supported View PR after the verdict/summary. Preserve
  `DecisionActionRanking`, served-action filtering, stale-action disabling,
  and all existing command handlers.
- Keep passed checks compact; lead with exceptions. Move review yield and
  routine coordinates below the decision. Explicitly split visible notices
  from folded facts instead of folding the entire existing facts section.
- Render the supplied dispute claim before routine metadata. Keep the
  current attachment renderer as the fallback when no inline text exists.
  Retain attachment loading/error states and current action meanings.

**Acceptance:** D03/D06/D07/D08 pass the verification matrix, including
degraded readiness, no supported View PR, missing inline claim text, a stale
snapshot, and a long question. Update only composition assertions that the
merged prerequisite deliberately changes. Existing action, recommendation,
summary-excerpt and capability tests must still pass.

## Unit C: Findings And Explicit Approval Scope

Decision D09. Reuse the subject of #1033 rather than create a competing
finding-card implementation. Coordinate #1141's budget/fixture work within
the same scoped change or through an explicit dependency before pickup.
Existing issue contracts require an authorized planning refresh; this
handoff does not silently rewrite them.

Scope: `DecisionDetailView.swift`, the relevant composition and action-label
presentation, finding fixtures, focused tests, baselines and `app/SURFACES.md`.

- Put each finding's message, route and producer label together in one
  container keyed by `finding_id`. Render daemon-authenticated finding text
  distinctly from model-derived rationale; removing border repetition must
  not erase the source register.
- Move that finding's full detail and alternative-route picker into its own
  Reason and alternatives disclosure. Remove the duplicate top-level detail
  rendering only after every field and interaction has a tested destination.
- Label the existing `accept_recommended_route` action “Accept all
  dispositions” in its batch context and explain its scope using the actual
  bound proposal count. This changes wording, not the action or payload.
- Preserve `choose_alternative_route` and its selection by finding ID.
  Expanding, collapsing or reordering cards must not transfer selections
  between findings or change bulk acceptance to a partial selection.
- Keep unsupported actions omitted, stale decisions disabled, confirmation
  and command submission unchanged. Preserve the phone action footer.

**Acceptance:** D09 passes the verification matrix with one and multiple
findings, mixed producers/routes, missing location, long messages, alternate
selection, and stale replacement. Prove bulk acceptance still applies to
the complete bound item and alternatives still submit the intended
`finding_id`/route pairs. No new API shape or per-finding approval is allowed.

## Source And Test Map

Paths are relative to `app/`. These symbols were checked at the planning
base; resolve them again at the implementation base instead of trusting old
line numbers.

| Concern | Existing Evidence To Extend Or Preserve |
| --- | --- |
| Inbox order/filter/counts | `Tests/FreesideCoreTests/InboxStoreTests.swift`; `Sources/FreesideCore/InboxView.swift` |
| Task state and holds | `Tests/FreesideCoreTests/HoldPresentationTests.swift`, `TasksListViewTests.swift` |
| Module order and source register | `Tests/FreesideCoreTests/DecisionCardCompositionTests.swift`; `DecisionCardComposition.forType` |
| Recommendations and capability-ranked actions | `Tests/FreesideCoreTests/DecisionRecommendationTests.swift`; `DecisionActionRanking` and `DecisionDetailView.actionRanking` |
| Summary and full report | `Tests/FreesideCoreTests/DecisionSummaryPresentationTests.swift` |
| Finding action contract | `Tests/FreesideAPITests/MockServerTests.swift`, `MockContractValidationTests.swift`, `ActionOutcomeTests.swift`; inspect production command handling before claiming semantic equivalence |
| First-viewport budget | `Tests/FreesideCoreTests/DecisionLayoutBudgetTests.swift`: 560pt card, `.large`, 520pt action-region offset |
| Native regression captures | `Tests/FreesideCoreTests/ScreenshotRegressionTests.swift`, `Resources/ScreenshotDigests.json`; `Sources/FreesideAPI/AttentionFixtures.swift` |

The earlier [task-refinement handoff](../handoff-2026-09-18-task-refinement/README.md)
remains the baseline for keyword headings, state chips, navigation links,
chronology and native controls. D02 changes hold prominence only; it does not
replace those recipes. Current `app/SURFACES.md` records their implemented
exceptions and coverage. Re-read it at pickup so an older illustration does
not undo a subsequent correction.

## Sequencing And Coordination

Use the sequence **specification prerequisite → A → B → C** by default.
The presentation dependency on the prerequisite is hard for B and C. A can
be independent of that rule change, but sequential delivery keeps visual
review and shared baseline updates manageable. This is a proposed sequence,
not a declaration that incidental shared paths always require serialization.

Before issuing or activating the units, the coordinator must:

1. Resolve current wave, claims, open PRs and contract serialization. At the
   planning observation, #1613 was the active Wave 8 tracker; this handoff
   neither adds work to it nor changes its start order.
2. Use [#1724](https://github.com/freeside-ai/freeside/issues/1724) to create the missing work-unit contracts
   and refresh #1033/#1141 through the project's authorized planning route.
   Assign typed dependencies, exact paths and the acceptance IDs above.
   Resolve whether #1141 is incorporated or remains a distinct unit before
   claiming either overlapping scope.
3. Link this handoff and the owner-decision note from each contract. Do not
   leave the implementation instructions only in a chat or a local report.
4. Compare fresh main against the prototype base. Preserve all newer
   production behavior, especially readiness invalidation, drift verdicts,
   typed blocked/system-health facts, capability filtering and confirmation
   sheets. Do not import audit toggles, UserDefaults fixture injection, mock
   launch settings, unrelated run-outcome labels, or rejected compact rows.

## Delivery And Rollback

For each unit, capture the current native baseline before editing. Commit
only the declared change, test it, and produce current/implemented/reference
comparisons with the verification ledger. Update only affected SURFACES rows.
Run the repository's standard app checks and refresh integration evidence
against the current default branch before handoff.

Keep each PR open for human merge. Owner review uses a separately named mock
build, so it cannot submit production decisions. Document the launch recipe
and fixture identities in that PR. New uncertainty about command behavior or
meaning blocks handoff even if the screenshots look right.

If an accepted property is lost, fix the unit or revert its isolated change;
do not compensate with an unrelated redesign. No permanent feature flag is
required for this presentation pass. Any rollback of a material specification
change must be reviewed together with the behavior it governs.
