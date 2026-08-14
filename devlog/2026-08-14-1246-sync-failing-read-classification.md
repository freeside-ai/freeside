# Render a Non-401 Sync Failure Distinctly From Daemon-Unreachable

Work unit: #771 (`kind:fix`, `lane:saddle`), PR #772. Source: the #767
note `devlog/2026-08-14-1134-sync-projection-run-isolation.md`, whose
Follow-ups deferred this client-side rendering split. This note records
the returned-object trust-boundary verification for the read-error
classification the fix introduces.

## Decision

Chose **classify a failed sync read by whether the daemon answered**,
adding a third client freshness state `syncFailing` (reachable but its
reads are failing) alongside `unreachable` (no answer). An answered
non-401 status, a 200 whose body this client cannot decode, and a
timeline whose `run_id` does not match the request all map to
`syncFailing`; only a call that received no answer stays `unreachable`.
`syncFailing` keeps the cached view read-only exactly as `unreachable`
does, so the split is operator-signal only, not an actions change.

Named `syncFailing`, not "degraded", to leave "degraded" to the
#657/#733 degraded-run wire vocabulary (the #767 note's Revisit-when).

The load-bearing implementation choice: the generated OpenAPI client
**decodes response bodies eagerly** on the operation call, so a
malformed or schema-incompatible 200 throws from `try await client.X()`
into the read `catch`, not from `try ok.body.json`. Distinguishing that
answered-decode-failure from a transport outage at that single catch is
the trust boundary. Chose **key on `ClientError.response`**:
OpenAPIRuntime raises a `ClientError` for both cases, populating
`response` only when a response actually arrived, so
`freshnessForReadError` maps response-present to `syncFailing` and
response-absent (and any non-`ClientError`) to `unreachable`. This
needed `OpenAPIRuntime` as a direct `FreesideCore` dependency (already
transitive via `FreesideAPI`).

## Rejected Alternatives

- **Inner `do/catch` around each `.ok` body, mapping a `try
  ok.body.json` throw to `syncFailing`** (the first attempt): rejected
  because eager decoding means that throw never occurs there; the
  decode failure surfaces one level up at the operation call. A test
  driving an undecodable 200 proved the state was still `unreachable`,
  which is what redirected the fix to the outer catch.
- **Treat every read-`catch` error as `unreachable`** (the prior
  behavior): rejected as the bug itself. A reachable daemon under
  client/daemon schema skew (a case `app/README.md` calls out) answers
  200 with a body this client cannot read, and the operator saw "daemon
  unreachable", the exact misread #771 exists to remove.
- **Map an undecodable 200 to a hard error the user must resolve**:
  rejected against the §5.14 contract that every sync failure degrades
  to the cached read-only view with a banner, never a blocking error.

## Returned-Object Trust-Boundary Verification (refute-first)

The boundary: `freshnessForReadError` trusts one field (`response`) of a
`ClientError` handed back by the external generated client to choose an
operator-visible state. Refutation lenses, each trying to disprove that
the classification is correct and safe:

- **Can a transport outage be misclassified as `syncFailing`?** Only if
  a no-answer failure produced a `ClientError` carrying a non-nil
  `response`. OpenAPIRuntime sets `response` only after a response is
  received; a connection/transport failure carries none. Confirmed by
  the unchanged `unreachableDaemonDegradesToTheBannerAndRecovers` test
  (a thrown transport error still yields `unreachable`).
- **Can an answered decode failure be misclassified as `unreachable`?**
  Only if the decode `ClientError` lacked its `response`. The new
  `answered200WithUndecodableBodySurfacesSyncFailing` test drives an
  undecodable 200 on all four reads and observes `syncFailing`,
  confirming the response is present at classification time.
- **Does a wrong classification cause harm?** No. `syncFailing` and
  `unreachable` are both cached-read-only states that disable actions;
  a misclassification swaps one conservative banner for another and
  never enables a write or exposes stale state as fresh. The boundary
  fails safe in both directions, and a non-`ClientError` fails closed to
  `unreachable`.
- **Is the trusted field authoritative for something it should not be?**
  No. `response`'s presence is used only to pick a banner, never to
  grant eligibility, trust a payload's contents, or advance a cursor;
  the decoded body is never consumed on the failure path.

Findings: all confirmed; none rejected-by-verification; none
accepted-by-decision beyond the eager-decode implementation choice
recorded above.

## Revisit When

The generated client is regenerated or upgraded such that response
bodies decode lazily (a `try ok.body.json` throw inside the `.ok` arm)
or `ClientError` stops carrying `response` on a decode failure: at that
point `freshnessForReadError`'s discriminator moves, and the decode
failure must be reclassified at wherever the throw then surfaces.
