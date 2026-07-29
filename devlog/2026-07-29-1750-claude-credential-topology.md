# Claude Credential Delivery and Writer Outcome Topology (#380)

**Decision.** The Claude setup token reaches the pinned CLI only through
the daemon-supplied launcher argv: a per-identity, read-only token volume
(a single 0400 `token` file, authored at enrollment under the auth-store
mutation lease) is the only identity-persistent mount, and the launcher reads
it into `CLAUDE_CODE_OAUTH_TOKEN` in the CLI process environment at exec. The
writer's spec environment carries no entries at all. Process success is
carried by a gate-authored per-invocation nonce marker the launcher writes as
its final act. `ExecutionOutcome` remains canonical for failed, canceled, and
lost invocations, while the ward journal durably bridges marker
classification, cancellation, recovery capture, and cleanup.
`WriterComplete` additionally requires zero status and the live daemon's
proxy-health-throughout observation; recovery never synthesizes it. Every
gate-mediated Claude launch uses `--safe-mode`, a verified read-only clean
config root, a fresh writable `session-env/`, and a read-only explicit
instruction bundle; only one invocation's untrusted `projects/` continuity
crosses its exact forked-resume boundary. All three legs were proved by
execution on
the exact pinned image (`freeside-agent-claude`
`sha256:fec0dbe220718b760af8b1e5da0595acad53d492316488e0aaa1669cf968fd30`,
CLI 2.1.220, Apple container 1.1.0) on 2026-07-29, including real
authenticated inference through an allowlisting CONNECT proxy.

This note records three evidence-driven revisions of prior explicit
decisions, each argued below: the #380 issue text's "privilege-separated
wrapper / external authority" bar for the success signal, the same text's
"/root/.claude read-only" instruction premise (inherited from #375/#378),
and PR #378's `spec.go` constants (`agentEnv`, `agentCommand`,
`credentialMountTarget` semantics).

## The Credential Leg

- The vendor surface is exhausted: `claude setup-token` output
  authenticates only through `CLAUDE_CODE_OAUTH_TOKEN` (re-proved by the
  2026-07-29 gate), `apiKeyHelper` is the API-key path rather than the
  OAuth path, `.credentials.json` is vendor-managed and unsupported to
  hand-author, and `/login` provisioning would change onboarding,
  refresh, revocation, and the inference-only scope contract under test.
  Delivery into the process environment is therefore fixed; the decision
  space is only how the value gets there without entering durable state.
- `AgentSpec.Env` stays rejected, now on three independent grounds: the
  value would ride `container create --env` argv (host process listings),
  land in the inspect report ward's allowlist check compares, and join
  #323's nondeterministic-order comparison. The launcher path instead
  *empties* the driver's spec environment (the six `agentEnv()` entries
  move into the launcher string), leaving only ward's proxy injection
  subject to #323.
- Pre-start rootfs copy was refuted by execution: `container cp` into a
  created-but-not-started container fails with `invalidState: not running`
  on 1.1.0. The per-identity read-only token volume needs no new ward
  vocabulary (`CredentialMounts` already models it), and its authoring reuses
  the seeder pattern (copy into a running seeder's rootfs, `exec mv` onto the
  volume), proved during the probe run. Claude state is not an auth-store
  mount: #378 must add explicit lifecycle-scoped state-mount vocabulary for
  the clean root, invocation continuity, and per-launch scratch rather than
  hide them behind `CredentialMounts`.
- Leak checks on the live writer: configured environment in inspect was
  exactly the fixed `PATH`; the token substring appeared nowhere in the
  inspect report, launcher text, workspace or state volumes (the CLI
  persisted no credential, re-confirming the gate's observation; this is a
  per-CLI-version empirical property, re-proved on every CLI bump).
- Accepted residual (unchanged §5.4 class, now stated concretely): the
  token is ambient in the writer process tree; children inherit it and
  the mounted file is readable at agent privilege. Backstops are
  provider_only egress and export secret scanning. The scanner today has
  no Anthropic token rule; Follow-up: #384 adds the `sk-ant` pattern
  before real exports carry this exposure.

## The Outcome Leg

