# Publish Disposition History From Re-Derived Authority

Issue #525 makes the production pull request carry the review and readiness
derivation that authorized publication. This is a returned-object and
publication trust-boundary change, so the choices below are durable even
though the rendering itself is straightforward.

## Decisions

- Chose a publisher-owned, versioned, marker-delimited Markdown section over
  extending operator prose because retries and recovery need one canonical
  byte representation that the daemon can replace exactly without trusting
  prose as authority. The publication identity remains unchanged, while the
  durable publication intent freezes the section's content digest.
- Chose transaction-time reconstruction over accepting the engine's rendered
  snapshot because review records, final dispositions, failures, trust
  profile, authorization, and run policy are the authority. The loader derives
  readiness from those records and the compiled production requirement set,
  and the section names the resulting resolution, proof, and recipe digests.
  The publication decision transaction rebuilds the same section and compares
  its digest before committing the intent.
- Chose the latest review's exact base, reviewer configuration, and instruction
  digest as the current lineage. Older rounds under superseded authority remain
  durable store history but are excluded from this publication derivation;
  remediation rounds under the same authority remain included. Every finding
  in that lineage requires exactly one final disposition, and a fixed finding
  requires a later same-base, different-head remediation review.
- Chose exact recovery reconvergence over identity-only outcome verification.
  A finalized outcome or ready item reconstructs the current trusted history,
  validates it against the intent digest when present, and patches the existing
  PR back to the canonical title and body. Recovery never creates a replacement
  PR. Historical intents without the new optional digest remain recoverable,
  but still pass current authority reconstruction before their existing PR is
  repaired.
- Chose bounded failure for the composed PR body over truncating operator prose
  or publisher-owned structure. The complete body must fit the publisher's
  64 KiB limit, including operator prose, disposition history, and identity
  marker; content that remains oversized fails before an intent or forge
  effect.
- Chose a bounded, digest-addressed representation for each rendered finding
  and disposition claim, plus a 48 KiB aggregate section budget, over
  republishing complete text. Review results admit up to 1 MiB of third-party
  text and an unbounded finding count, while the forge body admits only 64 KiB.
  The durable records retain every complete claim. The published history uses
  deterministic prefixes with per-claim digests, and if their aggregate still
  crosses the section budget it preserves complete rendered lines plus a
  digest of the full canonical section.
- Chose escaped, explicitly labeled recorded claims for disposition rationale.
  Raw reviewer finding text is not republished, and marker-shaped text cannot
  escape into daemon-owned structure.

## Verification Findings

PR #705's automated review confirmed two P1 members of the same publication-
budget class. First, one complete persisted claim could exceed GitHub's 64 KiB
PR-body limit and make a cleanly reviewed candidate unpublishable. The initial
per-claim bound closed that case but exposed the wider input: the review-result
contract permits enough findings for individually bounded claims to exceed the
limit in aggregate. Both findings are fixed by deterministic claim and section
bounds whose omitted content is digest-addressed; regressions cover a 1 MiB
message and eight multi-field findings while preserving one matched marker
pair. A fresh-context refute pass then rejected aggregate-cap arithmetic,
UTF-8 line-boundary, marker closure, omitted-tail digest, and decision-time
reconstruction regressions against the final implementation.

Independent refute-first reviews found and closed five P1 gaps across two
passes: history originally loaded before the publication decision transaction;
crash recovery verified only publication identity and coordinates; the
transaction did not bind the engine's expected review-instruction digest;
legacy-intent compatibility stopped before finalization; and recovery could
patch content before authenticating the persisted PR number. The final design
reconstructs authority inside the decision transaction, carries the expected
instruction authority in the immutable snapshot, freezes the canonical digest
in the intent, applies narrow legacy compatibility at preparation and
finalization, binds the persisted PR number to the same forge observation used
for any repair, and reconverges exact content on recovery.

The same review rejected marker injection, duplicate-section, silent
truncation, missing or duplicate disposition, stale-head, stale-authority,
fixed-remediation, slice-mutation, and raw-reviewer-text hypotheses after the
corresponding validation and adversarial tests were inspected.

