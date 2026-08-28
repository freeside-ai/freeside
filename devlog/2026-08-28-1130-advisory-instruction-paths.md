# Publish Reviewer-Instruction Edits as Advisory Findings

Decided by the user, 2026-08-28, during the wave-6 (1B.0) exit run on
tracker #835. Plan revision 42; promoted to ADR 0002 because it revisits
the revision-4 decision "Reviewer-instruction poisoning closed".

## Decision

Chose a third finding disposition, `advisory`, for reviewer-instruction path
findings over keeping them publish-blocking and non-waivable. Detection is
unchanged and remains a mandatory, widen-only minimum. An advisory finding
never blocks publication, never carries a waiver, and is rendered by the
publisher in its own PR-body section that candidate prose cannot forge. Every
other control-plane category stays blocking and non-waivable; the domain
predicate `ControlPlaneCategory.Advisory` is the single place that names the
advisory set, so a new category must take a stance.

## Why the Block Was Revisited

The exit run's first real work item needed an `AGENTS.md` edit and hit a
definitive `reviewer_instruction_path` publish-block with no configuration,
human override, or exception. That is by design in three places: plan §3.1's
non-waivable list, the §5.8 poisoning paragraph, and
`CandidateFindingClass.Waivable`. The block's stated rationale was review
independence: an automatic review is not independent when its PR edits the
instructions governing it.

That rationale is now satisfied mechanically, which is the changed
assumption. Since #709 and #713, the Freeside-invoked reviewer composes its
instruction bundle from the exact trusted base
(`daemon/internal/exec/review_instruction.go`), and since the §5.8
`--safe-mode` topology the implementing agent does too, via ward's
digest-bound `--append-system-prompt-file` bundle. A candidate's edited copy
is diff content only. The remaining leverage of an instruction edit is that,
once merged, it governs later runs; the human merge gate already reads every
merged change, and the surfaced advisory makes the instruction edit visible
there without reading the diff.

## Rejected Options

- **Human waiver flow.** The principled shape, but no waiver producer exists
  and the verification block is terminal (`completeBlockedTask` ends the run;
  recovery re-derives the same item). It needs a new `Action`, a
  `WaiverRecord` producer bound to a human author, and a resumable hold in
  place of the terminal block: a multi-unit contract build, not a fix that
  lands during an exit run. Revisit if a second control-plane category ever
  warrants a human door; advisory should not widen to cover it.
- **Per-repository policy key** (`candidate_instruction_changes: block |
  advisory`). Adds a `ProtectedPathConfig` field, which bumps the trust-profile
  encoding version and makes every stored profile fail its digest recompute
  until a human re-records it (§5.5 drift recovery). Too disruptive mid-exit,
  and the default would still have to be decided.
- **Pin the reviewer to the base.** Already the design and the
  implementation; re-proposing it was a misdiagnosis of the mechanism.
- **Status quo: operator-authored PRs only.** The plan's documented route for
  configuration changes. Coherent for Freeside-owned configuration (prompts,
  policy, recipes, egress and trust profiles, materiality rules), which stays
  blocking; wrong for vendor instruction files, which are repository content
  that routine work maintains.

## Boundaries Kept

- The block for the specification profile is unchanged: an instruction path
  inside a specification import stays fatal (`fatalForSpecification`).
- GitHub-native Codex review reads the PR head and so can see the candidate's
  edit; §7 already records it as best-effort extra evidence that never
  satisfies the review requirement, so nothing load-bearing changed.
- The advisories section is bounded (32 entries, remainder counted) so a
  candidate touching many instruction files cannot push the composed body
  past the forge limit and fail its own publication; the complete set stays
  bound in the candidate authorization.

## Verification Findings (Refute-First Pass)

Recorded per `docs/agent-workflow.md` §refute-first because the change moves
a safety-policy boundary. A fresh-context reviewer was briefed to refute the
diff; each finding's disposition is below, and fixes are folded into their
owning commits.

Confirmed and fixed:

- **The first cut only unblocked the fake publication path.** The stage
  pipeline's default (nil) finding profile treated every import finding as
  fatal (`daemon/internal/exec/stage/pipeline.go`, `partitionFindings`), so a
  production export with an instruction edit was still definitively rejected
  before any advisory existed, and production reconstruction demanded zero
  findings (`production_publication.go`, the head-and-findings check and the
  remediation checkpoint check). Fixed by making `Finding.Fatal` exempt
  advisory kinds under the default and publish-strict profiles and by
  accepting an all-advisory finding set as clean in both engine checks;
  covered by `TestPublishStrictToleratesAdvisoryFinding` and the production
  integration test `TestProductionReviewerInstructionEditPublishesAsAdvisory`.
- **The rendered advisories were caller-asserted.** The publisher trusted
  `Candidate.Advisories`; a caller passing a subset could under-report. Fixed
  by requiring the set to equal `AdvisoryFindings` of the re-validated
  authorization in `validateAuthorizationCandidate` (the #52 re-gate).
- **The entry cap did not bound the section in bytes.** Paths are up to 4 KB
  and escape up to five-to-one, so 32 entries could exceed the 64 KB forge
  limit and fail the publication the section exists to describe. Fixed with
  eight entries of 512-byte bounded claims (ceiling pinned by test).
- **Candidate prose could carry the section heading** above the real one.
  The body validator now refuses the heading as well as the marker, and the
  duplicate validator in `daemon/internal/fakepublication` was aligned.
- **Section boilerplate over-claimed on the fake path**; reworded to the
  general statement.

Disproved by inspection (recorded so the claim does not return):

- The authorization encoding version needs no bump: a new value in an
  existing field is not a field-set, ordering, or separator change, and an
  older binary decoding `advisory` fails `valid()` closed.
- No downgrade path: `Validate` rejects advisory outside the one category,
  `NewCandidateAuthorization` and `Validate` re-derive both the ID and the
  authorizing bit, and store decoding re-runs `Validate`, so a forged row,
  backup restore, or hand-edited JSON fails closed.
- No other consumer sees the new value: no store column, migration, OpenAPI
  schema, or app type carries `FindingDisposition`.
- Base pinning holds: ward composes the agent bundle from the seed snapshot,
  and production composes reviewer instructions from the checkout before the
  candidate is imported into it.
- Escaping is adequate: claims strip CR/LF and HTML-escape, and the category
  label is an enum.

## Revisit When

- A waiver producer and a resumable publication hold exist: the advisory
  stance could then become "advisory with an explicit acknowledgement" if
  the merge gate proves too coarse.
- A second control-plane category argues for advisory treatment: that is a
  new decision, not an extension of this one.
- The reviewer or agent instruction composition ever reads from the candidate
  head: the independence argument collapses and the block must return.
