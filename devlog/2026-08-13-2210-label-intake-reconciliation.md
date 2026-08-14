# Label-Intake Reconciliation: Reserved-Run Adoption and the Work-Item Spec Role

Work unit: #659 (lane:publish, kind:feature, wave 5, tracking #651). Scope:
`daemon/` only (engine, cmd/freesided, store, intake). Builds on the merged
contract chain #720 (occurrence/WIP/subject), #740 (project authority), and
#744/#745 (admission reserves the *elaboration* run). Mandatory note: the
issue-subject elaboration arm is a returned-object trust boundary (it adopts a
persisted reserved run and drives it), and the assembly lands two consequential
non-obvious choices below.

## Decision 1: The work-item doc is a Specification artifact in the spec role,
## not an empty-input invocation

The prior-session design called for the issue-subject arm to submit with
**empty `InputArtifactIDs` at iteration 1**, delivering the coordinates-only
work-item document to the elaborator purely through `run.SpecDigest`
(`SpecificationDigest` in the stage-input snapshot). That is not
implementable without a domain change: `domain.NewAgentInvocation` (and
`AgentInvocation.Validate`) reject an invocation bound to neither inputs nor a
conversation (`ErrUnboundInvocation`, `conversation.go`). The elaboration
dispatch requires the iteration-1 invocation, and `loadElaborationBinding`
re-binds `invocation.InputIDs == request.InputArtifactIDs`, so an empty-input
invocation cannot exist. Changing `internal/domain` is out of #659's scope
(#659 is `lane:publish` feature work, not a contract unit), so the empty-input
arm would have required filing a contract unit.

The in-scope realization that preserves every observable property: the
daemon-authored, **issue-content-free** work-item document is registered as a
daemon-produced `Specification` artifact and bound as the reserved elaboration
run's index-0 source input, with `run.SpecDigest` equal to its digest. This is
exactly the shape owner-ratified GQ1 blesses — "a daemon-authored,
issue-content-free coordinates-only work-item document **in the spec role** is
within GQ1; the elaborator still produces the real spec." Consequences:

- **§5.13 holds structurally, unchanged.** The work-item doc is a pure function
  of the occurrence coordinates (`repo`, `repository_id`, `issue_number`,
  `label`) — never the observed issue title, body, author, or labels. The
  forge observation (`LabelIssue`) carries only `Number/State/HasLabel`, so no
  issue content can reach the doc even from a hostile issue.
- **Engine surgery is minimal and the spec-artifact path is untouched.**
  Because the index-0 input is a real `Specification` whose digest equals
  `run.SpecDigest`, `elaborationRequest.validate` (>= 1 input),
  `authenticateElaborationRoot` (len == 1 source), and `loadElaborationBinding`
  (index-0-is-spec) all pass **unchanged**. The only engine additions are the
  `IssueSubject *domain.IssueSubjectRef` field on `elaborationRequest` (pinned
  in the canonical encode/decode and in `authenticateElaborationRoot` so a
  retargeted request cannot swap the bound subject) and the reserved-run
  adoption path in `SubmitElaborationRun`.
- **The daemon-produced artifact is never publish-eligible** (daemon provenance
  carries no recipe), so the coordinates doc cannot leak as a publishable
  artifact.

Rejected: filing a domain contract unit to permit specification-only
invocations. The spec-role realization is behaviour-identical to the intended
design, GQ1 already names it, and it avoids widening the invocation contract
for a single caller.

## Decision 2: Admission reserves the run; start adopts it

`MintIntakeDeclaration` requires the reserved run to exist (`GetRun`) before the
proposal is admitted (`ResolveProposalSubject` reads the minted declaration).
So admission — not start — creates the bare reserved elaboration run (the
elaboration stage shell, `SpecDigest` = work-item digest, `PolicyDigest` =
resolved-policy digest), its resolved policy, the policy artifact, and the
work-item spec artifact. `SubmitElaborationRun`'s issue-subject arm then
**adopts** that pre-existing run: it verifies the run/policy/artifact shape and
adds only the iteration-1 invocation, dispatch marker, implementation claim,
and run-submitted milestone (converging when they already exist). The
spec-artifact arm still *creates* its run, so the two arms diverge only on
create-vs-adopt; `ownsElaborationRun` returns false until the marker exists, so
a reserved-but-unstarted run is never driven by the elaboration reconciler.

## Finding: the WIP cap counts reserved runs; the gate excludes the occurrence's own

`store.CountActiveProjectRuns` counts every non-final run of a project
(`domain.ConcludeRun(observation).Final == false`), and a bare reserved run
concludes `Pending` (non-final), so it counts. The auto_start WIP gate
therefore counts active runs and **excludes the occurrence's own reserved run**
(always active at the decision point, just admitted): a single occurrence at
`cap == 1` sees zero *other* active runs and starts. Consequences and the
deliberate conservatism:

- Reserved runs for *propose* cards (and refused auto_start occurrences) count
  toward the cap until they conclude, so a project with many pending propose
  cards will refuse further auto_starts. This is **fail-closed and intended**:
  the cap is a safety limit on outstanding autonomous work, and awaiting cards
  are outstanding work. It is not a compute-cost measure (a card awaiting a
  human consumes no compute), so it is conservative, not wrong.
- **Revisit when** a janitor concludes or reaps reserved runs whose proposal
  was declined or superseded: today those runs keep counting until the run
  itself reaches a terminal outcome, which for a never-started run is never.
  Freeing the slot on decline/supersede is a follow-up (not #659 scope).

## Decision 3: Auto_start records a daemon-attributed decision (GQ2)

`RecordProposalDecision` requires a `Command`, and the operator `Submit` path is
gated on an active device. Auto_start is daemon-initiated, so it records its
decision through the ledger (GQ2 convergence with the manual path) by creating a
daemon-attributed start `Command` directly via `tx.PutCommand` — reserved
`DeviceID` `daemon-label-intake`, a `CommandID` derived from the occurrence — then
`RecordProposalDecision(ActionStart)` and resolving the item, all in one write
transaction, before executing `SubmitElaborationRun`. `PutCommand` still re-gates
item openness, digest binding, and the offered-action set; it enforces no device
FK, so a synthetic daemon device is the correct attribution for a decision no
operator made. The device-active gate lives only in `Submit`, which is the
operator surface, not this one.

## Decision 4: One execution trigger for both start paths

The loop executes elaboration for any admitted occurrence whose proposal is
decided `start` and not yet started (dispatch marker absent). This unifies:
auto_start (the loop records the decision, above) and an operator's manual start
on a propose card (`applyStartProposal` records the decision and resolves the
item but does not launch elaboration — nothing else consumes a decided
`run_proposal` yet). So propose mode is functional end-to-end: the operator
starts, the next intake pass launches. `SubmitElaborationRun`'s convergence makes
the trigger crash-safe. `start_with_changes` is **not offered** on a label
proposal (the offered set is narrowed at admission): the issue-subject subject is
fixed to the occurrence's own issue, so revising the subject is not a
label-intake flow, and offering an action the loop's launch trigger does not
consume would strand the occurrence (round-2 finding B below).

The WIP cap gates only the auto_start *decision* (card still open); an operator
who explicitly starts a propose card is a deliberate human decision and is not
WIP-capped. The `subject_input_missing` / `subject_input_stale` refusals are the
auto_start pre-decision availability check (card open, refusable); if a
manually-decided start finds its input gone, `SubmitElaborationRun` fails and the
loop logs it (the card is already resolved, so no refusal can be recorded).

## Verification posture (returned-object trust boundary)

The issue-subject arm adopts a persisted run and drives it, so a refute-first
pass runs before commit: lenses that try to make the adoption trust a foreign
or tampered reserved run, a mismatched policy, or a swapped issue subject. The
occurrence re-gate (#720/#740, already merged) authenticates the admission
binding; this unit adds the engine-side adoption checks and the start-time
`subject_input_missing`/`subject_input_stale` refusals.

## Review findings (Codex, PR #746): three confirmed, all fixed

All three round-1 findings were confirmed against the code and fixed; none were
rejected-by-verification or accepted-by-decision.

- **P1, auto_start decide/launch race (confirmed).** `autoStart` recorded the
  start decision and then launched unconditionally. If an operator declined (or
  a departure superseded) the card between the WIP gate and the decision call,
  `StartRunProposalUnattended` silently no-opped on the now-non-open item and the
  launch still ran, turning an explicit non-start decision into an autonomous
  run. Fix: `StartRunProposalUnattended` now reports whether it recorded the
  start; `autoStart` launches only that. Convergence-launch of an
  already-decided-start card stays owned by `decide`'s decided-start path
  (`StatusResolved` is reachable only via a start, never a decline/supersede).
- **P2, first-page validator on the labeled-open scan (confirmed).** The intake
  label observation rode `fetchConditionalList`'s first-page ETag, so on a
  >100-issue labeled set a later-page departure or arrival could be masked by a
  first-page 304 indefinitely. Unlike best-effort review evidence, the intake
  scan is correctness-critical. Fix: `fetchConditionalList` reports `multiPage`,
  and `getLabelIssues` drops the validator for a multi-page list so the next poll
  re-reads every page unconditionally; the review-evidence callers keep the
  accepted best-effort degradation.
- **P2, unadmitted departures skipped forever (confirmed).** `reconcileDepartures`
  skipped occurrences with no admission, so an occurrence whose admission failed
  durably (e.g. the cross-repo mint refusal) lingered `present` forever and a
  re-label reused its ordinal. Fix: a departed unadmitted occurrence is advanced
  out of `present` with `RecordIntakeObservation` (no proposal to supersede), so
  a re-label allocates a fresh occurrence.

Round-2 findings (all confirmed):

- **P1, label scan not bound to the configured repository id (fixed).** The scan
  fetched by repository *name* and never checked the observed canonical numeric
  id against the configured `RepositoryID`, so a name rebound to a different
  repository could intake the replacement repo's issues while recording
  occurrence, project, and work-unit authority under the old id — the §5.18
  rebinding hole (`forge.go` already states the rebinding is detectable only
  through the observed id). Fix: `ReconcileLabelIssues` resolves the repository's
  canonical id (`getRepositoryID`, an unconditional `GET /repos/{owner}/{repo}`
  each pass) into `LabelIssuesObservation.RepositoryID`, and the loop fails the
  pass closed (`errIntakeRepositoryRebound`) before allocating any occurrence
  when it disagrees with `init.RepositoryID`.
- **P1, `start_with_changes` offered but strands the occurrence (fixed, aligns
  Decision 4).** The shared admission path offered `start_with_changes` on every
  run_proposal; an operator choosing it supersedes the original item and creates a
  *resolved replacement*, but the loop's launch trigger (`proposalDecidedStart`)
  reads only the original item's status, so the occurrence never launched despite
  a recorded start. Fix: `ProposalAdmission.RequestedDecision` lets a caller
  narrow the offered set, and label intake omits `start_with_changes` (subject is
  fixed to the occurrence's issue). This makes the offered set match Decision 4
  rather than reversing it; other run_proposal consumers keep the full set.
- **P2, single-page-to-multi-page validator edge (deferred, #747).** The round-1
  multi-page-drop fix learns `multiPage` only from a 200, so a cached single-page
  set that grows past 100 via an *older* issue can stay hidden behind a page-1
  304. The complete fix drops conditional requests for the intake label scan
  entirely, which reverses this PR's conditional-observation design and is a
  decision in its own right; tracked as #747 rather than rushed here.

Round-3 findings (all confirmed, all fixed):

- **P1, decided start stranded by a departure (fixed).** A start decided (by an
  operator on a propose card) between the present pass and the next pass's
  observation of a label removal / issue close was never launched: the departure
  retired the occurrence, and only present labeled issues reach the launch
  trigger. Fix: `reconcileDepartures` launches a decided-but-unstarted proposal
  (`launchDecidedDeparture`) before `advanceDeparture` retires the occurrence, and
  does not retire until the launch lands. `SubmitElaborationRun` converges, so a
  replay is a no-op. Test: `TestIntakeLaunchesDepartedDecidedStart`.
- **P1, provenance falsified by allowlist injection (fixed).** The loop appended
  the forge host to the resolved `research.allowlist` value while keeping the
  key's original preset/override provenance, so the persisted policy falsely
  attested an egress the source never authorized (defeating the per-key
  provenance audit and silently widening operator policy). Fix: the loop no
  longer rewrites the key; `requireForgeResearchHost` fails admission closed
  (`errIntakeForgeHostNotAllowed`) when the operator's own allowlist omits the
  forge host, so the egress carries authentic provenance from the rein. Test:
  `TestIntakeRequiresForgeHostInAllowlist`.
- **P1, adoption accepted a non-bare reserved run (fixed).** The issue-subject
  adoption gate checked the elaboration stage's identity and count but not that
  it was bare, so a reserved run whose stage carried a pre-existing attempt
  (corruption or tampering between admission and start) was accepted and wrapped
  in ownership markers, and the reconciler would stall on the rogue attempt. Fix:
  at first start (marker absent) the stage must have zero attempts, else the
  adoption fails closed (`ErrImmutableTransition`). Test:
  `TestSubmitIssueSubjectElaborationRunRejectsNonBareStage`.

Round-4 findings (all confirmed, all fixed):

- **P1, departure retire non-atomic with the decided-start launch (fixed; a
  completion of the round-3 D fix, not thrash).** The round-3 fix checked the
  decision and retired in separate transactions, so a start decided in that
  window was retired without launching. Fix: `advanceDeparture` re-reads the
  proposal status and the dispatch marker in the *same* write as the retire, and
  defers (`errIntakeDeferDepartureRetire`, launched next pass) when the proposal
  is resolved but has no marker; the store write lock serializes this against a
  concurrent operator start. Test:
  `TestIntakeDefersDepartureUntilDecidedStartLaunches`.
- **P1, adopted work unit not bound to the minted declaration (fixed; closes the
  adoption-trust class).** The caller's `WorkUnit` flowed unchanged to the
  implementation run at spec approval after only structural validation, so a
  wider `DeclaredPaths`, a retargeted `BoundIssue`, or a nil declaration could
  diverge from the intake declaration the admission minted. Fix: the adopter
  loads the minted declaration for the reserved run and binds the caller's work
  unit to it (`elaborationWorkUnitMatchesDeclaration`), failing closed on any
  divergence. With this, every caller-supplied adoption input is bound to
  authoritative state (source artifact ↔ SpecDigest, policy artifact ↔ resolved
  policy, issue subject pinned, work unit ↔ declaration); the daemon-composed
  publication has no authority to bind to. Test:
  `TestSubmitIssueSubjectElaborationRunBindsWorkUnitToDeclaration`.

Round-5 findings (both confirmed, both fixed):

- **P1, launch trigger trusted a decoded item status (fixed; new class).**
  `proposalDecidedStart` (and the departure re-check) treated a decoded
  `resolved` attention-item status as proof of a start, so a corrupt or tampered
  row could launch a run autonomously with no genuine decision. Fix:
  `store.AuthenticateStartDecision` requires a matching `effect_proposal_decisions`
  row bound to the admitted proposal digest — the independent ledger authority —
  and both the launch trigger and the atomic departure guard now authenticate
  against it. Tests: `TestAuthenticateStartDecision`, and the existing decided-start
  loop tests exercise the real ledger path.
- **P2, initial issue subject not authenticated at adoption (fixed; my round-4
  class-sweep miss).** Round 4's note over-claimed that "issue subject [was]
  bound": `sameIssueSubject` only pins the subject across *later* iterations
  against the root marker, never authenticating the *initial* subject against the
  reservation, so a caller could adopt a run under a foreign issue. Fix: the
  adopter binds `Source.IssueSubject.IssueNumber` to the minted declaration's
  `BoundIssue`; the repository and project remain the store occurrence re-gate's
  authority (#720/#740, which binds them to the admitted proposal). Test:
  `TestSubmitIssueSubjectElaborationRunBindsIssueToDeclaration`.

**Review-tail note (updated after round 5).** The adoption trust boundary was
enumerated across rounds 2–5 (repository id, bare stage, work unit, issue
subject), the #720 pattern. With round 5 every caller-supplied adoption input is
now bound to authoritative state — source ↔ SpecDigest, policy ↔ resolved
policy, work unit ↔ declaration, issue number ↔ declaration, repository/project ↔
the store occurrence re-gate — and the daemon-composed publication has no
authority to bind to. The launch trigger authenticates against the decision
ledger and the departure retire is atomic. Both classes are now closed by
binding, not by one-more-field verification. A recurrence on either class after
this complete sweep is thrash: reframe or escalate rather than fold again.
