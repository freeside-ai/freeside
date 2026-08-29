# Decision-Surface Identity (Plan Revision 43)

**Work unit:** #942, contract chain head of Wave 7 (#1001). **Origin:**
#916 fixed the four invariants a recommendation source record's commitment
must satisfy and deferred the mechanism after three same-class review
findings showed the inline attempts were not converging
(`devlog/2026-08-25-1154-recommendation-led-attention.md`).

## Decisions

1. **The identity is an epoch plus a digest over structural fields, never a
   content address of the presented set** (§4, §5.13, §5.14). Each item has
   one daemon-owned `DecisionSurface {item_id, epoch, subject,
   requested_decision, pr_head_sha, presented_artifact_digests, digest}`
   record; `digest` hashes only `{item_id, epoch, subject, requested_decision
   (sorted set), pr_head_sha}`. Chose sequence over content because both
   rejected inline attempts were content addresses: subtracting the derived
   record's own artifact coupled the identity to eligibility (a second
   applicable source flips the input and monotonicity strands the first
   record), and dropping the sibling-artifact surface collapsed two different
   judgment outputs on one `(subject, requested_decisions, pr_head)` to one
   digest. Two presented sets on one item are always distinct epochs, and two
   items are distinct ids, so distinct surfaces never share a digest, while a
   preimage with no artifact hash cannot cycle and is computable before the
   artifact that opens the epoch is finalized. Rejected: binding to
   `item_version` (the first inline attempt; telemetry strands the delivered
   card).

2. **The epoch advances by exactly one, when and only when a structural field
   or the presented set changes, and only in the store's single item writer.**
   A field returning to a prior value is a new epoch, never a reuse, so a
   source record committed at epoch 1 cannot be replayed onto an A→B→A
   surface. `ValidateDecisionSurfaceTransition` refuses a regressed epoch, a
   changed surface under the same epoch, a skipped epoch, and an advance with
   no change; the last two are unreachable through the store and exist so the
   validator states the rule, not merely the store's habit.

