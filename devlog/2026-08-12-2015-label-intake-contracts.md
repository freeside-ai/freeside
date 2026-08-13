# Durable Label-Intake Occurrence and WIP Contracts

Work unit: #720 (wave-5 contract chain head, tracking #651; blocks #659
and #732). Mandatory note: `kind:contract` change and a
returned-object trust boundary (a decoded occurrence trusting a stored
admission binding). Scope: `daemon/` (`internal/domain`, `migrations`,
`internal/store`, engine seams). `api/` stays out of scope (GQ3).

## Gating Decisions (Owner-Confirmed)

Four gates were resolved with the owner before implementation; the first
three are the plan's stated questions, the fourth surfaced during
discovery.

- **GQ1 — elaboration source: typed union.** A label occurrence has no
  spec-source artifact and no operator-authored publication. The
  elaboration intake widens to a typed union
  (`ElaborationSource` = `spec_artifact` | `issue_subject`) so an
  occurrence-bound issue subject is nameable with no placeholder
  specification artifact. The issue-subject arm carries repository and
  issue coordinates only; issue text enters elaboration as
  elaborator-fetched research, never authority.
- **GQ2 — under-cap auto_start: existing decision ledger.** A
  daemon-attributed start records through `RecordProposalDecision`, so
  `propose` and `auto_start` converge on one downstream decision shape
  for #659.
- **GQ3 — no `api/` exposure.** Occurrence records, refusals, and
  supersession reasons stay daemon-internal (recorded on the occurrence
  row), so no sync-carried shape changes and `api/` stays out of scope.
  This is why supersession records its reason on the occurrence row and
  supersedes the proposal's item through the *existing* item-status
  mechanism (no new item field), and why the occurrence table carries no
  `entity_version`/`as_of_revision`.
- **GQ1 depth — contract surface, not full engine execution.** Discovery
  found the issue-subject arm lacks not only a spec artifact but the
  operator-authored `ProductionPublication` (title/body/commit-author)
  that `SubmitElaborationRun` requires. The issue acceptance says the
  binding "names the exact elaboration/start input #659 *may invoke*."
  So this unit lands the typed-union contract type, the occurrence
  subject/start binding, and the fail-closed re-gate; #659 wires the
  reconciliation loop that assembles and submits label-initiated
  elaboration (including deferred publication composition). This keeps
  the change off the 1915-line replay-critical elaboration engine.

## Contract Shape

- `IntakeOccurrence` (domain): the repository identity pair, issue,
  initiator label, a 1-based ordinal, observed state, and — once
  admission runs — the proposal admitted under the occurrence's *derived*
  upstream-event key plus the daemon-selected subject binding. The
  decline latch is derived, not stored: a present occurrence admits
  nothing after its proposal is decided; only a recorded absence (or a
  closed occurrence then a reopen) allocates ordinal n+1.
  `CanTransitionTo` holds the per-occurrence state machine (absent and
  closed never return to present).
- `ElaborationSource` (domain): the GQ1 union, referenced by the subject
  binding and threaded into `ElaborationRunSpec` (see Engine Contract
  Surface). Domain-owned because the engine imports domain, not the
  reverse.
- Migration `0043` + store accessors: a daemon-internal current-state
  row keyed by `(repository_id, issue_number, label, ordinal)`, upserted
  under the store's immediate write lock so the ordinal latch and its
  mutations serialize as one decision.

## Returned-Object Trust Boundary (Refute-First Pass)

The occurrence trusts fields of a decoded row and of the admission
binding it carries. Two layers re-gate, both failing closed:

- **Domain `Validate`** (deserialization backstop): the recorded
  admission key must *derive* from the occurrence's own coordinates (a
  digest of the typed key, so a tampered ordinal/label cannot smuggle a
  foreign key), an `issue_subject` source must name the occurrence's own
  repository and issue, the work-unit id must derive from the bound run,
  and a supersession exists only alongside an admission on a
  no-longer-present occurrence.
- **Store reconstruction** (`ErrIntakeAdmissionInconsistent`): foreign
  keys stop a dangling reference, but a tampered row could still point
  the instance id at a *real-but-foreign* proposal or the work-unit id at
  a foreign declaration. The read path re-resolves both parents and
  rejects a binding whose admission key, digest, or subject coordinates
  disagree with the live parent.

