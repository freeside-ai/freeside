# Subscription Usage Visibility Before the 1B.1 Exit

Plan revision 77. Owner decision of 2026-10-02. Follows revision 73
(`devlog/2026-09-28-1830-per-task-agent-choice.md`), which let the operator
choose an agent per task, and revision 74
(`devlog/2026-09-29-0913-capacity-wave-placement.md`), which placed that work
in wave 9.

## Changed Assumptions

- **The plan treated Claude account state as unobservable.** Section 5.4 said
  the pinned Claude CLI exposes a token digest and an auth check, so plan,
  quota, and expiry were out of reach and the probe's floor was integrity
  plus authentication. T3 Code reads subscription windows from the Agent
  SDK's usage control request on an idle query and from streamed rate-limit
  events, so the question is not whether the CLI family can report usage but
  whether Freeside's pinned build does so under its setup-token credential.
  That is now an open question for a spike, not a settled limit.
- **Usage display was Phase 3 work.** Revision 73 gave the operator a task
  line to move waiting work between accounts, and its note said repeated
  manual moves are the signal that routing should absorb. The operator makes
  those moves today with no reading of any account's allowance. Routing that
  acts on usage needs reliable observations first, so the observations and
  an explicit choice come before automation, inside 1B.1 rather than after
  it.

## Chose Observation and Explicit Choice Over Automatic Balancing

Chose to deliver, before the 1B.1 exit, a usage observation per usage pool
and provider window, its collection and refresh, an account usage view on
Mac and iPhone, and the same facts beside the agent choices the task lines
introduced. The operator reads the allowance and picks the agent; nothing
switches on a reading. Rejected:

- **Automatic account balancing now.** It would act on observations no spike
  has proven, and a switch driven by a reading is the silent fallback the
  attention model forbids. It stays Phase 3 routing work.
- **Reset-credit redemption by Freeside.** A credit restores allowance by
  spending something the provider accounts for; the operator redeems it, and
  Freeside only shows that one exists.
- **Switching credentials inside a running attempt.** An attempt's admission
  record names one identity; a change takes effect on the next attempt.

## Chose the Usage Pool as the Observation Key

Chose to attach observations to the usage pool, with the enrollment as the
observing source, because the provider meters the pool: two enrollments on
one subscription see one allowance. The pool record and its migration arrive
with #1596, so the observation record is `starts-after` #1596 rather than a
second pool definition. Rejected: keying by identity (a shared subscription
would show its allowance once per enrollment, and a display could add them
up).

One enrollment per pool is the collector (owner decision after review): the
pool's first enrollment by default, changed only by explicit operator
reassignment, never by silent failover. Only the collector supplies the
pool's window readings and refresh status, so a full response replaces the
window set with no flicker between clients; each enrollment may still record
its own availability answer, which is what shows the operator which
enrollment could be assigned. Rejected: taking windows from whichever
enrollment collected last (two clients exposing different window subsets
would make a model-scoped window appear and disappear); and reconciling
per-enrollment snapshots under completeness rules (a merge no spike has
evidence for).

Five readings stay distinct fields: credential expiry (an
enrollment-generation fact), usage reset (one window renewing), billing
renewal (where exposed), reset credit (an operator action), and run
consumption (what one invocation drew). A single "resets at" would make a
credential that expires tomorrow and a window that renews in an hour
indistinguishable.

## Chose a New Wave 10 Over Folding Into Wave 9

Revision 74 rejected a new wave because it renumbers the table for one
cluster. What changed: this is not one deferral cluster but a feature across
four lanes (ward collectors, spine contracts, signet sync and projection,
saddle views) with its own contract chain, two spike gates, and an exit proof,
and none of it was in the queue when revision 74 was decided. Rejected:

- **Wave 9.** It is already split-eligible on chain length, and the
  collectors wait on wave-9 units (#979 for agent facts, #406 for the Codex
  adapter, #866 for refresh safety), so most of this work could not start
  until wave 9 was nearly closed.
- **The initiative-view wave.** It shares nothing with this work and is
  1B.2, after the 1B.1 exit this feature is meant to precede.

Cost accepted: the 1B.1 exit and the initiative view each slip one wave.

## Evidence From T3 Code and CLIProxyAPI

Both were read at pinned revisions: T3 Code `fd7ee2c3` (2026-10-02) and
CLIProxyAPI `2044a01` (2026-10-03). They are implementation references for
normalization and failure handling, not proof of what Freeside can observe.

| Question | T3 Code | CLIProxyAPI | Unproven for Freeside |
| --- | --- | --- | --- |
| Claude idle read | Agent SDK (`^0.3.276`) usage control request on a query whose prompt never yields, after initialization; the SDK names the method experimental. Fields: `five_hour`, `seven_day` with `utilization` and `resets_at`, plus model-scoped limits. | None; it only observes `anthropic-ratelimit-unified-*` headers (5h, 7d, overage, status, utilization, reset) on real responses. Its direct `api/oauth/usage` call runs through its own refreshable token store. | Whether the pinned CLI (`2.1.220`) answers the control request over stream-json under a setup token, and without an inference request. |
| Claude live read | `rate_limit_event` stream messages during a turn. | Same headers per response, kept as one replaced snapshot. | Whether the event appears in Freeside's `-p --output-format stream-json` mode. |
| Codex idle read | App-server `account/rateLimits/read`; windows carry `usedPercent`, `resetsAt`, `windowDurationMins`, plus per-limit snapshots, plan, and credits. T3 also reads `chatgpt.com/backend-api/wham/usage` through CLIProxyAPI. | Parses `codex.rate_limits` websocket events and `x-codex-*` headers into one bounded snapshot. | Whether the read runs against the access-only snapshot without a refresh (#866's rule) under Freeside's pinned Codex (`0.147.0`). |
| Codex live read | `account/rateLimits/updated`, a partial view merged onto earlier windows; a model-scoped snapshot never overwrites the main one. | Same event, snapshot-replaced per response. | Same as above. |
| Freshness and failure | `checkedAt` per reading; a failed probe keeps the published reading; `unsupported` and `probeFailed` are distinct. | Observation signals are kept apart from cooldown fields so the management view cannot be mistaken for scheduler state. | None; these shape Freeside's rules directly. |
| Credential effects | Probe reads only. Credit redemption is a separate operator action with its own confirmation. | The management proxy resolves and may refresh the stored token before any call. | Freeside has no refreshable store; collection must never write the auth store. |

Lessons carried into the plan: keep the observation separate from any
scheduler state; replace a snapshot rather than accumulate windows across
responses; merge partial live updates onto the last full reading; treat a
model-scoped limit as its own window; and report unsupported, failed, and
stale as three different things.

## Revisit When

- A usage spike shows a provider cannot be observed with Freeside's supported
  credential. `unsupported` is honest behavior, but it does not deliver the
  feature for that provider, and the owner decides whether a wider credential
  mode is acceptable.
- The operator still moves waiting tasks between accounts by hand after the
  readings exist. That is the routing signal revision 73 named, and the
  observations are then the input routing was waiting for.
- A provider exposes a billing renewal or a reset credit through the
  supported credential. The fields are reserved; collectors fill them only on
  evidence.
