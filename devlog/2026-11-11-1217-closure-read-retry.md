# A Transient Closure-Checkpoint Read Retries Instead of Holding

Issue #3001, found by the fresh-context review of #2857's PR. Tier A by path
(`publication_closure*.go`): the closure reference decides what source-issue
section a published PR body carries.

Chose to classify the error in `composePublicationCandidate` with the
existing `productionPublicationRetryableFailure` over marking closure errors
retryable inside `applyClosureReference`, because the hold decision lives in
`composePublicationCandidate` and that classifier is the one place that
decides retryability. The planner made this choice in the issue.

## The Defect

`composePublicationCandidate` handed every `productionCandidate` error to
`holdPublicMetadataTask`. A busy SQLite read of the closure checkpoint
therefore wrote a durable `HoldTrustBlocked` hold whose recovery text tells
the operator to cancel the task and resubmit: a healthy run ended over a
transient fault. `applyClosureReference`'s doc comment also claimed it
"never blocks", which was false.

## Decision

- **Retryable errors return with no outcome.** The run loop records its usual
  `publication_environment` back-off and defers the task.
- **State contradictions still hold.** The classifier rejects
  `ErrParentKeyMismatch` before it looks for an environmental cause, so a
  closure row of the wrong kind, an undecodable payload, or a failed
  identity re-check keeps today's public-metadata hold, even when the error
  is joined with a SQLite error. Untyped errors also hold.
- **Rejected: marking every non-mismatch closure error retryable.** Untyped
  corruption, such as an inbox row with an invalid `created_at`, would become
  an endless back-off, which the classifier's doc comment rules out.
- **Rejected: rendering without the closure reference.** A recovered or
  drift-repaired PR must replay byte-identical metadata, so a retry beats a
  fallback (issue non-goal).

## Refute-First Findings

- **Disproved: a metadata failure could now retry forever.**
  `publicationTitleBody` errors come only from `publicationMetadata`,
  `ProductionPublication.Validate`, and `publicationtext.Screen`; every one is
  a plain `errors.New` or `fmt.Errorf` without `%w`, so the classifier never
  accepts them. Revisit if one starts wrapping a store, network, path, or
  context error.
- **Allowed: any SQLite error retries, including one from a corrupt file.**
  The classifier accepts every `*sqlite.Error`, as it does for every other
  publication store read; this unit doesn't narrow it.
- **Unchanged: a canceled context.** `context.Canceled` isn't retryable, so a
  closure read canceled at shutdown still takes the hold branch, whose own
  store reads fail under the same canceled context. That is today's behavior.
- **Confirmed sound by a fresh-context reviewer:** only the closure read can
  reach the new branch (the authoring read falls back to v1), both callers
  return a nil outcome with the error to the run loop, and the busy test
  fails the closure read itself, since every store read takes the write lock
  (`_txlock=immediate`). The test's proof is that composition returns no
  outcome; the no-hold check after the lock releases can't fail on its own,
  because a hold write would also hit the lock.
