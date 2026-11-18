# The Daemon No Longer Reads Three Old Execution-Admission Formats

Contract unit #3325, family 7 of #3266's inventory. Mandatory note: Tier A
(`domain/execution.go`, `exec/stage/state.go`, `store/conformance.go`), a
change to stored formats, and an owner choice. The owner decided on
2026-11-16 that prod history is disposable and prod starts on a fresh
database (#3265), so nothing needs to read these formats.

## Decisions

- **Chose to remove three readers over keeping them for old rows.** Each
  removed format now fails loudly:
  - **The `attended` flag on an agent binding**
    (`AdmissionAgentBinding.LegacyAttended`). An execution admission
    stored with it decodes without it, resolves to another id, and fails
    with `ErrAdmissionInconsistent`. The ward's completion evidence and a
    Codex review's authority digest, both recomputed over the decoded
    binding, fail their comparisons the same way.
  - **An unattended admission with no `backend_configuration_digest`.**
    `ExecutionAdmission.Validate` now applies `NewExecutionAdmission`'s
    rule and returns `ErrConformanceConfigurationUnbound`.
  - **A vendor-instruction snapshot with an empty `delivery`** (the
    `freeside.stage.inputs/v2` encoding). Validation refuses it with
    `ErrUnsupportedVendorInstructionBinding`, and
    `exec.EffectiveVendorInstructionDelivery` is gone. A stage intent
    whose instructions carry no delivery fails recovery with
    `ErrUnsupportedStart`.
- **Accepted one silent read, as planning proposed.** A review admission,
  review record, shadow review record, or review request whose binding
  carries `"attended"` reads as a current row. Their body digest covers
  the stored bytes, not a re-encoding, and their decodes ignore unknown
  keys. Nothing ever read the flag, so such a row means what it meant
  before. A replay of one re-encodes without the key, and the store
  refuses it with `ErrImmutableConflict` (checked with a scratch test
  that was not kept). Rejected: making those four decodes refuse unknown
  keys. That changes four Tier B decodes that #3324 also edits, to refuse
  rows whose meaning is unchanged.
- **Kept the conformance gate's own refusal of an admission with no
  configuration** (`store/conformance.go`). No stored admission reaches it
  now, because `Validate` runs first, but the gate is exported and a
  caller could pass an unvalidated value. Only its comment changed.
- **Kept `ward/spec.go`'s default**, as the contract requires. The
  conformance suite still builds a spec without a delivery. Its comment
  still says pre-v3 Claude intents "remain recoverable", which the stage
  driver no longer allows; `ward/` was outside this unit's paths.

## Kept, With the Reason

- `domain.TestLegacyExecutionAdmissionRemainsReconstructable`: covers an
  admission with no stage inputs, which the seed fixture and
  `cmd/freeside-signet-dev` write.
- `store.TestLegacyAdmissionKeepsItsRecord`,
  `engine.TestCutoverLeavesLegacyAdmissionsAndBindsQueuedRuns`, and
  `invocation_admission_facts_execution_legacy.golden`: cover an admission
  with an identity and no agent binding, which `engine/admission.go`
  still writes.
- `store.TestRequireUnattendedAdmissibleLegacyNoticeNeedsHealthyEncryption`:
  covers the backup-encryption waiver notice.
- `ErrStageInputsMissing` and the nil stage-input refusals.
- The read of an admission recorded under the backup-encryption waiver
  with no waiver configured. The removed
  `store.TestLegacyUnboundUnattendedAdmissionReconstructs` was its only
  test, so that half stays as
  `TestWaivedAdmissionReadsWithoutAConfiguredWaiver`.

## Evidence

- **Last writers.** The `attended` flag: af17d7a5d (2026-10-24) stopped
  recording it. The unbound unattended admission: no writer, since
  `NewExecutionAdmission` refuses one. The empty delivery: no writer;
  `engine/vendor_instruction.go` writes the configured delivery, which
  `ValidateVendorInstructionBinding` already requires, and every fixture
  sets one. Searches covered production code, `seedfixture`,
  `freeside-signet-dev`, and `domaintest`.
- **Instance counts (2026-11-17, branch build, counts only).** Counted
  in a deleted copy of each database, and in the intent files under
  `claude-driver/`.
  - **Dev instance:** 0 of every shape. `check-database` passes.
  - **Kept roots at schema 128 or later (9):** 0 of every shape. Two
    pass `check-database`. Seven fail on kinds this unit doesn't touch
    (check proof, schedule occurrence, artifact, attention item), the
    same under `main`'s build (4481f907a).
  - **Kept roots below the migration baseline (27, schema 22 to 104):**
    no current build opens them, `main`'s included. One holds 4
    execution admissions with the flag, 1 review admission, and 1 review
    record. Five hold 26 admissions and 25 intent files with no delivery.
    No root holds an unbound unattended admission.
- **Prod.** Not read by an agent. #3265 records that prod started on a
  fresh database.
- **Restore.** Restore copies rows, not schema. A same-version backup
  taken before the fresh database restores, and its rows in these formats
  then fail to decode when read, as above.

## Refute-First Record

- **A removed shape is read as a current one somewhere else (disproved).**
  A fresh-context reviewer traced every decoder, canonicalizer, replay,
  and digest site over the binding and the snapshot. Only the four
  review bodies above read silently.
- **A current writer still produces a removed shape (disproved).** Same
  review, plus the stored corpus (`TestStoredCorpusResumes`), which
  passes unchanged.
- **A current id or encoding moved (disproved).** No golden file changed,
  and `ComputeID` versions and `omitempty` tags are unchanged.
- **The new refusal tests would pass against the old code (disproved by
  a check).** The golden refusal of the `attended` id and the
  `implicit instruction delivery` intent case each fail with the old
  reader restored.
- **A removed test took current coverage with it (confirmed, fixed).**
  See the waiver read under Kept.

## Left for the Owner

- **Two shapes planning found** (#3325, Notes): the flag-style admission
  path in `engine/admission.go`, and the backup-encryption waiver.
- **The no-selection-source reader** in `AdmissionAgentBinding` (a
  non-goal here). Every stored binding without a selection source also
  carries `attended`: the source arrived on 2026-10-10 (ace562233) and
  the flag stopped on 2026-10-24. Such an admission now fails its id
  check, so that reader no longer reads any execution admission.

Revisit when a root below the migration baseline needs to resume: it
needs a migration path first, and then these readers.
