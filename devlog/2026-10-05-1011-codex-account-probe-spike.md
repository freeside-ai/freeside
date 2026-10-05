# Codex Account Probe Attempts Refresh Outside the Lease

Work unit: #866. Scope: test-only ward code, daemon run instructions, this note.

## Decision

Do not enable the proposed doctor account probe (#868). The pinned CLI
attempts OAuth refreshes with a synthetic near-expiry access-only store even
when `account/read` requests `refreshToken: false`. The proxy blocks those
requests, but the attempt alone violates the spike's safety criterion.

Keep the account probe behind the empirical gate in plan §5.4 and §10. A
source reading of `account/read` does not prove that startup and initialization
avoid the CLI's proactive refresh path. Measure the whole invocation, with
the real-account case, a synthetic near-expiry case, and a forced-refresh
control kept separate. A missing control or real-account observation cannot
produce an overall pass.

The owner declined manual credential preparation in the implementation
session. Continue with synthetic measurements and leave the real-account
evidence gap explicit. Do not read the refreshable host store or derive a
credential from it. That would violate this spike's input boundary merely to
make the experiment convenient.

## Pinned Invocation

CLI: `codex-cli 0.147.0`, verified by running the binary with no credentials
or networking. Image built from the repository's unchanged Codex Containerfile:

`127.0.0.1:5066/freeside-account-866@sha256:396ff6ddddd32b274811875180d9fea131bb06b430dce58ae43844f3080fe1ed`

The image contains `jq`. The previously cached images lacked that dependency,
so they could verify `--version` and `app-server --help`, but could not run this
driver. Rebuilding from pinned inputs is preferable to changing production
image definitions for the experiment.

The driver starts `codex app-server` with its default `stdio://` transport,
then sends these newline-delimited messages:

```json
{"id":1,"method":"initialize","params":{"clientInfo":{"name":"freeside_account_probe","version":"1"}}}
{"method":"initialized"}
{"id":2,"method":"account/read","params":{"refreshToken":false}}
```

The control changes only `refreshToken` to `true`. It uses the synthetic
access-only store, never a real refresh credential. The synthetic access and
ID tokens carry a made-up account and expire two minutes after construction.
The real case requires more than ten minutes remaining.

The CLI environment is empty except for `PATH`, the production `HOME` and
`CODEX_HOME`, the six production proxy variables, and `CODEX_CA_CERTIFICATE`.
There is no API key or refresh-endpoint override. `CODEX_HOME/auth.json` is a
symlink to `/var/lib/freeside/codex-snapshot/auth.json`, on a read-only volume.
The networkless seeder stops before the observer mounts that volume.

## What the Measurement Proves

The observer has exactly one host-only network, no published ports or sockets,
no SSH forwarding, and the declared read-only mounts. Its only permitted
HTTP route is the test proxy. The proxy never forwards a request; it terminates
TLS and labels requests to `auth.openai.com` or a path containing `oauth/token`
as `blocked_refresh`. All other routes are denied. A completed denied ancillary
request does not invalidate a completed account read. TLS failures prevent
proving absence of refresh, but cannot erase a refresh the proxy actually
decoded and blocked. With a valid completed capture, positive detection takes
precedence: the read fails; the control proves the detector. Any extra TLS
failures stay in evidence. Without positive detection, transport failures
mean `probe_failed`. Parsing, timeout, and capture failures always mean
`probe_failed`. Independent review confirmed this distinction between proving
presence and proving absence; no failed transport can produce a read pass.
Headers, bodies, query strings, and arbitrary request paths never enter logs.

The root CA and server leaf are separate certificates. Reusing the CA as the
server certificate worked with the Go test client but produced TLS failures
with the pinned Rust client. The offline proxy test alone was insufficient to
prove the CLI's trust configuration.

Raw RPC output stays inside the observer. The driver bounds it to 1 MiB plus
one overflow-detection byte, and bounds the protocol session to 60 seconds.
Bytewise `dd` forwards small responses immediately; a buffered bounder can
hide initialization until the deadline and manufacture a timeout.

The sanitizer permits only the pinned subscription response shape and plan
enum. Its schema was checked against upstream tag `rust-v0.147.0`,
`app-server-protocol/schema/json/v2/GetAccountResponse.json`: `account` can be
null; a ChatGPT account has `type`, nullable `email`, and `planType`, with
`requiresOpenaiAuth` on the outer result. Only field names, types and the plan
enum leave the container. RPC error text never does.

Hashes prove that the operator input and snapshot did not change. The driver
also checks symlink preservation and proves append and unlink are rejected.
Cleanup requires the original ownership labels and creation dates; its
manifest survives failures so resources are not deleted by name alone.

## Refutation

- Confirmed and fixed: the original jq parameter named `$keys` shadowed the
  builtin `keys` filter. Unknown-field regression cases exercise the actual
  reducer and prevent this false acceptance from returning.
- Disproved by offline checks: a refresh request can pass through the proxy;
  both the OAuth host and token path return a denial without an upstream
  transport.
- Disproved by offline checks: private email, account-ID, unknown-field, or
  arbitrary plan values can enter sanitized evidence. Unknown shapes fail
  capture; only the pinned plan enum is retained.
- Disproved by the verdict table: a missing control, timeout, malformed
  capture, or transport failure can become a safe read pass. A partial run reports
  `overall=probe_failed`.
- Independent static review checked credential handling, verdict gates,
  network topology, and ownership checks. Live evidence remains a separate
  requirement from that review.
- Confirmed and fixed by independent review: `http.ReadRequest` can return
  `io.EOF` after consuming a long unterminated request line. An idle shutdown
  connection is exempt from failure only if `Peek(1)` proves zero decrypted
  HTTP bytes arrived. Both short and 8-KiB partial requests remain failures.

## Observations and Limits

| Case | Verdict | Account Response | Auth Store |
| --- | --- | --- | --- |
| Near expiry, synthetic, `refreshToken: false` | `fail`: refresh requests attempted and blocked | ChatGPT shape, plan `plus` | Hash unchanged; symlink preserved; append and unlink denied |
| Control, synthetic, `refreshToken: true` | `pass`: detector sees and blocks refresh; one additional TLS failure retained | ChatGPT shape, plan `plus` | Same protections held |
| Real account | Not run: no operator-provided access-only input | Unverified | Unverified |

The committed `codex_account_observed_*.jsonl` files are sanitized captures
from the synthetic live run, not fabricated provider responses. The field
types observed were `account: object`, `requiresOpenaiAuth: boolean`, and
`account.type`, `account.email`, and `account.planType`: `string`. The retained
plan was `plus`, from the made-up token claims; it is not a claim about an
operator's subscription.

The final near-expiry capture records four `blocked_refresh` requests and
zero transport failures. The control records six `blocked_refresh` requests
and one `tls_failed`. Its pass means the refresh detector was proved, not
that every background connection completed successfully. The experiment does
not establish the cause of that additional TLS failure.

The full three-case suite remains `probe_failed` because the real-account
measurement is missing. The independently measured near-expiry counterexample
still disproves the proposed no-refresh guarantee. It needs no real credential
to show that startup/initialization plus the account read is unsafe outside
the lease. The experiment does not isolate which startup task triggered each
refresh, and the number and ordering of background requests can vary.

The live test deliberately exits nonzero for this negative result. Offline
fixture tests preserve that verdict instead of making the live check pass by
accepting refresh. #866 retains the unrun real-account acceptance item; this
evidence does not imply that every acceptance bullet is complete. #1714 can
reuse the bounded driver, but must keep the same refresh boundary in view.

## Revisit When

Rerun when the CLI version or image digest changes, the account response
schema changes, the production snapshot layout or auth mode changes, or the
app-server startup path changes. #868 must cite this experiment's verdict
before enabling the doctor account probe. #1714 may extend the request list
and sanitizer for usage observation, but an account result proves nothing
about `account/rateLimits/read`.
