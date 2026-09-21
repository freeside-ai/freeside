# Recipe v2 Author Run, Stored Artifact, and Replay (Issue #1419, Part B)

Part B of #1419: the publication-author role runs once per reviewed candidate,
its screened prose is stored as a `domain.PublicationAuthoring` artifact, and
every publication path re-renders it byte-identically. An unavailable author or
a screen failure records a durable v1 fallback. Nothing freezes a v2 record in
production yet (Part E); the engine only renders one when a record froze v2.
This note records the trust-boundary and design decisions; Part A's screen and
renderer note stands.

## Keyed the Authoring Answer by Run, Head, and Base

Chose an engine-private inbox checkpoint keyed by `(runID, headSHA, baseSHA)`
over reusing the verification checkpoint key (head plus command context, no
base). A reviewed candidate is one head on one base (§7); a base advance voids
the review, so it must get a fresh author run and must never render the
artifact written for the old base. Binding the base into the key makes both
automatic: the new base selects a new key (no artifact yet, so the author
runs), and the render path reads the checkpoint for the current base, never the
old one. Rejected relying on the load-time base revalidation the verification
checkpoint uses: that rejects a stale load but does not, by itself, give the
new candidate a distinct authoring identity.

## Author-Role Faults Record a Durable v1 Fallback; Only Store Faults Propagate

Chose to record a durable fallback checkpoint for every author-role fault
(inference unavailable, inference error, an explicit fallback result, a screen
refusal, or an input-resolution failure) and to let only a store-write fault
propagate as a retryable error. This keeps the contract "publication is never
blocked by the author role": an author-role fault always yields a recorded v1
fallback, and a retry reads that checkpoint instead of calling the author
again. A store fault is infrastructure, not the author role, so propagating it
(retryable) neither blocks permanently nor silently masks an infra error, and
never re-calls the author once the checkpoint is written.

Accepted tradeoff: an input-resolution failure (a visibility read, an artifact
read, or the diff render) records a *permanent* v1 fallback for that candidate,
where the render path treats the same failure per-pass. Accepted because these
reads are against a deterministic candidate checkout, the local store, and the
same publisher the publish path already uses, so a failure is structural and
would recur; never-block dominates. Revisit if a transient input-read failure
is observed downgrading a candidate that would otherwise author cleanly.

## The Screen Is Syntactic; the Sensitivity Class Guards Content Sensitivity

`screenAuthoredText` runs the full public-output screen on every authored
artifact and again on every render of the reconstructed artifact: the screen
policy can tighten between authoring and a later replay, and the store re-gates
evidence, digest, and class but not the text screen, so `renderStoredAuthoring`
re-runs the screen (and the single-line title contract) and fails closed to v1
on refusal. The screen catches known secret and directive *patterns*, but it is
syntactic: it cannot recognize confidential context the model paraphrased into
ordinary prose. So the screen alone does not make sensitive-authored prose safe
on a more open repository; the derived sensitivity class is the load-bearing
control for content sensitivity.

The render therefore gates the class against the repository's current
visibility. With a successful read it falls back to v1 when the repository is
now more open than the stored class allows
(`storedClass.MoreRestrictiveThan(freshClass)`). With a *failed* visibility read
the current openness is unknown, so it fails closed to v1 for any
sensitive-or-higher class (a first publish after a private -> public transition
must not put private-authored prose on a now-public PR) and keeps the stored
class only for normal, public-grade content, which is safe under any visibility.
That last case preserves byte-identical replay for the common public-repo path
without trusting an unconfirmed visibility for sensitive content: leak-safety
outranks byte-identical replay, which assumes stable repository state.

## Returned Object Is Never Trusted (refute-first)

The inference-returned `AuthoredPublication` reaches a PR only after:
`domain.NewPublicationAuthoring` validates the fields and computes the digest
(never caller-supplied); `screenAuthoredText` screens title, body, reviewer
notes, and outcome summary; `PutPublicationAuthoring` re-runs the evidence gate
and `Validate` under the current approved-recipe set; and `GetPublicationAuthoring`
re-runs the evidence gate on every render. `Producer` and `InputDigest` come
from the inference `Call`, not the model. `EvidenceRefs` are resolved against
the evidence the engine supplied and re-gated by the store (publish-eligibility
and class). `TargetClass` is derived from the visibility the engine supplied,
not model-controlled.

## RepositoryClass Returns a Domain Class, Not a New Visibility Type

The publisher's new read maps GitHub visibility to `domain.SensitivityClass`
(public -> normal, private/internal -> sensitive, unknown -> error). Chose this
over a new domain type (a non-goal for this unit) or a duplicate publish-side
visibility enum; the engine maps the class to `inference.RepositoryVisibility`
for the author input and compares classes directly at render time.

## Refute-First Findings

A fresh-context reviewer attacked the diff to prove it wrong. Outcomes:

- **Trust boundary holds (disproved attack).** The model controls only the four
  text fields, all screened twice and rendered inert; `TargetClass`,
  `EvidenceRefs` digests, `Producer`, and `InputDigest` are engine- or
  infra-supplied, not model JSON. The hypothesis that a model could under-report
  its class to defeat the openness check was refuted: the class is derived from
  the engine-supplied visibility.
