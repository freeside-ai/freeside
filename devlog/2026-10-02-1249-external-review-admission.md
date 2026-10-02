# External Review Admission Through the Trust Profile

Issue #1623 is the contract half of external review on published pull
requests. It adds the allowlist that says which outside reviewers may start a
review round, the provenance an external finding carries, and the authority
that lets such a finding re-enter review. #524 carries the behavior: fetching
review activity, storing findings, sealing the authority, and adjudicating.

Nothing in this unit ingests anything. Every gate here is exercised by store
and domain tests, not by a running daemon path.

## Decisions Stated in the Issue at Planning

The plan put these in the issue body and asked the owner to veto any of them.
The owner handed the unit to implementation without changing them.

1. **No encoding version bump for the allowlist.** Chose to omit
   `external_reviewers` from the canonical form when empty, over bumping
   `freeside-trust-profile/v6` as every earlier field change did. The
   acceptance requires an existing profile to keep its digest, and a bump
   would force owner re-approval of every onboarded repository for a field
   nobody has set. The cost is that the canonical form now has one optional
   member, so "same version" no longer means "same field set".
   **Revisit when** a second optional field is proposed: two optional members
   make the canonical form hard to reason about, and the next required-field
   change should fold both into a bumped version.
2. **A reviewer is forge, immutable account ID, and login, and all three must
   match.** Chose this over login alone, because a login can be renamed and
   then reused by someone else, and over account ID alone, because a listed
   account that renames itself should stop being admitted until the owner
   looks. Validation refuses two entries for one forge and account ID.
   `NativeReviewObservation` records only `AuthorLogin`, so #524 has to fetch
   the account ID.
3. **One authority, `drive_round`.** Rejected a second, advisory level. An
   unlisted identity's finding is already stored and drives nothing, so an
   advisory level has no behavior to attach to yet.
4. **A sealed authority names the profile that admitted its reviewer.**
   Chose naming the profile digest in the `external_review` successor, with
   sealing requiring that profile to be the active one, over re-checking the
   active profile on every read. Re-checking on read would make a finished
   cycle, and the ready item it re-earned, unreadable as soon as the owner
   removed the reviewer. #1622 made the same choice for recipe approval
   (`devlog/2026-10-02-0222-readiness-reentry-authority.md`).
   **Revisit when** the trust profile encoding version is bumped: a pre-bump
   profile no longer validates on read, so an `external_review` authority
   naming one stops reading. Candidate authorizations already have this
   property; the bump has to decide both together.
5. **The predecessor must already be superseded, with no readiness
   invalidation.** #524 supersedes the ready item in the transaction that
   seals the authority. Rejected a new sync-carried field explaining the
   withdrawal on the card, which would have pulled `api/` and `app/` into a
   daemon contract unit.
   **Revisit when** the wave 8 exit run shows the owner needs the reason on
   the superseded card; that is a new contract unit.
6. **The authority names one triggering finding.** Which external findings a
   round adjudicates is #524's decision, not this record's.
7. **The owner sets the allowlist with a repeatable onboard flag**,
   `-external-reviewer <forge>:<account-id>:<login>=<authority>`. No
   trust-profile config parser exists to extend. Re-running `freesided
   onboard` on an onboarded repository records and activates the new
   revision; the plan left that unverified and
   `TestOnboardReRunAdmitsExternalReviewers` now pins it.

## Decisions Made in Implementation

- **Admission is not stored anywhere.** `PutExternalFinding` only stores.
  `GetAdmittedExternalFinding` re-runs the allowlist check against the
  repository's active profile on every call, and `GetFinding` stays the
  ungated history read. Rejected an `admitted` bit on the finding: it would
  be a decoded trust bit, and an allowlist edit could not withdraw it.
- **The admitted read finds the repository through the published ready
  binding**, `PublishedProductionReadyItemID` then `GetReadyItemPRBinding`,
  not through `EffectiveWorkUnitPRBinding` as the plan guessed. The work-unit
  binding exists only for runs with a work-unit declaration; the ready
  binding exists for every published ready item and carries the immutable
  repository ID the profile is compared against.
- **The quarantine is one refusal in `PutFinding`.** Routed and shadow review
  records both store findings through it, so neither can carry an external
  finding, and a stored external ID cannot be relinked because finding bytes
  are immutable. The shadow path also names the refusal explicitly, because
  its source check would otherwise refuse for an unrelated reason. A native
  review observation refuses one in its own validation.
- **An external finding's ID is derived, not supplied**: a digest over run,
  forge, reviewer account ID, thread, raw-source digest, and head. An edited
  comment is therefore a new finding, and validation recomputes the ID, the
  source label, and the raw digest, so a relabelled row fails reconstruction.
  The account is in the digest so that an unlisted account posting the same
  text in the same thread cannot occupy an admitted reviewer's row.
- **`external_review` shares the commandless identities of a readiness
  re-entry.** One superseded item admits one commandless successor whatever
  its origin. The two origins cannot both pass the gate for one item: one
  requires an invalidation fact and the other forbids it.
- **Sealing also requires the predecessor to be the run's current ready
  item.** This was not in the plan. An operator's feedback return leaves the
  same state the gate admits (superseded, not invalidated), and a feedback
  successor has a command-keyed identity, so without the check a second
  successor of that item would seal under a different key and branch the
  chain, after which no read of the run's chain succeeds. No store-level test
  covers the refusal: building a sealed feedback successor needs the engine's
  fixtures. The check is five lines over `CurrentProductionReadyItemID`.
