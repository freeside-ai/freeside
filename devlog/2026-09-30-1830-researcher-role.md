# A Researcher Role for the Specification Stage

Plan revision 76, issue #1614. The owner decided on 2026-09-29 to add a
researcher role that does the specification stage's web research. The
specifier writes research questions, the researcher searches in its own
ward, and the specifier reads a stored report instead of raw pages.

## Why

Before this revision the specifier could only request URLs it already knew,
from a host allowlist of at most 64 entries, and each fetch round cost a
full specifier invocation. That design protected the specifier's credential.
Research quality wasn't its goal: the specifier couldn't search at all.

Chose a separate researcher over two alternatives:

- **Search in the daemon fetcher.** It keeps the specifier sealed, but every
  refinement step reinvokes the whole specifier, up to 16 times. That is slow,
  and expensive on a large model.
- **`provider_web_read` for the specifier.** It gives the best research, but
  the stage most exposed to prompt injection would hold open web access
  beside its provider credential and the task. Plan §5.4 rules that out.

The researcher gets real search and refinement while the injection-exposed
stage stays off the web, and research can run on a cheaper model than the
specification.

## What the Plan PR Settled

The issue left six questions open with a recommended answer each. Revision
76 writes the recommendations in, and the owner settles them by reviewing
that PR.

1. **Egress: `provider_only`, search as the only tool.** The #1620 spike
   (`2026-09-30-1020-claude-web-tools-under-provider-only.md`) showed that
   Claude Code 2.1.220's `WebSearch` runs on Anthropic's servers and its
   `WebFetch` runs inside the ward. So search needs no wider profile, and the
   researcher can't read a full page: it gets result URLs, titles, and a
   provider-written summary. Rejected `provider_web_read` for the researcher,
   because no run has yet shown that summaries aren't enough, and the
   profile's wider exposure should be bought with evidence. Rejected an API
   key under `api_key_isolated` calling the server-side fetch tool: it is
   untested and adds a second credential type for one role.
2. **Inputs: only the research questions.** A leak from the researcher's ward
   then exposes at most the questions and the researcher's own credential.
   The daemon bounds and secret-scans the questions, since they are the one
   channel from the specifier's ward to the researcher's. The scan is an
   addition beyond the issue's sketch.
3. **Selection: the lineup only.** No task line covers the researcher,
   because no task has needed its own researcher agent. The alternate-agent
   card doesn't select it either: that card answers a blocked stage, and a
   failed research request doesn't block one.
4. **Budget.** Policy limits research requests per specification run,
   searches per request, and report size, all charged to the task. A
   research request adds no specifier iteration of its own; the invocations
   on either side of it already count. Searches are counted from
   `modelUsage`, because `usage.server_tool_use` reads zero under the CLI.
   That count exists only after the launch, so the search limit is checked
   after the launch ends and doesn't stop a running one. The daemon discards
   the report of a launch over the limit. The searches it made stay spent
   and are charged to the task, and the stage's active-time budget is the
   only thing that stops a running launch. Considered stopping a launch at
   the limit by counting the search events in the harness's output stream,
   which the #1620 spike transcript shows. Left to #1657, because no run has
   checked that those events match the `modelUsage` count.
5. **Default model: Sonnet 5.5** in the baseline lineup (#1426). The plan
   names no model for any lineup line, so it says only that the researcher
   has its own line. This note is the record of the pick.
6. **Launch: a second, narrower launch in the specification stage.** The plan
   used to say every role in a stage runs that stage's launch. It now says
   every role runs a launch its stage defines, and specification defines two.
   The research launch is a stage launch with an empty workspace, not a third
   launch shape, and the researcher is a ward role because its agent has a
   tool.

Two further choices came up while writing the launch in:

- **A failed research request fails safe.** The specifier gets a typed failed
  request and the run continues; the specification's recorded inputs show
  the failure at spec approval. A missing lineup line or failed admission
  does the same, as it does for a wardless role, and those cases and a
  quota, expiry, or capacity failure also raise a `system_health` item so
  missing research is never silent. Rejected blocking the stage: a search outage
  would stop specifications that can proceed without research, and it would
  interrupt the owner for something the specifier can route around.
- **The report records an observation time.** No tool records when a page was
  retrieved. The nearest value is when the CLI received the search result, so
  the report calls it an observation time. The issue's first sketch said
  "retrieval time".

- **The specifier's launch must withhold web tools.** The spike ran the
  writer's launch line, and search worked under `provider_only`. So the
  profile never kept the specifier off the web by itself; the plan now makes
  it a launch requirement the adapter proves per build (#1657). The same
  search is open to the implementation and review launches. That is the
  owner's call and outside this unit, so it is filed as #1659.

## What Is Reasoned, Not Tested

- **The questions as a channel.** An injected specifier can write task or
  repository text into its questions, and they leave as search queries. The
  count and size bounds and the secret scan narrow this; nothing closes it.
  Plan §14 records it as a residual.
- **The researcher's leak channel.** Under `provider_only` its ward connected
  only to the provider in the spike, so its search queries are its one
  outward channel. That follows from the connection record. No run tried to
  exfiltrate through a query.
- **Search as the only tool.** In the spike `--allowedTools` only
  pre-approved a tool; the CLI still listed every tool. The plan states the
  requirement and leaves enforcement and its proof to the adapter (#1657).
- **Other builds.** The egress answer holds for Claude Code 2.1.220, which the
  agent image still pins at this revision.

## Revisit When

- The pinned CLI moves search or fetch between the ward and the provider.
- Specifications suffer because the researcher can't read full page text.
  That is the evidence `provider_web_read` for the researcher would need.
- A task needs its own researcher agent, which would put the researcher under
  task lines.
- Failed research requests go unnoticed at spec approval, which would argue
  for blocking the stage instead.

Follow-up: #1656 (contract), #1657 (ward research launch and the
specifier's web tools), #1658 (specification loop), #1659 (web search for
the other ward launches).