Automated review then confirmed two recovery-order gaps: a drifted PR could be
patched after current recipe authority was revoked, and ready-item recovery
could patch before authenticating the complete durable item. Recovery now
re-runs artifact eligibility immediately before a repair, and the ready path
authenticates the item before entering that repair. Regression tests prove
both refusals leave the external PR unchanged. The first pre-push refute pass
then found that recipe revocation was preserved at the mutation boundary but
reclassified as durable-state corruption by outcome reconstruction. Recovery
now preserves that policy refusal and records a recipe-revoked hold at all
three callers. The final refute pass found no remaining
mutation-before-authentication or undispositioned-refusal path.

A second automated-review pass found that the repair gate was still narrower
than the ordinary publication gate: it refreshed the artifact and recipe but
did not refresh trust-profile drift or authorization immediately before the
patch. It also found that committed-intent retries skipped readiness-proof
reconstruction for every existing publication, rather than only for historical
intents that predate the disposition-history digest. Repairs now rerun the full
identity, artifact, trust-drift, and authorization gate at the mutation
boundary. Modern committed intents reconstruct and validate their persisted
proofs on every retry; only legacy intents with no disposition-history digest
retain the compatibility skip. Regression tests prove that trust drift cannot
mutate the PR and that a damaged modern proof set fails before any forge
effect.

The final refute pass found two coverage gaps: authorization loss at the
repair boundary and the legacy proof-compatibility branch were not exercised
directly. Added tests now remove authorization after outcome authentication but
before the repair gate, proving a trust-blocked hold with no PATCH, and contrast
a modern missing-proof refusal with the narrowly compatible legacy retry. The
follow-up refute pass found no remaining actionable gap.

A third automated-review pass found one remaining mutation-boundary race: a
late review failure or disposition could land while recovery observed the
drifted PR, after the earlier history reconstruction but before PATCH. The
repair gate now performs a final store-backed disposition-history
reconstruction and exact digest comparison after its fresh trust and
authorization checks. A race regression writes an authoritative review failure
during the forge read and proves recovery refuses without mutating the PR.

A fourth automated-review pass found that the final history reconstruction
introduced a smaller trust race because it followed separate trust and
authorization reads. For history-bearing recovery, the mutation-boundary gate
now evaluates current trust drift, current authorization, and canonical
disposition history inside one store read transaction against the same fresh
workflow audit. The trust regression now changes a non-review profile axis
during the forge read and proves the atomic final snapshot refuses the repair.

A fifth automated-review pass found two final recovery-authentication gaps.
First, repair compared reconstructed readiness-proof digests without reading
the persisted resolution and proof rows. The same final authority transaction
now reconstructs those rows before PATCH. Second, field absence alone could
not distinguish a genuine pre-history intent from a damaged modern intent.
Migration 39 now gives every outbox row a store-owned payload version and
content digest, rewrites valid pre-migration publication intents with an
explicit v1 contract, and quarantines malformed ones. New publication intents
use v2; durable decoders require the payload contract to match the authenticated
outbox version. Legacy compatibility therefore applies only to rows attested by
the migration, while deleting or rewriting a modern history digest fails
closed even if the damaged payload is rehashed. The schema makes that migration
provenance monotonic: new and promoted publication intents must be v2, and an
existing payload version cannot be downgraded, so coordinated payload, digest,
and version edits cannot turn a modern row into a legacy one. Same-schema
checkpoint restore temporarily suspends only the new-v1 insert guard inside
its atomic copy transaction and reinstates it before commit, preserving genuine
migrated v1 rows without opening an ordinary write path.

The current runtime still produces no `ReviewDispositionRecord`: a review with
findings escalates before remediation. This work deliberately renders trusted
records when a disposition-producing convergence flow persists them; it does
not invent dispositions or change review mechanics, matching issue #525's
non-goals and its dependency boundary.

Revisit when the remediation/convergence producer lands, when publication
identity begins to include PR body content, or when the forge body-size limit
changes.
