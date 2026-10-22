# Claude Code 2.1.282 and Codex 0.162.1 Contract Re-Proof (#2195)

Work unit: #2195. Scope: `images/agent-claude/`, `images/agent-codex/`,
`images/README.md`, `daemon/internal/agentbaseline/`,
`daemon/internal/exec/claude/`, `daemon/internal/ward/` (a comment),
`daemon/README.md`, and this note. Successor to
`2026-08-09-1925-codex-0147-contract-reproof.md` in the #395/#401 lineage:
a pin bump re-proves the contracts those notes list under Pinning.

## Why the Pins Moved

The Wave 8 exit run (#1613, target freeasinbird/gh-imgup#125) picked models
the pinned CLIs refuse:

- **Claude Code 2.1.220 refuses `claude-fable-5-1`** ("version 2.1.251 or
  newer is required"), and it also failed `claude-opus-5-5` in a model
  probe on the old image, so the implementer pick would have failed too.
  2.1.282 runs both.
- **Codex 0.146.0 and 0.147.0 refuse `gpt-6.1-sol`** on a ChatGPT account
  ("not supported when using Codex with a ChatGPT account"). 0.158.0's
  binary lacks the model and 0.160.0's has it; 0.162.1 is the release
  pinned.

## Decisions

**Drop the package's `codex-resources/voice`.** 0.162.1's package adds a
voice host and its GStreamer libraries, absent from 0.147.0's. No review or
writer launch speaks, so the build stage removes it rather than carrying an
unused executable with its own shared libraries. The image keeps omitting
`codex-path/rg` for the same reason as before (Debian ripgrep is the
recorded dependency).

**Keep the code-mode host.** Still load-bearing on 0.162.1: with it
removed, a composite shell command fails ("the execution tool failed to
start"). Unlike 0.147.0, the model reported the failure instead of claiming
success, but the tool is still unusable without the host.

**Move the v1 review adapter's digest with the image.** `codex_review_v1`
is built from `CodexHarnessBuild`, so its digest changes with the pin, and
`TestReviewAdapterV2AddsOnlyTheEffortsItCanSend` pins it. Chose to update
the pinned value over freezing v1 at 0.147.0: a conformance record for a
build that no longer runs must not keep admitting, which is the pin test's
own rule.

**Leave the spike probes at their measured versions.** The Codex usage and
account spikes and the Claude usage spike back no production code (ADR 0004
reads account facts locally) and pin 0.147.0/2.1.220 with fixtures recorded
there. They stay evidence about those builds.

**Leave `ClaudeCallHarnessBuild` at 2.1.267.** It names the audited host
build the judgment call runs, not an image pin.

## Re-Proven on Claude Code 2.1.282

Run on the built image under Apple `container`, mirroring the writer launch
(uid 1001 through `setpriv`, the golden's environment, prompt on stdin).
The proxy saw only `api.anthropic.com`.

- Root refuses `--dangerously-skip-permissions` without `IS_SANDBOX=1`.
- An authentication failure exits nonzero.
- The writer launch on `claude-fable-5-1` exits 0: Bash runs as uid 1001,
  the appended system prompt is delivered, and `modelUsage` names the model.
- `--safe-mode` suppresses planted project and user hooks; without it the
  same hooks fire.
- Setup-token auth leaves no token or `.credentials.json` in any store; the
  host setup-token scope tests pass on 2.1.282.
- 2.1.220's attempted Datadog connection wasn't observed this time.
- Ward live tests pass on the new image: handoff lifecycle, workspace
  seeding, worker git identity, setup-token read and integrity, and the
  conformance suite with its same-VM refutation.

## Re-Proven on Codex 0.162.1

Same methodology as 0.147.0's: canary workspaces, an access-token-only auth
snapshot, a CONNECT proxy allowing only `chatgpt.com`, ward's review flags.

**Unchanged:**

- The JSONL vocabulary and exit codes. `turn.completed` usage adds
  `cache_write_input_tokens`, which the parser ignores.
- Skills are discovered from the workspace and `$HOME`, not from an
  ancestor outside the git root, so the shadow mounts still matter.
- `project_doc_max_bytes=0` severs the workspace `AGENTS.md` (the control
  without it reads the canary); `$CODEX_HOME/AGENTS.md` still appends.
- Composite commands work under `workspace-write`; `read-only` denies the
  write and the model says so.
- Egress: `chatgpt.com` only; `ab.chatgpt.com` and
  `sdmntpr*.oaiusercontent.com` are denied and tolerated.
- A temporary `CODEX_HOME` still refuses to create helper binaries.
- `TestLiveCodexReviewSandboxWorkspaceAccess` passes on the new image with a
  paid `gpt-6.1-sol` collection, along with the review lifecycle and
  snapshot tests.
- The auth refresh client id and endpoint `codex_auth_refresh.go` mirrors
  are present in the 0.162.1 binary.

**Changed (none breaks a launch):**

- System skills are now imagegen, openai-docs, review-agent,
  skill-creator, and skill-installer (three new).
- The child sees `CODEX_SANDBOX_NETWORK_DISABLED=1` and no
  `CODEX_SANDBOX`; the arg0 helpers no longer show on the child `PATH`.
- `shell_environment_policy` is now honored: `inherit="none"` and `exclude`
  strip variables from children. Ward still passes no policy and treats its
  launcher environment as the containment.
- `CODEX_HOME` gains SQLite state (goals, logs, memories, queue), apps
  caches, and `.sandbox_migration`; it's per-invocation and ephemeral, so
  nothing persists.
- **The reviewer's tool surface includes the account's connected apps and
  the provider web tool.** This isn't new: 0.146.0 has it too. It's a
  path around the sandbox and egress, fixed separately in #2196
  (`2026-10-22-1700-review-connector-paths.md`). Future bumps should probe
  the tool surface as part of this re-proof.

## Not Measured Here

- **`TestLiveClaudeReviewLifecycleCrossesReconstructionStartBoundary`**
  fails with ECONNREFUSED to the test's own proxy on both the new image and
  the old 2.1.220 one, so it's this host's environment, not the bump.
- **`TestPinnedClaudePromptStdinLive`** needs a held rig, which the exit
  run's state left unavailable.
- **A live Codex auth refresh**: the scratch snapshots carry no refresh
  token by design. The refresh request shape is unchanged in the binary;
  the daemon's refresh path is unit-tested.

Revisit when the pins move again.
