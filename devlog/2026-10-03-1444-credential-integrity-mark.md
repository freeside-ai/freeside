# Credential-Integrity Mark and Its Admission Refusal

Issue #1624 is the contract half of the doctor credential-integrity probe
(plan §11 row 8). It adds the mark plan §5.4 admission rule 4 already requires
admission to check, and the refusal. #1630 adds the probe that sets the mark.

Nothing in this unit records a mark in a running daemon. Every gate here is
exercised by domain, store, and engine tests.

## Posture

Planning (2026-10-03) read §5.4 and §10 together and found no conflict, so
this unit implements rule 4 as written.

- **The mark closes admission for the one generation it names.** It files no
  item and adds no `system_health` posture of its own. While the daemon runs,
  other enrollments and other generations keep admitting.
- **Startup is wider than that, through a check this unit does not change.**
  The startup role check (`cmd/freesided/agent_cutover.go`) treats a writer
  role whose current generation is marked like any role that fails
  admission: it holds every lineup admission for the daemon's run and files
  a blocking `agent_selection_inactive` item. Re-enrollment clears the mark
  but the hold lasts until restart. A marked reviewer role stops an
  unattended start. Planning called the hold intended; the daemon-wide reach
  and the restart are an owner decision, escalated in the follow-up below.
- **§10's "nothing else reads them" covers the account probe.** That sentence
  cites §5.4 and names the profile projection's display fields, which only
  the account probe feeds. §5.4 exempts the credential-integrity marker from
  the observation-only rule, and rule 4 names the mark as an admission input.
- **The item's posture is #1630's.** Whether the `system_health` item the
  probe files is `blocking` or `advisory` is settled there. This unit files
  no item.

If the owner reads §10 as making integrity results observation-only, this
contract is wrong.

## Decisions

1. **The mark is a row in its own table, not a field on
   `EnrollmentGeneration`.** Generations are immutable and append-only, so a
   field would either rewrite a row or move the `enrollment_generation`
   golden for every stored generation. A carried bit would also be a decoded
   trust claim needing its own re-gate. With a separate table, no record
   carries a marked or unmarked bit: each boundary reads the mark rows in its
   own transaction, and an "unmarked" claim has nowhere to live.
2. **The check runs where work starts, not where history is read.** Role
   resolution (`resolveRole`) and the new-record path of
   `RecordExecutionAdmission` call `RequireGenerationUnmarked`. The read
   re-gate (`scanExecutionAdmission`) does not. Rejected: checking in the
   re-gate, because a mark landing after an admission would make every
   admission on that generation unreadable, and a mark is about what starts
   next. This is the rule `RequireBackendConformant` follows for lapsed
   conformance. The check sits after the replay branch, so a byte-identical
   replay of a recorded admission still converges.
3. **Recording is first-observation-wins, not write-once.** Chose
   `INSERT ... ON CONFLICT DO NOTHING` plus a read-back over `putImmutable`.
   The scheduled probe records on every pass with a later `observed_at`;
   `putImmutable` would report each later pass as `ErrImmutableConflict`.
   The primary key is `(enrollment_id, ordinal, finding)`, so a generation
   can hold one mark per finding class.
4. **An unreadable mark is an error, never an absent mark.**
   `GenerationIntegrityMarks` first requires the generation to reconstruct
   under its enrollment, then decodes, validates, and cross-checks every row.
   `RequireGenerationUnmarked` on a generation that does not exist returns
   `ErrNotFound` instead of passing.
5. **The refusal is typed through stage admission.** `resolveAgentAdmission`
   flattened a failed read transaction to text. It now wraps that error, so
   `errors.Is(err, domain.ErrGenerationIntegrityMarked)` and `errors.As` work
   on every resolution path. Chose a two-`%w` wrap over `errors.Join` so the
   printed refusal is byte-identical to what it was.
6. **A refusal from the admitting transaction holds the invocation.** A mark
   can land between the role-resolution read and the admitting write. That
   refusal carries `ErrGenerationIntegrityMarked` without
   `ErrAgentNotAdmissible`, and a dispatch error neither hold classifier
   recognizes ends the reconcile loop. `invocationDispatchHold` and
   `dispatchHoldReason` now classify it as an admission-policy refusal, the
   same hold the resolution-side refusal takes. Rejected: wrapping the store
   error in `ErrAgentNotAdmissible`, because the store does not import the
   engine and the domain sentinel already names the cause.
7. **`CurrentEnrollmentGeneration` is unchanged.** `auth list` and
   re-enrollment read a marked generation on purpose, and re-enrollment is
   how the refusal clears: generation N+1 has no mark row, and generation N's
   mark stays as history.

## Not Done

- **No recheck at dispatch.** A mark recorded after the admitting transaction
  commits does not stop that attempt from starting. It fails at
  authentication, as it does today.
- **No sync-carried mark.** The mark is daemon-internal like the generations
  it names; `api/openapi.yaml` and the clients do not change.
- **No wardless call site.** `AdmitWardlessRole` has no caller. #1425 adds
  one and makes the same store read.

## Refute-First Findings

- **Disproved: an admission built by hand for a marked generation records.**
  `TestMarkedGenerationRefusesNewAdmission` builds the admission directly,
  skipping role resolution, and the admitting transaction refuses it for each
  finding class.
- **Disproved: a tampered or undecodable mark row reads as unmarked.**
  `TestGenerationIntegrityMarkReconstructionFailsClosed` changes a column,
  the body's enrollment, the finding, and the body's syntax with raw SQL; the
  read and the gate each return an error, and the admitting transaction
  fails closed on a tampered row.
- **Confirmed and fixed: a mark landing after resolution ended the reconcile
  loop.** An independent review traced the store-side refusal to a dispatch
  error neither hold classifier matched. Decision 6 is the fix;
  `TestMarkAfterResolutionHoldsTheInvocation` pins it.
- **Confirmed, not fixed here: the startup check widens the refusal.** The
  same review found the daemon-wide hold recorded under Posture. The code is
  in `cmd/freesided`, outside this unit's scope.
- **Disproved: a mark can name a generation that does not exist or belongs
  elsewhere.** The mark's key is the generation's own `(enrollment, ordinal)`
  key, recording requires that generation to reconstruct, and the foreign key
  backs it. A mark for one enrollment is never returned for another: the read
  filters on both key columns and cross-checks the body against the
  generation it was read for.
- **Disproved: a caller reaches admission around the check.** The only
  admission site that reads a generation is `resolveRole`; `ResolveRole`,
  `CheckRole`, stage admission, the startup role check, and preflight all go
  through it. The other generation readers do not admit: `auth list`,
  re-enrollment and adoption's enrolled check and bootstrap reads in
  `wardstore`, the leaser's volume lookup at dispatch, and `gateAdmission`,
  the read re-gate decision 2 leaves alone.
- **Allowed: a restore from a checkpoint taken before a mark reads
  unmarked.** The restored database has no mark row until the probe's next
  pass records one. The mark is a probe observation, and the damaged
  credential still fails at authentication.
- **Allowed: deleting or re-keying a mark row with raw SQL removes the
  mark.** A row moved to another key is indistinguishable from an absent one.
  Adversarial edits to the daemon's own database are outside this boundary's
  threat model, as they are for generation rows.

## Revisit When

- The owner decides how startup and preflight treat a marked generation.
  Follow-up: #1740.
- #1630 shows marks landing between admission and start often enough to
  matter. That is the case for a dispatch recheck.
- #1425 wires a wardless caller. It must call `RequireGenerationUnmarked` in
  the transaction that reads the generation.
- A client needs to show the mark. That makes it sync-carried and an API
  contract change.
