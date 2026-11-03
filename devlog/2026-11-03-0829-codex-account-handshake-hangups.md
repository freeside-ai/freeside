# Codex Account Probe Handshake Hang-Ups

Issue #2477. Decided by the agent under the owner's planned approach.

## Decision

Chose to classify a TLS handshake hang-up in the Codex account probe's proxy
by authority, as the usage probe already does
(`2026-10-05-1743-codex-usage-spike.md`, "The CLI abandons connections
routinely"): EOF, unexpected EOF, or a TCP reset during the handshake is
`handshake_closed` on `chatgpt.com`, which is not a transport failure, and
`auth_handshake_closed` on `auth.openai.com`, which is. Every other handshake
error stays `tls_failed` and still fails a read pass.

## Evidence

The `fresh` case's intermittent `probe_failed` came from these hang-ups. A
scratch build of the proxy logged each handshake error's class and authority
(never its text) over six full runs of the cached 0.147.0 image on
2026-11-03:

| Run | `fresh` | `control` | `near_expiry` |
| --- | --- | --- | --- |
| 1 | `chatgpt.com`: 2 reset | none | none |
| 2 | none | none | `chatgpt.com`: 1 EOF |
| 3 | `chatgpt.com`: 10 EOF | none | none |
| 4 | `chatgpt.com`: 5 reset, 1 EOF | `auth.openai.com`: 1 EOF | none |
| 5 | `chatgpt.com`: 2 reset | `chatgpt.com`: 1 reset | none |
| 6 | `chatgpt.com`: 1 reset, 8 EOF | `chatgpt.com`: 2 EOF | none |

All 34 errors were EOF or reset; none was a CA rejection or any other
handshake failure. Five of six `fresh` runs scored `probe_failed` only
because of them. One hang-up landed on the auth host, in `control`, which
passed on its observed refresh.

A second scratch build logged the authority of each request the proxy
decoded in one `near_expiry` and `control` pass: all 11 `POST /oauth/token`
requests arrived on `auth.openai.com`, and `chatgpt.com` saw only ancillary
requests. That supports ignoring a hang-up on `chatgpt.com`: the request the
CLI abandoned there would have been denied as ancillary traffic, not a
refresh.

After the change, five consecutive full runs each reported `fresh` `pass`,
zero `tls_failed`, and `overall=fail`, the negative result recorded in
`2026-10-05-1130-codex-account-negative-result.md` (`near_expiry` attempts a
refresh). `fresh` saw 0 to 9 `handshake_closed` per run; `near_expiry` and
`control` each saw one `auth_handshake_closed` in one run.

## Rejected Options

- **Count every hang-up as a failure.** It leaves most `fresh` runs
  inconclusive on this machine, as it did for the usage probe.
- **Ignore hang-ups on both hosts.** On the auth host the abandoned request
  could have been a refresh, so a read pass would no longer prove none was
  attempted.
- **Relax the analyzer's transport-failure rule.** A CA rejection or any
  other handshake error still means the probe can't prove absence.

## Revisit When

The probe moves to a newer Codex CLI, a run shows a `tls_failed` again, or a
refresh is ever observed on `chatgpt.com`.
