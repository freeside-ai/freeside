# Retire the Attended Fake Publication Lane

Work unit: #2786. Mandatory note: an owner decision on a lane that
publishes to GitHub, which would otherwise exist only in chat. No code
changes here; the removal is planned in #2790.

## Decision: Retire It

The owner decided on 2026-11-07 to retire the attended fake publication
lane: the `-fake-publication` flag, the engine's copy of the publication
workflow behind it, and the doc mentions. The `fakepublication` decode
that store migrations call stays, so old rows still migrate.

Chose retirement over keeping the lane:

- **Nothing operational uses it.** Only the one-shot
  `-fake-publication` flag drives it (`cmd/freesided/main.go:254`), plus
  tests. No script, app code, CI job, or skill runs it. Exit runs use
  `freesided deputy`, and scenario mode uses the production workflow.
- **It publishes for real through a copy.** The lane calls the real
  publisher (`engine/fake_publication.go:1273`,
  `PublishAfterGateAndFinalize`) with GitHub App credentials built at
  `cmd/freesided/fake_publication.go:129-186`. Its copies differ from
  production: its outcome re-check (`fake_publication.go:1490`, against
  `production_publication.go:4634`) accepts a candidate with no
  `TrustProfileDigest`, and its verify call passes no control-pattern
  policy (`:1722` against `:5009`). A second publish path with weaker
  checks is cost with no user.
- **It taxes production changes.** It ships in the production binary,
  `ConvergeLegacyFakePublicationPolicies` runs on every Reconcile
  (`engine.go:261`), and about a quarter of the 116 PRs that touched
  `production_publication.go` (28 or 29, by two counts) also touched the
  lane. Deferrals #1007 and #1224 track drift between the copies, and
  #309 covers fake-lane review gaps along with reconcile starvation.
- **No recorded decision keeps it.** #559 treated the lane as permanent
  under plan §5.3, but §5.3 makes only the StageDriver and ReviewSource
  fakes permanent, not this workflow.

Chose to file one planning unit over a direct removal: about 12
integration test files build on the lane's `publicationHarness`, which
has to move off lane code first, and whether any database holds fake-lane
state that the Reconcile converge step still needs is unverified. #2790
carries both questions to its `Plan #N` session.

## What Is Given Up

An attended way to publish a fake candidate by hand. The production
workflow, driven through `freesided deputy` or scenario mode, covers the
same publish path with the real checks.

## Revisit When

Revisit if a need appears to exercise publication without a production
candidate that `freesided deputy` and scenario mode can't meet.
