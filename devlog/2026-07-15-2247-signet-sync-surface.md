---
run: manual
stage: signet-sync-surface
date: 2026-07-15
branch: feat/signet-sync-surface
---

# Serve canonical signet synchronization (issue #66)

Signet-lane unit after #23 and the discovered spine contract #98. Declared
paths: `daemon/internal/signet` plus this mandatory returned-object
trust-boundary note. The unit consumes `store.Snapshotted[T]` values and
projects them onto the device-facing HTTP API, so the refute-first rule
applies.

## Decisions

**Bootstrap adds the pure-read upper-bound gate the store cannot.** One
`Store.Read` callback reads `ServerState` and all four deterministic
collections, then requires every row's positive `entity_version` and
`as_of_revision` to satisfy `as_of_revision <= ServerState.revision` before
serving anything. The store deliberately cannot enforce that upper bound
because its read methods are also reachable inside a write transaction, where
`current + 1` is legitimate; signet's sync projections are pure reads, so the
bound is valid here. One invalid row fails the whole bootstrap closed.

**Only bootstrap carries the full-cache cursor.** List and entity endpoints
return ResourceSnapshots with row-local metadata but no `sync_epoch` or global
`revision`; `/sync/revision` is a data-free heartbeat. A client that misses an
invalidation can therefore observe a higher heartbeat after a partial refetch
and must bootstrap or refetch every potentially affected resource. Rejected:
inventing an event-log or invalidation-stream endpoint that the provisional
OpenAPI contract does not define; plan §5.14 explicitly makes push latency-only
and the heartbeat the loss detector.

**HTTP authentication is injected and fail-closed.** `NewHTTPHandler` requires
a `RequestAuthorizer` that returns the authenticated device identity; nil,
failed, or identity-less authorization yields 401. Command submission also
requires the JSON `device_id` to equal that authenticated identity, preventing
one valid device credential from naming another device. #67 supplies the real
credential verifier and revocation policy; this unit does not preempt its
lifecycle decisions. Listener binding stays with daemon composition (which
must choose loopback or Tailscale per §5.2), not the HTTP handler.

**The handler projects the complete currently implemented signet API.** The
nine OpenAPI operations from bootstrap through commands are registered and
strictly decode the command body; pairing, device revocation, and attachment
storage remain with their owning units. Single-run and single-conversation
reads filter the deterministic snapshot lists because #98 intentionally
exposed collection reads rather than new single-get metadata methods. Accepted
for local-daemon scale; revisit with the store list materialization if
collections need pagination.

## Refute-first verification

Two independent lenses tried to disprove the boundary before handoff:

- **Returned-object and cursor lens.** Attacked zero/negative metadata, a row
  revision ahead of the same-read ServerState, empty/corrupt server state, a
  missed invalidation followed by a partial refetch, and an epoch change at an
  unchanged revision. Confirmed and fixed: signet needed the #98 note's
  upper-bound gate. Permanent tests now reject impossible metadata and prove a
  partial fetch cannot erase the heartbeat gap. Rejected by execution: torn
  bootstrap state; the service uses one `Store.Read`, and the store's permanent
  concurrent-write test plus `-race -count=10` remained clean.
- **Wire and authority lens.** Compared every current signet OpenAPI route with
  the mux registrations, exercised real JSON envelopes, mismatched the bearer
  identity and body `device_id`, and attacked missing/unknown/multiple/oversized
  command bodies. Confirmed and fixed: `CommandResult` initially lacked JSON
  tags and would have emitted `Record`/`Revision`; it now emits the contracted
  `record`/`revision`, pinned by a response-key test. Codex review then found a
  second member of the wire-shape class: valid nil domain slices would encode
  as `null` against non-null OpenAPI arrays. The widened class sweep now
  normalizes item evidence/claims/digests, run stages and attempts,
  conversation messages, and command-record digests at the projection
  boundary; one nil-heavy fixture pins them together. Codex's next pass found
  the input-side counterpart: required payload bindings whose legal value can
  be empty. The complete required-zero-value sweep found `expected_bindings`
  already guarded and added presence-aware decoding for `pr_head_sha` and
  `artifact_digests`, rejecting omitted or null fields while continuing to
  accept present `""` and `[]`. A fresh-context refuter then caught the
  remaining value-validation gap: `expected_bindings` is deliberately not
  carried into the decision command, so its empty/null digest values escaped
  domain validation. The HTTP boundary now rejects every empty value while
  retaining legal `{}`; `payload.artifact_digests` already receives the same
  non-empty-entry gate in `domain.Command.Validate`. Rejected by execution:
  unauthenticated fallback, cross-device command acceptance, permissive unknown
  fields, second JSON objects, revision consumption on rejected input, null
  top-level or nested response arrays, omitted/null required binding fields,
  and empty/null expected-binding digests.

Verification at the refute-first boundary: `go test -race -count=10
./internal/signet` and 25 repeated runs of the HTTP, snapshot, bootstrap, and
epoch tests passed. Revisit when #67 replaces the test authorizer with real
credential and revocation enforcement, or when a daemon listener is composed.