- **Remediation follows the `base_advanced` rule, so the gate refuses the
  origin on a head Freeside did not push.** A remediation candidate replaces
  the published head. After a `head_changed` re-entry re-earns readiness in
  place, that head is someone else's commits, and it stays so through any
  `base_advanced` re-entry on top. The gate walks the in-place ancestry and
  refuses an `external_review` authority there. Rejected a review-only
  variant of the authority: `AllowsRemediation` is a domain method on the
  record, the engine calls it directly, and the record has no field that
  could say "review only", so that variant is a further contract change. The
  finding is still stored; it drives nothing.
  **Revisit when** #524 needs external review to drive a round on a pull
  request someone else pushed to.
- **One finding starts one cycle.** Sealing refuses a finding that an earlier
  cycle of the run already named. A cycle that re-earns readiness in place
  leaves the finding on the published head, where the gate alone would admit
  it again without limit.

## Refute-First Pass

The admitted read and the re-entry gate are returned-object trust
boundaries. A fresh-context reviewer, given the diff and the intended
outcome only, tried to break them.

Confirmed and fixed:

- An `external_review` authority sealed on a head that a `head_changed`
  re-entry had re-earned in place, and admitted remediation there. Fixed by
  the foreign-head refusal above. The same ancestry admits remediation
  through a `base_advanced` re-entry on top of a `head_changed` one, which
  predates this unit. Follow-up: #1703.
- One finding sealed a second and third cycle. Fixed by the one-cycle rule.
- An unlisted account's identical comment occupied the admitted reviewer's
  finding row. Fixed by putting the account ID in the finding ID.

Allowed by decision:

- A repeated identical comment by one reviewer in one thread on one head has
  the same ID as the first and conflicts if its `CreatedAt` differs. It
  carries no new information; #524 treats the conflict as a duplicate and
  uses the forge's comment timestamp so a replay is deterministic. Adding a
  comment ID to the provenance would change the shape the issue fixes.
- The admitted read does not compare the finding's head with the published
  one. It answers who may drive a round; the re-entry gate binds the head.
- The onboard flag accepts `+41` and `041` as account IDs. The stored integer
  is canonical.
- The provenance names no repository or pull request, and the gate does not
  ask for one. A finding is tied to a pull request by its run: the gate and
  the admitted read both take the repository, repository ID, and pull request
  number from the run's ready binding. No caller stores a finding fetched from
  one pull request under another's run, and source fields on the finding
  would be filled by that same caller from that same binding, so they would
  prove nothing the run does not. `NativeReviewObservation` does carry them;
  adding them here would change the shape the issue fixes, which is the
  owner's call.
  **Revisit when** ingestion reads review activity from anywhere other than
  the run's bound pull request.
- A finding is admitted on the login it was stored with, not on the account's
  login at the time of the read. The store makes no forge calls, so every
  forge fact on a finding (account ID, login, thread, head) is what ingestion
  observed, and the allowlist is compared with that. An account that leaves a
  comment under its listed login and renames itself afterwards still has that
  comment admitted: the listed account wrote it under the listed name. Its
  later comments carry the new login and are not admitted. Re-resolving the
  login at admission would need a stored identity observation, which is a
  further contract shape, and nothing ingests yet.
  **Revisit when** #524 leaves a material gap between observing a comment and
  sealing its authority.

Disproved by a check:

- Relabelling a stored external finding into a routed or shadow review
  record: refused by the immutable byte comparison, in both orders.
- A fail-open error chain in the admitted read or either gate: each joins a
  sentinel, and a missing active profile reports not admitted.
- Byte drift in v1 to v3 successors, existing findings, or existing profiles:
  every new field is `omitempty`, and no pre-existing golden changed.
- A second commandless successor branching the chain: the predecessor key is
  shared, so it is an immutable conflict.
- Another dispatch site mishandling the new origin or an empty reason: no
  code outside the gate reads `Reentry.Reason`.

## Unit Size

The plan estimated 1,700 to 2,000 changed lines against a 1,000-line budget
and proposed a three-part split. The owner handed over the whole issue
without applying the split, so the unit stayed whole and the parts landed as
separate commits in the plan's order, which keeps a split possible. The gate
in part 3 reads the finding from part 2 and the allowlist from part 1, and no
part is usable by #524 alone.

## What #524 Inherits

- Ingestion has to store, under a run, only review activity it fetched for
  that run's bound pull request. The store cannot check this: a finding
  carries no source repository or pull request, and the run is its only tie
  to one.
- The account ID and login on a finding have to come from the forge's own
  record of the comment's author, read when the comment is ingested. The
  store compares the allowlist with those stored values and cannot refresh
  them.
- The adjudication store gate requires an artifact's entries to equal the
  review record's findings exactly, and the quarantine keeps external
  findings out of review records. #524 has to widen that gate.
- The fixed-disposition proof keys on `Source`, and an external source never
  appears in a Freeside review. #524 has to decide how an external finding is
  proven fixed.
- An external finding's `CreatedAt` has to be the forge's timestamp for the
  comment, not the ingest time, or a replayed ingest conflicts instead of
  converging.
- No index table was added. A run's external findings are the `findings` rows
  for its `run_id` that carry an `external` block.
