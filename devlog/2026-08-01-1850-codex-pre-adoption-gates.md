# Codex Driver Pre-Adoption Gates: Probe Results

Work unit: #401. Scope: `devlog/` only. Probes, no product code.
Carries forward `devlog/2026-07-30-1620-codex-driver-feasibility.md`
(#395), which opened these gates; that note is frozen, this one records
their outcomes.

## Decision

**Gates 1, 2, 4, and 5 are closed on observed evidence; gate 3 stays
deferred to the wave-7 execution tail per the scheduling decision on
#401. The pinned CLI stays codex-cli 0.137.0.** 0.146.0 was checked
specifically for gate 2 and adds no severance switch, so a bump buys
nothing for the gate that motivated it and would force re-proving every
#395 empirical contract; #404 pins 0.137.0.

Three results change the Phase 2 driver design, and each is a
correction to an assumption the #395 note or a downstream issue carries:

1. **Refresh tokens are single-use, and reuse revokes the whole token
   family** (observed, gate 1). The agent-facing snapshot must never
   refresh: an in-container refresh attempt either fails the turn (when
   the token is already spent) or, if it succeeds against a read-only
   store it cannot persist, silently spends the daemon's copy. Two
   consumers of one identity's store do not merely diverge, they can
   revoke the identity. The daemon refreshes proactively between
   executions under the §5.4 `auth_store_mutation_lease`.
2. **Vendor instructions bind to `$CODEX_HOME/AGENTS.md`**, an
   append-authority path (observed additive), not the `instructions`
   config key (replace authority). This is the direct analogue of
   Claude's `--append-system-prompt-file`: daemon-owned, outside the
   workspace, additive, and compatible with severing every workspace
   instruction surface. It supersedes #406's stated premise of "the
   `VendorInstructions` role for a replace-authority mechanism".
3. **`shell_environment_policy` is inert on the `codex exec` path**
   (three variants, no observable effect). Model-spawned commands
   inherit the launcher environment verbatim, including arbitrary
   secrets. Containment is ward's launcher environment, never CLI
   configuration.

Evidence below is observed behavior of **codex-cli 0.137.0** (Homebrew
binary, the pinned version) and **codex-cli 0.146.0** (npm
`@openai/codex`, installed side-by-side in a scratch prefix), on a
macOS host with ChatGPT-subscription auth. Every run used `env -i` with
an isolated `HOME` and `CODEX_HOME` seeded only with a copy of the auth
store, `codex exec --json --skip-git-repo-check -s read-only` (or
`-s workspace-write` where a run had to write), and stdin from
`/dev/null`. Per the Revision-22 pattern each observation is a
pinned-CLI empirical contract to re-prove on version bump, not a vendor
guarantee.

## Gate 1: Auth Refresh. Met

**Observed**: forced expiry triggers a refresh, the refresh endpoint is
`auth.openai.com:443`, refresh tokens rotate and are single-use, and
reuse detection revokes the family.

Method: copy the auth store into an isolated `CODEX_HOME`, rewrite the
access token's `exp` claim to a past instant and `last_refresh` to
2026-06-01 (header and signature bytes untouched), run one two-word
prompt through a local CONNECT-logging proxy, and compare the store's
sha256 and per-field digests before and after. Probe scripts and raw
logs are session-local; the observations are:

- **Expiry triggers refresh, and it persists.** Run A (writable store):
  exit 0, `turn.completed`, `CONNECT auth.openai.com:443` observed, the
  store rewritten in place with a new access token
  (`exp` 2026-08-11), a new `last_refresh`, and a **different**
  refresh-token digest. So 0.137.0 refreshes on its own when the access
  token is expired, and rotation is real.
- **Refresh is single-use, and reuse revokes the family.** Run A2
  replayed the pre-rotation refresh token 82 seconds later and
  succeeded (issuing a third distinct token), which looked like a
  tolerated replay. It was not: every subsequent refresh, including run
  D with a **never-used** token issued by A2 and a fully writable
  store, failed with `Failed to refresh token: Your access token could
  not be refreshed because your refresh token was already used. Please
  log out and sign in again.` A never-used token being rejected is
  family-level revocation, the standard OAuth reuse-detection response,
  triggered by A2's deliberate replay.
- **A store that cannot refresh fails the turn, it does not degrade.**
  Runs B1 and C (store `chmod 400`, home writable): four
  `codex_login::auth::manager` refresh errors, `401 Unauthorized` on
  `wss://chatgpt.com/backend-api/codex/responses`, terminal
  `turn.failed`, exit 1. The failure is loud and terminal, which is the
  behavior the driver wants.
- **A read-only `CODEX_HOME` is not viable at all.** The first
  read-only attempt (`chmod 500` on the whole home) died before any
  turn with `failed to initialize in-process app-server client:
  Permission denied`. Only the *store* may be read-only; the home must
  be writable. This matches the #395 per-invocation-home shape.

**Consequences for the driver.** The daemon refreshes proactively
between executions inside the lease-held transaction and hands the
agent a snapshot whose access token comfortably outlives the
execution; the agent-facing mount stays read-only, and a refresh
attempt inside the container is a failure to surface, not a fallback to
tolerate. Identity availability is now a first-class failure mode: a
spent or revoked chain means every execution for that identity fails
until a human re-authenticates. Detection and the operator attention
item are #448; the re-enrollment path that actually clears them is
#451, because single-use rotation makes re-authentication a standing
recovery path rather than a one-time bootstrap.

**The refresh host is daemon-side only, and the snapshot drops the
refresh token.** `auth.openai.com:443` must **not** appear in the
writer's `provider_only` profile, and the agent-facing snapshot must
not carry a refresh token at all. The writer can read its mounted
store, so a snapshot containing a refresh token plus egress to the
refresh host lets model-run commands copy the token into a writable
home and force a nested refresh, which spends the daemon's token and,
on the replay path this note's own probes walked, revokes the whole
family. Both halves are observed to be unnecessary:

- **A snapshot with an empty `refresh_token` works.** Run with the
  field emptied and a valid access token: exit 0, `turn.completed`,
  correct reply. The writer therefore never needs to possess the
  family-revoking credential; the daemon keeps it on its side of the
  lease and mounts an access-token-only snapshot.
- **Expiry without a refresh token fails closed, loudly.** Same
  snapshot with the access token's `exp` forced into the past: exit 1,
  `turn.failed`, ten `error` events, and the explicit
  `Failed to refresh token: ... Invalid 'refresh_token': empty string.`
  No hang and no silent degradation, which is what makes an
  admission-time remaining-lifetime floor (#448) a sufficient guard.

So the containment is structural rather than policy-only: deny the
refresh host to the writer *and* withhold the refresh token, and an
in-container refresh cannot succeed even if the egress profile is
later misconfigured.

**Accepted residual.** One combination stayed unobserved: a refresh
that *succeeds* server-side while the store is read-only, so the new
token exists only in memory. The probe sequence revoked the family
before that case could be isolated. It does not change the design,
because a successful in-container refresh spends the daemon's token
either way, which the proactive-refresh rule already forbids. Re-prove
opportunistically after the next enrollment, before any resume-capable
execution work.

**Operator impact, recorded deliberately.** These probes revoked the
host identity's refresh chain (owner-approved beforehand, worst case
understood as a re-login). The live access token remains valid until
2026-08-11; `codex login` re-enrolls.

## Gate 2: Workspace-Skill Surface. Closed by Topology and Policy

**No severance switch exists on 0.146.0 either, so the gate closes the
other way the issue allows: a topology-and-policy argument, with the
refutation attempts below run as probes rather than asserted.**

Switch search on 0.146.0 (all rejected under `--strict-config` as
unknown configuration fields): `skills.enabled`, `skills.paths`,
`skills.load`, `skills_dir`, `features.skills`. `skills=[]` parses and,
as on 0.137.0, **does not sever** (canary still fired).
`codex features list` (100 flags on 0.146.0 vs 81 on 0.137.0) has no
skills on/off flag; the version adds `skill_search` (stable), which is
discovery, not severance. `--disable`/`--enable` are just
`features.<name>` aliases, so they reach nothing that severs either.

Canary matrix, 0.146.0, workspace holding both an `AGENTS.md` canary
and a `.agents/skills/canary/SKILL.md` canary, prompt "Reply with
exactly: PROBE-OK":

| Topology / flags | AGENTS.md | Workspace skill |
| --- | --- | --- |
| git workspace, no overrides | fired | fired |
| `-c project_doc_max_bytes=0` | severed | fired |
| `-c project_doc_max_bytes=0 -c skills=[]` | severed | fired |
| non-git workspace | severed by same flag | fired |
| cwd is a subdirectory, `.agents` at the parent | severed | fired |

`$HOME/.agents/skills` also still loads on 0.146.0 (separate canary),
so #395's clean-container-`$HOME` requirement stands unchanged.

**Refutation attempts, and what survived.**

- *"Discovery is git-root-anchored, so a non-git workspace severs it"*
  (the lever that works for `AGENTS.md`): **refuted by probe**. The
  skill fired with no git root at all.
- *"Discovery is workspace-root-anchored, so running from a
  subdirectory severs it"*: **refuted by probe**. With `.agents` at the
  parent and cwd in a child, the skill fired: discovery walks
  ancestors. Any shadowing mechanism must therefore cover the workspace
  root **and every ancestor path inside the container**, not just the
  root.
- *"A loaded skill is weak context that the stage prompt outranks"*:
  **refuted by observation**. The prompt said "Reply with exactly:
  PROBE-OK"; every run with the skill loaded appended the canary token
  anyway, and one run narrated "I'm applying the workspace's mandatory
  reply-formatting skill". A workspace skill overrides explicit stage
  instructions, which is why tolerating the surface is not an option.
- *"The surface is severable by policy"*: **survives**. §5.8 already
  classifies workspace copies of vendor instructions as untrusted data,
  so nothing legitimate is lost by making the path unreadable to the
  CLI. Ward mounts an **empty read-only overlay at `<workspace>/.agents`
  and at every ancestor `.agents` path inside the container**, keeps the
  container `$HOME` clean, passes `-c project_doc_max_bytes=0` and
  `--ignore-rules` in the fixed argv on every start, and journals the
  overlay bindings the way §5.8 already journals the config root. The
  overlay is a **writer-launch mount only**: the underlying repository
  bytes stay in the volume, and the export runs in the fresh,
  credential-free context after the writer terminates, without the
  overlay, so it still walks the real `.agents/**` content.
- *"Then a candidate that must edit `.agents/**` cannot be worked on"*:
  **conceded, and it is a real limitation rather than a non-issue.**
  The overlay makes the path unwritable, so a writer cannot produce
  `.agents/**` edits in the workspace that becomes the
  `ExecutionExport`, and handing the content in as read-only stage-input
  data exposes it without giving edits a way back. §5.8 keeps
  agent-modified instruction files as candidate diff content, so the
  topology owes one of two things, and #407 must pick before the driver
  runs such a work item: a specified editable round-trip (a shadowed
  edit path reconciled into the export under the same risk-flagging), or
  an explicit admission-time refusal of work items whose declared paths
  touch `.agents/**`. Until one exists, treat such work items as
  **unsupported and refused**, not silently mis-executed. Recorded as
  accepted-by-decision in the ledger below.

**Residual, stated plainly**: the closure is a ward obligation, not a
CLI guarantee. If a future version discovers skills from a path ward
does not shadow, the argument fails silently. The empirical contract to
re-prove on bump is therefore the *discovery set*, not just the absence
of a switch.

## Gate 3: Poisoned-Rollout Resume. Deferred to Wave 7

Not probed, per the 2026-08-01 scheduling decision on #401: a
fresh-context review pass never resumes, so gate 3 gates execution
adoption rather than the review substrate wave 3 stands up. Recorded
disposition is "deferred to wave 7", not "unknown".

## Gate 4: Vendor-Instruction Delivery. Decided

**Binding: deliver the composed, digest-bound instruction bundle as
`$CODEX_HOME/AGENTS.md`, mounted read-only inside the per-invocation,
otherwise-writable `CODEX_HOME`.** Observed on both versions:

| Delivery path | Canary | Input tokens (0.146.0) |
| --- | --- | --- |
| baseline, no instructions | n/a | 14,022 |
| `-c instructions="…"` | fired | 10,715 (**-3,307**) |
| `$CODEX_HOME/AGENTS.md` | fired | 14,271 (**+249**) |
| `$CODEX_HOME/AGENTS.md` + `-c project_doc_max_bytes=0` | fired | 14,285 |
| `$CODEX_HOME/AGENTS.md` + `--ignore-user-config` + `project_doc_max_bytes=0` | fired | 14,285 |
| instructions inside the prompt text | fired | 14,269 |

The token deltas are the authority evidence: the `instructions` key
*displaces* the built-in base template (-3,307 here, matching #395's
12,803 → 8,437 on 0.137.0), while the home document *adds* to it
(+249). The home path also fired on 0.137.0 (12,260 input tokens with
the document), so the binding is available on the pinned version, not
only on the version checked for gate 2.

**Why this path wins.**

- It preserves §5.8's composition contract unchanged: the bundle
  deterministically composes the admitted host instruction (including
  explicit absence) with repository vendor instructions from the
  trusted base, preserving path scopes and precedence, and its source
  digests, composition version, and result digest are journalled before
  launch. Only the delivery mechanism differs from Claude's.
- It is orthogonal to severance: it survives both
  `project_doc_max_bytes=0` (which severs workspace documents) and
  `--ignore-user-config` (which blocks `config.toml`), so ward keeps
  the full fixed severance argv and still delivers instructions.
- It keeps `VendorInstructions` a distinct stage-input role rather than
  folding it into the prompt package.

**Rejected: the `instructions` key.** Replace authority makes the
daemon the author of the CLI's entire base template, an undocumented,
unversioned surface that changes between releases; a partial
reimplementation would silently degrade tool-use, sandbox, and
apply-patch behavior, and every version bump would require re-authoring
it. The -3,307-token drop is exactly that displacement, measured.

**Rejected: delivery inside the prompt package.** It works (canary
fired) but conflates two roles §5.8 keeps distinct, gives instructions
user-turn authority below the surfaces the driver is trying to
outrank, and loses the digest-bound-file shape the ward journal
records.

**Containment note for the implementing unit.** `CODEX_HOME` must be
writable for the CLI's own state (gate 1's read-only-home failure), so
the instruction document is placed as a read-only bind mount of a single
file inside that writable home. Without that, model-run commands could
rewrite it; even then the blast radius is the next launch from the same
home, which the per-invocation home already precludes.