- Apple container 1.1.0 exposes no exit code (inspect models only
  running/stopped; no `wait`). Attach was examined at the pinned CLI
  commit (5973b9c) and rejected: `ContainerStart.swift` initializes the
  exit code to 127 (infrastructure faults become indistinguishable from
  agent failure), destructively stops the container when the attach path
  errors (a name-addressed destructive call outside ward's same-object
  evidence discipline), and cannot re-attach to a running container, so
  the observation is one-shot and dies with the daemon.
- Revision of the issue's stated bar, and the assumption that changed:
  the issue demanded an authority "independent of agent-writable state,"
  but the exit status's *value* is agent-controlled under every
  mechanism, including attach, because the agent process chooses its own
  exit code. Exit status is crash and refusal detection, not adversarial
  proof; acceptance authority remains ward's output verification and
  export gates. What actually needs trust is freshness (no stale marker
  surviving from a prior run or seeded base) and delivery. A
  per-invocation nonce, journalled before start and echoed with the
  status as the launcher's final act, provides both, is durable in the
  workspace volume (so restart-adopted recovery can still authenticate a
  finished writer, which attach can never offer), and needs no runtime
  interface change. Probes confirmed: success `0`, invalid token `1`,
  missing token the reserved `86` written before the CLI ever runs, and
  a crash-before-marker leaves the stale nonce, detectably mismatched.
- The stream-json transcript is not an outcome signal: the terminal
  result event reported `"subtype":"success"` alongside
  `"api_error_status":401` on an invalid token. Only the CLI exit code,
  as captured by the marker, distinguished the failure.
- Cancellation precedence (Codex P1, round 2): a daemon-commanded stop
  leaves a signal-derived nonzero marker or none at all, which the
  marker rules alone would misreport as failed or lost, contradicting
  the StageDriver Cancel contract (a canceled invocation commits a
  reconcilable canceled result). The daemon journals cancellation
  intent before issuing the stop, that record outranks marker
  classification, and the marker rules classify only uncommanded
  terminations. Cancellation never makes partial work a publication
  candidate or clean-verifier input. Graceful portable handoff still
  performs its mandatory post-quiescence normalized workspace capture and
  verifies only that encrypted recovery object's integrity and frontier
  closure, not the candidate's correctness. A canceled invocation remains
  terminal; continuation starts a new attempt from the restored workspace
  as untrusted input.
- Crash-safe terminal classification (Codex P1, round 5):
  `ExecutionOutcome` remains canonical invocation authority, and
  `ExecutionExport` remains canonical completed authority; the ward journal
  is their crash bridge and cleanup authority. Its open record carries
  durable cancellation intent, an optional validated nonzero failure
  status, and, when graceful handoff requires it, the verified recovery
  capture digest. Cancellation intent is durable before stop. A matching
  nonzero status is durable before workspace cleanup. Ward closes canceled
  or failed only after required capture and teardown, then the driver
  idempotently converges the write-once execution outcome. On recovery,
  cancellation intent takes first precedence, then the durable failure
  status controls even if cleanup erased the marker; marker classification
  runs only when neither amendment exists.
- Successful recovery is deliberately asymmetric. Only the live daemon may
  set `WriterComplete`, after stopped or absent, matching nonce, zero status,
  and proxy-health-throughout all hold. Recovery may adopt a zero marker only
  when that bit was already durable; it never recreates the lost proxy
  observation. Zero without the bit is loss. Missing, malformed, or
  mismatched evidence is loss. Those loss paths close only after absence
  proof and teardown. Matching nonzero is failure even if a stale or legacy
  completion bit exists.

## The Instruction and Resume Leg

- Revision of the #375-derived premise, and the assumption that changed:
  with `CLAUDE_CONFIG_DIR` relocated, the pinned CLI resolves user
  instructions and executable configuration beside writable session state.
  The prior gate proved only mutation-refusal of `/root/.claude`, not
  loading. The originally proposed shared identity config contained
  `CLAUDE.md`, backups, policy limits, `projects`, remote settings,
  `session-env`, sessions, and shell snapshots. A launcher copy of only
  `CLAUDE.md` could therefore leave hooks, settings, plugins, or other
  auto-loaded state from an earlier writer authoritative.
