# Publication Candidate Composition Is One Helper

#2857 moves the candidate steps that `reconcileTask`'s recovery arm and
forward tail both ran into `composePublicationCandidate`
(`production_publication.go`): `productionCandidate` with its
public-metadata hold, the handoff binding, `bindSuccessorCandidate`, and
the disposition-history load with its two holds. Each caller passes the
handoff check it chose. `applySealedHandoffCheck` lost its only caller
and is gone. Behavior is unchanged apart from one moved read and one
error text, both below. This is link 3 of the publication-states chain
(#2761's plan comment); link 2 is
`2026-11-09-2147-publication-task-progress.md`.

## Decision

- **The handoff check is a nullable input, not a choice the helper
  makes.** Forward passes the check `reconcileHandoff` just returned,
  which is never empty (an intent committed before handoff checks gets a
  synthesized check at the evidence base). Recovery passes the sealed
  check, or nil when none was sealed, which keeps the evidence base.
  Rejected: a boolean or mode argument that has the helper read or
  synthesize the check itself, which would hide the one real difference
  between the branches inside the shared code.
- **Authoring and closure stay in the forward caller.** They run before
  the candidate is composed and need the verification report and the
  handoff tip, so the helper takes no input for them. Rejected: the
  issue's first draft, which gave the helper an authoring-and-closure
  switch; with them running first, the switch would have had nothing to
  switch.
- **The report load and the adopted-profile read stay in the callers.**
  Forward needs the report before authoring and reads the adopted
  profile after the review gate; recovery reads both at its own points.
- **Recovery's sealed-check read moved ahead of `productionCandidate`.**
  The helper binds the handoff right after composing the metadata, so
  the caller has to have the check first. Rejected: passing a loader
  callback to keep the read after `productionCandidate`, which buys
  nothing (Refute-First Findings).
- **One disposition-history error text.** Recovery's
  `load recovered publication disposition history` and forward's
  `load publication disposition history` became the latter. No test,
  doc, or caller matches either text.
- **A restart test pins recovery's binding first.** No test restarted
  after the publication effect at an advanced target, and removing
  recovery's sealed-check binding left every handoff test green.
  `TestHandoffRecoversAPublicationAgainstTheSealedTip` stops right after
  the effect with a policy-approved close bound to the merge into the
  advanced tip, restarts, and asserts recovery leaves the published body
  unchanged. With the binding removed, the recovered candidate renders
  the close against the evidence base and rewrites the body, and the
  test fails. It passed before and after the refactor.
- **The recovery arm still serves both intent finalized and ready.** The
  composition is identical for both states, so this unit had no reason
  to split the arm by state, which link 2's note left open.

## Refute-First Findings

Tier A by path (`production_publication.go`, `production_handoff.go`).
Each claim below tried to show the change wrong.

- **"The moved read changes a decision."** Disproved by reading. Store
  and artifact reads in the recovery arm from the report load to the
  disposition-history load, in call order:
  - Before: verification report (artifacts), `productionCandidate`
    (`publicationTitleBody`, `applyClosureReference`), sealed handoff
    check, successor target (only for a successor), disposition history.
  - After: verification report, sealed handoff check,
    `productionCandidate`, successor target, disposition history.

  Every step is a read, so nothing is written between the old and new
  positions. Earlier in the same arm, `withPublishedHandoff` calls
  `loadHandoffCheck` and returns on its error, including the head
  mismatch it raises, so by the new position the read has already
  succeeded once this pass. The forward tail's reads are unchanged.
- **"A failing pass now does something different."** Confirmed and
  allowed: if the sealed-check read fails on a transient store error and
  `productionCandidate` would also have failed, the pass now returns the
  read error instead of writing the public-metadata hold. The next pass
  decides again.
- **"The binding order changed what the metadata renders."** Disproved
  by reading: `productionCandidate` (with `publicationTitleBody` and
  `applyClosureReference`) still runs before the handoff is bound, and
  `bindSuccessorCandidate` and the disposition-history load still run
  after it, in both branches.
- **"A hold or its error changed."** Disproved by reading: each hold
  keeps its function, reason, and `checkpoint.Imported`, and the helper
  returns the hold's outcome and error together; both callers return
  `derefOutcome(held), err` exactly as forward already did for
  `reconcileHandoff`.
- **"The new test doesn't reach the recovery arm or doesn't pin the
  binding."** Disproved by check, twice: with recovery's binding removed
  on unchanged code, and with the refactored recovery passing nil in
  place of the sealed check, the test fails on its body assertion.

A fresh-context reviewer tried each claim above against the diff and
confirmed none. It also found a defect that predates this change:
`applyClosureReference` returns a transient closure-checkpoint read
error, which `holdPublicMetadataTask` turns into a durable trust hold,
despite its doc saying it never blocks. Follow-up: #3001.

## Revisit When

- `withPublishedHandoff` moves after the candidate composition or stops
  reading the sealed check. Then the moved read is the arm's first, and
  its failure needs fresh analysis.
- A branch needs a step the other doesn't, between metadata and
  disposition history. Then it belongs in that caller or as a new helper
  input, not as a branch inside the helper.