## Gate 5: Child-Environment Credential Exposure. Met

**Observed: model-spawned commands inherit the launcher environment
verbatim, and the CLI's own environment policy does not constrain
them.** Method: launch with `env -i` plus two marker variables
(`FREESIDE_MARKER_SECRET`, a fake `OPENAI_API_KEY`), have the model run
`env | sort > env-dump.txt` under `-s workspace-write`, then compare
every value against the live credential material by sha256.

- **Default policy**: both markers reached the child verbatim. The
  child environment was `CODEX_CI`, `CODEX_HOME`, `CODEX_SANDBOX`,
  `CODEX_SANDBOX_NETWORK_DISABLED`, `CODEX_THREAD_ID`, `HOME`, `PATH`,
  `PWD`, `LANG`/`LC_*`, `TERM`/`COLORTERM`, pager variables, plus
  everything the launcher set.
- **No provider credential leaks by default**: no value matched the
  live access or refresh token. ChatGPT-mode credentials stay in
  `auth.json`, confirming #395's "no credential in argv or events" for
  the child-process path too. The residual #395 assumed by analogy with
  Claude's ambient environment does **not** exist for the tokens.
- **`shell_environment_policy` is inert here**:
  `inherit="none"`, `inherit="core"`, and
  `exclude=["FREESIDE_*","OPENAI_API_KEY"]` each parsed (valid config
  fields under `--strict-config`) and each produced a byte-identical
  child environment, markers included. The key exists and does nothing
  on the `exec` path.
