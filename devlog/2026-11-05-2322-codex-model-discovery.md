# Discover Codex Models From the App-Server's model/list

Issue #2595; follows `2026-11-05-1156-claude-model-discovery.md` (#2594) and
`2026-11-05-1035-select-by-enrollment-model-effort.md` (plan revision 111).
Tier A by path: the probe mode lives in `daemon/internal/ward/`, the
credential read in `daemon/internal/wardstore/`, and the command wiring in
`daemon/cmd/freesided/auth_add.go` and `renew_codex.go`; the new mode copies
a Codex access token into a probe container. The catalog it fills stays
Tier B, as #2594 classified it.

Chose the pinned CLI's `codex app-server` `model/list` over reading
`models_cache.json`, because the cache is the CLI's internal by-product of the
same fetch, with no contract, while `model/list` is the app-server's published
method (its schema comes from `codex app-server generate-json-schema`).

## Decisions

- **Chose to drive paging inside the container with `jq`.** The app-server
  answers out of order, among notifications, and exits at end of input
  without answering requests still in flight. So the script holds its input
  pipe open, waits for each answer in stdout before sending the next request,
  and builds each `model/list` request with `jq`, the cursor passed only as
  `--arg`. Rejected: piping every request at once, which loses unanswered
  pages, and a host-side JSON-RPC client, which would need a live stdio
  channel into the container that the lineup probe's copy-in, export-out
  containment does not have.
- **Chose bounded paging the decoder judges.** Pages of 100, at most 8, and
  one 120-tick answer budget that, with the 60-tick ready wait, stays inside
  the probe's four-minute timeout so the script records the app-server's
  status. The decoder refuses a missing page, a last page that still carries
  a cursor, and a middle page without one. A server-sent request carries a
  method beside its id, so neither the script nor the decoder reads it as an
  answer.
- **Chose `model` over `id` for the record's id.** They were equal for every
  model in both hand runs; `model` is what `codex exec -m` takes. A list that
  ever maps two entries to one `model` is refused as a duplicate.
- **Chose the default list only.** `includeHidden` stays unset: the record
  feeds a picker, and hidden models are not offered there.
- **Chose to record efforts as listed, with no remapping.** Both hand runs
  listed `max`, and most models `ultra`, beside `xhigh`. The review launch's
  `max`-to-`xhigh` mapping (`policy/README.md`) is unchanged.
- **Chose the current generation's store under the review input root, under
  a read hold, for the startup refresh.** `wardstore.CodexProbeCredentialReader`
  takes the identity's read hold and resolves the current generation in one
  transaction, reads the store through `ward.ReviewSnapshotProbeCredential`
  (the private-root check, the refresh-token strip, and the ten-minute
  expiry floor), releases the hold, and refuses a read that outlived it. A
  live mutation lease skips the enrollment until the next start. It lives in
  `wardstore`, beside the Claude `CredentialReader`, so the credential read
  stays on a listed Tier A path. Rejected: reading in
  `cmd/freesided/model_discovery.go`, which is not on the list and would
  have to join it.
- **Chose to tell a Codex reviewer by its auth mode.** The refresh starts
  only after the driver composed, and `resolveReviewSelection` admits an
  unattended review auth mode only when it fits the reviewer's harness, so
  `subscription` means the reviewer is Codex and its image is
  `-review-image`. An `api_key` reviewer is Codex too, but its input root
  holds a key file, not the OAuth stores discovery reads, so it starts no
  discovery rather than warn for every enrollment on every start. Without a
  subscription reviewer the daemon logs one line and skips Codex discovery.
- **Chose no read hold in `auth add` and `renew-codex`.** Both hold the
  daemon lock and have released their own mutation lease when discovery
  runs, so no mutation can run beside the read; they read the store they
  just wrote under `-auth-store-root`. Discovery there is optional
  (`-agent-image`) and never fails the command.

## Findings

- **The pinned 0.162.1 speaks the protocol the plan assumed.** Its generated
  schema has `model/list` taking `cursor`, `limit`, and `includeHidden`,
  answering `{data, nextCursor}`, with `supportedReasoningEfforts` as
  `{reasoningEffort, description}`, and `initialize` requiring
  `clientInfo: {name, version}`.
- **The list does not prove the account.** Run by hand with an empty
  `CODEX_HOME`, 0.162.1 answered `model/list` with a bundled list and no
  error. So a catalog shows what the pinned CLI lists, as #2594 found for
  Claude's invalid-token list. The probe reports `ProviderReached`, and the
  live test logs it with the models.

## Refute-First Pass

A fresh-context reviewer read the diff and tried to prove it wrong, and ran
the session script under dash with a fake app-server.

- **Confirmed and fixed: a token spelled with JSON escapes passed the scan.**
  The scan read the raw bytes, so a display name of `\u` escapes decoded to
  the token and would be recorded and listed. Both decoders now also scan
  every decoded string, and the Codex scan covers the id token and any
  exchanged API key beside the access token.
- **Confirmed and fixed: an API-key Codex reviewer started discovery that
  could only fail.** Its input root holds a key file, so every OAuth
  enrollment warned on every start. Discovery now needs a subscription
  reviewer.
- **Confirmed and fixed: the script waited on an app-server that had
  exited, and never stopped one that lingered.** An answer wait now ends
  when the app-server has exited, and once the input closes the script
  waits at most ten ticks and kills it, so the script, not the host's
  timeout, ends the session. A test now runs the generated script against a
  fake app-server, beside the golden.
- **Fixed: test gaps.** Hold release after the near-expiry refusal, a read
  that outlives its hold, and `renew-codex` keeping the last catalog after
  a failed discovery are now tested.
- **Allowed by decision: a bundled list can replace the last catalog.** A
  revoked token may still get the CLI's bundled list, which decodes. The
  Claude path records the same way (#2594), and refusing when the provider
  was not reached would not catch a revoked token, which still reaches it.
  The Findings above record it.
- **Disproved by a check: the call sites could pass a refresh token.**
  `ReviewSnapshotProbeCredential` is the only constructor of a Codex probe
  credential, and it strips the refresh token, so no call site can skip it.
- **Declined: the failure log can name a stale generation.** The field comes
  from the same listing the Claude path logs; it is advisory and changes no
  outcome.
- **Held under the reviewer's checks:** a cursor carrying shell syntax
  reaches the request as data; `CODEX_HOME` and the pipe are removed on the
  normal and crash paths; the decoder's refusals, and its order with the
  credential scan first; no error quotes CLI output; the read hold is taken
  with the generation in one transaction and released on every path; the
  Claude path is unchanged by the rename.

## Revisit When

- A CLI release reshapes `model/list` or stops answering it before
  authentication.
- A live run shows the list never depends on the account; then a per-image
  catalog would replace the per-enrollment one.
- A Codex writer image exists, which would give Codex discovery an image
  that does not depend on the reviewer.
