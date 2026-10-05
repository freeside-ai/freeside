# Codex Account Facts Need No App-Server

Work unit: #866, follow-up to PR #1753 and
[the original measurement](2026-10-05-1011-codex-account-probe-spike.md).

## Decision

The owner chose to finish the spike with synthetic evidence and retire the
app-server account probe. Account facts come from stored ID-token claims; a
real account would add no evidence about whether this invocation attempts
refresh. A fresh synthetic baseline replaces the real-account acceptance
case. No real credential or host auth store is read.

The original note counted any request to the auth host as refresh. The
corrected detector requires exactly `POST /oauth/token`, regardless of host,
and emits only the fixed `blocked_refresh` label. Other methods and paths are
denied and labeled `blocked_other`. This closes the host-only ambiguity in the
original evidence without relaxing the deny-all proxy.

## Measurement

CLI: `codex-cli 0.147.0`. The unchanged image from the original run:

`127.0.0.1:5066/freeside-account-866@sha256:396ff6ddddd32b274811875180d9fea131bb06b430dce58ae43844f3080fe1ed`

With that digest in `FREESIDE_WARD_CODEX_AGENT_IMAGE`, run:

```sh
FREESIDE_WARD_LIVE_TEST=1 go -C daemon test ./internal/ward \
  -run '^TestLiveCodexAccountProbe$' -count=1 -v
```

The same bounded driver sends `initialize`, `initialized`, and `account/read`
with `refreshToken: false`, except the detector control asks for `true`.
Each store has made-up access and ID tokens and an empty refresh token.

| Case | Token Life | Token-Endpoint POSTs | Verdict |
| --- | --- | --- | --- |
| Fresh | One hour | 0 | `pass` |
| Near expiry | Two minutes | 4, all blocked | `fail` |
| Forced-refresh control | Two minutes | 7, all blocked | Detector `pass` |

All three returned the ChatGPT account shape and synthetic plan `plus`.
All three had zero transport failures, unchanged snapshot hashes, preserved
symlinks, and denied append/unlink attempts. Sanitized fixtures record the
field names, types, plan enum, fixed request labels, and protection flags.
The complete measurement is `overall=fail`, a valid negative result for the
original unconditional no-refresh-attempt hypothesis. The live test exits
nonzero intentionally; the regression fixtures preserve that result.

## Meaning and Limits

These tokenless requests could not rotate a credential: the snapshot has no
refresh token and the proxy forwards nothing. The negative result does not
prove that the contained review path can mutate credentials. The owner's
rule is no refresh that could change a credential; a tokenless attempt blocked
by egress is tolerated but scheduled around. The separate material plan
revision carries that policy correction; this harness continues to report
attempts distinctly from mutation.

For #868, decode account claims in the daemon and label them "as of the last
ID-token refresh". Use the ID token's own issuance time when available; otherwise
report unknown claim age. Never infer claim age from the auth store's
`last_refresh`: the refresh path can preserve the previous ID token when the
provider omits a replacement while advancing that timestamp. Claims cannot
report a subsequent revocation or plan change. The existing daemon parser
already decodes the access token for expiry; that is not yet an ID-token
account-facts implementation.

For #1714, measure the usage read separately. In upstream tag
`rust-v0.147.0`, `get_account_rate_limits_response` calls
`AuthManager::auth()`, whose proactive refresh window is five minutes. Its
collector needs a lifetime margin beyond that window, the access-only snapshot,
and `provider_only` egress. An account-read baseline proves nothing about a
network usage response or update notification.

## Refutation

The proxy regression sends GET to the token path, POST to another auth-host
path, and prefix/suffix lookalikes. None counts as refresh. Exact token POSTs
over both CONNECT/TLS and plain HTTP count and remain denied. The actual jq
reducer tests still reject unknown fields and private values; missing jq now
fails before running any case with an actionable prerequisite message.
Independent review found no further correctness, security, or regression
defect in the changed harness and fixtures.

## Revisit When

Rerun on a CLI or image change, snapshot-layout change, or startup-path change.
The request counts are observations, not stable API guarantees. Usage
collection remains its own spike and cannot inherit the account verdict.
