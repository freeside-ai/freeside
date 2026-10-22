# Proxy Credential Injection: The Writer Holds A Placeholder

Plan revision 95 (§5.2, §5.4 `subscription_injected`, §5.7
`supports_proxy_credential_injection` and Execution Hosts, §5.8, §11, §14),
stacked on revision 94 (#2169). Decided by the agent
as a proposal for the owner, under the mandatory-note rule for
credential-leak-surface work; the owner decisions it needs are at the end.
Spikes: #2173, #2174. Contract unit: #2175. This unit changes no
daemon code.

## Context

- **The writer holds the real token.** Under `subscription_contained` the
  Claude setup token (`sk-ant-oat01…`, a one-year OAuth grant) sits as a
  read-only file on the per-identity credential volume, and the launcher
  reads it into `CLAUDE_CODE_OAUTH_TOKEN` at exec
  (`daemon/internal/exec/claude/spec.go`). The Codex review container holds
  the access JWT only: the agent snapshot blanks the refresh token
  (`daemon/internal/ward/codex_review.go`) and the daemon refreshes on the
  host (`codex_auth_refresh.go`), with the refresh endpoint refused in the
  review profile.
- **The proxy sees SNI, not HTTP.** Each run gets one host-only network and
  one in-process CONNECT proxy at its gateway, admitting exact authorities
  with the server name pinned and never terminating TLS
  (`daemon/internal/ward/doc.go`, `egress.go`). The ward is the only real
  `RunnerBackend` (`backend.go`). The build proxy
  (`daemon/internal/buildproxy`) is the other daemon-side proxy and shares
  the gateway-binding discipline.
- **The Claude CLI reaches one host.** Spike #1620 recorded Claude Code
  2.1.220 under `provider_only` connecting only to `api.anthropic.com:443`.
  Codex review admits `chatgpt.com:443` (`api.openai.com:443` in API-key
  mode) (`daemon/cmd/freesided/claude_driver.go`).
- **Two directions wait on this decision.** Revision 94 (execution hosts,
  #2161) copies each identity's credential volume to every enrolled host
  for its owner-held first increment and records injection as the open
  decision gating any host the owner does not physically hold. The macOS
  execution class (revision 92) is open between a Virtualization.framework
  guest and a dedicated-user host process; a host-run writer would hold
  the token as a file on the host.

## Decisions

1. **The writer holds a placeholder; injection at the control plane's proxy
   is the end state, recorded as the credential mode `subscription_injected`.**
   Chose a new mode over changing `subscription_contained` in place. A
   credential mode is recorded on every admission and re-gated when the
   store reconstructs it, so the two postures must stay distinguishable in
   the record; contained keeps its documented residual and stays the
   fallback class. What injection buys: a one-year secret (Claude) or an
   hours-long one (Codex) in the writer becomes a capability that lives as
   long as the run and works only through one proxy instance; a leaked
   placeholder is worthless off-run; the proxy forwards a provider request
   only under the run's placeholder, so a writer cannot speak to the
   provider under a credential it finds in a workspace; and an execution
   host or a host-side execution context holds no provider secret at all.
2. **Termination happens at the control plane's per-run proxy under a
   per-run CA, delivered as a read-only mount named through each CLI's
   additive trust variable; no image change.** Anthropic documents
   `NODE_EXTRA_CA_CERTS` and a default store of bundled Mozilla roots plus
   the OS store, and states that TLS-inspection proxies work once their
   root is trusted, so the pinned Claude CLI does not pin the provider
   certificate. The pinned Codex 0.147.0 reads `CODEX_CA_CERTIFICATE`, then
   `SSL_CERT_FILE`, into the rustls root store shared by its HTTP and
   websocket clients (`codex-rs/http-client/src/custom_ca.rs` at
   `rust-v0.147.0`); a search of `openai/codex` for certificate pinning
   finds nothing. The repository already drives both live: the usage spike's
   terminating CONNECT fake under a self-signed CA against the pinned Claude
   image with `NODE_EXTRA_CA_CERTS`
   (`daemon/internal/ward/claude_usage_probe_test.go`,
   `claude_usage_live_test.go`) and the Codex account probe with
   `CODEX_CA_CERTIFICATE` (`daemon/internal/ward/testdata/codex_account_probe.sh`).
   Only the additive variables are set: `SSL_CERT_FILE` and its peers
   replace the store for git and libcurl, which a `provider_registry` run
   needs intact for its opaque registry tunnels. Both CLIs already honour
   `HTTPS_PROXY`, which is how the ward works today. The CA is generated at
   handoff and discarded at teardown: no durable key material, nothing to
   rotate, and a leaked CA signs nothing a later run trusts. The environment
   additions join the exact set the gate compares. Rejected: a
   daemon-lifetime CA (a durable secret with a rotation story, for no gain);
   setting the replacing variables; a plaintext gateway through
   `ANTHROPIC_BASE_URL` or Codex's `chatgpt_base_url` (it changes the pinned
   CLI's own code path, gateway semantics and the watchdog table in
   Anthropic's documentation, and departs from "vendor tooling stays native
   and unmodified"); it remains the fallback if a pinned CLI refuses the CA.
3. **Per-run issuance is structural.** Each run already has its own
   network and its own proxy; the placeholder is minted at handoff, bound
   to that proxy's run and identity, and the mapping dies with teardown. A
   placeholder presented to another run's proxy matches nothing and is
   refused without a dial. The proxy forwards a request only when it
   carries exactly the run's placeholder as its bearer and names a declared
   path for the identity: inference and the CLI's metadata paths, never
   token refresh, rotation, or account endpoints (the usage probe already
   classifies and blocks `/v1/oauth/token`, and the Codex review profile
   refuses `auth.openai.com`). On a forwarded request it substitutes the
   token, pins any account header the CLI sends (Codex's
   `chatgpt-account-id`) to the identity, and refuses credential shape
   anywhere else: another bearer, Anthropic's `x-api-key` header, cookies,
   query parameters. The terminating listener admits only the run's own
   network, with concurrent runs' subnets proven distinct; an execution
   host's requests arrive over the runner session, never as raw
   connections. Today's listener binds every interface and admits by
   subnet because it holds nothing. Registry authorities stay opaque tunnels:
   nothing there needs the token, and terminating them would widen the
   daemon's parsing surface for no credential gain.
4. **Every substitution is recorded; bodies never are.** Per request: run,
   identity, authority, method, path, status, bytes, and timing, on the
   handoff result and journal beside the realized allowlist
   (`EgressObservation`), readable through `freesided inspect`. Prompt and
   response bodies are never logged or retained.
5. **A new capability, not a redefinition.** `supports_proxy_credential_injection`
   is additive and earned by its own conformance probes: a placeholder
   launch reaches the declared provider authenticated as its identity; the
   same placeholder from a second run is refused; the identity's token is
   absent from the writer's filesystem, environment, inspect report, and
   export (a variant of the credential-marker probe); a provider request
   carrying a foreign bearer is refused; the proxy's CA and keys are absent
   after teardown. Rejected: redefining `supports_enforced_provider_egress`.
   It is persisted in admission records and re-gated on read, so a record
   proven under the old meaning would admit under the new.
6. **The credential volume stays the control plane's at-rest store.** The
   enrollment transaction still authors it; the proxy reads the token once
   per handoff through the exporter observer a judgment call uses (#1426,
   one exporter boot per read) and holds it in memory for the run; an
   injected run mounts none, so
   `supports_credential_volume_detach` is vacuous for it; an execution host
   never receives a credential volume for an injected run. Revision 94's
   per-host volume authoring (#2164) narrows to the contained first
   increment.
7. **On an execution host, the runner terminates and the control plane
   substitutes.** This adopts the shape revision 94 recorded once #2175
   existed: the runner's per-run proxy terminates the writer's TLS under the
   per-run CA, forwards the request over the authenticated runner session,
   and the control plane substitutes the credential and originates the
   provider connection from the daemon host, so the host holds the CA key
   and the placeholder and never the token. Rejected: this note's first
   shape, relaying the writer's opaque TLS bytes to the control plane's
   terminating proxy. It would make the one token-holding listener admit
   remote connections, where the runner session already authenticates the
   host and carries parsed requests, and the plaintext the runner sees is
   the writer's own traffic on the same host. Revision 94's rejection of a
   bare CONNECT relay as a credential measure stands.
8. **The file-based half-step does not open untrusted hosts.** Codex
   writers already have it: the access token only, refresh on the host.
   The Claude subscription's interactive login does issue an expiring
   access token with a refresh token, so a Codex-style host-side refresh is
   possible in principle, though the delivered form is the one-year setup
   token and no per-run mint exists. The rejection does not rest on that:
   a file token is usable from any host for its remaining lifetime, however
   short, so the half-step narrows the window without changing who can
   spend it. So the owner-held MacBook proceeds on
   the contained posture, as revision 94 says, and the Linux desktop, any
   cloud host, and any host-side writer context wait for injection.
   Rejected: short-lived file tokens as the multi-host answer.
9. **Sequencing.** Two spikes first, with no plan dependency: #2173
   proves placeholder acceptance and token placement per host and path on
   the exact pinned images (CA trust is already driven live in the
   repository, decision 2; the usage probe also admits `claude.ai` and
   `console.anthropic.com`, so the terminating allowlist must decide those
   hosts); #2174 proves the provider answers requests re-originated by the
   daemon's TLS client across a real run, including Codex's websocket
   transport and both CLIs' event streams. The contract unit #2175 follows them. Phase 2 by default;
   owner fiat pulls it forward when a host the owner does not physically
   hold is wanted, which is the Linux trigger revision 94 names.

## Threat Review

- **A compromised writer with a placeholder** can replay it within its run,
  which is what it can do with the token today. Exfiltration keeps the one
  channel `provider_only` leaves, the provider itself (search queries,
  #1620), and the placeholder is worthless there. Prompt-injected provider
  requests under the run's identity are unchanged in power, rate-limited by
  the provider as before, and now visible request by request in the audit.
  New: the writer can present only its own placeholder, so a credential it
  finds in a workspace no longer reaches the provider through the tunnel.
- **A compromised proxy is a compromised daemon host**, which already holds
  every token, the GitHub App key, every prompt, and every transcript. No
  new principal and no new secret holder. The per-run CA key is trusted by
  one writer whose only route is that proxy, so its leak gains nothing. The
  new surface is the daemon's HTTP stack parsing bytes a compromised writer
  sends, which the opaque tunnel never did. The contract unit bounds it:
  header, body, and connection limits (Go's server caps headers only);
  hop-by-hop headers stripped and no forwarding headers added; Codex's
  websocket upgrade and both CLIs' event streams carried through without
  interpretation; one HTTP version negotiated with the provider on purpose.
  The §5.6 gauntlet is out of process for a similar reason, and the
  contract unit may move the proxy out of process if the spikes' evidence
  asks for it.
- **What the owner loses.** Provider-side IP signals are unchanged: the
  daemon host is already the egress of every tunnel and remains the egress
  for every host. What changes is the client the provider sees: Go's TLS
  and HTTP stack instead of Node's or Rust's, which bot defences on
  `chatgpt.com` (behind Cloudflare; Codex keeps a cookie-affinity store for
  it) and `api.anthropic.com` may treat differently. The provider's
  certificate is verified by the proxy against the system roots with the
  server name pinned, not by the CLI. Prompts are already readable on the
  host, so no confidentiality is lost to a party that lacked it. The
  substituting proxy sends the request the CLI formed with one header
  changed; whether a vendor's subscription terms admit that is the owner's
  judgment under the §14 subscription-terms risk.

## Refute-First

A fresh-context reviewer was asked to refute the note and the plan diff
(`docs/agent-workflow.md` §refute-first). Its findings and their outcomes:

- **Confirmed, fixed: the branch was cut from `main` while revision 94 is
  open (#2169).** Restacked on `docs/execution-hosts`; §13 and the history
  move revision 94, and the Execution Hosts subsection records the decision
  instead of leaving it open. Revision 94 was then rewritten to cite #2175
  and choose runner-side termination with control-plane substitution;
  decision 7 now adopts that shape.
- **Confirmed, fixed: refusing "any other bearer" was narrower than the
  gain claimed.** `x-api-key`, `chatgpt-account-id`, cookies, and query
  parameters passed. Decision 3 and §5.4 now refuse credential shape
  anywhere and pin the account header.
- **Confirmed, fixed: authority-only substitution forwarded the token to
  refresh and account paths.** Decision 3 and §5.4 scope substitution to
  declared paths and refuse refresh, rotation, and account endpoints, as
  the usage probe already does.
- **Confirmed, fixed: `SSL_CERT_FILE` replaces the store for git and
  libcurl**, which would break `provider_registry` tunnels. Only the
  additive variables are set (decision 2, §5.4).
- **Allowed, scoped: the forwarder's surface is wider than one header.**
  Websocket upgrade, event streams, HTTP version, hop-by-hop and forwarding
  headers, body caps, and timeouts are named as the contract unit's bound
  (§5.8, #2175) and the spike's scope (#2174).
- **Confirmed, fixed: the listener binds every interface and admits by
  subnet.** Harmless while it holds nothing; a terminating listener admits
  only the run's network; execution-host requests arrive over the runner
  session instead (decision 3, §5.8, #2175).
- **Disproved: CA trust is unproven.** The repository already drives both
  CLIs against a terminating fake under a self-signed CA; #2173 narrows to
  placeholder acceptance and token placement.
- **Allowed, scoped: the Claude CLI also dials `claude.ai` and
  `console.anthropic.com` on some paths.** #2173 records which hosts and
  paths carry the token; the terminating allowlist decides them.
- **Disproved as a defect, refined: redefining the egress capability.**
  Re-gating compares names, so the rejection stands; the plan text now says
  the capability joins the provable set and a credential-mode floor the
  contract unit adds makes it a minimum, rather than naming a gate that
  does not exist.
- **Allowed, reworded: "the setup token has no short-lived form"
  overreached.** The interactive login issues an expiring access token with
  refresh; the rejection rests on a file token's portability (decision 8).
- **Allowed, recorded: the per-handoff token read boots an exporter and
  holds the token in proxy memory for the run** (decision 6).
- **Confirmed, fixed: the §14 registry row named `api_key_isolated` as the
  only escape.** It now says what injection changes there.
- **Disproved as an attack, caveat recorded: a placeholder off-run.** One
  proxy per handoff and the mapping dies at close; the only cross-run path
  is a colliding subnet on the shared listener, which the gate checks per
  run only, so #2175 proves concurrent subnets distinct.
- **Confirmed, fixed: unfilled issue placeholders in the note.**

## Owner Decisions Needed

- Adopt injection as the end state and the Phase 2 default, with fiat
  pulling it forward.
- Whether the spikes may spend real subscription calls against the live
  providers; both need them.
- The subscription-terms judgment on a substituting proxy, per vendor.

## Revisit When

- A pinned CLI release pins certificates, drops its CA variable, or moves
  the token out of the authorization header; the CA-trust probe joins the
  §5.8 per-bump probe set.
- `api_key_isolated` lands; it reuses this path with a key in place of a
  token.
- A provider offers short-lived or per-run subscription tokens.
- #2174 finds a provider refusing the proxy's TLS client. The fallback
  is the contained posture for that provider, not a client that imitates
  the CLI's fingerprint.
