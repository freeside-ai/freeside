# Evidence Metadata on the Sync Contract (#922)

Contract change: `EvidenceArtifact` and `AgentClaim` each gain a required
`EvidenceMetadata` object carrying the §5.15 daemon-validated facts
(`media_type`, `size_bytes`, `created_at`, `source`, `availability`), so
clients render an explicit evidence state from typed fields instead of
inferring it from a fetch outcome.

## Decisions

- **Nested `metadata` object over flat fields.** One named `EvidenceMetadata`
  schema (mirrored by `domain.EvidenceMetadata`) reused on both references,
  over five sibling fields duplicated on each. Keeps the shape in one place and
  lets `source` pin per container.
- **Read-time `availability` recompute in the sync projection over a
  persisted-only value or a wire-only DTO.** `availability` is derived from the
  blob store in `signet.projectEvidenceAvailability`, run per synced item
  immediately before serialization, on the same re-gate discipline as
  `publish_eligible`: a persisted value is never trusted for the wire.
  Producers stamp `available` at construction; the projection overwrites it. A
  nil blob store or an unresolvable stat projects `bytes_absent` (the
  conservative truth: a fetch could not be served). Chose this over a persisted
  value (would go stale as blobs come and go) and over a separate wire DTO
  (signet serializes `domain.AttentionItem` verbatim; a parallel DTO would
  duplicate the whole item shape). Cost: one `blobs.Has` stat per reference per
  serialized item, fine at bootstrap sizes.
- **Inline text claims project `available` regardless of blob state.** An
  `AgentClaim` carrying `ClaimText` renders its content in-band, so the client
  never fetches it; the projection marks it `available` and only stats the blob
  store for referenced content (images, out-of-line evidence). This keeps a
  read via the signet service consistent with the persisted value for inline
  claims (a raw-store read and a projected read agree on them).
- **Client-derived "oversized"/"unsupported" over a daemon disposition enum.**
  Those are dispositions of `size_bytes`/`media_type` against a device-specific
  render cap (8 MiB iOS, 64 MiB macOS), and over-cap content never passes the
  daemon's own validation gates, so the daemon carries no such enum.
- **Closed `EvidenceMediaType` enum.** The member set is the union of the
  run-evidence types the verifier/engine producers emit (`application/json`,
  `text/plain`, `text/markdown`) and the agent-claim types the importer admits
  (the opaque image set, `application/jsonl`, `text/markdown`). A closed enum
  means the generated Swift client rejects an unknown value; adding a member is
  a contract change. It is a distinct Go type from `ClaimMediaType` (which it
  overlaps by string) because it also names binary image types that never ride
  inline.
- **`ClaimText.media_type` kept; `GET /attachments/{digest}` unchanged.** For an
  inline text claim, `ClaimText.media_type` must equal `metadata.media_type`
  (validated string-equal in `AgentClaim.Validate`). The attachment endpoint
  stays `application/octet-stream`; the typed `media_type` on the reference is
  the client's source of type.
- **`created_at` from a stable recorded time, not a wall clock, at every
  content-addressed producer.** Artifacts and claims are content-addressed and
  persisted write-once (`putImmutable` byte-compares on replay), and several
  producers re-run on replay (`verifyAndCheckpoint`, the intake reconcile loop,
  a replayed `submit`) relying on byte-identity for idempotent convergence. A
  wall-clock `created_at` would break that. So verify evidence uses the
  checkpoint's stable time (`export.RecordedAt` / `task.StartedAt`, matching the
  co-persisted `CandidateAuthorization`), the importer uses `Options.Now` →
  `CommitDate` → current (pinned by the daemon), the effect-proposal artifact
  uses the proposal instance's `CreatedAt`, and shadow-review claims use
  `record.CompletedAt`. Most engine producers keep the injected transition
  clock because their transition is guarded (an inbox/outbox replay check
  returns before the artifact write, or the write shares the item's single
  atomic transition). Three reconcile producers were NOT so guarded and write
  the artifact before their outbox idempotence check while re-running on every
  tick (`enqueueElaborationAnswer`, `enqueueSpecRevision`,
  `enqueueSpecDiscussion`): a fresh-clock re-put would raise ErrImmutableConflict
  and wedge the reconcile loop (caught in fresh-context review; the pre-existing
  idempotence tests missed it because they froze the clock). They now use an
  idempotent get-or-put (`putArtifactIdempotent`) that keeps the existing
  write-once row when the digest matches, and the answer-and-retry replay test
  advances the clock to exercise it. The `submit`/reconcile registration
  artifacts use the same pattern (`registerSubmissionArtifact`).
- **`EvidenceMetadata` has no `clone`.** All five fields are value types, so a
  struct copy is a deep copy; `Artifact.clone`/`AgentClaim.clone` already copy
  it. The plan listed a `clone`; it would be dead code.
- **`AgentClaim.Validate` order: text shape before metadata.** The text
  media-type validity check runs before `Metadata.Validate`, so an unregistered
  inline media type still surfaces as `ErrInvalidClaimMediaType` (its own
  defect) rather than being masked by the metadata gate; the media-type-match
  check runs last.
- **`AgentClaim` inline-text `size_bytes` binds to the content.** For a claim
  carrying `ClaimText`, `Metadata.SizeBytes` must equal `len(Text.Content)`,
  enforced in `AgentClaim.Validate` alongside the digest and media-type
  bindings. The client renders `size_bytes` as a daemon-validated length, so an
  inline claim whose size disagrees with the bytes it displays is the same
  forged/corrupted class the digest check refuses; leaving it unchecked would
  make the "daemon-validated" promise false for inline claims. Referenced
  (out-of-line) claims and artifacts keep an unconstrained-by-content size (the
  bytes live in the blob store, not the row).
- **Legacy rows without metadata: fail closed, no backfill (declared clean
  break).** Making `metadata` required means a pre-metadata `artifacts` or
  `agent_claims` body rejects on reconstruction (`decode` re-runs `Validate`),
  and no data migration accompanies this change. A backfill was rejected
  because nothing is derivable: `created_at` has no source in a pre-metadata
  body, and #774 (devlog/2026-08-14-1438-attention-created-time.md) already
  rejected deriving a creation instant from a later lifecycle event and forbade
  nil→value backfill, so any backfill would fabricate a timestamp;
  `media_type`/`size_bytes` need the blob bytes, which live in `freeside.db`'s
  external blob store, unreachable from an in-DB Go migration hook (the
  migration-total invariant as practiced in 0036/0059 backfills only
  in-row-derivable fields). This follows the #984 clean-break precedent (same
  reviewer finding class, "clean break for a pre-release type"). Blast radius is
  operator rig DBs only (e.g. the real-run rig `~/.freeside/state-482`, which
  holds legacy artifacts/claims/evidence items and will not bootstrap after
  upgrade without a reset); the live daemon DB has zero affected rows, and no
  external installations exist. The failure is loud and legible: one legacy
  evidence item 5xx's sync bootstrap (`signet/sync.go`) and can fail startup
  reconciliation (`store/list.go`), with the error naming the offending
  artifact/claim and the missing metadata field. Rejected alternatives: a
  fabricated `created_at` backfill (violates #774); a nullable `created_at`
  (same, and erodes the byte-identity idempotence the created_at decision above
  rests on).

## Revisit when

Blob retention, cleanup, or integrity re-verification lands (would make
`available` more than a presence stat); a second sync boundary appears (the
projection would need to run there too); or an installation outside the
operator's rigs exists (the legacy-row clean break would then need a real
migration path rather than a reset).
