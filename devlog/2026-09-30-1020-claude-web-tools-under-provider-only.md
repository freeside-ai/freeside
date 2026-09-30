# Claude Web Tools Under Provider-Only Egress

Spike #1620, evidence for the researcher's egress in #1614 (open question
1). Tested on Claude Code 2.1.220, the build the agent image pins.

## Answers

1. **`WebSearch` works under `provider_only`.** The search runs on
   Anthropic's servers. The ward made no connection except to
   `api.anthropic.com:443`, and the proxy refused nothing.
2. **`WebFetch` does not work under `provider_only`.** The CLI fetches the
   page from inside the ward. The proxy refused `CONNECT go.dev:443`, and the
   tool returned `Error: Socket is closed`.
3. **No provider-side fetch was observed with the setup token.** Called
   directly, the API answered every request with `429 rate_limit_error`,
   including a plain control with no tools, while the CLI kept working on
   the same token. The runs don't show why. The 429s may be how the API
   refuses a direct setup-token call, or throttling on a route or bucket the
   CLI doesn't share; telling them apart needs a retry after the limit
   clears, which wasn't run. Through the CLI, the token reaches only the
   CLI's own tools, and its `WebFetch` is local. Whether an API key could use
   the server-side fetch tool wasn't tested.
4. **The transcript records queries and URLs, not retrieval times.** A
   search records its query, each result's title and URL, and a
   provider-written summary. A fetch records its URL and prompt. Neither
   tool records when a page was retrieved, so retrieval time is
   unavailable. The nearest record is the `timestamp` on the `user` event
   that carries the tool result: when the CLI received the result, which
   bounds retrieval from above. One search result can cover several pages
   retrieved at unknown times, so the researcher should record that
   timestamp as an observation time, not as each source's retrieval time.

## Recommendation for #1614

Keep the researcher on `provider_only` and give it search only. Search
alone refines well: each query returns result URLs and titles plus a summary
that Anthropic writes from the pages. The researcher can't read a full page
under this profile.

If the specification needs full page text, there are two ways to get it, and
neither is free:

- **`provider_web_read`** for the CLI's local `WebFetch`, with that
  profile's explicit record of the wider exposure. #1614 already argues this
  is tolerable only because the researcher's ward holds nothing but the
  questions and its own credential.
- **An API key under `api_key_isolated`** calling the server-side fetch tool
  on the allowed authority. This is untested, and it means a second
  credential type for one role.

Chose search-only `provider_only` over both as the recommendation, because
it keeps today's egress floor and still lets the researcher search and
refine. It drops the "fetches" step in #1614's sketch; the plan PR decides
whether that loss is acceptable.

## Evidence

### Setup

- **Build.** `127.0.0.1:5014/freeside-agent-claude@sha256:fec0dbe220718b760af8b1e5da0595acad53d492316488e0aaa1669cf968fd30`,
  label `ai.freeside.claude-code.version=2.1.220`; `claude --version` in the
  ward printed `2.1.220 (Claude Code)`. Exporter
  `127.0.0.1:5012/freeside-exporter@sha256:427a8359ff7d5009d369cca3ea6a5e684de2ca7a149f172702a2595aa80012ef`.
