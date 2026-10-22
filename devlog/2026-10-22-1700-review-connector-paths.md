# Close the Codex Reviewer's Connector and Provider Web Paths (#2196)

Work unit: #2196. Scope: `daemon/internal/ward` (the review command),
`daemon/README.md`, and `devlog/`. The reviewer could act with the ChatGPT account's GitHub
grant, so this is credential-leak work and the note is mandatory.

## What the Probes Found

Found by the #2195 pin re-proof during the Wave 8 exit run (#1613). Every
probe ran ward's exact review command (`codex exec --json --ephemeral
--skip-git-repo-check -s read-only ... -c project_doc_max_bytes=0
--ignore-user-config --ignore-rules`) inside the agent image, through a
proxy that allowed only `chatgpt.com`, on codex-cli 0.146.0 (the gh-imgup
reviewer project image production runs) and 0.162.1 (the #2195 pin).

- **Connected apps were live.** Both versions fetched the account's apps
  tool list from `chatgpt.com` into `$CODEX_HOME/cache/codex_apps_tools`:
  158 tools, among them `github.create_commit`, `github.delete_file`,
  `github.create_pull_request`, `github.enable_auto_merge`, and
  `github.add_comment_to_issue`. Asked to, the reviewer called
  `codex_apps/github.get_user_login` and returned the owner's GitHub login
  (an `mcp_tool_call` item in the event stream) on both versions.
- **The web tool fetched outside ward's egress.** `web__run` opened
  `https://example.com` and returned its title on both versions (a
  `web_search` item); the proxy saw only `chatgpt.com`.
- **The read-only sandbox doesn't reach either.** Both calls run on the
  provider's side. The connector acts with the GitHub grant the account
  gave ChatGPT, and the fetch leaves from the provider's network, so
  neither the sandbox nor the allowlisting proxy sees them.
  `--ignore-user-config` doesn't help: the apps list comes from the
  account, and both features are on by default (`codex features list`:
  `apps stable true`).

A review reads untrusted diff and repository content. An injected
instruction could have written to any repository the connector reaches, as
the owner, or sent reviewed content to an arbitrary URL.

## Decisions

**Disable both in the launch command, over removing the account's
connectors.** Chose `--disable apps -c web_search="disabled"` on the review
command. With them the apps cache is never fetched, the
`list_mcp_*` and `request_plugin_install` tools disappear, the same requests
answer that no connector or web tool exists, and a review still completes.
Disconnecting GitHub in ChatGPT would close today's instance but leaves the
launch's safety to an account setting ward can't see, and any connector the
owner adds later would return the path.

**`--disable apps` over `-c features.apps=false`.** Both turn the feature
off, but the CLI accepts any `-c features.<name>` silently, while
`--disable` refuses an unknown name (`Unknown feature flag`, exit 1, on
0.146.0, 0.147.0, and 0.162.1). So a CLI that renames the apps feature
fails the review loudly instead of reopening the path. `web_search` refuses
an unknown value but accepts a renamed key silently, like any `-c` key; the
per-bump re-proof is its guard.

**Leave the other default tools.** Image generation, goals,
`request_user_input`, and multi-agent collaboration stay. None reaches a
third party or acts with an outside credential: image generation and
sub-agents run on the same provider under the same launch (a sub-agent
inherits the disabled features), and goals and user input write only the
reviewer's ephemeral state. Disabling them would trim surface without
closing a path.

**Keep the adapter build; accept the review configuration re-approval.**
The adapter declares read tools and no mutation tools; the launch now
honours that, so `codex_review_v2` and its conformance records stand and no
agent tree changes. The command is part of the review configuration digest
(`CommandTemplateDigest`), so the digest moves and an unattended deployment
refuses its reviews until the operator approves the new digest. That's the
gate working as designed: a command change is material, and the refusal
fails closed. `daemon/README.md` gives the upgrade steps.

**Stop the daemon between reviews to upgrade.** A review still running at
restart fails as transient (its egress proxy died with the old daemon) and
reruns under the new command. One whose container exited while the daemon
was down is collected and stamped with the running daemon's configuration
digest, so it would be recorded under the new approval though it ran
without the flags. That relabelling predates this change; binding the
launch-time digest is #2197.

## Refute-First Record

- **"`--ignore-user-config` already disables apps."** Disproved: the probes
  above ran with it.
- **"0.146.0 predates the apps surface, so production was never exposed."**
  Disproved: 0.146.0 fetched the same 158-tool list and made the connector
  call.
- **"The flags are accepted but ignored."** Disproved per version: with
  them, no apps cache is written and the connector and web requests come
  back unreachable, on 0.146.0 and 0.162.1.
- **"Disabling apps breaks the review."** Disproved:
  `TestLiveCodexReviewSandboxWorkspaceAccess` passed with the new command on
  both images.
- **"A future CLI renames the keys and the flags go quiet."** Disproved for
  apps: `--disable` refuses an unknown feature name, which is why the
  launch doesn't use `-c features.apps`. Confirmed as a residual for
  `web_search`: an unknown `-c` key is accepted silently, and only an
  unknown value is refused.
- **"An in-flight review fails conformance after the upgrade."** Disproved
  by the refute-first reviewer: no restart path re-derives the command. A
  review that exited during the downtime is collected under the new digest
  (#2197).
- **"A new default feature opens another provider-side path."** Allowed as
  a residual: the flags close the paths known today. The per-bump re-proof
  (#2195's note) checks the reviewer's tool surface. Follow-up: #2198, a
  check that refuses a review whose tool surface reopens a path.

Revisit when the pinned CLI changes how apps or web search are configured,
or when a reviewer needs a provider-side tool on purpose.