3. **The presented set is a structural predicate over presentation slots.**
   An artifact is presented iff `evidence_snapshot`, `agent_claims`, or a
   type-specific binding such as `finding_adjudication.adjudication_digest`
   references it. A digest reaching `artifact_digests` only through a
   recommendation provenance slot (#917) never enters it. `daemon_policy`
   rule and input digests and `project_policy` application records are not
   artifacts and are never members of either set, which answers the
   policy-axis stranding question left open in #916: the policy axis cannot
   strand a record by construction, not by a subtraction rule. Today
   `PresentedArtifactDigests` and the item's binding set are one function;
   #917 diverges them in exactly one place.

4. **The persisted record is authority and reconstruction fails the item
   closed.** `PutAttentionItem` derives the record; a decoded or
   caller-supplied value grants none. Both reconstruction tiers refuse an
   item whose record is missing, fails its own digest recomputation, has a
   column diverging from its body, or disagrees with the item's structural
   fields or presented set: `scanAttentionItemSnapshot` and, through
   `scanAttentionItemHistory`, the record tier
   (`GetAttentionItemRecord`, `ListOpenAttentionItemRecordsForRun`). The
   record tier's documented exemption is mutable current policy, and the
   surface record is the opposite of that: daemon-owned identity that
   advances only with the item's own structural fields, so a missing or
   disagreeing row is corruption, not the later policy change terminal
   recovery and historical authentication exist to see past. The snapshot
   path keeps its own surface gate last rather than delegating to the
   history scan, so a row tampered to reach a typed binding gate still
   reports that gate's more specific error. Chose failing the whole item
   over failing only the recommendation because the record is not yet on the
   item body (kept off it until #917 so this unit adds no wire field) and a
   missing or forged identity is row corruption, not a recommendation
   defect; #917's per-record check (`VerifyDecisionSurfaceCommitment`) is
   what fails only the recommendation.

   The writer re-gates too. `putDecisionSurface` compares the stored record
   with the decoded pre-update item before deriving the next epoch, because
   `DecisionSurface` proves only that a row validates against itself and
   `PutAttentionItem` reaches the surface write from a raw body with no gated
   reconstruction anywhere on the path. Without that comparison a
   self-consistent row planted on another surface is laundered into a
   valid-looking successor: the advance derives from the incoming item, the
   forged epoch lineage is blessed, and every read the re-gate had failed
   closed re-opens. Fail-closed at a reconstruction boundary is therefore not
   enough on its own; the read-modify-write is a boundary of the same kind.

5. **The migration backfills every derivable item at epoch 1 and leaves an
   underivable row without a record.** The planning comment proposed failing
   the migration on any undecodable row. Chose tolerance instead, matching
   the 0036 and 0040 data migrations: an undecodable body, a body naming
   another item, or an invalid subject already fails every gated read, and a
   missing record keeps it refused there, so the migration changes no item's
   readability and cannot brick a daemon over a row it could never serve.
   Deferring the write-path invariant (item row ⇔ surface row) to the
   migration and the writer keeps reads from self-healing corruption.

6. **Tests that raw-seed item rows seed the record; tests that tamper a
   structural field to exercise a later gate forge the record to match.**
   Seeding reproduces the post-migration state. Forging keeps the later gate
   under test reachable (the intake supersede gate and the
   review-diminishing decision gate) rather than letting the surface re-gate
   absorb their cases; a real forger would have to rewrite both rows.

## Verification Findings That Changed the Work

- The identity-level acceptance from the issue runs entirely in the domain
  package: telemetry-stable (`WithTiming`, a status transition,
  `WithDecidedAt`, `item_version`, `expires_when`), eligibility-independent
  (a non-slot digest in the binding set changes nothing; the preimage golden
  contains no artifact digest), surface-distinguishing (each slot, the head,
  the action set, two ids, reorder-stable, A→B→A one-way), and non-cyclic
  (pre-commit equals admission). The store repeats creation, telemetry
  replay, advance-by-one, prepare-then-put equality, every fail-closed
  tamper, the backfill, and the missing-record write refusal.
- The refute-first pass (fresh-context reviewer over the full diff) found
  nothing blocking. Confirmed and fixed: the backfill decoded bodies with the
  strict migration decoder while reads use the lenient one, so a body carrying
  a since-removed field would have been readable before 0058 and refused
  after; the backfill now decodes exactly as the read path does, making
  derivability equal readability by construction (latent: no field has ever
  been removed from `AttentionItem`). Confirmed and declined: the
  byte-identical and canonical-equal replay branches of `PutAttentionItem`
  return before the surface write, so a replay over an item whose record is
  missing converges without refusing; every gated read already refuses that
  item and a retried command reads it first, so the claim was narrowed to
  the transition path instead of adding a query to a branch that writes
  nothing. Disproved by tracing: telemetry advancing the epoch (both replay
  branches return early and `Matches` reads no telemetry field), a forged
  record or a forged body reconstructing (digest recomputation, column
  cross-check, `Matches`), an item row without a record on the write path
  (`putDecisionSurface` runs after the insert; no other runtime writer), and
  a concurrency race on the read-modify-write (one writer connection).
- Automated review (Codex, P1) found the gate applied to only one of the two
  reconstruction tiers, which the refute-first pass and the planning comment
  both missed: `GetAttentionItemRecord` returned a corrupt-identity item to
  `validateReadyItemPRBinding`, effect-proposal revision validation, and
  terminal production recovery. Confirmed and fixed at both members of the
  class, `GetAttentionItemRecord` and `ListOpenAttentionItemRecordsForRun`.
  The fix regresses no legitimate state: derivability equals readability by
  construction (above), so no readable item lacks a record, and the new
  refusals are exactly rows an external writer corrupted.
- Automated review (Codex, P1, round 2) argued the epoch must also advance on
  rendered-context changes, citing `operations/doctor.go` rewriting an
  existing item's `Reason` and a claim whose label, artifact id, or
  provenance changes while its content digest does not. Confirmed reachable
  (`ValidateAttentionItemTransition` does not fix `Reason`; the presented set
  is a digest set by construction) and declined, because the cited call site
  is the counterexample that makes the exclusion load-bearing: doctor
  refreshes that reason on every changed detail, so a reason-advancing epoch
  would strand every source record on the item on every doctor pass, which is
  invariant 1 and the failure mode that got `item_version` binding rejected
  in round 1. Plan §4 decides this explicitly ("other rendered facts are not
  surface members") and routes the dependency elsewhere: a source whose
  judgment depends on rendered prose binds it through its own input digest
  (`daemon_policy.input_digest`), never through the identity. Widening the
  preimage is a material plan change, not an implementation fix.
- Automated review (Codex, P1, round 3) found the second member of the
  round-1 class: `putDecisionSurface` read the stored record without checking
  it against the pre-update item, so a self-consistent row planted on another
  surface was laundered into a valid successor instead of failing closed.
  Confirmed by a negative control (the new planted-surface case passes with
  the check removed) and fixed by re-gating against the decoded old item.
  Root cause of the missed sweep: round 1 enumerated *read* accessors, when
  the real class is every boundary that decides whether the item row and the
  surface row agree. Re-enumerated at that width: the two reconstruction
  tiers (gated), the writer's read-modify-write (this fix), the create branch
  (a plain `INSERT` with no `ON CONFLICT`, so an orphan surface row fails the
  write closed), the two replay branches (they return before any surface
  read, so they cannot launder; the earlier decline stands), migration 0058
  (derives at epoch 1 from the decoded body), and the only other body-
  rewriting migrations, 0036 and 0040, which run before 0058 and so are
  backfilled from their rewritten bodies.
- The class-recurrence refute pass (second fresh-context reviewer, run on the
  widened class after round 3) broke nothing new but found the epoch's real
  limit. `Matches` compares structural fields and the presented set, never the
  epoch, because nothing on the item side carries one while the record stays
  off the item body. A writer able to rewrite only
  `attention_decision_surfaces` can therefore choose an epoch: rolling one
  back revives a superseded commitment, jumping forward pre-blesses one.
  Accepted as this unit's boundary rather than fixed, because closing it means
  binding the epoch to item-side state, which is exactly the wire/body
  decision this unit's non-goals defer to #917, and because the harm is latent
  until then (nothing outside the store and tests reads a record yet). Written
  into the type doc instead of left implicit, and carried below. Also
  confirmed and documented: `ReadTx.DecisionSurface` authenticates the row
  against itself only, so #917 must pair it with a gated item read; and any
  change to what the derivation reads (a `Subject` field, a
  `PresentedArtifactDigests` edit, canonicalizing a surface field in
  `CanonicalizeStoredRow`) needs a paired re-derivation migration, since the
  re-gate would otherwise refuse every existing item on both paths with no
  repair path. Disproved: any remaining ungated item read (every `SELECT` on
  `attention_items` goes through a gated scan; the divergence preflights
  return no item), any other writer of either table, any false refusal from
  `current.Matches(*old)` on a legitimate row (nil-vs-empty is neutralized,
  the migration decodes as the read path does, and canonicalization is applied
  to both sides), and a reachable create-branch collision (the foreign key and
  the whole-database restore keep the pair together; no `DELETE` exists).

## Revisit When

- A legitimate multi-source relationship needs an artifact that is presented
  without any presentation slot referencing it, or a presentation slot that
  should not count as presented; the predicate is structural and would need
  a declared exception.
- A data migration must rewrite `attention_items` bodies, or the surface
  derivation itself changes (a new `Subject` field, a
  `PresentedArtifactDigests` edit, a surface field canonicalized in
  `CanonicalizeStoredRow`). Either needs a paired migration that re-derives
  `attention_decision_surfaces`, or the re-gate refuses every existing item on
  both the read and the write path, with no repair path.
- #917 lands. Three things belong there: the end-to-end A eligible → B joins
  → B leaves → A re-derivable case through real source records; the wire
  projection `decision_surface {epoch, digest}`, which decides whether the
  record stays off the item body; and, gated on that decision, binding the
  epoch to item-side state so a surfaces-table-only writer cannot choose one.
  `VerifyDecisionSurfaceCommitment` must also take an item obtained through a
  gated read, not a bare `ReadTx.DecisionSurface` record.
- A recommendation is shown to depend on rendered prose that no source input
  digest binds, or a presentation slot's non-digest fields (a claim's label
  or provenance) become materially decision-bearing. Either would reopen the
  round-2 review argument for widening the preimage past the structural
  fields, which is a material plan change with an invariant 1 cost to price.

## Inputs

Issue #942 (contract and implementation plan, grounded at `7916aae9`);
tracker #1001; `docs/plan.md` §4, §5.13, §5.14 at revision 42;
`daemon/internal/domain/attention_item.go` (`bindingDigests`),
`daemon/internal/store/entities.go` (`PutAttentionItem`,
`scanAttentionItemSnapshot`), `daemon/migrations/0057`.
