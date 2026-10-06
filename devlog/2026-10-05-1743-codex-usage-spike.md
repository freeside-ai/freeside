# Codex Usage Read Answers Over the Access-Only Snapshot

Work unit: #1714. Scope: test-only ward code, daemon run instructions, this
note. Prior measurements:
[account probe](2026-10-05-1011-codex-account-probe-spike.md),
[its negative result](2026-10-05-1130-codex-account-negative-result.md).

## Decision

Verdict: `pass`. The pinned Codex CLI answers `account/rateLimits/read` over
the access-only, read-only snapshot under provider-only egress, with no
token-endpoint POST, when the access token has enough life left. Collectors
(#1716, #1718) inherit these constants:

| Constant | Value | Basis |
| --- | --- | --- |
| Refresh window | 5 minutes | Upstream constant, confirmed by the two-minute synthetic case |
| Invocation deadline | 60 seconds | Measured: read within 1 s, read plus one turn in 9 s |
| Skew and launch allowance | 60 seconds | Measured launch latency 0 to 1 s |
| Launch floor | 7 minutes | Sum of the three |

Launch only when the snapshot's access-token expiry is at least seven minutes
away. A missing expiry defers. The floor is deliberately not tightened to the
measurements: the deadline bounds a hung provider, not the common case.

The collector's egress needs exactly one route, `GET
https://chatgpt.com/backend-api/wham/usage`. Nothing on `auth.openai.com` is
needed, and the reset-credit routes stay closed.

Both operator inputs were supplied. The owner named the default CLI store and
consented to one inference turn, then to a second after the first run was
inconclusive for a harness reason (Harness Findings).

## Pinned Invocation

CLI `codex-cli 0.147.0`, in the unchanged image the account probe used:

`127.0.0.1:5066/freeside-account-866@sha256:396ff6ddddd32b274811875180d9fea131bb06b430dce58ae43844f3080fe1ed`

The driver starts `codex app-server` on stdio with the account probe's
environment and sends:

```json
{"id":1,"method":"initialize","params":{"clientInfo":{"name":"freeside_usage_probe","version":"1"}}}
{"method":"initialized"}
{"id":2,"method":"account/rateLimits/read"}
```

The turn case continues with `thread/start` (`approvalPolicy: never`,
`sandbox: read-only`, `ephemeral: true`) and one `turn/start` at `effort:
low` whose input asks for a one-word reply, then waits for `turn/completed`.

Method names, schemas, and routes were read from `openai/codex` at tag
`rust-v0.147.0` before the run and then confirmed by it:

- The read is `GET /backend-api/wham/usage`
  (`backend-client/src/client/rate_limit_resets.rs`). It runs beside a
  reset-credit list call whose failure the read tolerates.
- The read resolves authentication through the self-refreshing path, which
  refreshes when the access token's `exp` is five minutes away or less
  (`login/src/auth/manager.rs`, `should_refresh_proactively`). A failed
  refresh is logged and the existing token is used.
- The eight-day `last_refresh` rule applies only when the access token's
  expiry cannot be parsed. The gate requires a parsed expiry, so that rule
  never governs an admitted launch.

## Results

One complete live run, pinned as sanitized fixtures:

| Case | Credential | Token-Endpoint POSTs | Read | Verdict |
| --- | --- | --- | --- | --- |
| `synthetic_fresh` | Made-up, one hour | 0 | Answered | `pass` |
| `synthetic_near_expiry` | Made-up, two minutes | 7, all blocked | Answered | Detector `pass` |
| `real_idle` | Subscription | 0 | Answered by the provider | `pass` |
| `real_turn` | Subscription | 0 | Answered, turn completed, 1 notification | `pass` |

Every case kept the snapshot hash, the `auth.json` symlink, and the append
and unlink denials. The host store's hash was the same before and after both
real cases. No case had a transport failure.

The near-expiry case is the detector control and also shows the behavior the
gate exists for: inside the window the CLI posts to the token endpoint before
and after the read, and still answers the read with the existing token. A
blocked tokenless refresh does not fail collection; it is the attempt plan
§10 says to schedule around.

A real near-expiry run was a non-goal and was not made.

## Response Fields

The provider's answer on one Pro subscription, as field names and JSON types.
`rateLimitsByLimitId` held one bucket, keyed `codex`, with the same shape as
`rateLimits`.

| Field | Read | Notification |
| --- | --- | --- |
| `rateLimits.limitId` | string | string |
| `rateLimits.limitName` | null | null |
| `rateLimits.planType` | string | **null** |
| `rateLimits.primary` | object | object |
| `rateLimits.primary.usedPercent` | integer | integer |
| `rateLimits.primary.windowDurationMins` | integer | integer |
| `rateLimits.primary.resetsAt` | integer | integer |
| `rateLimits.secondary` | null | null |
| `rateLimits.credits` | object (`hasCredits`, `unlimited`, `balance`) | object, same members |
| `rateLimits.individualLimit` | null | null |
| `rateLimits.rateLimitReachedType` | null | null |
| `rateLimits.spendControlReached` | boolean | **null** |
| `rateLimitsByLimitId` | object | absent from the schema |
| `rateLimitResetCredits` | object (`availableCount` integer, `credits` null) | absent from the schema |

`secondary` was null on this account, so a collector must treat every window
as optional. `rateLimitResetCredits.credits` is null because the list route
is closed; the count still arrives from the usage route.

## Observation Lifecycle

`account/rateLimits/updated` is a partial update, never a replacement for the
last full read.

- **Source.** The app-server emits it only when a model response carries
  rate-limit data (`app-server/src/bespoke_event_handling.rs`,
  `handle_token_count_event`). The idle cases saw none; the one-request turn
  saw one. A collector that only reads never receives it.
- **Shape.** It carries a single `rateLimits` snapshot for one limit. It has
  no bucket map and no reset-credit data, and in the observed notification
  `planType` and `spendControlReached` were null where the read had values.
- **Meaning of null.** Upstream documents the notification as a sparse
  rolling update to merge into the most recent read, with null meaning
  unavailable (`app-server-protocol/src/protocol/v2/account.rs`). A null
  window in a notification is therefore not a dropped window.

This matches plan §10: a full read replaces the pool's window set, and a
partial update refreshes only the windows it names for the limit it names.
What record shape carries that is #1715's decision.

## Harness Findings

- **Synthetic cases never reach the provider.** Their tokens are fabricated,
  so the proxy answers their usage read with a canned body. They prove
  refresh behavior and the harness; only the real cases prove the provider
  answers. Rejected: forwarding fabricated tokens to the provider.
- **The relay must send a TLS close_notify.** The forwarded body is
  close-delimited. Without the alert the Go test client read the body and
  the pinned Rust client discarded it as truncated, reporting an empty body.
  As with the account probe's certificate finding, the offline proxy test
  alone did not prove the CLI's behavior.
- **The CLI abandons connections routinely.** About one run in three shows a
  tunnel closed during the TLS handshake or before any request, as EOF or as
  a TCP reset. Twenty-two synthetic runs with handshake errors logged showed
  eight, all on `chatgpt.com`. The first real turn completed and showed one
  notification, but two resets scored it `probe_failed`. Chose to classify a
  handshake hang-up by authority: ignored on `chatgpt.com`, where a lost
  request fails the read or turn, and a failure on `auth.openai.com`, where
  it could have been a refresh. Rejected: counting every hang-up as a
  failure, which makes a third of clean runs inconclusive and spends
  allowance on reruns.
- **WebSocket is denied, and the turn still fits.** The CLI tries a
  WebSocket upgrade first; the proxy answers 403 and the same turn falls
  back to `POST /backend-api/codex/responses`, completing in 9 s both times.
- **The probe lifecycle is duplicated.** Refactoring the account probe's
  files was outside this unit's declared paths. Follow-up: #1776.

## Refutation Pass

An independent reviewer tried to break the safety claims before any real
credential was used. It found no path for a token, account identifier, or
provider string to reach logs, evidence, or fixtures.

- **Confirmed, fixed.** A hang-up dropped its authority and was never a
  failure, so an abandoned connection to the auth host could hide an
  intended refresh. Hang-ups there now count against the run.
- **Confirmed, fixed.** The snapshot's host-side temporary copy outlived
  seeding. It and the seeder's copy are now removed once the volume holds
  it, and each case logs its recovery manifest before creating anything.
- **Confirmed, fixed.** Automated review found that a run killed between
  writing that host copy and removing it left a usable access token the
  manifest did not name. The manifest now records the copy's path before the
  write and marks it removed afterward. A second pass on that change found
  no copy the manifest fails to name; it did not check whether the runtime's
  copy command stages a host file of its own. Rejected for now: seeding over
  the seeder's standard input to avoid the host copy, which needs a live run
  to prove.
- **Confirmed, fixed.** Launch latency silently spent the skew allowance.
  The driver now reports it and a launch slower than the allowance is
  `probe_failed`.
- **Confirmed, fixed.** A query string or escaped path was labeled as the
  reviewed route and forwarded. Only the exact target is forwarded now.
- **Confirmed, fixed.** `probe_failed` outranked `fail` overall, and a pass
  did not require the proxy to have carried the read or the turn.
- **Allowed.** A second request pipelined on one tunnel is dropped unlabeled.
  The pinned client does not pipeline, and the proxy closes after one
  response.
- **Allowed.** Request headers are forwarded unfiltered to the fixed
  provider authority; the CLI needs them and no other host can receive them.
- **Out of scope.** The production derivation returns the host bytes
  unchanged when the decoded refresh token is already empty, so a store with
  a duplicated `refresh_token` key would pass through. The CLI does not write
  such a store, and whoever can write the host store already holds the
  credential.

## Limits

- One account, one plan, one bucket, one notification. Other plans may
  report a secondary window, more buckets, or a `rateLimitReachedType`; the
  sanitizer fails on any field or enum value outside the pinned schema.
- Timings have one-second resolution.
- Whether the notification's content differs when a turn makes several model
  requests, or arrives over WebSocket, was not measured.

Revisit when the pinned CLI version changes, when the provider moves the
usage route or the refresh window, or when a collector's measured launch
latency approaches the 60-second allowance.
