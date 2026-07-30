# Codex CLI StageDriver Feasibility: Go

Work unit: #395. Scope: `devlog/` only. Investigation, no product code.

## Decision

**Go: a pinned Codex CLI can satisfy the StageDriver contract under
ward-style containment, with one structural difference from Claude:
the per-identity auth store is periodically rewritten, so it needs the
plan's serialized daemon-owned mutation
(`AuthIdentity.auth_store_mutation_lease`) rather than Claude's
write-once enrollment; to the agent the store stays a read-only mount
either way.** Four pre-adoption
gates stay open for the driver unit, none of which changes the
architecture: a forced/expired-token refresh probe, closing the
workspace-skill instruction surface (unmet at the CLI level on
0.137.0), a poisoned-rollout resume probe, and the vendor-instruction
delivery binding; each is detailed in its section below, and #401
tracks them as the blocking follow-up. The plan already models exactly that shape
(§5.4 provider concurrency), so the Phase 2 `codex` driver needs no new
architecture, only a driver binding and an auth-volume lifecycle. The
follow-on plan revision can commit to the driver on this evidence.

All evidence below is observed behavior of **codex-cli 0.137.0**
(Homebrew binary, macOS host, ChatGPT-subscription auth), probed in an
isolated `CODEX_HOME` seeded with a copy of the live credential. Per
the Revision-22 pattern, each observation is a pinned-CLI empirical
contract to re-prove on version bump, not a vendor guarantee.

## Contract Operations (`daemon/internal/exec/driver.go`)

- **Start: met.** `codex exec --json [-C dir] [--skip-git-repo-check]
  [--ephemeral] [-s <sandbox>] '<prompt>'` runs headless and exits 0 on
  success. Gotcha: an open, never-closing stdin pipe blocks the run
  ("Reading additional input from stdin..."); the launcher must redirect
  stdin from `/dev/null` or close it.
- **Inspect: met (driver-side).** No out-of-band status query exists;
  like the Claude binding, status derives from process liveness plus the
  event stream, which the driver owns.
- **Stream: met.** `--json` emits typed JSONL on stdout:
  `thread.started` (carries the session UUID), `turn.started`,
  `item.started`/`item.completed` (agent messages; `command_execution`
  items carry argv, aggregated output, and exit code), and a terminal
  `turn.completed` (with token usage) or `turn.failed`. Durability is the
  driver's capture of stdout, as with Claude. `-o <file>` additionally
  writes the final message; `--output-schema` constrains its shape.
- **Cancel: met, container-assisted.** SIGTERM exits 143 and emits no
  terminal event (the JSONL simply stops); the driver commits the
  canceled result itself, which the contract already assigns to it.
  Observed caveat: a model-spawned child (`sleep 45`) survived the CLI's
  death, so process-tree teardown belongs to the container stop, not the
  CLI.
- **Collect: met.** Terminal outcome = exit code (0 success, 1 with
  typed `error` + `turn.failed` events on failure) plus the terminal
  event and last-message file. Idempotent redelivery is driver state, as
  the contract specifies.
- **Continuity: met, exceeds the §5.3 best-effort floor.** `codex exec
  resume <session-UUID> '<prompt>'` binds exactly to one recorded
  session; a session killed mid-turn resumed successfully with context
  intact. `--ephemeral` (observed) suppresses the session rollout file
  when continuity is not wanted.

## Credential Floor (§5.4)

- **ChatGPT-subscription mode: met, conditional on refresh semantics
  that remain unobserved.** `auth.json` (0600, in `CODEX_HOME`) holds
  access, refresh, and id tokens. Observed: the shipped access token had
  ~10-day validity (issued 2026-07-23, exp 2026-08-02), and a run
  against a `chmod 400` auth file succeeded while the token was fresh;
  nothing rewrote `auth.json` across the ten authenticated probe runs
  (hash-verified). **No refresh fired during probing, so refresh
  behavior is inference, not observation**: the store's `last_refresh`
  field and its matching file mtime show rewrites happen in real usage,
  but whether 0.137.0 refreshes mid-inference, what it does against a
  read-only store at expiry, and the refresh endpoint (documented
  `auth.openai.com`) are all **unknown**. The go therefore provisions
  the shape §5.4 already defines, with the writability placed on the
  daemon side of the boundary: the **agent-facing mount of the
  per-identity auth volume is read-only** (observed viable: the
  `chmod 400` run), and every mutation, refresh, login state, or store
  replacement happens in a **daemon-owned transaction** holding the
  serialized `auth_store_mutation_lease` while no execution can use
  that identity, the same §5.4 sentence Claude's enrollment transaction
  already uses. Model-run commands therefore cannot corrupt or
  substitute the shared store, so the per-identity volume is not an
  agent-writable cross-invocation carrier;
  `supports_read_only_auth_snapshot` is true during execution on the
  read-only-run evidence. A forced or expired-token refresh probe (does
  the CLI survive expiry against a read-only mount, or must the daemon
  refresh proactively between executions?) is a pre-adoption gate for
  the driver unit, not a reason to hold this verdict.