Findings this pass **confirmed and fixed before commit**: (1) the initial
replay-idempotency compare used `struct ==`, which tests
`*IssueSubjectRef` pointer identity and would have treated an equal-value
admission replay as a conflict — replaced with a canonical-JSON value
compare; (2) the foreign-proposal boundary test first tripped the
`admission_key` foreign key rather than the digest re-gate — the test
now allocates a real proposal under the occurrence's own key so the FK
passes and the re-gate is the thing under test. Findings **rejected by
verification** (not defects): the derived upstream-event id is injective
across distinct occurrence tuples even when a label contains a slash,
because the leading `repository_id`/`issue` and trailing `ordinal`
segments are integers and the string is only ever hashed, never parsed.

### Review Hardening (Codex Rounds 1–8, All Confirmed and Fixed)

Automated review drove the write/reconstruction boundary from a partial
re-gate to a complete one; every finding was a real trust-boundary or
invariant gap, folded into its owning commit with a regression test.
Round 9 then showed the accept-and-verify boundary itself was misplaced:
the minted-subject restructure below (Restructure to a Minted Subject)
supersedes the field-by-field `regateIntakeAdmissionParents` these rounds
built, so the specifics here are the history that motivated it, not the
current code.

- **Subject binding is authenticated against the proposal, not trusted.**
  `regateIntakeAdmissionParents` runs on both the write
  (`BindIntakeAdmission`) and the read, and pins every subject field to
  the authoritative proposal instance (admitted under the occurrence's own
  key): the work unit must equal the proposal's subject handle, project
  and run the handle-resolved declaration, the resolved-policy digest the
  run's current policy, and the policy artifact a real policy artifact of
  the recorded digest. The write re-gate (round 2) closed a hole where a
  real-but-foreign binding committed and left the row durably unreadable;
  the field-by-field authentication (round 3) closed the residual
  work-unit/handle, policy-digest, and policy-artifact forgeries. Round 4
  pinned the policy digest to the resolved policy; round 8 added the
  occurrence↔declaration issue check: the proposal's work-unit
  declaration must be `BoundIssue`-bound to the occurrence's own issue,
  else a proposal declared for issue A could be admitted to an occurrence
  about issue B. This last check is design-independent (a derived binding
  would still need it), so it is a genuine cross-check the field sweep
  had missed, not merely an accept-and-verify artifact.
- **A label admission's subject must be the `issue_subject` arm** (round
  1); a `spec_artifact` subject would smuggle an arbitrary artifact as
  authority.
- **Lifecycle freshness guards** (rounds 1–2): a refusal requires a bound,
  still-open, present proposal; a bare observation cannot leave `present`
  with an open admitted proposal (that departure must supersede it, else a
  reappearance duplicates the live proposal); supersession records its
  fact only when an open card is actually withdrawn. The proposal item is
  resolved through `ProposalForItem` (round 3) and pinned to the admitted
  proposal digest (round 6), never a raw item read, so neither a
  mis-pointed binding nor a same-instance revision can inspect or withdraw
  a card whose content the occurrence never admitted.
- **Policy artifact pinned to the resolved policy** (round 4): the policy
  artifact is the resolved policy's content, so its digest, the recorded
  `PolicyArtifactDigest`, and the run's resolved-policy digest must all
  agree — the invariant the elaboration path already enforces
  (`policyArtifact.Digest == resolvedPolicy.Digest`). Domain `Validate`
  requires the two recorded digests equal; the store re-gate ties both to
  the run's live policy digest. This closes the policy-artifact question:
  the digest is effectively derived from the resolved policy, not a
  free field.
- **Supersession reason must match its state** (round 4): label removal
  pairs only with `absent`, issue closure only with `closed`; a
  contradictory pair is refused before the item is touched, so audit and
  reconciliation always read a consistent reason/state. Enforced at the
  moment of supersession (the store method), not in domain `Validate`,
  because a later `absent → closed` observation legitimately leaves a
  `closed` occurrence carrying an earlier `label_removed` supersession.
- **Current-state timestamp tracks the state** (round 7, a correctness
  refinement, not a trust-boundary gap): a state transition restamps
  `RecordedAt` with the transition instant, so the current-state record
  reports when the occurrence became present/absent/closed, not when it
  was allocated (sub-facts keep their own timestamps).