- **Same on both versions**: the default run and the `inherit="none"`
  run were repeated on the pinned 0.137.0 and produced the identical
  child environment, so this gate is closed on the pin, not inferred
  from the newer version.

**Consequence**: ward launches the CLI with an explicitly constructed
minimal environment and treats anything it exports as reaching
untrusted model-spawned code. No CLI-side control may be relied on. In
particular an API-key deployment must not pass `OPENAI_API_KEY` in the
launcher environment: the auth-store path is the only non-leaking one,
and it is the path both auth modes already use.

## Egress, Corrected

Observed through the CONNECT-logging proxy on 0.137.0, and this
corrects #395's ancillary list, which was incomplete:

- Inference: `chatgpt.com:443`.
- Refresh: `auth.openai.com:443` (the endpoint #395 could not observe;
  the binary also carries `/oauth/revoke` and `/api/accounts` paths on
  the same host). **Daemon-side only**: it belongs to the lease-held
  refresh transaction, never to the writer's profile (gate 1).
- Ancillary, all deniable: `ab.chatgpt.com:443`, `github.com:443`,
  `api.github.com:443`, `files.openai.com:443`, and three
  `sdmntpr{central,northcentral,southcentral}us.oaiusercontent.com:443`
  hosts. The `oaiusercontent.com` and `files.openai.com` hosts appear
  even in a successful non-refresh run, so they are ordinary CLI
  behavior, not refresh traffic.
