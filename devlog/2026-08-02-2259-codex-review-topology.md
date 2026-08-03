# Codex Review Topology: Read-Only Fresh Context

Work unit: #480. Scope: `daemon/`.

## Decision

**Codex review uses a dedicated ward launch contract, separate from the
writable workspace-handoff/export path.** The contract builds one exact
container allowlist and a non-secret journal binding for a fresh `codex exec`
invocation. It cannot express resume or continuity, and the existing handoff
path continues to refuse the Codex vendor.

The topology is fixed as follows:

- the candidate workspace is one owned named volume mounted read-only; a
  pinned, networkless observer proves its detached head and clean tree before
  preparation and again after the review container is created, and launch
  requires the volume fingerprint, head, and tree digest to remain identical;
- the fresh container root filesystem supplies separate writable `HOME` and
  `CODEX_HOME` directories for the life of one invocation;
- daemon-prepared `auth.json` and `AGENTS.md` snapshots are independently
  re-opened under one trusted input root, content-checked, then mounted as
  read-only single files inside `CODEX_HOME`;
- one runtime-owned named volume shadows `.agents` read-only at `HOME`, the
  workspace root, and every in-container workspace ancestor, including `/`;
  a pinned, networkless observer first mounts it read-only and emits a
  nonce-bound empty-tree proof, then the pre-start gate requires a second,
  distinctly fingerprinted observer run to re-prove current ownership and
  emptiness after the review container is created; the final journal binding
  records both observer fingerprints, and reconstruction rejects a prepared
  binding until the distinct second fingerprint is present;
- ward constructs the whole launcher environment: only `HOME`, `CODEX_HOME`,
  and the upper/lower-case proxy variables reach Codex and its children;
- ward constructs the whole command: ephemeral `codex exec`, read-only
  sandbox, fixed workspace, `project_doc_max_bytes=0`,
  `--ignore-user-config`, and `--ignore-rules`; and
- the caller's digest-pinned image is accepted only when it exactly matches
  the deployment-owned approved Codex pin, before ward calls the runtime; and
- ward acquires one deployment/runtime-owned exclusive lifecycle lease for the
  candidate workspace and `.agents` shadow before provenance is loaded. Every
  preparation attachment runs within that lease; its `Start` operation
  atomically transfers the lease into the review container, closing the final
  observe-to-start attach/mutate/detach window without taking #427's
  post-start collection and cleanup authority; and
- subscription mode permits only `chatgpt.com:443`; API-key mode permits only
  `api.openai.com:443`. The pre-start gate re-inspects the owned host-only
  network and requires its fingerprint, gateway, subnet, and proxy binding to
  match the admitted observation. The journal binding records that network
  evidence and that the refresh endpoint and publication credentials are
  absent.

The subscription snapshot parser accepts only the pinned auth-file fields,
requires an access token and an explicit empty refresh-token field, rejects a
non-empty refresh token or a co-resident API key, derives expiry from the
access token's JWT `exp`, and refuses when the remaining lifetime is below the
configured floor. API-key mode accepts the
key only in `auth.json`, never in the launcher environment, and rejects a
co-resident token set. Every mutation of the source identity remains outside
the review container under the required `auth_store_mutation_lease`; #448 owns
the proactive refresh and snapshot-production transaction.

The returned journal binding carries only non-secret evidence: auth and instruction
digests, access-token expiry, workspace and shadow observation evidence,
host-only network/proxy evidence, exact endpoint list, and
command/environment digests. It also records the minted review-container
ownership token and creation fingerprint. Decoded binding fields are claims,
not authorization: the backend reloads the durable record and re-inspects the
workspace, shadow, network, and container, including fresh networkless
workspace and shadow observer runs, before it starts Codex.

## Boundary Revision

**The minimal Create/Inspect/Start lifecycle moved into ward in #480.** Seven
automated-review rounds showed that the original split left lifecycle authority
with a future ReviewSource while exporting stateless helpers that appeared to
be launch gates. That boundary could neither mint and retain an unpredictable
owner for the review container nor reconstruct runtime truth from a decoded
journal row. `Backend.CodexReview` now owns creation, observation, durable
binding, reconstruction, and start. The former binding validator and
allowlist verifier are package-private shape checks only.