- **Ward.** A real `Backend.Handoff` on Apple `container` 1.1.0, egress
  profile `provider_only`, `ProviderEndpoints` `api.anthropic.com:443` (the
  Claude writer's allowlist), launch class `writer` at its default size, a
  blank workspace, and the setup token on an opaque credential volume
  mounted at `/credentials`. The token was seeded into the volume through
  stdin, never argv or the agent's environment.
- **Command.** The writer's launch line without the `setpriv` drop and the
  session and instruction flags, plus `--allowedTools <tool>`:

  ```sh
  HOME=/root CLAUDE_CONFIG_DIR=/tmp/cfg DISABLE_AUTOUPDATER=1 \
  DISABLE_TELEMETRY=1 DISABLE_ERROR_REPORTING=1 \
  CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 IS_SANDBOX=1 \
  CLAUDE_CODE_OAUTH_TOKEN="$(cat /credentials/token)" \
  claude -p "<prompt>" --output-format stream-json --verbose \
    --dangerously-skip-permissions --safe-mode --allowedTools <tool> \
    > /workspace/probe.jsonl 2>&1
  ```

  The flags that might have disabled a tool (`--safe-mode`,
  `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`) stayed on, and both tools
  were still offered and called, so no rerun without them was needed.
  `--allowedTools` only pre-approves; the `init` event still listed every
  tool.
- **Connection record.** The proxy keeps no log, so the probe wrapped
  `Config.EgressDialContext` to log each admitted dial and added a temporary
  log line at the proxy's 400 and 403 branches. Neither change is in this
  PR.
- **Model.** The CLI chose `claude-sonnet-5` for both runs.

### Search Run (2026-09-30, 15:15:37Z to 15:15:54Z UTC, exit 0)

Prompt: `Use the WebSearch tool to search the web for "Apple container 1.1.0
release notes". Then list every result URL and title the search returned,
exactly as returned. Do not answer from memory.`

Proxy record, complete:

```text
15:15:38.087208Z admitted api.anthropic.com:443
15:15:38.087487Z admitted api.anthropic.com:443
15:15:38.593627Z admitted api.anthropic.com:443
(no refusals)
```

Transcript excerpt:

```text
15:15:41.767Z tool_use WebSearch {"query": "Apple container 1.1.0 release notes"}
15:15:51.278Z tool_use_result {"query": ..., "durationSeconds": 9.50,
  "searchCount": 1, "results": [{"tool_use_id": "srvtoolu_01VXgz...",
  "content": [{"title": "Release 1.1.0 · apple/container",
  "url": "https://github.com/apple/container/releases/tag/1.1.0"}, ...8 more]},
  "Here's what I found about Apple's container 1.1.0 release: ..."]}
result modelUsage.claude-sonnet-5.webSearchRequests = 1
result usage.server_tool_use.web_search_requests = 0
```

The `srvtoolu_` ID and the `webSearchRequests` count show that the CLI's
`WebSearch` is a side request that declares the API's server-side search
tool. The search count appears only in `modelUsage`; the top-level
`usage.server_tool_use` stays at zero, because it covers only the main
conversation's turns. A budget or audit that counts searches must read
`modelUsage`.

### Fetch Run (2026-09-30, 15:16:39Z to 15:16:45Z UTC, exit 0)

Prompt: `Use the WebFetch tool to fetch https://go.dev/doc/ and quote the page
title exactly. If the fetch fails, quote the exact error text. Do not answer
from memory and do not use any other tool.`

Proxy record, complete:

```text
15:16:40.025943Z admitted api.anthropic.com:443
15:16:40.025951Z admitted api.anthropic.com:443
15:16:40.403529Z admitted api.anthropic.com:443
15:16:44.268674Z admitted api.anthropic.com:443
15:16:44.470735Z refused  CONNECT go.dev:443 (403)
```

Transcript excerpt:

```text
15:16:44.510Z tool_use WebFetch {"url": "https://go.dev/doc/", "prompt": "What is the exact title ..."}
15:16:44.739Z tool_result is_error=true "Socket is closed"
result modelUsage.claude-sonnet-5.webSearchRequests = 0, server_tool_use.web_fetch_requests = 0
```

The refused CONNECT to the target host is direct evidence that the CLI
fetches from inside the ward. The provider dial just before it is likely
the CLI's pre-fetch domain check; the record doesn't show its path, so
that part is inferred.

### Provider-Side Fetch (Question 3)

The direct check ran from the host, outside the ward. That was the owner,
because the agent's permission policy blocked sending the token to the API
directly. The script POSTed `/v1/messages` with `authorization: Bearer
<setup token>` and `anthropic-version: 2023-06-01`, model `claude-sonnet-5`,
in four shapes: a plain one-message request and one declaring
`web_fetch_20250910`, each with and without the `oauth-2025-04-20` beta
header (the fetch shapes also sent `web-fetch-2025-09-10`). It recorded
status and error type only.

```text
15:21:47Z plain_nobeta -> 429 rate_limit_error
15:21:47Z plain_oauth  -> 429 rate_limit_error
15:21:47Z fetch_nobeta -> 429 rate_limit_error
15:21:48Z fetch_oauth  -> 429 rate_limit_error
```

A control run of the in-ward search probe at 15:22:14Z to 15:22:33Z, on the
same token, succeeded with a provider-side search. That rules out an account
quota shared by both routes, but not a limit specific to direct calls. The
error type says rate limit and the error body says only `Error`, so these
runs don't separate credential rejection from route-specific throttling.
What they do show is that no direct request shape got a provider-side fetch
out of this token.

## What This Rejects

- **Rejected: treating Claude Code's `WebFetch` as provider-side.** It
  isn't. Admitting the target hosts would be a `provider_web_read` decision.
- **Rejected: calling the Messages API directly with the setup token.** No
  direct call succeeded in these runs, and whether the API rejects the
  credential or only throttled it is unverified. Until a direct call is
  shown to work, treat the token's reach as whatever the pinned CLI exposes.
- **Rejected: counting searches from `usage.server_tool_use`.** Under the
  CLI it reads zero even when a search ran.

## Reproducing

The probe was a scratch live test in `daemon/internal/ward`, deleted before
commit. It builds `Config` and `HandoffSpec` as `TestLiveHandoffLifecycle`
does, adds `Class: LaunchWriter` and `Size: DefaultLaunchSize(LaunchWriter)`,
wraps `EgressDialContext` with a logging `net.Dialer`, sets a no-op
`Scanner`, runs the command above with one prompt per tool, and copies
`/workspace/probe.jsonl` out of the export with `readManifestBlob`. It needs
the same host inputs as the other ward live tests
(`FREESIDE_WARD_LIVE_TEST=1`, the two digest-pinned images, a setup token).

## Revisit When

- The agent image's `CLAUDE_CODE_VERSION` pin changes, since the CLI can move
  a tool between local and provider-side execution.
- Anthropic changes its server-side web search or web fetch tools, or what a
  setup token may use.
- The proxy gains permanent connection logging, which would make this probe
  a standing conformance check.
