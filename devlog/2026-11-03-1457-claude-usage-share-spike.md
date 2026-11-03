# Claude Usage Share: Weekly Share Only Through A Turn's Rate-Limit Headers

Decision scope: #2389. Extends the #1713 harness and its note,
[`2026-10-02-2245-claude-usage-spike.md`](2026-10-02-2245-claude-usage-spike.md).
Test-only evidence; no production, credential-mode or egress change.

## Verdict

Under the setup token alone, the provider shows a used share for one window,
the weekly one, and only on the rate-limit headers of an inference response.
The usage route refuses the setup token for lack of the `user:profile` scope,
so no idle read exists.

| Window kind | Path 1: pinned CLI | Path 2: direct read |
| --- | --- | --- |
| `five_hour` | `unsupported` on 2026-11-03; #1713 saw `status_and_reset` (event) | `unsupported` |
| `weekly` | `share` (`turn_seeded`) | `unsupported` |
| `model_scoped` | `unsupported` | `unsupported` |

`claudeUsageWindows` computed each verdict. A failed or unrun probe gives none.

- **#1717 may use path 1 only.** The usable path is the `turn_seeded` launch:
  a duplex stream-json launch (`--input-format stream-json`) with
  `CLAUDE_CODE_OAUTH_SCOPES='user:inference user:profile'`. After the
  writer's own turn, `get_usage` returns the share the CLI seeded from that
  turn's `anthropic-ratelimit-unified-*` headers. Nonessential traffic stays
  disabled. No usage read leaves the CLI, and there is no inference solely
  to observe usage. Path 2 is closed: it is `unsupported` for every window.
- **#1721 shows** weekly as a share with its reset, as of the last turn.
  Five-hour shows status and reset only when an event names it, and is
  otherwise `unsupported`. Model-scoped is `unsupported`.
- **The share is only as fresh as the last turn.** With no turn, the seed is
  empty and `get_usage` returns a null `rate_limits`.

The scope override is a launch change, not a credential change. The token,
its grant and its storage are unchanged. The variable only tells the CLI
which scopes to assume, which unlocks its `get_usage` and seed code paths.
The provider still enforces the token's real scopes, as path 2 shows.

## Pinned Build

- **CLI:** Claude `2.1.220`. `claude --version` ran inside each probe
  container. Binary SHA-256 `159e4a51d796f3bf14677577100f7efb845611b1ceaf0c30cbd8d4650d942185`.
- **Image:** `127.0.0.1:5014/freeside-agent-claude@sha256:6e208c8330834b846b7e0577acb9c3607813cd295de75e46f401c16eb57c8d58`.
- **Departure, image rebuilt:** the cached image was gone, because the local
  content store had been emptied after the planning check. Chose to rebuild
  with `scripts/build-agent-claude-image.sh --claude-version 2.1.220` over
  waiting for a cache that no longer exists. The rebuilt image's CLI binary
  is byte-identical to the one inspected offline (the SHA-256 above), so the
  offline findings below apply to it. The previous digest was
  `sha256:fec0dbe2…8fd30`.

## Candidates From Offline Inspection

Inspection of the pinned binary found exactly one source of an allowance:
`Rpn`, behind both `get_usage` and `/usage`. Every other candidate was
rejected on what the code does:

- **Gates:** `rate_limits_available` is `ii() && bA()`, the token's
  `user:inference` and `user:profile` scopes. Under
  `CLAUDE_CODE_OAUTH_TOKEN`, the scopes come from `CLAUDE_CODE_OAUTH_SCOPES`,
  else `["user:inference"]`. That is why plain `idle` reports unavailable.
- **Read and seed:** when available, the CLI reads `GET /api/oauth/usage`.
  If the read fails, it falls back to a seed built from inference response
  headers `anthropic-ratelimit-unified-{5h,7d,7d_oi,overage}-{utilization,reset}`.
- **Essential-traffic gate:** with
  `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC` set, as the production writer
  sets it, the first-party client never sends that read.
- **Model-scoped windows:** in `get_usage`, these follow the flag
  `tengu_usage_overage_included_models`. It defaults to empty, and its
  bootstrap route is blocked here.

Rejected without a run:

- **`/usage` slash command:** the same `Rpn` as `get_usage`.
- **Scope override with nonessential traffic disabled and no turn:** the
  first-party client short-circuits and the seed is empty.
- **`get_session_cost`, `get_context_usage`, `get_settings`:** they report no
  allowance.
