# Shared Paths and Independent Work

Work unit: #1393.

## Decision

Chose inspection of intended changes over automatic blocking on shared paths.
The owner requested this correction after #1373 stopped before implementation
because #1371 also touched screenshot tests, baseline resources, and
`app/SURFACES.md`. Those files contain separately editable cases, keys, and
rows. Their filenames alone did not establish a dependency between the units.

The old gate said any declared-path overlap meant stop and coordinate before
going further. It supplied neither a conflict test nor a completion condition
for coordination. An agent could therefore treat an occupancy claim as ownership
of every shared file and wait for a merge even when the edits were independent.
The July 13 Wave 0 decomposition already distinguished additive overlap in
different decision-note files from a coordination conflict; the gate did not
preserve that distinction explicitly.

Independent edits now proceed in isolated checkouts after the issue records
their concrete boundaries and why neither needs the other's unfinished result.
That record needs no acknowledgement when both existing scopes remain intact.
An agent cannot create independence by unilaterally narrowing another unit's
scope. It must inspect planned work as well as the current diff, and reconsider
the assessment when scope or base changes.

A real conflict pauses the affected work, with the competing behavior or
content and necessary resolution named. Competing edits need not depend on
each other's results: independent replacements of the same policy paragraph
can still impose incompatible requirements. Existing whole-unit gates apply:
authorization, claims, reservations, explicit typed relations, and serialized
shared-contract work. The unknown-dependency fallback does not turn a shared
filename into a dependency. Integration still preserves both changes and
revalidates evidence on the new base.

## Rejected Alternatives

- **Require acknowledgement for all shared files.** This replaces the blanket
  stop with a permission wait and leaves the reported failure intact.
- **Use disjoint diff hunks as the test.** Different files can change the same
  behavior, and an unfinished diff does not show all intended work. Separate
  screenshot keys can still depend on a changing shared renderer.
- **Wait for merges to avoid textual conflicts.** A resolvable textual conflict
  is integration work, not evidence that implementation must be serialized.
- **Split every shared test or documentation file.** File layout may improve
  independently, but it cannot substitute for assessing semantic dependencies.

Revisit when units that passed this assessment repeatedly collide semantically,
or agents continue to require acknowledgement for demonstrably independent
edits. Keep the existing concurrency capacity and integration role unchanged.
