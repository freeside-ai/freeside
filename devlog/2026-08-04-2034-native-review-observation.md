# Record native GitHub review activity as observation-only evidence (#497)

Returned-object-trust-boundary work: ingest third-party native (Codex)
review bodies into the durable store as best-effort extra evidence, with
no code path into readiness. This note records the trust-boundary design
choices and the mandatory refute-first verification pass.

## Chose: normalize in the reconciler, keep publish pure transport

The publish layer (`ReconcilePullReviewActivity`) decodes raw forge
fields and handles conditional requests only. The reviewer-login filter,
badge-to-severity lift, UTF-8/size bounding, and comment-to-review
grouping live in `cmd/freesided/native_review.go`, where the durable
domain observation is built and validated. Rationale: keep the trust
policy (which logins count, what is safe to persist) out of transport
and next to the durable-write boundary, matching how `active_resource.go`
already builds `PullMergeFact`/`IssueStateFact` from raw pull/issue
observations.

## Chose: reviewer login defaulted in main.go, not a config field

The default reviewer (`chatgpt-codex-connector`) is wired as a
`map[string]bool` in `main.go` and threaded into the reconciler, not made
a `claudeDriverConfig` field. There is exactly one native reviewer today;
a config field with validation is premature abstraction (the ~3-uses
rule) and adds contract surface for no consumer. The domain never names
the login. **Revisit when** a second native reviewer appears: promote the
set to configuration then.

## Chose: readiness-inert by construction, not by guard

`NativeReviewObservation` carries no trust bit and no readiness field, and
its store readers have no non-test callers. The observation commits in a
separate `WriteInternal` transaction from the pull/issue facts, so a
native failure cannot roll back the load-bearing commit. This is the
non-goal the issue stresses: native activity never creates, restores, or
substitutes readiness or suppresses re-review, which stay gated on the
exact Freeside `ReviewRecord` (§6). Proven at the integration level
(`production_native_review_test.go`): a native clean-pass never yields
ready while the Freeside pass has findings.

## Refute-first verification (trust-boundary requirement)

A fresh-context reviewer was tasked to *disprove* five safety claims.
Result: one blocking defect (confirmed and fixed), three non-blocking
(dispositioned), four claims held.

### Confirmed and fixed