- Resolution preserving #375's trust chain: every launch gets a newly seeded
  clean config-root volume containing only empty `projects/` and
  `session-env/` mountpoints. A credential-free observer verifies its full
  manifest before it is mounted read-only. One invocation's continuity volume
  is nested read-write only at `projects/`; a fresh volume is nested
  read-write at `session-env/` for each launch. No other config path is
  writable. Pre-creating the nested mountpoints is required by Apple
  container, and the `session-env/` mount is required for Bash tool startup;
  a read-only root with only `projects/` failed with `EROFS`.
- The nested mounts are returned-object boundaries, not names to trust. Ward
  creates each under a non-reusable opaque identity, proves first-use
  continuity and every per-launch scratch object empty, and journals their
  runtime fingerprints and lifecycle bindings. After container creation but
  before credential delivery or start, runtime inspection must match all
  three journalled volume fingerprints, exact targets and options, and no
  extra mount. A resume accepts untrusted contents only from the exact
  invocation-bound continuity object. Pre-existence, substitution,
  unexpected initial contents, or inspect mismatch fails closed.
- Every gate-mediated launch uses `--safe-mode`, leaving only the image-owned
  administrator policy at `/etc/claude-code/managed-settings.json`
  authoritative. The admitted host instruction and path-scoped repository
  instructions from the exact trusted base are composed deterministically
  into a digest-bound read-only bundle and passed with
  `--append-system-prompt-file`. Workspace `CLAUDE.md`, `.claude/settings`,
  skills, and `.mcp.json`, plus instruction, hook, plugin, and symlink poison
  injected into writable continuity state, all remained inert in the
  production-shaped probe.
- Startup uses a daemon-generated, pre-journalled `--session-id`. Resume is a
  separately journalled ward launch generation after predecessor absence,
  with the identity credential lease and fence retained. It uses a fresh
  verified root, fresh `session-env/`, the same invocation's `projects/`,
  fresh instructions, and `--fork-session --resume <exact-predecessor>
  --session-id <pre-journalled-successor>`. Ordinary resume retained the
  predecessor's system prompt and was rejected. Forked resume preserved the
  continuity phrase, accepted the new instruction marker, and returned the
  exact daemon-chosen successor UUID. A fresh invocation with a fresh
  `projects/` volume inherited neither continuity nor poison.
- Recovery adopts or reaps an already-journalled launch; it never starts a
  duplicate or substitutes a resume. Phase 1A still has no daemon-mediated
  local Claude follow-on, so the unused `InvocationChild` proposal remains
  rejected. A CLI process the agent itself spawns is untrusted agent activity
  bounded by the export gates.

## Pinned-CLI Empirical Contracts

Vendor behaviors this topology depends on, none vendor-documented as
stable, all re-proved on any CLI version bump: root execution refuses
`--dangerously-skip-permissions` unless `IS_SANDBOX=1` is set (the
launcher sets it; the ward-contained VM is the sandbox the flag asserts);
setup-token auth is env-var-only and leaves no reusable store state;
`--safe-mode` disables user and project executable configuration but retains
administrator policy; the CLI supports explicit instruction bundles and
daemon-chosen IDs on initial and forked exact resume; ordinary resume retains
the prior system prompt; Bash requires writable `session-env/`; the CLI exits
nonzero on authentication failure; telemetry endpoints are attempted even
with the disable flags set (the allowlisting proxy denied a Datadog intake
CONNECT while inference succeeded, evidence the provider_only boundary is
load-bearing).

## Rejected Alternatives

- `AgentSpec.Env` token (three grounds above).
- Pre-start rootfs copy (refuted by execution: cp requires running).
- Post-start copy handshake into the running writer (workable but adds a
  ward sequence step and a launcher wait; the volume needs neither).
- Attach-based exit observation (three hazards above; revisit if the
  runtime grows a wait/exit API).
- Privilege-separated wrapper via a non-root agent user (reopens the
  root-owned workspace and `HOME=/root` topology for a guarantee the
  nonce already provides).
- Hand-authored `.credentials.json` (unsupported; unchanged from #378's
  gate).