- **The ordinal latch re-gates the decoded state against the item** (round
  5): `AllocateNextIntakeOccurrence` releases the next ordinal for a
  non-present occurrence only if its admitted proposal actually ended
  (withdrawn or decided). The round-2 observation guard protects the write
  path, but a directly-tampered row could decode as `absent`/`closed` with
  an open item and the latch would allocate a duplicate live proposal.
  Tested faithfully by writing the tampered shape through
  `putIntakeOccurrence` (a store-internal test) and asserting the latch
  fails closed, since no write path can produce that shape.

### Restructure to a Minted Subject (Round 9 — Derive, Not Verify)

Round 9 (the re-gate pins the declaration's project/run and issue but
*cannot* pin its repository — `WorkUnitDeclaration` carries only
`ProjectID`, and `RepositoryID` lives on `ProjectImage` keyed by content
digest, with no `ProjectID → RepositoryID` accessor) is the fifth
consecutive same-class subject-authentication finding: write-time re-gate
(round 2), work-unit-vs-handle (round 3), policy digest/artifact (round 4),
declaration `BoundIssue` (round 8), declaration repository (round 9). Per
the review-tail heuristic — three-plus same-class rounds on a *design*
means the trust-boundary responsibility is misplaced — five such rounds
means one more field check is the wrong move. This **falsifies the
earlier "verify-complete and derive are security-equivalent, so derive is
a possible follow-up" call** recorded above: an accept-and-verify boundary
whose input has this many independent identity dimensions cannot be
enumerated to completeness by inspection, and round 9's dimension is not
even checkable (no accessor). The derive refactor is therefore **taken
here**, not deferred.

The restructure (plan §5.12 item 3, which always specified this):
admission **mints** the subject rather than accepting it.

- `MintIntakeDeclaration` mints and persists the implementation-run
  identity's `WorkUnitDeclaration` bound to the occurrence's own issue,
  as the admission transaction's first step (before
  `AllocateProposalInstance`, whose subject gate then resolves it). This
  is what plan item 3 meant by "admission mints the implementation-run
  identity and persists its `WorkUnitDeclaration` with `BoundIssue` set to
  the occurrence's issue in the same transaction."
- `BindIntakeAdmission` takes **no subject**: it derives the whole binding
  through `deriveIntakeAdmission`, the single derivation authority the read
  re-gate also uses. The read boundary re-derives the authoritative binding
  and requires the stored one to byte-equal it — one whole-struct compare,
  not an enumerable field list, so no dimension can be silently missed.
- The caller names only the admitted proposal instance and the one
  elaboration input with no accessor to derive it, the policy artifact,
  which is authenticated for existence, type, and a digest equal to the
  run's resolved policy (the round-4 invariant, now the only surviving
  named-and-authenticated field).

**What the restructure closes, and what it does not (round 10).** The
proposal admission key `label-intake/<repository_id>/<issue>/<label>/<ordinal>`
is authoritative for the occurrence's *own* repository and issue:
`authenticatedIntakeProposal` refuses a proposal whose admission key is not
the occurrence's, and the minted declaration's `BoundIssue` equals the
occurrence's issue, so the issue dimension and the caller-supplied-subject
surface are closed by construction. **But the restructure does not tie the
implementation run's *project* to the occurrence's repository**, and Codex
round 10 (P1) correctly re-surfaced this: `MintIntakeDeclaration` accepts a
`runID`, copies its `ProjectID`, and cannot check that the project belongs
to the occurrence's repository, because **no project→repository authority
exists in the model** — `Run` carries only `ProjectID`, `ProjectImage`
carries `RepositoryID` but no `ProjectID`, and there is no `Project`
entity or accessor joining the two. So a run for repository A's project,
supplied for an occurrence in repository B sharing an issue number, binds.
This is a **pre-existing gap the restructure surfaced, not introduced** (the
old re-gate could not check it either, round 9), and it is **unclosable at
#720's layer**. My round-9 reply's claim that the repository dimension
"closes structurally" was therefore over-stated for the *project↔repository*
tie; it holds only for the occurrence/issue tie.

**Read-re-gate over-reach cluster — owner-resolved.** Rounds 9–12 converged on
one root: the reconstruction re-gate authenticated more than it should. The
owner reviewed the escalation and ruled:

