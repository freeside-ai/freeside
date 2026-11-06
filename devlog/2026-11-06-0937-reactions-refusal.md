# A Refused Reactions Read Keeps Native Review Observation (#2717)

## Decision

- **Chose to return the reviews and review comments when GitHub refuses the
  reactions read with a 403 that isn't a rate limit, and mark the reactions
  unavailable (`PullReviewObservation.ReactionsUnavailable`).** The
  mid-wave rehearsal (#2396) saw one such 403 fail the whole native review
  observation every minute, which also dropped the reviews and comments that
  become external findings and can start a review cycle. Reactions only feed
  the clean-pass signal, which starts nothing, so losing them for a pass costs
  little; losing the reviews costs the review loop. Unavailable reactions are
  nil and mean unknown: the clean-pass builder already treats nil as no
  signal, and nothing reads a missing reaction as evidence.
- **Chose only a non-rate-limited 403 as "unavailable".** A rate-limited 403
  (`X-RateLimit-Remaining: 0` or `Retry-After`, the rule follow-up filing
  already uses, now shared as `forbiddenByRateLimit`), a 5xx, a 404, and a
  transport error still fail the whole observation and retry next pass. Those
  are transient or unexplained; a plain 403 is GitHub refusing this read for
  this installation, which a retry won't fix.
- **Chose a positive `refused` bit on `APIError`, set only by the list
  reader, over the issue's rate-limit bit.** The issue proposed marking rate
  limits and treating any other 403 as a refusal. But the token mint that
  precedes each read also returns `*APIError`, and a 403 from it would then
  pass as a refused reactions read and hide an installation fault. With the
  positive bit, only the list reader's own 403 response qualifies.
- **Chose to log the refusal once per ready item at WARN, from the
  active-resource reconciler, over a logger in `publish.Reconciler`.** The
  publish transport stays log-free, and its constructor is wired in
  `cmd/freesided/claude_driver.go`, a Tier A file this change doesn't need to
  touch. The reconciler already owns per-item state (`observationFailures`),
  so it keeps a process-local record of reported refusals, cleared when a
  pass reads the reactions or the item leaves the open state. A restart
  reports a standing refusal once more, which is acceptable.
- **Chose not to back off the reactions request.** It's still sent every
  pass, unconditionally after a refusal, so a fixed App permission shows up
  on the next pass. One extra request per pass per ready PR is small.
- **Chose to report `NotModified` on a refused pass only when the previous
  pass was refused too.** Otherwise a standing refusal would rebuild native
  observations and external findings from cached reviews every minute, and a
  refusal right after a successful read would hide that the reactions the
  caller last saw are no longer known.

## Refute-First Findings

- **Confirmed and fixed: a token-mint 403 would have read as a refused
  reactions read** under the issue's rate-limit-bit design. Fixed by the
  positive `refused` bit; `TestRefusedReadOnlyForListReaderRefusals` pins it.
- **Confirmed and fixed: a pass that fails after marking a refusal reported
  would drop the warning** and silence a standing refusal until the reactions
  read again. The loop now logs refusals before it handles the pass error.
- **Disproved: a refusal hides new reviews or comments.** `NotModified`
  still needs both of those reads to answer 304, and a refusal never
  short-circuits them.
- **Disproved: a rate-limited 403, 404, 5xx, transport, decode, page-bound,
  or cancellation error passes as a refusal.** None carries the bit; the
  rate-limit, 404, and 500 cases are tested and write no cache entry.
- **Disproved: the validator survives a refusal.** The reactions ETag is
  stored empty, and the recovery test asserts an unconditional read.
- **Disproved: nil reactions retract a recorded clean pass.** Native review
  rows are append-only, and the builder adds rows only for reactions present.
- **Found outside this change:** a failed ready-resource commit strands the
  same pass's native observations behind a 304 (#2723).

## Risks

- A secondary rate limit that GitHub answers with a 403 carrying neither
  header would read as a refusal. The cost is one warning and one pass
  without reactions; the next pass reads them again.
- GitHub may answer a missing permission with 404 on some repositories. The
  issue leaves 404 out on purpose, since a 404 can also mean a wrong PR.

## Revisit When

- A consumer starts reading the absence of a reaction as evidence; it must
  then check `ReactionsUnavailable`.
- Another best-effort sub-resource joins native review observation and needs
  the same treatment, which would make a shared "optional part" shape worth
  extracting.