- **`quota_check`:** it is an inference request.
- **`oauth_token_refresh`, `host_auth_token_refresh`:** these are refresh.
- **`rate_limit_event`:** carries utilization only in the `allowed_warning`
  case, so it was observed but not relied on.

## Observed Contract

The live runs used setup-token auth only. The owner consented to both turns.
All times are local on 2026-11-03.

- **`idle`, `turn` and `turn_seeded`:** from the full run at 14:37 to 14:38.
  Later harness fixes touched only the usage route and the
  `idle_profile_scope` gate, which these variants don't use.
- **`idle_profile_scope`:** from 16:41 on the fixed harness.
- **`read`:** from 14:54.

The path 1 row in the verdict table is `claudeUsageWindows` over `idle`,
`idle_profile_scope` and `turn_seeded`, pinned in
`TestClaudeUsageObservedFixtures`. The `turn` event matches the one
`turn_seeded` carries, so it adds nothing.

| Variant | Requests (sanitized trace) | Answer | Verdict |
| --- | --- | --- | --- |
| `idle` | startup GETs; no usage read | `rate_limits_available:false`, `rate_limits:null` | `usage_unavailable` |
| `idle_profile_scope` | profile 403; nonessential routes blocked; one usage GET | read 403 scope refusal; `rate_limits_available:true`, `rate_limits:null` | `usage_refused` |
| `turn` | startup GETs; one `POST /v1/messages`; headers `7d-utilization`, `7d-reset` only | one `rate_limit_event`: `seven_day`, `allowed`, `resetsAt` | `rate_limit_event_observed` |
| `turn_seeded` | profile 403; one `POST /v1/messages`; headers `7d-utilization`, `7d-reset` only | `rate_limits.seven_day`: `utilization` 57.0, `resets_at` | `usage_observed` |
| `read` | `GET https://api.anthropic.com/api/oauth/usage` only | 403 `permission_error`, `oauth_scope_insufficient`, requires `user:profile` | `unsupported` |

Sanitized examples are committed under `daemon/internal/ward/testdata/`:
`claude_usage_observed_{idle_profile_scope,turn_seeded}.jsonl` and
`claude_usage_read_observed.json`.

- **No five-hour or model-scoped headers.** The 2026-11-03 turns carried
  neither. The event named `seven_day`, where #1713's turn named
  `five_hour`: the event names whichever window binds at run time.
- **No refresh.** Every variant counted zero refresh attempts. The auth
  sentinel and token snapshot were unchanged before and after each run. The
  snapshot holds no refresh token.

### The 429 On The Usage Route

The usage route throttles hard, and the cause is not established. Every
real-token read on 2026-11-03:

| Time | Reader | Headers | Answer |
| --- | --- | --- | --- |
| 14:38:03 | CLI | `(external, sdk-cli)`, `Accept` | 403 (body not yet classified) |
| 14:38:52 | path 2 | `(external, cli)`, no `Accept` | 429 |
| 14:51:44 | path 2 | `(external, cli)`, no `Accept` | 429 |
| 14:54:54 | path 2 | `(external, sdk-cli)`, `Accept` | 403 scope refusal |
| 14:55:43 | CLI | `(external, sdk-cli)`, `Accept` | 429 |
| 15:07:51 | CLI | `(external, sdk-cli)`, `Accept` | 429 |
| 15:53:57 | CLI | `(external, sdk-cli)`, `Accept` | 403 (compressed; not classified) |
| 16:41:38 | CLI | `(external, sdk-cli)`, `Accept` | 403 scope refusal |

An invalid synthetic token also got 429, and those reads didn't block the
real token's 403 two minutes later. The same headers got both answers, so the
headers don't explain the 429. Chose to send the headers the proxy observed
on the CLI's own read, over the ones inferred offline, because path 2 asks
how the provider answers that read. A 429 is `failed:unreviewed`, never
`unsupported`.

## Path 2 Boundary

The read runs as the dropped identity in the read driver
(`testdata/claude_usage_read.cjs`):

- **One target.** It sends one request to
  `GET https://api.anthropic.com/api/oauth/usage`, through a hand-opened
  CONNECT tunnel.
- **Proxy limits.** The proxy admits only the `api.anthropic.com:443`
  authority and only that route, and it refuses refresh before forwarding.
  The CA check is pinned.
- **Fail closed.** The request names an inert `127.0.0.1:9` host, so any
  bypass of the tunnel fails locally.
- **Output.** Only status, a bounded body and failure flags leave the child.