- **Allowlist tolerance re-proven under denial**, not just observation:
  with the proxy allowing only `chatgpt.com` and `auth.openai.com` and
  returning 403 for everything else, the run completed exit 0 with the
  correct reply while seven distinct hosts were denied.

So the **writer's** `provider_only` profile is `chatgpt.com` alone for
the subscription mode, or `api.openai.com` alone for API-key mode. The
refresh host is reachable only from the daemon's trusted refresh
transaction, which runs outside the writer's network boundary; putting
it in the writer profile would hand an untrusted writer the nested
refresh described under gate 1.

## Pinning

Pin stays **0.137.0**. Contracts to re-prove on any bump now also
include: the skill discovery set (cwd plus ancestors plus `$HOME`), the
`$CODEX_HOME/AGENTS.md` append authority and its survival of
`--ignore-user-config`, the inertness of `shell_environment_policy`,
the refresh trigger and single-use rotation, and the egress set above.
No JSONL vocabulary difference appeared between the two versions on
these runs: both emit `thread.started`, `turn.started`, `item.completed`
with `item.type == "agent_message"`, and `turn.completed` carrying
usage, so #395's event contract holds on 0.146.0 as far as these probes
exercised it.

## Verification Ledger

This unit sits on a credential-leak surface, so the finish line's
refute-first discipline applies: the probes above were built to
*disprove* the closures, not to confirm them, and every finding lands in
one of three dispositions here.