This is intentionally narrower than #427. The returned launch keeps the
daemon CONNECT proxy alive, but #427 still owns review polling, result
collection, terminal classification, and runtime cleanup. Reusing Handoff's
full lease and teardown state machine here would couple a read-only review
launch to the writable execution/export lifecycle without improving the
pre-start trust boundary.

**The later review-loop decision expands #480 with two deployment seams.** A
digest shape is not approval for credential-bearing trusted compute, so
`ApprovedImage` is deployment-owned and caller intent must match it. Separate
runtime observations also cannot prove a volume stayed unchanged between the
last check and process start. `VolumeLifecycleLeaser` is therefore a required
runtime-facing contract, not a ward-local lock: it owns exclusive attachment
authority for both volumes and atomically transfers that authority into the
review start. A deployment that has not wired those two seams refuses launch
before any runtime call.

**The restart-recovery review finding expands #480 with a launch-intent
record.** The final binding was too late: a daemon crash after any deterministic
create but before `PutCodexReviewBinding` would lose the unpredictable owner,
leaving a same-name object neither safely adoptable nor safely removable. Ward
now begins a distinct non-secret Codex launch intent before it acquires the
lifecycle lease or calls the runtime. The intent binds the exact non-secret
launch shape, durable owner, and deterministic resource names; it earns each
creation fingerprint only after live authentication. Pre-ReviewSource recovery
is cleanup-only: it re-gates that intent, re-authenticates only objects bearing
the recorded owner, proves their absence, then closes the intent for a fresh
launch. A foreign or unprovable replacement stays untouched and leaves the
record open; a potentially credential-bearing review container whose absence
cannot be proved also keeps the lifecycle lease held. This deliberately reuses
Handoff's intent-before-create and claim/absence posture, not its
writer/export/AuthStore-specific record or recovery machinery. The durable
handoff record (`started`) is written only by the invocation that witnessed
the successful start; #427's ReviewSource owns a review only from that record
on and must never create a second review container.

**The final review round replaced crash adoption with reap-and-relaunch
(owner decision, 2026-08-03).** The adoption path tried to keep a started
review alive across a daemon restart: recovery would re-observe the topology,
authenticate the running container against the persisted binding, and record
the `started` handoff itself. Two findings showed that path structurally
unsound rather than incompletely patched: the container's proxy authority is
an immutable environment value whose listener dies with the launching process,
so a recovered daemon can only hand #427 a review with a dead egress path
(P1); and re-running observers during handoff completion overwrote a recorded
observer owner before reconciling a same-name survivor from the crashed
attempt, permanently losing deletion authority (P2). The decisive fact is
that a §7 review invocation is disposable by contract, fresh-context,
read-only, and side-effect-free, so a crash loses nothing but one Codex
invocation. Recovery of a transferred `starting` intent now authenticates the
deployment's transfer evidence against the durable intent, reaps the recorded
owner's container, proves it absent, re-acquires the freed lease, and runs the
ordinary cleanup-only path; a retry relaunches fresh. Both in-process failure
tails (a Start error and a failed `started` record) resolve the same way,
closing the proxy and reaping inline so an error return leaves nothing live.
Rejected alternatives: restoring or rebinding a reachable proxy during
recovery (rebuilds the adoption protocol the findings broke), and a
separately durable proxy service surviving daemon restarts (a materially
larger architecture serving only the preservation of a disposable
computation).

## Rejected Alternatives

- **Reuse the existing handoff.** That path deliberately gives the writer a
  writable workspace and follows it with export. Both are out of scope and
  wrong for a review invocation.
- **Mount all of `CODEX_HOME` read-only.** #401 observed that the CLI cannot
  start in that shape. The fresh root filesystem keeps the home writable while
  the two authority-bearing files stay read-only.
- **Copy auth or instructions into writable home state.** Model-spawned code
  could rewrite either for a later process. Single-file read-only binds keep
  the authority at the mount boundary.
- **Trust CLI environment policy or skill settings.** #401 disproved both
  controls. The exact launcher environment and complete ancestor shadow are
  ward obligations.
- **Permit refresh or ancillary provider hosts.** A nested refresh can spend
  the daemon's single-use token family. The writer receives neither the token
  nor the route.