- Shared writable identity config plus a launcher copy of `CLAUDE.md`
  (executable neighbors survive); in-place scrub or a denylist
  (version-sensitive and live-race-prone); `--safe-mode` with a writable root
  (unnecessary authority remains mutable); a read-only root with only
  `projects/` writable (Bash fails on `session-env/`); `--bare` (does not read
  setup-token auth and may still resolve skills); ordinary non-forked resume
  (retains the prior system prompt); and discarding all provider state
  (unnecessarily loses proven exact resume).
- Mapping canceled or failed to ward loss (erases terminal semantics);
  keeping classification only in `ExecutionOutcome` (cleanup can erase the
  only evidence before the outcome is durable); closing a terminal ward
  before teardown (can strand a credential-bearing object); reconstructing
  `WriterComplete` from status zero (the proxy observation died with the
  daemon); and having ward write engine outcomes directly (couples the gate
  to execution admission).
- A Phase 1A `InvocationChild` enum without a chained-launch call site or
  ward-owned lease/fence transfer. It promises an isolation boundary the
  driver does not implement.

## Verification Ledger (refute-first)

Confirmed by probes: all findings under the three legs above. On the exact
image, a provider-only authenticated run passed with a read-only clean config
root, only `projects/` and `session-env/` writable, safe mode, explicit
instructions, poisoned workspace and continuity state, exact forked resume
with daemon-chosen predecessor and successor UUIDs, and fresh-invocation
isolation. Initial launch ran Bash and returned `TOOL-OK-383`,
`CONTINUITY-383`, and `SAFE-A-383`; the fork returned
`CONTINUITY-383` and fresh marker `SAFE-B-383`; a new invocation returned
only `SAFE-C-383`. No hook or MCP tripwire fired.
Rejected by verification: pre-start rootfs copy (the fresh-context
refutation pass proposed it; execution refuted it); attach exit-status
propagation as sole authority (source-confirmed hazards).
Confirmed by review: cancellation and graceful-handoff capture need separate
publication and recovery semantics; the canceled workspace never becomes a
publication candidate or clean-verifier input, while §5.10's encrypted
post-quiescence capture remains mandatory recovery evidence whose integrity
and frontier closure are verified. A canceled invocation remains terminal;
continuation uses a new attempt seeded from the untrusted restored workspace.
Confirmed by review: terminal failure and cancellation classification must
be durable before cleanup destroys their source; recovery cannot infer the
live daemon's proxy-health-throughout observation from a zero marker.
Durable cancellation and failure amendments outrank any later marker
absence, so recovery cannot downgrade a partially cleaned failure to loss.
Confirmed by review: empirical verification records the complete image
digest above, not an abbreviation that becomes unresolvable after local image
deletion or rebuild.
Confirmed by implementation inspection: #378 exposes one ward
handoff/recover pair and one writer, with no daemon-mediated local Claude
follow-on; its unused `InvocationChild` proposal therefore has no honest
Phase 1A contract.
Confirmed by the final refute-first pass: observing only the clean outer root
would not authenticate nested mounts that mask its empty mountpoints. The
contract now binds and re-inspects every runtime volume identity and proves
new writable objects empty before exposure, closing the stale-object and
mount-substitution window rather than treating the successful happy-path
probe as TOCTOU evidence.
Accepted by decision: process-tree token ambience as the
subscription_contained residual; invocation-scoped transcript continuity as
untrusted resume data constrained by safe mode and exact IDs;
`IS_SANDBOX=1` as a pinned-CLI empirical contract; directly agent-spawned CLI
processes outside the gate-mediated launch boundaries (export gates bound
them).

Revisit when: the pinned CLI version changes (re-run the empirical contract
probes above, including the poison, minimal-state, exact-resume,
fresh-invocation, crash, and live-race matrix); Apple container gains an
exit-status or wait API (re-weigh attach as a liveness supplement); #323
lands (ward proxy env remains the only non-empty spec-environment consumer);
a second real driver arrives (the launcher/marker/state constants are
per-driver, the contract shape is shared); a daemon-mediated local Claude
follow-on becomes a real requirement (design and prove the ward-owned
chained-launch lease/fence boundary before adding a launch kind).