**Confirmed by probe** (each observed at least once, commands and
outputs in the gate sections): refresh fires on expiry against
`auth.openai.com`; rotation is real and single-use, with reuse revoking
the family; a store that cannot refresh fails the turn rather than
degrading; a read-only `CODEX_HOME` cannot start at all; the workspace
skill surface loads on both versions with no switch; skill discovery
walks cwd, ancestors, and `$HOME`; a loaded skill overrides an explicit
stage-prompt instruction; the `instructions` key replaces the base
template while `$CODEX_HOME/AGENTS.md` appends to it; model-spawned
children inherit the launcher environment verbatim on both versions;
a `chatgpt.com`-only allowlist survives active denial of seven other
hosts; a snapshot whose `refresh_token` is emptied still runs a normal
turn; and that same snapshot fails closed and loudly once its access
token expires.

**Rejected by verification** (hypotheses tested and disproved, recorded
so they are not re-raised): "a non-git workspace severs skills";
"running from a subdirectory severs skills"; "`skills=[]` severs";
"a feature flag or `--disable` can sever skills"; "0.146.0 ships a
severance switch"; "`shell_environment_policy` constrains model-spawned
children" (three variants, both versions); "a rotated-out refresh token
is safely replayable" (it appeared true once, then proved to be what
revoked the family); "the writer's egress profile needs the refresh
host" and "the agent-facing snapshot must carry a refresh token" (both
disproved by the access-token-only run, and both were wrong in an
earlier revision of this note, which put `auth.openai.com` in the
writer allowlist until review caught it); and "0.146.0 changed the
JSONL event vocabulary" (also a claim this note carried, disproved
against both versions' event streams before handoff).

**Accepted by decision**: gate 3's deferral to wave 7 (owner scheduling
decision on #401); gate 1's unobserved combination (a refresh that
succeeds while the store is read-only), because the proactive-refresh
rule forbids that path regardless; gate 2's closure resting on a ward
obligation rather than a CLI guarantee; and `.agents/**`-editing work
items staying unsupported until #407 specifies a round-trip or an
admission-time refusal.

**Not run, stated rather than implied**: no *independent* refute-first
lenses reviewed this note before commit. Delegation to fresh-context
reviewers was not available under this session's policy, so the
refutation here is probe-level and same-context, which the project's
independence ladder ranks below a fresh-context or different-vendor
pass. The compensating passes are the automated reviewer on this PR,
human review before merge, and the wave-3 exit fresh-context adversarial
review; a reader who wants the missing rung should ask for a delegated
refute pass over this note before the gates are treated as load-bearing
for #406, #407, and #408.

## Follow-Up

- #407 (ward's Codex topology) gains three concrete obligations from
  this note: the `.agents` shadow overlay at the workspace root and every
  in-container ancestor, the explicitly constructed minimal launcher
  environment, and the read-only single-file instruction mount inside
  the writable `CODEX_HOME`.
- #406 (driver-selection contract) carries the gate-4 binding, which is
  **append** authority via a delivered file, not the replace-authority
  mechanism its objective currently presumes.
- #451 (enrollment and re-enrollment under the lease) owns the recovery
  side: #448 makes a dead identity visible, #451 is what clears it.
- #448 (proactive refresh under the lease) additionally owes the
  access-token-only snapshot: the daemon keeps the refresh token on its
  side of the lease, mounts a snapshot with the field emptied, and
  guards on remaining access-token lifetime at admission, which the
  observed fail-closed behavior makes sufficient.

Revisit when: the pinned CLI version is bumped (re-prove the contracts
under Pinning); or when a Codex identity is re-enrolled, at which point
the one accepted residual under gate 1 (a successful refresh against a
read-only store) can be isolated cheaply before any resume-capable
execution work.