One rule (`claudeUsageReadClass`) separates the answers:

- **Supported:** a 200 whose fields are all reviewed.
- **`failed:auth`:** a 401, or a refusal that names an expired, revoked or
  invalid token.
- **`unsupported`:** any other 403 `permission_error` that names a scope.
- **`failed:unreviewed`:** anything else.
- **`failed:timeout`** and **`failed:transport`:** told apart from the above.

The fixture tests fixed this rule before the first live run. The second
bullet's wording was then tightened in the refute-first pass, so that a
scope mention no longer outranks an auth failure.

## Refute-First Pass

Path 2 adds a new holder of the real token, so an independent reviewer tried
to break the change, then re-reviewed the fixes. The offline tests and the
live runs found the first, third and sixth items below; the reviewer found
the rest.

- **Confirmed, fixed: the read driver bypassed the tunnel.**
  - **Defect:** with `agent: false`, Node ignores `createConnection` and
    sends the request in plain HTTP to the named host. A host-side offline
    test sent the synthetic bearer to the real provider host this way.
  - **Fix:** the driver now omits `agent` and names the inert loopback
    target.
  - **Proof:** reintroducing `agent: false` fails `TestClaudeUsageReadDriver`.
- **Confirmed, fixed: `idle_profile_scope` read any refused read as
  `usage_withheld`.**
  - **Defect:** the CLI folds an auth failure, a throttle, or no read at all
    into a null answer. An auth failure therefore gave every window
    `unsupported`.
  - **Fix:** the proxy now classifies the CLI's read by the path 2 rule and
    relays it unchanged. The variant counts only with exactly one read; a
    scope refusal is `usage_refused`, and anything else is `probe_failed`.
  - **Live check:** the run at 14:55 hit a 429 and is `probe_failed`.
- **Confirmed live, fixed: the proxy could not read the CLI's compressed
  answer.**
  - **Defect:** the CLI asks for brotli among other encodings, and Go
    relays an encoding the client asked for without decoding it. The 403 at
    15:53:57 was therefore classified `failed:unreviewed`.
  - **Fix:** on the usage route only, the proxy now drops the CLI's
    `Accept-Encoding`, so Go's transport negotiates gzip and decodes it.
    This is a recorded measurement deviation. An answer that still carries
    an encoding is `failed:body`, and the test pins both cases.
  - **Fail-safe held:** the defect produced `probe_failed`, never a verdict.
- **Confirmed, fixed: no test pinned the driver's CA check.**
  `TestClaudeUsageReadDriverVerifiesProxy` now fails if the driver accepts
  an untrusted proxy.
- **Plausible, fixed: a refusal that names both an expired token and a scope
  was `unsupported`.** Auth markers now take precedence.
- **Confirmed during development, fixed: a pattern-based path label could
  echo path text.** Labels now come from a fixed reviewed route set.
- **Confirmed on re-review, fixed: a readable answer the CLI dropped kept
  `usage_withheld`.** If the proxy classifies the CLI's read as supported
  but the CLI still answers null, the variant is now `probe_failed`, not an
  `unsupported` window.
- **Confirmed on re-review, fixed: no test pinned the scope of the
  `Accept-Encoding` drop.** `TestClaudeUsageAcceptEncodingScope` now fails
  if the drop reaches another route or mode.
- **Declined: a real-transport gzip test.** A transport the test built
  would not be the proxy's own, so it could not catch the regression it
  targets. The reviewer's scratch run confirmed the standard library
  decodes gzip and relays correct framing.
- **Disproved by checks:**
  - **No secrets in output:** no token, `Authorization` value, identifier or
    stderr reaches test output, fixtures or this note. Each log and capture
    was grepped for the token.
  - **One authority:** the read cannot reach a second authority or route.
    Read mode refuses `claude.ai` at CONNECT and refuses any other route;
    neither side follows redirects.
  - **Failed probes give no verdict:** a failed probe never yields a window
    verdict.
  - **No unearned share:** `share` requires a numeric, non-null
    `utilization`.
  - **No destructive overreach:** the change adds no resource type, and
    cleanup ownership checks are unchanged.

## Revisit When

- **The CLI pin moves:** the gates, seed headers and essential-traffic rule
  above are from 2.1.220's code.
- **Five-hour headers appear on a turn:** that would make five-hour a share
  through the same seed.
- **The owner widens the credential mode to include `user:profile`:** the
  usage route then needs a fresh path 2 run.
- **The 429's cause matters:** for example, a collector that reads the route
  on a timer.
