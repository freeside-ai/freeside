# Preflight Checks The Selected Writer Image

Mandatory note: returned-object trust-boundary work and an additive
composition-manifest contract change (#2401).

## Decisions

Chose the daemon's shared writer-selection rules over preflight's separate
base-admission gate. The owner assigned #2401 to remove the discrepancy
recorded in `2026-10-27-1646-writer-image-selection.md`: preflight could
refuse a base that the daemon starts on, or probe a seed the writer no
longer selects. The earlier choice to keep preflight stricter was temporary;
this issue explicitly replaces it.

Preflight reads the seed, admitted-image pointer, and recorded candidates
in one read-only production transaction. Store reconstruction validates
the rows and pointer consistency. The shared selector then re-gates each
candidate against the base and this binary's preparation sources. Seed
record checks still run first. With no pointer recorded, the seed stands
in for the pointer an unattended start would create.

Chose an optional `inspected` image field over changing what `requested`
means. It names a differing probed image, and `resolved_digest` belongs to
that image. The manifest version stays v2 because the field is additive
and no repository consumer parses the manifest. A base no image serves
passes only after the pointed-to image passes the recipe and tool checks;
the evidence names the `image_upkeep_pending` writer hold.

Selection deliberately supplies no proved image, as #2401 requires. A
restored proof can prefer another serving image, so preflight proves the
serves-or-holds verdict and probes its selected candidate, not necessarily
the exact image a restoring ward starts on. The lineup probe remains on
the seed, under the existing assumption that rebuilt images retain its
base image.

## Refute-First Findings

- **Disproved selection drift:** the original and extracted daemon
  selectors agreed across 2,304 generated cases on the returned image,
  found flag, error text and classification, and base-read count. Cases
  varied recorded inputs, proved image, repository, pointer absence, base,
  and read failures. This was a temporary Go-overlay comparison, not a new
  production test helper.
- **Disproved unchecked decoded state:** fresh-context review traced the
  store reconstruction and engine selection gates and found no bypass in
  the new preflight path.
- **Disproved a hold hiding probe failures:** fixture-level tests show a
  non-serving pointer passes only when its probes pass, and that the
  manifest names the actual inspected image and digest.

Revisit when #2338 changes what rebuilt images preserve, or when startup
restores an upkeep verdict that changes the hold named here.
