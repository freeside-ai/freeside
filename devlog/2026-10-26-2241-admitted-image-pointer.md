# Admitted-Image Pointer and Image-Upkeep Contract

Work unit: #2332 (contract). Plan: `docs/plan.md` §5.7, Project-image upkeep
(revision 98). Consumers: #2333 to #2336.

## Decisions

**Chose a pointer keyed by repository and harness over one keyed by base
digest.** `admitted_image_pointers` has one row per `(repository_id,
harness)`. The base image's digest changes whenever the base is rebuilt, so
a digest key would leave one stale pointer per base version and give the
upkeep job no single row to advance. The harness names the stable role (the
Claude Code base today, the Codex base when wave 9 builds it). The pointer
carries no verdict: every read re-gates it against its write-once
`project_images` row and fails closed when the row is missing or disagrees,
and admissibility is still derived at each boundary. The advance path does
not check the pointer's harness against the image, because a project image
records no agent base today and only the Claude base is in service.

**Chose the `-agent-image` flag and `images.agent` as the pointer's seed
over replacing them in this unit.** `freesided onboard` seeds the pointer in
the trust-profile transaction; daemon start seeds it from the configured
image only when none exists and never overwrites one. Selection stays on the
flag until #2333 moves composition, publication, and re-entry to the
pointer; the flag retires in its own unit after that. Switching selection
here would have changed run behaviour in a contract unit.

**Chose an `image_policy_adoption` effect kind over a new attention type.**
The adoption card is an `effect_proposal` item whose parameters bind the
refusal record ID to the exact recipe digest or registry authorities. The
`effect_proposal` path already carries bound parameters, a digest, and the
approve-to-effect flow; a new attention type would duplicate them.

**Moved `RebuildRefusal` from `projectimage` to `domain`,** so the refusal
record can name gate clauses without `domain` importing `projectimage`.
`projectimage` keeps aliases; the gate and engine are unchanged.

**The daemon origin is re-gated on every write and decode.** A
daemon-origin `TaskSubmission` skips the device gate, so the store never
trusts the origin column alone. It requires a recorded
`image_upkeep_refusals` row named by the submission's `SourceDigest`, of
class `repair`, for the project's own repository. The issue asked only that
the row exist. The class and repository checks follow §5.7's narrower
statement of the maintenance-task exception: a policy refusal or another
repository's refusal never mints work. The client's result trust also
rejects a daemon-origin record as the answer to a device's command.

## Departures From the Plan

- **Refusals insert-or-ignore, not `putImmutable`.** The refusal ID covers
  every fact except `RecordedAt`, so a rerun's body differs only in time.
  Byte-identical replay would turn every rerun into a conflict. The store
  keeps the first row and returns it.
- **`Record*` names, not `Put*`.** `InternalTx` cannot hold `Put*` methods
  (the synchronized-state guard), so the tip observation's upsert is
  `RecordRepositoryTipObservation`.
- **No `-- drop:` lines in 0106 or 0109.** Both rebuild a table under its own
  name; the `_v1` copy is created and dropped in the same migration, so no
  pre-existing name disappears, and `TestMigrationsDeclareDrops` rejects a
  declaration of a drop that did not happen.
- **The upkeep hold's facts are required beside its reason.** The plan
  allowed `image_upkeep` only beside `image_upkeep_pending`; it is also
  required there, because a pending hold that names no pending resolution
  could never be released by the rule that releases it.
- **The adoption arm sits on `EffectProposalFactsSnapshot`.** The API has no
  `EffectProposal` schema; the facts snapshot is where the other kinds'
  parameters cross the wire.
- **The adoption card has no work-unit subject yet.** Every existing kind
  resolves to a run or task subject; this one names a repository's refusal.
  The store refuses to admit the kind until #2335 defines its subject with
  its producer, rather than inventing one here.
- **Swift files beyond the `ScheduleKind` label.** Exhaustive switches over
  `RunHoldReason` and `EffectKind` in `RunDisplay.swift`,
  `DecisionModel.swift`, and `AttentionDisplay.swift` needed arms to
  compile; they label the new cases and show no actions.

Revisit when #2333 switches selection to the pointer: the seed path and the
flag should retire together. Revisit when a second agent base gets project
images (wave 9): the image record should name its base, and advancing a
pointer should require that base to match the pointer's harness.
