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
