# Measure Claude Usage Without Trusting Auth-File Stability Alone

Decision scope: #1713. Follows
[`2026-10-02-1819-subscription-usage-visibility.md`](2026-10-02-1819-subscription-usage-visibility.md),
the usage contract introduced in plan revision 77. No production collector
or credential-mode change.

## Measurement Boundary

Chose a test-only TLS request observer on a host-only container network over
counting CONNECT authorities. CONNECT cannot distinguish inference, usage
reads and refresh when they share a provider hostname. An unchanged token
file also cannot prove that no refresh request occurred.

The probe supplies only the setup token to the pinned CLI's child process.
The container record holds no credential environment variable. A networkless
seeder populates a named snapshot, then stops before the probe mounts it
read-only. The token stays on the snapshot. The empty auth file is root-owned
under root-owned sticky directories, allowing ordinary config writes while
protecting auth storage. Before launching Claude, the driver checks that the
dropped identity cannot append to or unlink either auth file; it checks their
contents again after the invocation. Before each live invocation, the root
driver deliberately changes the empty auth sentinel, proves that this same
check rejects the mutation, and restores it.

The test proxy terminates CLI TLS with an ephemeral CA and verifies provider
TLS normally. It blocks refresh routes and any inference request in the idle
probe before forwarding. The realized host-only network prevents direct
internet egress. A rejected route, TLS failure or provider error invalidates
the measurement rather than proving lack of provider support. The exception
is HTTP 404 from the exact startup GETs `/api/claude_code/settings` and
`/api/claude_code/policy_limits`: the pinned binary explicitly treats these
as successful empty settings/policy responses. Auth errors, server errors,
and a 404 from a usage endpoint remain failures. This
instrumentation is an experimental deviation from the writer's transport;
it changes no host trust store or production code.

Both proxy legs use HTTP/1.1. The ephemeral CA uses P-256 because the pinned
native CLI does not offer Ed25519 TLS signature algorithms. The proxy waits
for outstanding handlers before recording a verdict and counts late body or
relay failures, including failures followed by a successful CLI retry.

## Pinned Protocol And Launches

The cached image's manifest reports Claude `2.1.220` and Node `24.18.0`:

`docker.io/library/freeside-usage-1713@sha256:3c0874d23551aab90805279c29e231bf55cffefa1cd6788b9c894298358666f8`

This is a task-specific local alias of the verified cached image. The CLI
version check ran inside each probe container, not against the host CLI.

Offline inspection of its native CLI finds `get_usage`, `rate_limit_event`,
and the refusal string `get_usage is not supported in this context
(onGetUsage callback not registered)`. Strings establish probe candidates,
not runtime support. The harness separately verifies `claude --version`
before an experiment.

Both launches use `-p --output-format stream-json --verbose
--dangerously-skip-permissions --safe-mode`, a fixed session ID, an isolated
workspace, and an appended minimal instruction file. The writer-mode case
sends one fixed prompt on stdin. The idle variant additionally uses
`--input-format stream-json` and sends these newline-delimited requests,
waiting for the matching initialization response before the usage request:

```json
{"type":"control_request","request_id":"usage-init","request":{"subtype":"initialize"}}
{"type":"control_request","request_id":"usage-read","request":{"subtype":"get_usage"}}
```

The request is proved only for the launch where it actually runs. Each path
has a 60-second deadline and a 1-MiB output bound.

## Observed Contract

The live observations on 2026-10-03 used only the setup token. Both CLI
processes exited zero without timeout. The idle variant made zero inference
requests; the turn made one. Both recorded zero refresh attempts, unchanged
auth files, and zero transport failures. Sanitized captures are in
`daemon/internal/ward/testdata/claude_usage_observed_{idle,turn}.jsonl`.

**Idle verdict: protocol supported, allowance unavailable.** `initialize`
and `get_usage` both returned matching successful control responses. The
usage response contained this allowance subset:

```json
{"subscription_type":null,"rate_limits_available":false,"rate_limits":null}
```

The same envelope also contained `session` (run cost, duration, lines and
model consumption) and `behaviors` (local activity counters). They are not
subscription allowance and are omitted from the fixture. Initialization's
account and capability details are also omitted. Null plan and limits stay
null; availability is explicitly false, not a missing or failed response.
The harness verdict is `usage_unavailable`, not `unsupported_request`.