- **P1 (round 9/10) — project↔repository: option (a), with the authority
  scheduled ahead of #659.** #720 lands with a documented caller trust
  assumption — the caller mints the run under the occurrence's own repository's
  project, which the store cannot verify because no `project→repository`
  authority exists (`Run` has only `ProjectID`; `ProjectImage` has
  `RepositoryID` but no `ProjectID`; no `Project` entity). The durable
  authority is filed as **#740** (`kind:contract`) and inserted into the wave-5
  contract chain (#651) after #720 and **ahead of #659**, so the tie is
  store-enforced before #659 mints the run. `MintIntakeDeclaration` documents
  the assumption and names #740.
- **Round 11 #1 — input availability vs integrity: fixed as ratified.**
  `deriveIntakeAdmission` now authenticates the stored binding against
  **durable** parents only (the proposal, its minted declaration, the run's
  resolved policy — all write-once/immutable) and no longer re-resolves the
  *current* policy artifact on read, so an occurrence whose bound input later
  becomes unavailable stays readable and #659 can record a
  `subject_input_missing/stale` refusal. The binding's digests are still pinned
  to the durable resolved policy, so a tampered digest is caught; the artifact's
  admission-time presence is checked on the *write* path
  (`authenticateIntakePolicyArtifact`), not the read re-gate.
  `TestIntakeReGateToleratesUnavailablePolicyArtifact` covers it.
- **Round 12 P1 — item semantic type: fixed.** `authenticatedProposalItem`
  resolved the card through `ProposalForItem` (instance + digest) but not its
  `Type` / `Subject.Type`, so a tampered `attention_items` row of another type
  could be withdrawn as the admitted card. It now requires
  `AttentionRunProposal` over a `SubjectProposalBatch` and fails closed
  otherwise; `TestIntakeReGateRejectsTamperedItemType` writes the tampered
  shape.
- **Round 13 P1 — policy artifact id authenticity: fixed (a consequence of the
  round-11 #1 fix).** Dropping the on-read artifact resolution left the stored
  `PolicyArtifactID` fed back into the expected binding tautologically, so a
  body-only tamper of that one field survived reconstruction and would be
  misread later as a `subject_input_missing/stale` unavailability rather than a
  corrupt row. Unlike the work unit, admission key, and proposal instance, the
  policy artifact id has **no durable parent** the read can re-derive it from —
  it may legitimately be gone (the refusal enums encode exactly that:
  "policy artifact gone / superseded"), so resolving it on read is not an
  option. Fixed with the same mechanism the other three fields use: an extracted
  `policy_artifact_id` **column** on `intake_occurrences` (a tombstone, no
  foreign key so the artifact can still vanish) that `regateIntakeAdmissionColumns`
  cross-checks against the body. A body-only tamper now fails reconstruction
  (`errRowInconsistent`) while legitimate later unavailability stays readable;
  `TestIntakeReGateRejectsTamperedPolicyArtifactID` covers it. Migration 0043 (new
  in this PR) carries the column, so no new migration lands.
- **Round 14 (both fixed) — the last two decoded occurrence facts, closed as a
  class by owner direction.** Round 14 surfaced the same "authenticate a decoded
  occurrence fact against an independent authority" shape for the two remaining
  recorded facts, so rather than fold them one-by-one the owner directed closing
  the whole class in one pass (option a).
  - **#2 (refusal):** the decoded `Refusal` was not re-gated at all, so a
    body-only tamper could fabricate or swap a `wip_cap_exhausted` /
    `mode_not_authorized` / input-unavailability result (an open proposal cannot
    disprove it — legitimate refusals leave the item open) and suppress or
    misreport intake. Fixed with an extracted `refusal_reason` column
    (`regateIntakeRefusalColumn`), the same tombstone pattern as
    `policy_artifact_id`; `TestIntakeReGateRejectsFabricatedRefusal`.
  - **#1 (supersession vs decision ledger):** the round-11 #2 status check was
    insufficient because `start_with_changes` (`signet/proposal.go`) also
    supersedes the original item while recording a real start, so a tampered
    occurrence could claim an intake withdrawal on a decided card. Fixed by
    re-gating against the **decision ledger**: an intake withdrawal records no
    `effect_proposal_decisions` row (a decided card is never open for intake to
    withdraw), so a recorded decision now fails a `Supersession` fact
    (`proposalInstanceDecided`); `TestIntakeReGateRejectsSupersessionOfDecidedProposal`.
    The supersession *reason* is authenticated two ways: the direction decidable
    from state (round 15) — `issue_closed` is stamped only at terminal `closed`,
    so `issue_closed` on a non-closed occurrence is rejected
    (`TestIntakeReGateRejectsImpossibleSupersessionReason`) — and, for the
    ambiguous `closed` case a state check cannot decide (`label_removed` on
    `closed` is legal via `absent → closed`), an extracted `supersession_reason`
    column mirroring the refusal pattern (round 16 #1,
    `TestIntakeReGateRejectsTamperedSupersessionReason`).

## Scope Boundary of the Reconstruction Re-Gate (Round 16)

Round 16 also flagged the occurrence *timestamps* (`RecordedAt`,
`Refusal.RecordedAt`, `Supersession.RecordedAt`) as unauthenticated. This is
**declined**, and the decline draws the boundary the whole rounds-5→16
enumeration was circling:

- **What the re-gate authenticates:** every field that carries privilege or a
  cross-entity invariant, against a genuinely **independent** authority — the
  admission subject against the proposal, its minted declaration, and the
  resolved policy (cross-table); supersession-happened against the item status
  and the decision ledger (cross-table); the reason/id facts, which have no
  cross-parent authority, against their own extracted columns.
- **What it does not:** column-mirror daemon-authored **cosmetic** metadata. A
  timestamp's only failure mode is misreported audit history (no behaviour keys
  off it; the one durable copy, `RecordedAt → DeclaredAt` in
  `MintIntakeDeclaration`, is itself cosmetic on a write-once row), and a
  same-row column would authenticate only a body-only tamper — against the
  full-row tamper the threat model implies, the attacker forges the column too.
  So a timestamp column buys diminishing, largely-artificial protection over an
  unbounded field list. The AGENTS.md returned-object rule is about decoded
  *trust bits* (privilege / invariant-bearing); those are authenticated, and
  cosmetic timestamps are not trust bits in that sense.

**Convergence.** Sixteen rounds ran on this file's reconstruction boundary,
several (11 #1 → 13, and the fact-authentication chain 5 → 11#2 → 12 → 13 →
14 → 15 → 16) chaining as consequences of each fix. The tail was not thrash
(each finding was a distinct real gap), but it did signal two over-scopings and
one scope *boundary*: the read re-gate re-deriving against *current* parents
(owner-narrowed to durable-parent integrity plus the #740 authority); every
decoded occurrence *fact* needing an independent authority (owner-directed to
close as a class — extracted columns for the id/reason facts, the item status
and decision ledger for the supersession, and both reason directions); and the
line past which further column-mirroring is cosmetic (the timestamp decline
above). All privilege- and invariant-bearing decoded fields across rounds 1–16
are authenticated against an independent authority; the enumeration is closed by
that boundary rather than one-ahead of the
reviewer.

**Round 10 P2 (replay convergence) — fixed.** `MintIntakeDeclaration`
originally stamped `DeclaredAt` from a caller `now`, so a crash-recovery
replay on a later clock reconstructed a byte-different declaration and the
write-once store rejected it (`ErrImmutableConflict`), breaking the
admission sequence's convergence. Fixed by deriving `DeclaredAt` from the
occurrence's own stable `RecordedAt` and dropping the clock parameter;
`TestIntakeDeclarationReplayConverges` covers it.

**Round 11 #2 (false supersession) — fixed.** A stored `Supersession` is a
decoded trust bit the write path stamps only on a genuine withdrawal, but the
read re-gate did not re-check it, so a tampered absent/closed row could assert
a withdrawal its still-open item never took (the round-5 pattern on a
different fact). `verifyIntakeAdmission` now re-gates a present supersession
against the authenticated proposal item and fails closed unless the item is
actually `superseded`; `TestIntakeReGateRejectsFalseSupersession` writes the
tampered shape and asserts it.

**GQ1-depth is unchanged.** This is plan-item-3 conformance, not a
revision of the elaboration-engine boundary. #659 still owns elaboration
assembly and submission (the reconciliation loop, publication composition,
the `SubmitElaborationRun` execution of the `issue_subject` arm); #720
owns only the subject-binding construction and authentication, which it
already owned. Minting the declaration is the *construction*, and the
declaration store, `AllocateProposalInstance`'s subject gate, and
`ResolveProposalSubject` are all pre-existing, so nothing new lands on the
replay-critical elaboration engine.

## Rejected Alternatives

- **Placeholder specification artifact for label intake** (rejected by
  GQ1): would admit synthesized or observed issue content as authority.
- **A new sync-carried item field for the supersession reason** (rejected
  by GQ3): would change the wire contract; the reason lives on the
  occurrence row instead.
- **Storing the admission key as authority** rather than deriving it:
  rejected — the key derives from the occurrence, so storing it creates a
  consistency obligation the re-gate enforces (stored == derived) rather
  than a second source of truth.

## Engine Contract Surface (Landed)

The engine layer lands the *decidable contract* — the policy vocabulary,
the typed parse, the authorization predicates, the pure WIP-cap gate, and
the nameable elaboration source — while the live-state execution that
consumes it stays in #659. Concretely landed here:

- **`internal/intake/policy.go`**: the two point-of-use policy keys
  (`budgets.run_wip_cap`, `initiator.mode`) on the
  `internal/elaborate/policy.go` pattern, `ParseIntakePolicy` (fail-closed:
  validates the bag first, then requires both keys; a non-positive,
  non-integer, or out-of-range cap and an unknown mode are malformed), and
  the pure predicates a start decision composes: `AutoStartAuthorized`
  (the `override`-only provenance predicate), `EffectiveMode` /
  `Downgraded` (a preset-sourced `auto_start` reduces to `propose`, which
  the loop records as `IntakeRefusalModeNotAuthorized`), and
  `WIPCapExhausted(activeRuns)` (the pure at/over-cap gate).
- **`ElaborationSource` threaded into `ElaborationRunSpec`**: the source is
  nameable in the spec; `SubmitElaborationRun` executes only the
  `spec_artifact` arm (and re-gates it equal to `SourceArtifactID`) and
  fails closed on `issue_subject`, whose assembly is #659's.

### Settled Judgment Call — Count and Start Are #659's Execution

The open call the PR flagged: is the WIP-start step (the active-run count
under the write lock, and the under-cap decision-ledger command) #720's
contract or #659's execution? **Settled: #659's execution**, extending
the GQ2/GQ1-depth boundary rather than overturning it.

- **Why the count is #659.** No project-scoped active-non-terminal-run
  projection exists (`RequireIdentityExecutionCapacity` counts an *auth
  identity's* executions — the inference-parallelism limit the issue
  non-goal explicitly forbids reusing as a run cap). Building that
  projection here would ship an untested-in-path store query whose only
  caller is #659's reconciliation loop, which is exactly the speculative
  execution GQ1-depth keeps out of this unit. The count and its
  consequence (record the refusal / author the start) must serialize as
  one write-locked decision, so the count belongs *with* its consumer, not
  split from it.
- **Why the under-cap start is #659.** Authoring a decision-ledger command
  is start *execution* (GQ2); it has no caller in #720.
- **What #720 owns instead.** The pure gate `WIPCapExhausted(count)` takes
  the count as input, so the *contract* — how a cap and a count combine
  into a refusal — is testable in isolation here; the durable refusal
  vocabulary and `RecordIntakeRefusal` are already built. #659 composes
  `ParseIntakePolicy` + the predicates + `WIPCapExhausted` under the write
  lock, counting live runs and calling `RecordIntakeRefusal` (at cap /
  downgrade) or authoring the start (under cap).

## Revisit When

- **#740 lands the durable project↔repository authority** (owner-ratified
  option (a); scheduled in the wave-5 contract chain ahead of #659): when it
  merges, `MintIntakeDeclaration`'s caller trust assumption is replaced by a
  store-enforced re-gate that the run's project belongs to the occurrence's
  repository, and the tampered-cross-repo case the #720 re-gate cannot catch
  becomes covered. Until then the tie is the caller's (#659's) responsibility.
- **A timestamp becomes load-bearing** (round-16 timestamp decline, Scope
  Boundary above): if anything ever keys behaviour off an occurrence timestamp
  or the derived `DeclaredAt` (ordering, an audit-integrity requirement),
  authenticate the occurrence timestamps then — until one does, they stay
  cosmetic and out of the re-gate's scope.
- #659 begins the reconciliation loop: confirm the run-count semantics it
  needs (which run states are "active non-terminal" for a project) and
  that composing this unit's pure gates under one write lock matches the
  loop's shape.
