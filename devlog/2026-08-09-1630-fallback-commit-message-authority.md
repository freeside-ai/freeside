# Fallback Commit Message Authority

For #614, chose the attended fake-publication task's durable `Title` and its
task-derived `Run.SpecDigest` over loading that digest as an approved
specification artifact. The production path still derives the title only from
the approved, digest-verified spec blob and the issue suffix only from the
write-once work-unit declaration.

The issue's implementation-plan comment assumed the fake task's
`Run.SpecDigest` addressed spec bytes in the blob store. It instead hashes the
whole private task payload, and the task predates both production specification
artifacts and work-unit declarations. Treating that payload as Markdown would
mislabel task JSON as an approved spec. Adding new spec and declaration fields
to the legacy task was rejected because the task already carries an
operator-approved title inside the same immutable digest binding, has no
issue-bound workflow semantics, and widening its durable schema would add a
migration solely for a test-era lane.

Revisit when attended fake publication adopts production submission artifacts
or work-unit declarations; at that point it should use the production
specification derivation without a separate title path.