- **Fail-closed visibility (input builder).** A `RepositoryClass` error in the
  input builder records a v1 fallback, so authoring never proceeds on an unknown
  visibility.
- **Class-conditioned visibility on render (revised).** A visibility read
  failure at render is handled by the stored class, not unconditionally. For a
  normal (public-grade) artifact the render keeps the stored class and replays
  v2: public content is safe under any visibility, and this preserves
  byte-identical replay of an already published public PR through a transient
  outage (the valid core of the earlier "must not flip a published v2 PR to v1"
  fix). For a sensitive-or-higher artifact the render fails closed to v1: the
  screen is syntactic and cannot vouch for paraphrased confidential content, so
  an unconfirmed visibility must not risk private-authored prose on a first
  publish after a private -> public transition. Two integration tests pin both
  arms: a public-repo outage keeps v2, a private-repo outage renders v1.
- **Allowed: a store-gate rejection at author time propagates as retryable.** If
  cited evidence loses approval in the sub-120s window between the build filter
  and the store write, `PutPublicationAuthoring` rejects and the pass retries;
  the retry re-runs the input builder against the current approved set, drops the
  now-unapproved evidence, and authors cleanly. This is a one-cycle retry (not a
  durable block, and preferable to a permanent v1 fallback that a later
  re-approval could not undo), consistent with "only a store fault the daemon
  should retry propagates."
- **Allowed: no nil-guard on `w.publisher`.** No path constructs the workflow
  with a nil publisher, and the existing workflow calls the publisher unguarded
  elsewhere; a guard here would be hardening for an unreachable state.
- **Fixed: the decoded authoring checkpoint is validated against its key.**
  `loadAuthoringCheckpoint` re-checks the decoded `Version`, `RunID`,
  `HeadSHA`, `BaseSHA`, and the `Fallback == (ArtifactDigest == "")` invariant
  against the candidate identity the key encodes, failing closed with
  `ErrParentKeyMismatch`. The stored row is untrusted at this reconstruction
  boundary: a restored or corrupted row under the current key could otherwise
  render prose authored for a different head or base, or suppress authoring
  permanently with a forged fallback bit. On mismatch reconcile retries and the
  render path falls back to v1.
- **Fixed: the PR template is read from the base commit's objects, not the
  FetchBase worktree.** Production `FetchBase` materializes no working tree, so
  `readPRTemplate` reading filesystem paths always returned empty and only
  appeared to work behind the materialized integration transport. It now
  resolves the four conventional template paths from the base commit via
  `git cat-file blob`, so the author follows the repository's required PR
  sections.
- **Fixed: the stored artifact is re-screened on every render.** The store
  re-gates evidence, digest, and class on read but never the text screen, so a
  screen-policy tightening between authoring and a later replay could otherwise
  publish now-banned prose. `renderStoredAuthoring` re-runs `screenAuthoredText`
  on the reconstructed artifact and falls back to v1 on refusal, keeping the
  screen the leak guard at the render boundary too.
- **Fixed: the visibility decode fails closed on an untrusted response.**
  `RepositoryClass` mapped an empty or partial forge response (`200 {}`, which
  decodes to no signals) to the least restrictive normal class; combined with the
  normal-class render exception above, that could label a private repository
  normal and later publish private-derived prose. The mapping now treats
  `private` as optional and requires at least one visibility signal; a missing
  signal returns an error the caller handles as an unavailable read (author-time
  v1 fallback; render fails closed per stored class). The only contradiction it
  rejects is `public` with `private=true` (the sole pair that could wrongly
  downgrade a restricted repository to normal); a visibility that maps to
  sensitive is safe regardless of the private bool, so `internal` with
  `private=false` (a legitimate GitHub pair, since internal is org-visible, not
  private) is accepted as sensitive. This is the documented `unknown -> error`
  behavior made real.
- **Fixed: the instruction snapshot is verified against its bound digest.**
  `readArtifactText` re-hashes the bytes the store returns and rejects a mismatch
  against the requested digest, matching the launcher's blob reconstruction. The
  content-addressed store does not guarantee a read matches its key (a corrupted
  blob or an incorrectly restored row), so an unverified read could substitute
  trusted control-plane instructions; a mismatch becomes a v1 fallback.
- **Fixed: the authored title holds the single-line contract.** The model
  controls the title and neither the explain-site schema, the artifact
  constructor, nor the screen enforces the one-non-empty-trimmed-line contract
  that literal and v1 titles hold. The title is validated before storing (record
  a fallback on violation) and again on render (fall back to v1), so a multiline
  or padded title never becomes malformed forge metadata.

**Revisit when:** Part D adds the closure proposal and the publisher draft
hold; Part E freezes v2 for new client submissions and adds the intake recipe.
When the EvidencePublisher lands, evidence references become active links
(Part A note, item 8). #1425 and #1428 may move the author sites or prompts;
recheck the input builder in `publication_authoring.go` if either merges first.
