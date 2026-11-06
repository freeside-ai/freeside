# Review No Longer Runs a Prospective Merge; Stored Requests Still Read (#2647)

## Decision

- **Chose to remove the review side of the prospective merge.** Since plan
  revision 110 review covers the head, and after #2660 and #2662 no
  production code builds a review request that names a merge. The ward's
  merge review (the v2 access line, `CodexReviewSpec.EvaluatedSHA`,
  `CodexReviewLaunchSpec.ExpectedEvaluated`, the merge prompt),
  `PublicationSuccessor.AllowsProspectiveMerge`, and the merge proofs in
  the disposition history are gone. The handoff check still verifies the
  merge (`engine/production_handoff.go`), and nothing on that side changed.
- **Chose to keep `evaluated_sha` on `exec.ReviewRequest` as
  `LegacyEvaluatedSHA`, read-only.** The Codex review journal stores each
  request, and its readers decode strictly: the review evidence view, the
  local backup's artifact closure, the closed-review observer, and the
  journal's own reads. Rows written before #2660 can carry the field.
  Validation and the authority digest keep treating it as before, so a
  retained request still validates and its digest still recomputes.
- **Rejected deleting the field and decoding history leniently.** Lenient
  decoding of a stored request would weaken the strict decode every history
  reader relies on to reject damaged rows, and a missed reader would fail a
  backup's closure. Keeping one inert field costs less.
- **Chose to refuse, not run, a retained request that names a merge.** Every
  review source refuses it with a non-transient failure (configuration in
  the fake and the ward); the review-source contract pins this. The ward
  refuses at `requestLaunch`, which both the first request and the relaunch
  of a recorded request call, so a request is refused before it is
  recorded. In the engine's own flow a recorded legacy request usually
  fails earlier: the engine's fresh request names no merge, so its authority
  digest disagrees with the recorded one. The source refusal holds for any
  caller that skips that check.

## Consequence

The reviewer configuration digest moves for both Codex and Claude. The
envelope's command template digest hashed the head and merge command
shapes together since `e5fa025b8`; it now hashes the head shape alone.
Keeping the old digest would mean keeping the v2 command code only to hash
it. As with every prompt or command change, a run admitted under the old
digest parks at the review gate until an operator adopts the new
configuration. The head command, the v1 access line, and the launch intent
digest of every launch without a merge are byte-identical.

A review still running at the upgrade, with a merge or without, collects
under the configuration it launched with, so the source refuses its result
as another configuration's and fails it as transient; the engine's next
round builds a fresh request, which names no merge.

Revisit when review is asked to cover anything other than the head again;
that would need a new request field, not this legacy one.