**Turn verdict: a rate-limit event is emitted, but no used amount.** The
ordinary writer output mode emitted this event and a successful final result:

```json
{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1791072000,"rateLimitType":"five_hour","overageStatus":"rejected","overageDisabledReason":"org_level_disabled","isUsingOverage":false}}
```

| Field | Observed Meaning And Limit |
| --- | --- |
| `rateLimitType: "five_hour"` | Provider window identifier. No separate duration field was returned. |
| `resetsAt: 1791072000` | Unix epoch seconds, corresponding to `2026-10-04T00:00:00Z`. No duration follows from this instant. |
| `status: "allowed"` | Categorical permission status, not a used or remaining share. |
| `overageStatus: "rejected"` | Categorical overage status, not a quantity. |
| `overageDisabledReason: "org_level_disabled"` | Provider's explicit overage reason. |
| `isUsingOverage: false` | Explicit boolean. |

No used share, quantity, unit, explicit window length, weekly window,
model-scoped window, or overage reset was returned in this event. Those
fields are absent, not zero. No percent scale can be normalized from it.
The pinned binary establishes the reset unit: it compares `resetsAt` with
`Date.now()/1000` and converts usage reset values to ISO timestamps with
`new Date(value*1000)`. Its availability flag combines OAuth eligibility and
the required profile scope; the observed false/null response does not tell
which prerequisite is absent, so this note makes no account-scope claim.
The final result's run usage and cost are excluded. This is one partial
event, not a complete allowance snapshot or proof that other events can
never carry more fields.

## Collector Consequence

#1717 may use the idle **duplex-input variant** for the enrollment's
availability answer: this setup token reports allowance unavailable. No
quantitative allowance collection path was proved, so its collector must
report `unsupported` with no synthetic window readings under the current
contract. The ordinary writer launch is proved to emit incidental limit
metadata during a real turn; it is not an idle polling path. Never run
inference solely to obtain an event, infer utilization from `allowed`, or
treat a window name/reset as an explicit duration.

If allowance visibility requires a broader credential mode, the owner must
decide that separately, as the source note requires. This experiment does
not authorize interactive login, token refresh or a direct usage API fallback.

## Evidence Checks

Synthetic fixtures test response correlation, field and null preservation,
missing fields, malformed or truncated messages, secret rejection, and the
distinction between unrelated result usage and subscription allowance. They
must never be promoted to positive provider fixtures. No normalization scale
or window duration is inferred from their example numbers or timestamps.

The refutation checks inject inference and refresh requests through the TLS
observer, verify forbidden requests never reach its synthetic upstream, and
change auth files to prove the post-run check notices. Raw CLI stderr is
discarded. A private bounded stdout capture is sanitized before any evidence
is printed; unknown usage fields or values fail capture for explicit review.

Independent refutation found that null values bypassed field admission and
that late response-body failures could escape the verdict. Both now have
regression coverage. Further checks cover HTTP/2 response conversion at the
proxy boundary and the narrow successful-empty startup 404 classification.
Early invalid attempts remain failed probes; they are not evidence against
provider support. The observed schema, including the unavailable wrapper
and overage reason, was admitted only after reviewing the private captures.

## Ephemeral Ownership

The planning example's production rig requirement does not apply here. Plan
§10 requires that lease for attended real-work runs sharing production
GitHub App authority. This probe starts no daemon and has no publication
credentials. Borrowing production resources would add disruption without
protecting that boundary, so it uses standalone ephemeral containers instead.
The operator-owned supervised dev instance is also outside this experiment.

Each invocation records random names and labels before resource creation,
then records observed creation dates. Cleanup checks those fingerprints
before stopping or deleting anything. Failed cleanup retains a private,
non-secret manifest for recovery. Host secret inputs use temporary-directory
cleanup; snapshot credentials and raw capture require successful runtime
cleanup. A failed cleanup can retain those private resources and must be
recovered by the recorded identities. An independent review confirmed the
ownership and auth-file protections. It found a recovery-record truncation
risk, fixed by writing a sibling and renaming it over the manifest.

## Revisit When

- A later pinned-build experiment returns actual allowance quantities or a
  usable idle rate-limit snapshot under the setup token.
- The owner chooses whether to widen the credential mode for allowance
  visibility. No such choice is implied by this spike.
- The CLI pin, launch flags, credential delivery or transport changes.
