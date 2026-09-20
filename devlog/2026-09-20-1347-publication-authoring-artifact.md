# Publication-Authoring Artifact and Its Store

Work unit #1447 (kind:contract). Defines `PublicationAuthoring`: the advisory,
schema-validated, producer-labeled, digest-addressed record of the prose the
`explain` site writes for a publication, plus its store persistence (put, get
by digest, list by run). Split from #1417 steps 7-8; see
[the closure note](2026-09-20-1104-closure-effect-and-approval.md). This note
records the lasting contract decisions and the refute-first findings for the
returned-object trust boundary.

## Stored in `daemon/internal/store`, Not the Advisory Store (Departs From Plan)

Chose `daemon/internal/store` over `daemon/internal/advisory`, departing from
plan §9 ("kept in the advisory store") and §5.13 ("retains its advisory
artifact under the advisory-store retention"). The advisory store drops an entry
once its `RetainUntil` passes and keeps only the newest `maxEntries`. #1419 must
render the same bytes on every retry, restart, and drift repair, for as long as
a publication PR stays open, so the artifact needs storage that never prunes.
The store is that storage; #1417's plan already proposed it. The plan wording is
a separate issue (linked from the PR); this unit does not edit `docs/plan.md`.
Rejected: keeping it advisory and having #1419 tolerate loss, which would make
the rendered PR body non-deterministic across a restart.

## Its Own Type and Table, Not a `domain.Artifact`

Chose a standalone type with its own table over reusing `domain.Artifact`.
Reusing `Artifact` would pull the prose into evidence snapshots, the
publish-eligibility computation, and the synced artifact list, where policy
evaluation could reach it (plan §5.13 wants it kept out). A distinct type with
no `publish_eligible` bit makes that structurally impossible rather than merely
avoided. Cost accepted: a parallel put/get/list surface and a second immutable
table, modeled on `FindingAdjudication`.

## Producer Label: Site, Producer, Input Digest, No Invocation ID

The producer label is `{site, producer, input_digest}` with no
`domain.InvocationID`. The author runs as an inference call, not a ward
invocation, so no invocation identity exists for it today. The input digest is a
content address of the call's inputs; it also lets #1419 tell whether a stored
artifact still matches the run's current inputs after a base advance. Rejected:
inventing an invocation ID, which would fabricate an identity the runtime does
not mint.

## Evidence Gate Checks More Than Publish Eligibility

The store gate re-resolves each evidence reference through `GetArtifact` (which
re-runs `domain.ValidatePublishEligibility` against the current approved-recipe
set), then requires a matching digest, `PublishEligible` true, and a sensitivity
class no more restrictive than the authoring artifact's target class.
`ValidatePublishEligibility` alone is insufficient: it passes a legal artifact
whose `PublishEligible` is false, so it would let the prose link evidence that
cannot be published. Publish eligibility ignores sensitivity, so the class check
is separate; it needed a new `SensitivityClass.MoreRestrictiveThan` ordering
(none existed), which fails closed by ranking an invalid class above every valid
one. The single gate runs on put and on every read (get and list), so no path
can persist or return an artifact whose prose cites evidence it must not.

## List Uses a Run-Filtered Read, Not a Full-Table Scan

Chose a `WHERE run_id = ?` read (served by the `(run_id, created_at,
content_digest)` index) over `FindingAdjudication`'s enumerate-all-and-filter
pattern. The reconstruction cross-check ties each returned row's decoded
`run_id` to the queried run, so a surviving row cannot belong to another run.
The residual omission risk (a row whose `run_id` column was tampered away from
its true run, hiding it from that run's list) is out of scope: this unit stores
what it is given, and #1419 decides staleness and completeness. The plan added
the run index specifically for this filtered read.

The oldest-first order is computed in Go from the decoded `time.Time`, not from a
SQL `ORDER BY created_at`. `formatTime` writes RFC3339Nano, whose trimmed
fractional part is variable width, so a text sort places `...05.5Z` before
`...05Z` and inverts two same-second timestamps. Sorting the reconstructed rows
by `(CreatedAt, Digest)` in Go is correct regardless of the stored text width;
the index still serves the run filter.

## Encoded Size Capped at the Persist Boundary

`Encode` rejects a marshaled body over `MaxPublicationAuthoringBytes`, the same
cap `DecodePublicationAuthoring` enforces on read. The per-field raw bounds are
not sufficient on their own: `json.Marshal` escapes control characters (a
valid-UTF-8 control byte becomes a six-byte `\uXXXX`), so a field within its raw
bound can expand past the decode cap. Without the symmetric put-side cap, a put
could persist an immutable row that no later read can reconstruct. Chose to
enforce it in `Encode` (the persist boundary the store's put uses) rather than in
`Validate` (on the hot decode path) or only in the constructor.

## Target Class Is a Caller-Supplied Contract Input

The evidence gate compares each reference against the authoring artifact's own
`sensitivity_class`, which the contract defines as the target-repository class
(issue body item 2) and this unit stores as given. The store cannot re-derive an
authenticated target class: no target-repository sensitivity class exists in the
domain (it lives only on artifact provenance; neither `Project` nor `Run` carries
one), and the publication target is bound by #1419. Binding the declared class to
authenticated repository state, and re-checking it when a repository changes from
private to public, is therefore #1419's responsibility (see Revisit when).

## Trust-Boundary Review Evidence

A fresh-context adversarial review tried to prove the gate wrong across seven
failure modes; all were disproved, no defect confirmed. Recorded so the same
claims do not return:

- **No ungated path.** All four SQL references to `publication_authorings` are
  in the store file; `reconstructPublicationAuthoring` is the sole decode path
  and always calls the gate; put gates before the immutable write, so replays
  are gated too. The type is not a `domain.Artifact`, so no snapshot, sync, or
  export reader can reach it.
- **Every lookup column tied to the body.** Reconstruct checks the body
  integrity digest, decodes and revalidates (recomputing the content digest),
  then rejects unless the decoded digest, run_id, and created_at equal their
  columns. A tampered column trips `errRowInconsistent`; for list it fails the
  whole read rather than reordering or mislabeling.
- **Sensitivity direction correct.** The gate rejects evidence strictly more
  restrictive than the target; equal is allowed; an invalid class ranks highest
  and fails closed. The target class is always valid (Validate gates it first).
- **Eligibility re-gated.** `GetArtifact` re-runs `ValidatePublishEligibility`
  against the current approved recipes; the gate additionally requires a digest
  match and `PublishEligible` true. A stored authoring becomes unreadable if a
  cited recipe later loses approval, by design.
- **No digest forgery.** Validate recomputes the content address on both paths;
  the constructor never accepts a caller digest; nil refs are normalized to `[]`
  so the digest is deterministic.
- **Foreign key enforced.** The store opens both pools with
  `foreign_keys=ON`; the migration is STRICT with the run foreign key and the
  list-order index.

## Revisit When

- Inference calls gain a durable call identity (#1425). The producer label can
  then carry it, and #1419 can bind an authoring artifact to the exact call that
  produced it rather than only to its inputs.
- A later encoding version is needed. Version 1 can never be dropped while a
  publication PR renders stored bytes (#1419), unlike `FindingAdjudication`
  which dropped its version 1.
- #1419 binds the publication target. It must set the authoring artifact's
  `sensitivity_class` from the authenticated target-repository class and re-check
  it against current repository state, since the gate trusts that field as the
  ceiling for admitting evidence and this unit cannot derive it.

Follow-up: #1454 (plan-wording fix for §9 and §5.13).
