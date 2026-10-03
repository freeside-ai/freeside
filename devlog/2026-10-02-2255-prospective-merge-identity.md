# Prospective Merge Identity for Base-Advance Re-Entry

Issue #1706 is the contract half of re-entering verification and review after
a base advance. It defines what such a re-entry evaluates and carries that
identity through the shared surfaces: the domain proofs, the verifier, the
review request, and the two reviewer adapters. #502 carries the behavior:
building the merge, running the cycle, and creating the fresh ready item.

Nothing in this unit builds a merge or sets the new fields in a running
daemon path. Every gate here is exercised by domain, verifier, contract-suite,
and ward tests.

## What a Base-Advance Re-Entry Evaluates

The owner decided on 2026-10-02 (on #502): a base-advance re-entry evaluates
the **prospective merge** `M` of the unchanged head `H` into the new base
`B'`. `M` is a merge commit with parents `(B', H)`, built locally with a fixed
author, committer, timestamp, and message, and never pushed. `H` stays the head
the forge and its reviewers see.

Rejected:

- **Compare `B'` with `H` directly.** `H` was built on the old base. The
  two-dot review diff then shows every commit that landed on the base as if
  the pull request reverted it, and verifying `H`'s tree says nothing about
  `B'`.
- **Wait for someone to update the branch.** A ready item would stay stale
  for as long as nobody acts, with the daemon holding evidence it knows is
  bound to a prior base.
- **Have the daemon push a merge.** Rejected by the owner: the daemon does
  not move a pull request's head on its own authority, and a pushed merge
  would change the head every earlier review and approval bound.

## Decisions

1. **A new type, not `domain.ProspectiveMerge`.** Chose
   `domain.ProspectiveMergeIdentity{BaseSHA, HeadSHA, MergeSHA}` over reusing
   or extending the closure-approval binding. That type requires a
   publication identity digest, which a re-entry does not have until it
   publishes, and the store and the sync projection persist and compare it
   whole. Extending it would widen closure approval, four store columns, and
   the wire shape for a field none of them reads.
2. **`M` is not recorded on the authority.** `PublicationSuccessor` is sealed
   at supersession, before any checkout exists, and `M` can only be built
   later. Chose a derived gate, `AllowsProspectiveMerge`, over a new field: it
   is true only for a v3 `readiness_invalidation` authority with reason
   `base_advanced` whose base and head equal the identity's. The persisted
   authority and its golden are unchanged.
3. **`MergeSHA` is omitted from the proof digest body when empty.** Chose
   `omitempty` on `CheckProof.MergeSHA` and `EvaluationTarget.MergeSHA` over
   the explicit-null convention goldens normally pin. An explicit null would
   change the digest of every stored proof, which fails each one on read and
   leaves no existing run able to recompute readiness. The encoding version
   stays `freeside.check-proof/v1`, and the unchanged `check_proof` golden is
   the guard. The same reasoning covers `verify.Report.EvaluatedSHA`, the
   review request's `evaluated_sha`, and the ward launch intent digest, where
   an open intent must re-derive to its pre-upgrade digest.
4. **The merge compare applies to base-dependent proofs only.** A proof for a
   requirement that does not depend on the base binds the head alone and
   carries no merge, so it still covers a target that names one. For a
   base-dependent requirement the proof's merge must equal the target's in
   both directions: a proof naming `M` never covers a bare head, and a proof
   naming none never covers a target that names `M`.
5. **A sibling constructor, `NewProspectiveMergeCheckProof`.** Chose it over
   changing `NewCheckProof`'s signature, which would have reached the engine.
   The engine is #502's to change.
6. **The request authority digest moves to `review-request-authority-v3`.**
   The evaluated commit changes which candidate the reviewer reads, so it
   belongs in the digest. A version bump is safe because no copy of the
   digest is persisted: the engine, the ward, and the fake each recompute it
   from the request.
7. **The verifier checks parents, the ward checks observation.** The verifier
   refuses an evaluated commit whose parents are not exactly the base then
   the head. That binds the commit's ancestry, not its tree: nothing checks
   that the tree is what merging the two produces, which rests on the daemon
   having built the commit. The ward trusts an evaluated commit only as the
   commit the runtime observed in the review workspace, and a decoded request
   naming the head or base as its evaluated commit is rejected like any other
   invalid row. The ward does not re-prove parentage: building `M` and
   verifying it precede review in the cycle #502 runs.
8. **The launch keeps `ExpectedHead` as the forge head.** The plan had
   `ExpectedHead` take the evaluated commit. Chose a separate
   `ExpectedEvaluated` with the head unchanged, because the access check must
   still read the head, and a field named for the head that held the merge
   would misstate what the journal binds.
9. **The head-shape access line keeps the `v1` marker.** The plan bumped the
   marker to `freeside-review-access-v2` for both shapes. The app parses the
   `v1` line (`app/Sources/FreesideCore/ReviewEvidencePresentation.swift`) and
   the issue requires the access check to be unchanged when no evaluated
   commit is set, so only the merge shape reports under `v2`. The head-shape
   command is byte-identical to the one before this unit. The app does not
   yet recognize the `v2` line; it shows it as an ordinary diagnostic.
   Follow-up: #1734.
10. **`ReadinessDetail` does not carry the merge.** It is part of the synced
    card shape in `api/openapi.yaml`, and `api/` and `app/` are outside this
    unit's scope. Follow-up: #1735.

## What Deploying This Changes

The prompt protocol labels move to `codex-production-review-prompt-v6` and
`claude-production-review-prompt-v4`, and the access command template gains
the evaluated-commit shape. Both feed the reviewer configuration digest, whose
command template member now hashes the head shape and the merge shape
together, so an edit to either moves it. A run
admitted under the old digest parks at the review gate until an operator
adopts the new configuration through the review-configuration recovery item,
as with every prompt bump. No new recovery code is needed.

The head-against-base prompt text and access command are unchanged; only the
labels and the digest move.

## Refute-First Findings

- **Verifier.** Tried a merge with swapped parents, a non-merge commit on the
  base and on the head, a root commit, a merge into the old base, a merge of
  a different head, an octopus merge, a tree object, an absent object, and
  the head or base itself. Each is refused before a result exists. The two
  checks that read a tree before any command runs, recipe divergence and the
  symlink entrypoint, read the merge's tree; reverting either to the head's
  fails a test.
- **Ward.** Tried a persisted request rewritten to name the head, then the
  base, as its evaluated commit: both abort the invocation as contradictions
  and leave no runtime objects. Tried a review spec whose evaluated commit
  the workspace observation does not back, and one built for the merge then
  re-validated as a head review: both are refused.
- **Access probe.** Ran the generated shell against a real checkout holding a
  base, a head, and their merge. A workspace at the head is refused when the
  merge is requested, and a workspace at the merge is refused when the head
  is requested.

## Revisit When

- #502 builds `M`. The store's ready-binding gate and the engine then consult
  `AllowsProspectiveMerge`, and the engine's finding-location gate must take
  its changed lines from the base-to-merge diff the reviewer read. The publish
  gate (`daemon/internal/publish/verification_section.go`) binds a report by
  head and base only and words its section as run at the head, so it must
  refuse or name a report that carries an evaluated commit before one can
  reach it.
- An evaluated commit can come from anywhere but the daemon's own merge
  build. The verifier would then need to bind the tree, for example against
  `git merge-tree`, and not only the parents.
- #1230 changes which image verifies a merged tree. The re-entered cycle
  keeps its admission's image today.
- A second optional member joins the `CheckProof` digest body. Two optional
  members make "same version" hard to reason about; fold both into a bumped
  encoding version at the next required-field change.
