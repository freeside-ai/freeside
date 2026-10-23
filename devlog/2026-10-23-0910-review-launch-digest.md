# Stamp a Collected Review With the Configuration It Launched Under (#2197)

Work unit: #2197. Scope: `daemon/internal/ward`, `daemon/README.md`, and
`devlog/`. Collection now trusts a field it reads back from the journal, so
this is returned-object trust-boundary work and the note is mandatory.

## The Defect

Found by the #2196 refute-first review
(`devlog/2026-10-22-1700-review-connector-paths.md`). A review container
that exits while the daemon is down is collected after restart, and
`normalizeCollection` stamped the result with the running daemon's
configuration digest. The engine's acceptance check compares that stamp
with the approved digest, so after an upgrade that changes the review
command (as #2196 does) a review that ran under the old, unapproved
configuration was recorded under the new approval. No journaled record
carried the launch-time digest.

## Decisions

**Journal the digest in the binding, over re-deriving it at collection.**
The digest covers the command, model, effort, image, workspace size, auth
mode, identity, and cost owner. A live container exposes only part of that,
so the value the launch was approved under is the only complete record. The
binding is an opaque JSON body in `codex_review_bindings`, so the field
needs no migration and no shared type; it is not a contract change.

**Refuse a mismatch as transient, over contradiction or configuration.**
The reviewer contradicted nothing: the daemon changed under it. A
configuration failure would park the run for the operator, but the current
configuration may well be approved, and if it is not the engine's existing
gate parks the rerun anyway. Transient makes the engine advance the round
and rerun under what the daemon runs now. No result is normalized for the
refused invocation, so nothing can be recorded under the new approval. The
engine's own digest check stays as a backstop.

**Refuse before classifying any collected outcome, not only a clean one.**
An invalid output or a memory-limit kill under the old digest is that
configuration's failure. Workspace size is part of the digest, so a kill at
the old size says nothing about the new one; recording it as a
configuration failure would park a run that a rerun could finish.

**Fail closed on a binding with no digest, over refusing it as invalid
topology.** A binding written before this fix decodes with the field empty.
Making the field required in the binding's own validation would also fail
teardown and recovery, which authenticate the same binding, and would wedge
an exited review's topology. Instead the binding stays valid for cleanup,
and collection treats the empty digest as a mismatch: the review is
refused, cleaned up, and rerun.

**Keep the retained collection at three fields.** `collection_evidence`
hashes exactly the exit status, result, and events. The digest travels on
`CodexReviewCollection` and is copied out explicitly, so every persisted
outcome's evidence still verifies.

## Refute-First Record

A fresh-context reviewer tried to break the full change.

- **"Another path builds a ReviewResult without the check."** Disproved:
  `normalizeCollection` has one non-test caller, reached only when the
  digests match; the invalid-output and memory-limit branches sit behind
  the same check. The Claude source wraps the same `CodexReviewSource`.
- **"Teardown or recovery now rejects a pre-fix binding."** Disproved:
  `validateShape`, `validateForTeardown`, and the cleanup comparisons never
  read the digest; the new comparison runs only inside launch, against that
  launch's own request.
- **"The launch intent digest changes and breaks recovery."** Disproved:
  `codexReviewIntentDigest` hashes an explicit field list without it.
- **"Persisted evidence changes."** Disproved: `retained()` copies the
  three hashed fields, and the outcome goldens are unchanged.
- **"The outcome golden fixtures break."** Confirmed and fixed: the shared
  golden collection, and three normalization tests that build a collection
  by hand, carried no digest, so their normalized results failed
  validation. Each now journals its source's digest, as a real collection
  does.
- **"A refusal always reruns."** Confirmed as stated too strongly: the
  refusal advances the round like any transient failure, so one in the last
  allowed round exhausts the review. `daemon/README.md` says so.
- **"Rolling back to a pre-fix build wedges a new binding."** Allowed: the
  binding decoder refuses unknown fields, so an older build refuses a
  binding that carries the digest, as it would any added binding field.
  Upgrades are unaffected because a missing field decodes empty.

Revisit when a review provider stops sharing `CodexReviewSource`, or when
the engine's digest check moves into the source.
