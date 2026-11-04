# Codex Account Probe Auth-Host Idle Closes

Issue #2498. Decided by the agent under the owner's planned approach.

## Decision

Chose to count an idle close on `auth.openai.com:443` in the Codex account
probe's proxy as a failure, labeled `auth_connection_closed`, as the usage
probe already does (`2026-10-05-1743-codex-usage-spike.md`). An idle close is
a tunnel that completes the TLS handshake and then closes with no request
bytes. On `chatgpt.com` it stays `connection_closed`, which doesn't count.

This extends the handshake rule in
`2026-11-03-0829-codex-account-handshake-hangups.md`. Its argument applies
unchanged: on the auth host, the request the CLI never sent could have been a
refresh, so a read pass can't prove that no refresh was attempted. An idle
close on `chatgpt.com` stays harmless, because every refresh that note
observed arrived on the auth host.

## Evidence

Six full `TestLiveCodexAccountProbe` runs on 2026-11-04, after the change:

| Run | `fresh` | `near_expiry` | `control` | Overall |
| --- | --- | --- | --- | --- |
| 1 | `pass`, 0 | `fail`, 0 | `pass`, 0 | `fail` |
| 2 | `pass`, 0 | `fail`, 0 | `pass`, 0 | `fail` |
| 3 | `pass`, 0 | `fail`, 0 | `pass`, 0 | `fail` |
| 4 | `pass`, 0 | `fail`, 0 | `pass`, 0 | `fail` |
| 5 | `pass`, 0 | `fail`, 0 | `pass`, 0 | `fail` |
| 6 | `pass`, 0 | `fail`, 0 | `pass`, 0 | `fail` |

Each cell is the case's verdict and its count of `auth_connection_closed`
labels. No case in any run recorded one, so the rule cost `fresh` nothing
here. `connection_closed` on `chatgpt.com` appeared 0 to 2 times per case,
and no case recorded `tls_failed`. `overall=fail` is the recorded negative
result (`2026-10-05-1130-codex-account-negative-result.md`): `near_expiry`
attempts a refresh, 3 or 4 times per run.

The README's pinned image
(`127.0.0.1:5066/freeside-account-866@sha256:396ff6dd…`) was no longer in
the local store. The runs used a rebuild of the same Codex 0.147.0 image:
the Containerfile and build script from the parent of the 0.162.1 pin
(`99f6b0b9^`), whose base image and package are both sha256-pinned. That
gives `127.0.0.1:5066/freeside-account-2498@sha256:4cb55e782140c65f7219bc272319cdb06e6b6be5c5bb61d8c0133d10ee1b55b9`.
The build script asserts the CLI version, and the probe rejects any other.
Only the unpinned Debian packages, such as `jq`, can differ.

## Fixtures

The observed fixtures stay as they are. They hold labels recorded under the
old rule, and the fixture test analyzes those stored labels, so it still
passes. `codex_account_observed_fresh.jsonl` has two `connection_closed`
labels whose host was never recorded. The passing test doesn't show those
closes were on `chatgpt.com`. It shows only that the stored labels still
analyze the same way.

## Rejected Options

- **Keep ignoring idle closes on both hosts.** A read could pass after the
  CLI opened a tunnel to the auth host and dropped it before sending a
  refresh.
- **Count idle closes on both hosts.** `chatgpt.com` carries no refresh, and
  `fresh` sees idle closes there in a third of these runs.

## Revisit When

A run shows `auth_connection_closed` making `fresh` mostly `probe_failed`,
the probe moves to a newer Codex CLI, or a refresh is ever observed on
`chatgpt.com`.
