# Admitted Agents

Work unit: plan revision 39 (docs-only, material under §9: credential
policy, §7 independence semantics, driver-selection vocabulary). A
contract and trust-boundary change, so a note is mandatory. The revision
makes the agent's own configuration an admitted input, the way every
other input already is, and names what that supersedes.

## Principle

Freeside admits what an agent consumes by digest: base commit, prompt
package, vendor instructions, policy, input artifacts. The agent's
configuration (which harness, through which credential route, asking
which model at which effort) was the one major input still taken on
faith and hardcoded in deployment wiring. An **agent** is one
operator-authored, content-addressed document of four role-free lines;
a **lineup** maps roles to agents; admission treats the agent exactly as
it treats every other input; runs record what was requested, admitted,
and observed.

## Why Now

The owner's goal (2026-08-22) is to mix and match harnesses, providers,
models, and effort per role cheaply enough to learn which model does
which job well at what price, and to switch when usage runs out. The
first concrete case is OpenAI Sol through the pi harness on the ChatGPT
subscription, which the owner records OpenAI as permitting for
third-party tools (OpenAI's Codex-for-open-source material names pi and
OpenCode; no stability commitment exists on that OAuth interface, so the
route carries a dated terms basis and the §14 terms-drift risk watches
it). Revision 36's `ProviderProfile` could not express it: `provider`
did triple duty as credential vendor, model developer, and execution
harness, and the Codex driver units (#406–#408) keyed dispatch on that
conflation. Sol-via-pi (an OpenAI credential driven by a non-OpenAI
harness) and OpenRouter (one credential, many model vendors) both break
the one-to-one.

## Decisions

**Chose one atomic agent document over per-axis policy keys** (harness,
model, effort as independent `ResolvedPolicy` entries). Per-axis keys
produce joins no one reviewed or ran; an agent is a reviewed diff whose
canonical body hashes resolved ids and fragment digests, never names.
Changing a line is a different agent. "Harness, model, effort" is how a
client renders one, not a selection vocabulary. (Owner, on review
rounds 4–5.)

**Chose stage-owned launch over a launch line on the agent.**
Elaboration, implementation, and review each define their launch (writer
or read-only, output contract, severance, session mode,
auxiliary-inference policy); the adapter maps it or declares it cannot.
This makes "any stage runs on any adapter whose proved capabilities
cover its launch" literally true and removes the role suffix from agent
names. The treatment digest includes the launch so experiments can still
vary tools. Rejected: launch as an agent fragment (role-specific agents,
redundant per-role approval).

**Chose one identity with many client enrollments over one identity per
harness client, and over one untyped store for all clients.** pi and the
Codex CLI on one subscription are one account, one lease, one budget
(the rule revision 36 hardened, kept), but two OAuth clients with
different `auth.json` shapes, so two `ClientEnrollment`s with sanitized
single-route stores and append-only generations. Two identities would
give one subscription two leases and two budgets; one untyped store
would leave the lease no longer naming one exact store. Consequence: the
exact store locator, refresh strategy, and snapshot support leave
`AuthIdentity` for the enrollment and its generations; the lease stays on the
identity and fences every mutation with enrollment id, generation,
locator, and manifest digest. This reverses revision 36's "`AuthIdentity`
stays unchanged"; the assumption that changed is one harness client per
provider account.

**Chose to dissolve `ProviderProfile` as an approval object.** With
agents and lineups in the reviewed tree, a second approval list on the
profile mirrors the same fact in two places, the class of defect
revision 36 spent rounds removing. What the profile still owned,
`enabled` and `cost_owner`, are two identity fields; `freesided auth`
keeps the profile name for the operator. Rejected: keeping
`approved_model_configuration` beside agents.

**Chose "never silent" over "never automatic" for provider switching.**
Revision 36 and §2 item 5 banned automatic fallback when fallback meant
an unrecorded provider swap. A lineup line naming the alternate per
failure class is reviewed, admitted with a full snapshot, recorded as a
new attempt bound to the failure, and carded; it is not the thing the
ban protected against. The human gate stays the default. Eligibility is
failure-specific: a quota failure needs a different usage pool, and two
clients on one subscription share one, so Codex → pi is an experiment,
not a hedge. (Owner.)

**Chose lineage as the default review-independence rule, with a recorded
knob.** The offers' lineage groups differ, at vendor-family granularity,
curated conservatively, the same weights through any route one group,
unknown failing closed; a project lineup may relax it with a stated
reason and every record carries which rule applied. Supersedes the
provider-plus-identity comparison (stricter by default, because Sol
implementing with a Codex model reviewing is same-family; explicit when
relaxed, because an experimentation platform must be able to run that
pairing knowingly). Switching the review agent opens a new convergence
segment so the yield policy never counts a new reviewer's first pass as
the old reviewer's next round. (Owner.)

**Chose two proofs over a qualification ledger.** The adapter's stage
contract suite, one record per build with proved capabilities (the
`BackendConformance` pattern, including the store contract), and an
attended first run per agent × launch that an operator marks in the tree,
the mark naming the agent and launch digests so a line edit (a new
digest) runs attended again. Runner conformance is untouched. Admission
step 4 reads expiry only where the auth method exposes one; the Claude
setup token does not, so its generation has no expiry and an auth failure
at use fails closed (review finding, round 1). Rejected, as machinery beside the
mechanism rather than the mechanism pointed at a new input: a
projection-keyed claim ledger with generations, supersession, and
expiry; a separate credential-pass record (it is a proved adapter
capability); a smoke-record type; an alias and withdrawal registry (the
tree is the active set: admission resolves only agents whose closure is
present in the current approved revision, and git is the history).

**Chose to not pre-prove harness × model beyond the attended first run.**
A request works or fails closed against the stage's output validation;
an observed model, operator, or route that contradicts a pinned admitted
value fails the attempt as a durable contradiction. Pre-proving a rolling
upstream is fiction; offers declare `identity_stability` and records claim
only what the route exposed. Accepted residual: the first unattended run
of a new pairing can fail for a reason a test would have caught.

**Chose source research over a design spike for pi.** Read from pi
0.84.2: it hard-fails on a read-only store when its token needs refresh
(no retry, no in-memory fallback; refresh host `auth.openai.com`,
inference `chatgpt.com`), so admission step 4 (generation expiry covers
the attempt deadline, daemon refreshes first) and the Codex #448 refresh
pattern contain it; provider ids `openai-codex` and `openai` are distinct,
so a one-entry store per route is natural; severance is flag-complete
(`--no-context-files`, `--no-extensions`, `--no-skills`,
`--no-prompt-templates`, `--no-approve`, `--tools`, `PI_OFFLINE=1`,
`PI_SKIP_VERSION_CHECK=1`, a fresh `PI_CODING_AGENT_DIR`); a missing
`--session` id warns and creates since 0.82.0, so the adapter pre-checks;
`--append-system-prompt` gives the instruction-delivery binding; child
`bash` receives no auth variables; compaction uses the selected model; no
core subagents; usage comes from the provider per message;
`npm-shrinkwrap.json` ships and Node ≥ 22.19. Left to the conformance run
on the pinned build: the generated per-model thinking map, `--no-approve`
against a saved trust decision, exit and cancel in the chosen mode,
usage reconciliation against the proxy. Rejected: a pre-adoption spike
ahead of the adapter, because nothing pi-specific can shape a schema in
which launch belongs to the stage and capabilities are adapter-declared,
and two harnesses that already disagree on every axis (Claude Code, Codex
CLI) are the data points the contract generalizes from.

**Chose not to collapse `StageDriver` and `ReviewSource`.** They may share
a harness runtime beneath (as #872 did); the upper contracts differ in
lifecycle and stay distinct. A stage count is not evidence for interface
unification.

## Verification

Docs-only. The refute lens was four independent external reviews of the
design summary across five rounds, the last two against the
distilled form; every blocking finding is either applied in the plan
text or recorded above as a rejected alternative with its reason. The
one-afternoon premise check (headless pi ChatGPT login into a sanitized
one-entry store, then a daemon-driven refresh) is not done and gates the
pi unit, not this revision.

## Revisit When

A third stage kind appears (reconsider stage-owned launch's vocabulary
and the adapter capability set); a harness needs an auxiliary-inference
policy the launch cannot express; the attended-first-run gate misses a
class of failure twice (then the smoke becomes a record with a subject
key); or OpenAI changes the standing of third-party clients on the
subscription (the route's terms basis is dated for this).