- **API-key mode: available, not exercised.** `codex login
  --with-api-key` (stdin) writes the key into `auth.json`. A bare
  `OPENAI_API_KEY` env var is **not** honored: with no `auth.json` the
  request went out with no bearer at all (observed 401 "Missing bearer",
  against `https://api.openai.com/v1/responses` and
  `wss://api.openai.com/v1/responses`). So both auth modes flow through
  the same single-file store, which keeps the launcher/volume design
  uniform.
- **No credential in argv or events: consistent with the floor.** The
  token lives only in `auth.json`; prompts and JSONL carried none of it.
  **Unknown: whether spawned commands inherit any credential-bearing
  environment** (Claude's documented ambient residual); assume the same
  residual until probed.

## Configuration and Continuity Boundary

Reads: `CODEX_HOME` (`config.toml`, profiles, `auth.json`), workspace
`AGENTS.md`, project/user `.rules`, and, **observed even with an
isolated `CODEX_HOME`, the cross-agent `$HOME/.agents/skills`
directory**, so the container needs a clean `$HOME`, not just a clean
`CODEX_HOME`. Severing flags exist and are the driver's to pin:
`--ignore-user-config`, `--ignore-rules`, explicit `-c` overrides.

**Workspace-instruction severance (§5.8): split verdict.** §5.8 makes
vendor auto-loaded instructions control-plane content and workspace
copies untrusted data, so the driver must be able to stop the CLI from
self-loading instruction surfaces out of the workspace.

- **Workspace `AGENTS.md`: met, observed both ways.** A canary
  `AGENTS.md` instructing "end every reply with CANARY" fired in the
  positive control (git-rooted workspace, no override) and was fully
  severed under `-c project_doc_max_bytes=0`. Discovery is
  git-root-anchored (the same canary did not load in a non-git
  directory), and the override is per-invocation argv, so the
  launcher's fixed argv must carry it on every start and resume.
- **Workspace `.agents/skills/**`: unmet on 0.137.0.** A canary skill
  in the workspace's `.agents/skills/` fired even with `AGENTS.md`
  severed, and no disablement was found: `--disable skills` is an
  unknown feature flag, `--disable plugins` does not stop the load, and
  under `--strict-config` the `skills` config key accepts a list
  (`skills=[]` parses but does not sever) while `skills.enabled`,
  `skills.paths`, and `skills.load` are unknown fields. `$HOME/.agents`
  skills are severed by the clean container home, but the
  workspace-local surface has no CLI off-switch on this version.

So "trusted instructions arrive only through the digest-bound prompt
package" does **not** hold unconditionally on 0.137.0: closing the
workspace-skill surface is a pre-adoption gate for the driver unit,
via a severance switch on a later pinned version or an explicit
topology-and-policy argument (the trusted base seeds the workspace,
`.agents/**` is already a gated instruction surface on import, and a
mid-run self-write feeds only the same invocation's untrusted context);
that argument needs its own refute-first pass, not this note's
assertion.

**Trusted instruction delivery: mechanism observed, binding open.**
Severance alone is not enough: a stage with admitted vendor
instructions must also *deliver* them, the way ward passes Claude a
digest-bound bundle with `--append-system-prompt-file`, and
`VendorInstructions` is a stage-input role distinct from the prompt
package. Observed on 0.137.0: the `instructions` config key (valid
under `--strict-config`, settable via `-c` or a ward-supplied read-only
`config.toml`) injects instructions the model follows (canary fired),
but at **replace authority**: input tokens dropped 12,803 → 8,437 with
the override, showing it displaces the built-in base template rather
than appending. No append-equivalent exists
(`experimental_instructions_file` and `base_instructions` are unknown
config fields). So Codex has a delivery mechanism, at a different
authority than Claude's append path; choosing and proving the binding
for the `VendorInstructions` role (daemon-composed full template
versus delivery inside the prompt package, preserving §5.8 scope and
precedence) is a pre-adoption gate.

Writes per run (observed diff of a fresh `CODEX_HOME` after one
two-word run): `sessions/YYYY/MM/DD/` rollout, `state_5.sqlite`,
`logs_2.sqlite`(+wal/shm), `memories_1.sqlite`, `installation_id`,
`models_cache.json`, `shell_snapshots/`, and ~8.5k plugin/skill
marketplace cache files under `.tmp/plugins` and `plugins/cache`. The
memories and state databases are cross-invocation carriers, so the
1A-shape answer is the same as Claude's: per-invocation home, with only
the per-identity auth volume shared, mounted read-only to the agent and
mutated only by daemon-owned lease-held transactions between
executions (Credential Floor above).

**Narrow continuity mount for resume: sufficiency met; the rollout is
untrusted input.** Reusing a home the model already ran in would carry
its writable surfaces (`config.toml`, skills, plugin caches, the
databases above) forward into the resumed launch, so resume must not
remount the dirty home. Observed: `codex exec resume <UUID>` succeeded
from a **fresh** `CODEX_HOME` containing exactly two files, the auth
store and the invocation's own `sessions/YYYY/MM/DD/rollout-*.jsonl`,
resuming the same thread UUID with context intact; every other surface
a poisoned launch could have written stays behind in the discarded
home. That proves the rollout is *sufficient* continuity state, not
that it is safe to trust: the rollout is plain JSONL writable by
model-run commands, its first record carries `base_instructions` and
`cwd` (observed), so a rewritten rollout can alter what the resumed
session treats as system-prompt-class state, and resume against a
deliberately corrupted rollout **succeeded silently** (exit 0, the
malformed record tolerated, no integrity failure). A driver therefore
treats the remounted rollout as agent-trust-class input: a resumed
launch runs under agent-influenceable instructions inside the same
containment as the agent itself, the per-invocation mount is what
keeps it from crossing invocations or identities, and a refute-first
poisoned-rollout probe (e.g. an injected `base_instructions` swap) is a
pre-adoption gate for the driver unit.

## Egress

Observed through a local CONNECT-logging proxy (the CLI honors
`HTTP(S)_PROXY`, matching the ward's allowlisting-proxy pattern):

- ChatGPT-auth inference: `chatgpt.com:443` only.
- Ancillary: `ab.chatgpt.com:443`, `github.com:443`,
  `api.github.com:443` (telemetry/marketplace). **Denying all three is
  tolerated**: under a `chatgpt.com`-only allowlist the run completed
  with exit 0 and correct output.
- API-key mode: `api.openai.com:443`, HTTPS and websocket.
- Not observed: the auth-refresh endpoint (no refresh fired).

So the `provider_only` allowlist is `chatgpt.com` (subscription) or
`api.openai.com` (API key), plus the refresh endpoint once observed.

## Pinning

The CLI is a versioned standalone binary (Homebrew, npm
`@openai/codex`, GitHub releases); digest-pinning in an agent image
follows the existing pattern. The empirical contracts to re-prove on
bump: stdin behavior, JSONL event vocabulary, exit codes (0/1/143), the
resume binding, the narrow-continuity resume (fresh home plus one
rollout file), rollout record semantics (`base_instructions`, silent
malformed-record tolerance), auth-store layout and refresh semantics,
the `$HOME/.agents` read, the `project_doc_max_bytes=0` instruction
severance, the workspace-skill auto-load, the `instructions` key's
replace authority, and the egress set.

## Rejected Alternative

Treating the Codex store like Claude's write-once enrollment (authored
once, never rewritten) was rejected because it cannot be proven safe on
current evidence: the access token carries ~10-day validity and the
store records rewrites (`last_refresh`), so a never-mutated volume
risks failing closed weeks after enrollment unless a refresh probe
proves the CLI survives expiry without persisting. An
agent-writable auth mount was rejected outright: it would let model-run
commands corrupt or substitute the shared token, reintroducing the
cross-invocation carrier the per-invocation home exists to eliminate.
The daemon-mutated, agent-read-only shape costs no new architecture
(§5.4 defines both the lease and the no-execution-during-mutation
rule), so it wins; revisit write-once only if the refresh probe proves
expiry is survivable without persisting.

Follow-up: #401 (carries the pre-adoption gates as tracked work).

Revisit when: the Codex CLI version is bumped (re-prove every empirical
contract above, and re-check for a workspace-skill severance switch and
an append-authority instruction path); or when a refresh is first
observed (record endpoint and read-only-store failure mode).