- **Randomize names or make create idempotent.** Either avoids one collision
  but cannot authenticate or reap a credential-bearing survivor, and makes
  forensic correlation worse.
- **Journal only after create.** A returned error may still have created the
  object, so ownership must be durable before the call, not merely after its
  successful response.
- **Adopt a started review across a daemon restart.** The review is
  fresh-context, read-only, and side-effect-free, so preservation buys one
  Codex invocation while demanding proxy restoration and observer-owner
  reconciliation a restarted daemon cannot honestly provide; recovery reaps
  and a retry relaunches instead (the reap-and-relaunch owner decision).

## Verification Ledger

**Confirmed by checks:** the generated golden carries one read-only workspace,
two read-only single-file binds, every `.agents` shadow target, the exact
minimal environment, fixed severance command, a fresh-context/no-continuity
journal posture, exact provider endpoint, and no publication credential
surface. Fixture checks reject refresh-bearing, mixed-credential, expired,
outside-root, symlinked, hard-linked, shared-permission, or digest-mismatched
input; a shared-permission input root; widened egress; resume and continuity;
writable realized mounts; missing or nonempty shadows; reused or replaced
shadow observations; prepared bindings without independently recorded
pre-start evidence; protected-home or nested-`.agents` workspace overlap and
CLI delimiters at both admission and journal reconstruction; extra
environment; changed workspace identity, head, or tree; replaced or widened
provider networks; predictable or malformed ownership claims;
credential-bearing proxy URLs; oversized prompts; and command or journal
drift. The lifecycle regression additionally proves that the minted owner is
present and authenticated before start, that workspace and shadow observers
run again after durable reload, and that a forged persisted fingerprint or a
same-name replacement review container refuses launch. The final review pass
also confirmed two adjacent admission gaps: private modes do not establish
daemon ownership when `freesided` runs as root, so the input root and both
opened snapshots now require the daemon's effective UID; and candidate mounts
now refuse the image's system, virtual-filesystem, and tool roots so an
admitted workspace cannot hide the pinned Codex or observer executables,
runtime mounts, or provider TLS configuration. A later pass confirmed that a
fresh ext4 named volume is not literally empty because it may contain the
runtime-created `lost+found`; ward now runs the existing constrained empty
state initializer before the first read-only shadow observation, with owned
cleanup on every failure path. The same pass found that launch admission was
split across pre- and post-observation code. Prompt, instructions, auth
snapshot, and static config admission now all complete before the first
runtime call, with a regression table proving malformed caller input launches
nothing. The following pass closed two runtime-identity gaps: every container
spec is now deep-cloned before it crosses the runtime boundary, so runtime
normalization cannot rewrite ward's expected allowlist; and every observer
run mints a fresh ownership nonce. The observer fingerprint binds that nonce
to the reported creation instant, so distinct passes remain distinguishable
even when the runtime timestamps several creations in the same second. A
subsequent provenance pass rejected self-authentication of the candidate
volume: the review journal must now return the source ward lifecycle's durable
run, volume, ownership token, and creation fingerprint, and launch re-matches
all four to the live volume. Two attachment sweeps bracket the final
reconstruction and refuse any other container retaining the candidate. The
token lifetime floor is re-evaluated from a live UTC clock after that final
sweep, immediately before start.
The scope expansion adds adversarial boundary tests: a caller
image that differs from the approved pin is rejected before any runtime call;
an attach/mutate/detach contender cannot acquire the two-volume lifecycle
lease while its `Start` operation is in progress; and a later pre-start
refusal releases the lease rather than stranding the candidate.
The next pass found three lifecycle clean-up boundaries. The returned proxy now
uses a context detached from the completed launch request and remains owned by
`CodexReviewLaunch.Close`; every observer delete proves absence before it
forgets ownership; and failed preparation retains the volume lifecycle lease
when the credential-bearing review container cannot be proven absent. The
last posture favors a visible held lease over releasing a potentially
startable credential-bearing container.
The restart pass additionally proves the launch-intent begin happens before
the first lease/runtime operation; a failed earned-evidence update leaves its
intent open for recovery; recovery reaps a pre-start owned shadow, network,
and review container then closes idempotently; and a same-name replacement
with a different owner is neither deleted nor treated as recoverable.
The reap-and-relaunch pass replaces the adoption regressions: a transferred
`starting` intent recovers by authenticated reap, lease return, and intent
close, after which the same launch spec relaunches successfully; a failed
`started` record reaps inline and relaunches; forged transfer evidence
(foreign holder, volume set, or target container) refuses without deleting
the same-name running container; and both in-process failure tails leave the
credential proxy unreachable.
The final teardown pass also requires a fresh volume listing after shadow
deletion: a successful delete response alone cannot close the intent, because
a survivor would otherwise block the deterministic retry without authorization
for another recovery attempt.