- **Round-trip churn via unsanitized third-party strings (blocking).**
  The `json.Marshal` U+FFFD substitution trap (issue #180) was guarded on
  the finding *body* but not on `Finding.Location` (from the comment
  `path`, which git stores as raw bytes, so invalid UTF-8 needs no
  adversary) or `NativeReviewObservation.ReviewCommitSHA` (from
  `commit_id`). Both are persisted and compared by `MaterialChangeFrom`,
  so an invalid-UTF-8 path made the raw-rebuilt observation never equal
  the sanitized-stored one, appending a new row on every tick with no
  unique constraint to arrest it. Fixed by sweeping the whole class on
  both axes: ingestion sanitizes `path` and `commit_id`, and
  `Validate` now enforces UTF-8 + size on `author_login`,
  `review_commit_sha`, and `Finding.Location` (fail-closed on
  reconstruction). Regression test: `TestActiveResourceNativeConverges`
  `WithInvalidUTF8`.

### Rejected-by-verification (not re-raised)

- **Fails-closed-forever on a crafted body.** Could not be constructed:
  the sanitized fields are byte-stable across the round-trip, so read-side
  `Validate` stays consistent with write-side.
- **Wrong-PR binding / unsolicited 304 / null list / non-terminating
  pagination / off-host redirect / id ≤ 0.** All held: the PR identity
  comes from the trusted binding not the response; `resolveListPart`
  refuses an unsolicited 304; `fetchConditionalList` rejects a null batch,
  bounds pages, and prefix-checks the `Link` host; ids ≤ 0 are filtered.
- **Time-based churn/lost-update in `MaterialChangeFrom`.** Held:
  `SubmittedAt` uses `.Equal`; finding `CreatedAt` is forced `.UTC()` at
  ingestion and decodes to `time.UTC` with no monotonic reading, so
  `slices.Equal`'s `==` matches.

### Accepted-by-decision (non-blocking)

- **First-page-only conditional ETag.** A review comment appended to a
  later page of a >100-item list while page 1 is unchanged is observed
  only once page 1 changes. This degrades best-effort (readiness-inert)
  evidence completeness, never a safety property; the lists are
  single-page in practice. Documented at the code and left as a limitation.
  **Revisit when** a native reviewer's activity on one PR realistically
  exceeds one page.
- **Order-sensitive finding comparison.** `slices.Equal` is order
  sensitive; closed proactively by sorting findings by native comment id
  at ingestion, so a forge reorder cannot register a spurious material
  change.
- **Degenerate comment id.** A comment with `id ≤ 0` is now skipped rather
  than producing a `native-comment-0` finding id.

## Codex review round (trust-boundary fixes)

Three P2 findings, all confirmed against the code and fixed in-place.

- **Bot-form reviewer login (correctness blocker).** GitHub's REST
  reviews, review-comments, and reactions endpoints (the ones the
  transport reads) return a GitHub App's login as
  `chatgpt-codex-connector[bot]`, but the reviewer set held only the
  canonical `chatgpt-codex-connector`, so the exact-login filter dropped
  every review, comment, and reaction: the feature observed nothing in
  production. Fixed with `canonicalReviewerLogin`, which strips the
  reserved `[bot]` suffix at all three filter sites before the membership
  test; the canonical login is what the durable observation stores, so
  evidence identity stays stable across API forms. Verified against this
  PR's own Codex review, whose REST author login is the `[bot]` form.
- **Stale clean-pass reaction re-bound to the current head.** A reaction
  carries no `commit_id` and persists across pushes, so a leftover `+1`
  from an earlier head kept its id and `CreatedAt` and was re-recorded as
  a clean pass for the new `BindingHeadSHA`, falsely asserting the current
  head passed. Fixed by rejecting any reaction created before
  `binding.RecordedAt` (the head-binding instant); reviews and comments
  were already safe because they bind by `commit_id` and record the
  divergence. Regression: `TestActiveResourceRejectsStaleCleanPassReaction`.
- **State-only dismissal coalesced.** `MaterialChangeFrom` compared
  author, commit, timestamps, binding head, and findings but not review
  state, so a dismissal that left the inline comments unchanged coalesced
  and the timeline never showed it, contradicting the edited-or-dismissed
  promise below. Fixed by adding `ReviewState` to the observation as a
  bounded third-party string (required for a findings_review, empty for a
  clean_pass_signal), populated from the review state and compared in
  `MaterialChangeFrom`. Regression: the state-only append case in
  `TestNativeReviewObservationAppendOnMaterialChange`.

### Round 2: clean-pass anchor (accepted-by-decision, deferred)

Codex round 2 raised one valid P2 on the round-1 stale-reaction fix: because
`ReadyItemPRBinding.RecordedAt` is stamped after Freeside's own review gate
(`recordReadyItemPRBinding` runs downstream of `reconcileReviewGate` in
`completePublishedTask`, `internal/engine/production_publication.go`), the
`CreatedAt >= RecordedAt` reaction filter also drops a *legitimate* clean-pass
reaction posted after the push but before Freeside's local review finishes, so
that current-head clean pass is not recorded until (if ever) the reviewer
re-signals after readiness. The two attribution failures point opposite ways:
round 1 was a false positive (a stale reaction fabricated as a clean pass for a
new head), round 2 is a false negative (a real clean pass dropped).

The fully-correct anchor is the head-publication time, which no field on
`ReadyItemPRBinding` or the pull observation carries; adding it means editing
`internal/engine` and the `ReadyItemPRBinding` domain type, outside this unit's
declared scope (and likely `kind:contract`, spine-owned). Per monorepo scope
discipline that is filed, not fixed in passing: follow-up **#505**. The
conservative `RecordedAt` filter is retained deliberately: for readiness-inert
best-effort evidence a dropped clean pass is strictly safer than a fabricated
one, and the higher-value commit-bound findings-review observations are
unaffected. **Revisit when** #505 plumbs a head-publication timestamp; re-anchor
the reaction filter against it then.

### Round 3: cache-retry and shield-badge (both fixed)

The round-2 note push triggered a re-review that surfaced two real blockers,
both fixed in-place.

- **Native observations lost after a write failure (data-loss/retry).**
  `ReconcilePullReviewActivity` advances its per-sub-resource ETags on a
  successful fetch; if `commitNativeReview` then failed, the next tick sent the
  advanced ETags, GitHub answered 304, `observe` suppressed
  `buildNativeReviewObservations`, and the un-persisted rows were never retried
  until external review activity changed, violating #497's "observation
  failures are isolated and retryable". Fixed by adding
  `Reconciler.EvictPullReviewActivity(repo, number)` (drops the cached
  validators/lists so the next poll re-fetches unconditionally) and calling it
  from the active-resource reconciler whenever `commitNativeReview` returns an
  error. Regressions: `TestEvictPullReviewActivityForcesUnconditionalRefetch`
  (publish) and `TestActiveResourceNativeCommitFailureEvictsCache` (reconciler,
  isolates the failure by dropping the native table so only the append fails).
- **Severity always dropped for real Codex comments.** `nativeReviewBadge`
  trimmed only framing punctuation before checking a leading `P`, but real
  Codex bodies open with a shields.io badge image
  (`**<sub><sub>![P2 Badge](https://img.shields.io/badge/P2-...)`), so the scan
  hit `<sub>` and returned "" for every real finding, dropping the priority
  from the durable `Finding`. Fixed by parsing the `![Pn Badge]` image alt text
  in a bounded prefix first, falling back to the plain leading-`Pn`-token parse.
  `TestNativeReviewBadge` now covers the real shield format plus the plain-text
  cases.

## Assumptions carried (issue plan, unvetoed)

- Inline-comment findings are in scope this round (path/line/badge/bounded
  body), not split out.
- Recording is first-class facts over time: an edited or dismissed native
  review appends, never rewrites (a dismissal is now caught by the
  `ReviewState` comparison, not only by changed finding text).
- No post-settle trailing window: once the item leaves ready, observation
  stops; a review landing after merge is out of scope (§5.16).