**Rejected by verification:** a caller-supplied `OPENAI_API_KEY` environment
is not needed for API-key mode; a single shadow at the workspace root is not
complete for a nested workspace; a read-only intent bit alone is not evidence,
so runtime inspection must match the generated mounts before start; and a
caller-supplied shadow fingerprint or digest is not evidence of an empty
volume, so the builder accepts only the opaque result of the observer proof.
A digest-shaped journal value alone is not enough, so reconstruction also
re-gates the fixed targets, booleans, endpoint set, exact empty-tree digest,
observer image, and auth-mode-specific expiry shape.

**Continue-or-escalate call:** after several blocker-sustained automated
review rounds, continue because the ext4 initialization and preflight-ordering
findings are distinct, verified trust-boundary omissions within #480's
declared empty-shadow and invalid-input-launches-nothing acceptance, and each
prior round made measurable progress. The later spec-aliasing and coarse-clock
findings are also new runtime-boundary classes, closed by mechanical sweeps of
every new container-create path and every observer invocation. Stop and
escalate if another pass repeats a completed class or shows fixes creating new
regressions rather than closing the declared topology. The final provenance
and live-clock findings complete two previously implicit dependencies rather
than relaxing the topology: review depends on a prior ward-owned candidate
record, and a configured lifetime floor is a start-time invariant. That stop
condition then fired: the recovered-proxy P1 recurred after a correct fix to
its previous instance, and the observer-owner P2 showed the adoption protocol
generating new durability obligations, so the loop stopped at the `bc25275`
AttentionItem checkpoint instead of patching again. The owner resolved it by
deleting the adoption path (the reap-and-relaunch decision above), which
closes both findings by construction rather than by protocol extension.

**Accepted by decision:** #401 gate 3 remains deferred because this path has no
resume surface; #401 gate 2 remains a ward obligation whose discovery set must
be re-proved on a Codex version bump; #448 still owns proactive refresh,
revocation attention, and production snapshot creation; #427 owns the
ReviewSource operations after ward has durably authenticated and started the
review. The trusted input root must remain daemon-owned and non-shared;
ward requires private root and file permissions, refuses multiply linked
files, re-opens each file without following a final symlink, and detects
replacement during admission. Ownership of that private directory excludes an
untrusted post-admission writer.

The intentional #480/#427 boundary is now explicit: #480 owns all crashes
before the durable ReviewSource handoff and never resumes a partial topology;
#427 owns reconciliation after that handoff. This does not add result
collection, session continuity, proactive refresh, or writer/export recovery
to #480.

The pre-start state machine now has `preparing → prepared → starting → started`.
Ward persists `starting` before calling the atomic runtime Start operation,
and records `started` only in the invocation that witnessed the successful
start. A recovery lease may be adopted only when the deployment coordinator
proves it is free or held by the exact durable owner and exact two-volume
set; that path proves absence, cleans up, releases, and closes the intent. A
coordinator that proves transfer instead supplies the same owner, volumes,
and review-container target; ward authenticates that evidence against the
durable intent, reaps the recorded owner's container, proves it absent,
re-acquires the lease the ended window freed, and continues the ordinary
cleanup path, never adopting the started review. A foreign or malformed
transfer, or an unprovable container, refuses and remains a visible durable
ambiguity with the transferred lease held. An in-process failure on either
side of Start (an ambiguous Start error, or a failed `started` record)
resolves the same way before returning: the proxy closes, recovery reaps, and
the error return leaves nothing live.

Revisit when: the pinned Codex CLI changes; Apple container's mount grammar
gains a proven named-volume single-file/subpath primitive; #448 defines the
production snapshot transaction; or #427 adds collection and terminal cleanup
around this launch gate.
