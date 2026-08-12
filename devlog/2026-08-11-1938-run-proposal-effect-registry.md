# Run-Proposal Effect Registry Trust Boundaries

Date: 2026-08-11. Author: implementation agent. Issue: #654. Source
planning note: `devlog/2026-08-10-1730-wave5-planning.md`.

## Decisions

**Chose a dedicated `EffectProposal` union over reusing `Artifact` because
proposal validity and approval binding are a different trust model from
publish eligibility.** The closed registry dispatches each `EffectKind` to one
fixed Go parameter type, constructor, and current-policy gate. The existing
`Artifact` shape remains only the client-visible digest metadata carrier: a
compiled deterministic recipe identifies that carrier, while the proposal
instance row holds and re-gates the authority-bearing body. Rejected
alternative: treating a proposal as ordinary verification evidence, which
would let recipe eligibility stand in for registry admission.

**Chose enum and numeric run parameters over free-form intent, cost, and scope
text because bounded byte strings still provide an event-body and target-data
channel.** `RunProposalParameters` carries one daemon-enumerated opaque subject
handle, a closed intent, bounded cost units, and bounded scope counts plus a
control-plane flag. It structurally has no event body, path, target identity,
or authority field. Rejected alternative: size-capped prose fields, which
bounded storage but not meaning.

**Chose occurrence-key allocation over content-derived identity because retry
and deliberate repetition are independent of semantics.** The typed union
admits only canonical upstream event IDs, client submission-command IDs, or
one accepted invocation/export identity plus a positive emission ordinal. One
SQLite upsert allocates or returns the daemon-generated proposal-instance ID;
a changed body under the same occurrence key is an immutable conflict. The
instance ID anchors item bindings, revisions, decisions, snoozes, and crash
reconciliation, while proposal digests bind reviewed content.

**Chose a resolved replacement card for `start_with_changes` over mutating the
original card because the original reviewed digest must remain auditable.**
The accepting transaction stores the revised artifact, supersedes and bumps
the original item, creates a resolved replacement at the next item version
bound to the exact revised digest, and records that digest in the
instance-ledger decision. `start` remains decision-recording; its downstream
run consumer is outside #654. `snooze` records the UTC deferral and advances
the still-open item version so pre-snooze commands become stale.

**Used migration 0041 instead of the issue comment's provisional 0037 because
the serialized contract chain advanced schema head to 0040 before #654
started.** The initial implementation assumed the existing evidence metadata,
artifact-digest command binding, `run_proposal` type, and `proposal_batch`
subject were a sufficient rendered contract. Automated review disproved that
assumption: those fields authenticated a digest but did not expose the intent,
cost, scope, or exact revision context the operator was deciding.

**Chose canonical `WorkUnitID` values as opaque run-proposal subject handles
because the durable declaration is the independent authority that binds a
selected subject to its project, run, and resolved policy.** The store resolves
that declaration at admission and again at decision time; the proposal cannot
choose its authority by supplying a policy or an arbitrary enumeration beside
its parameters. Rejected alternatives: reloading the policy run named inside
the proposal, which made the current-policy check tautological, and keeping a
production callback whose authority depended on conventional wiring.

**Chose a durable active-snooze gate over reusing `expires_when` because a
snooze defers an otherwise-open decision rather than expiring its lifecycle.**
Bootstrap, item reads, command acceptance, and delivery submission omit or
reject the proposal until the latest snooze instant passes. The first query or
heartbeat at expiry authenticates and releases the ledger row, then advances
the item version and server revision so heartbeat-only clients invalidate the
hidden snapshot without changing `expires_when` or losing the command ledger.
Rejected alternatives: using only the admission-time item-version bump, which
left the card visible and deliverable, and making visibility depend only on
wall-clock time, which left a hidden cached snapshot apparently current.

**Chose a separate authenticated proposal-facts projection plus typed decision
inputs once review showed the generic card was not an invokable control.** The
daemon reconstructs the proposal through its store binding and current
work-unit policy, then exposes only intent, cost, scope, proposal digest,
superseded digest, and the exact item/entity/revision tuple. The client matches
that tuple to the card before enabling any proposal action. `start_with_changes`
can change only the bounded public parameters; the server overlays the stored
opaque handle. `snooze` carries only a typed UTC instant. Rejected alternatives:
keeping this PR daemon-only, which would offer decisions without authenticated
review facts or callable parameter controls; embedding authority-bearing policy
or target identities in the wire shape; and accepting caller-authored JSON in
the generic message field.

## Verification Findings

**Confirmed and fixed: ledger methods initially trusted their internal caller
to pair a persisted command with the right instance, action, rendered digest,
and selected revision.** The store boundary now reconstructs the immutable
command and item binding before every revision, terminal decision, and snooze
write. Adversarial tests prove wrong actions and digests fail with
`ErrTransitionCommandMismatch`.

**Confirmed and fixed: adding the compiled proposal carrier recipe to the
ordinary approved-recipe set initially widened trust to any artifact claiming
that recipe.** Artifact put, read, and item reconstruction now prove that a
carrier exactly matches `EvidenceArtifact` derived from a durable proposal
instance or revision. A forged carrier is rejected even when its recipe bit
and producer class otherwise look eligible.

**Confirmed and fixed during automated review: decision re-gating had loaded
the policy named by the proposal itself, production Signet had no subject
resolver, and snooze records did not affect visibility or delivery.** The
store now resolves canonical work-unit handles independently at admission and
decision, so production uses the same boundary without optional wiring;
active snoozes gate bootstrap, list/get, new commands, and notifications until
expiry. Adversarial tests reject a proposal bound to a historical policy for a
currently resolved handle and exercise the full snooze visibility interval.

**Confirmed and fixed during refute-first review: a revised proposal could
retarget its opaque handle, snooze expiry did not advance the synchronization
revision, snooze rejections became ambiguous HTTP 500s, and snooze reads
trusted partial rows.** `start_with_changes` now preserves the original
handle and rechecks its declaration project at the store boundary. Expiry has
one durable release transition. Snoozed commands use the client's generic
authoritative 400 path rather than its stale-version-specific 409 decoder;
hidden reads return 404. Reconstruction authenticates the complete snooze
sequence against durable commands, instance bindings, canonical timestamps,
and non-overlapping intervals before any row can hide or release a proposal.

**Confirmed and fixed after widening the unit to the wire and clients: the
first proposal-facts mock fixture carried unrelated claims, the projected
revision did not reconstruct the command-authored delta, and the mock's
revision/snooze transitions diverged from the daemon.** Run-proposal fixtures
now carry only their proposal artifact; facts reconstruction decodes the
canonical durable command, overlays its bounded delta on the stored handle,
re-resolves current policy, and requires the resulting digest to equal the
rendered revision. The mock now creates the distinct superseding replacement,
hides then version-releases snoozes, and rejects commands throughout the
active interval without revision movement. Focused tests exercise all four
controls and both lifecycle paths.

**Confirmed and fixed in the confirming review: an unchanged typed revision
fell through to a store invariant error, and proposal-carrier reconstruction
reproduced historical bytes without the current subject-policy gate.** The
decision service now rejects a no-op revision as definitive invalid input
before the ledger write, so HTTP returns an authoritative 400 and the client
does not preserve an ambiguous pending command. Carrier reconstruction now
resolves the opaque handle and re-runs the registered proposal gate before it
derives or serves evidence. Regression tests keep the item unchanged after a
no-op and reject a self-consistent carrier bound to historical policy.

**Confirmed and fixed in the next carrier review: current-policy validity did
not by itself authenticate a revision's durable author.** Carrier and facts
reconstruction now share one complete revision-ledger gate that binds the row
body and superseded digest to its `start_with_changes` command, the command's
immutable artifact/item tuple and original instance binding, the canonical
typed delta, the opaque subject, and current policy before deriving the expected digest. An adversarial row
rebound to a non-revision command fails closed despite carrying otherwise
self-consistent current-policy proposal bytes.

**Confirmed and fixed in the client lifecycle review: authenticated facts
were invalidated by a same-epoch item advance, but the selected view only
restarted validation after an epoch eviction.** A run-proposal view now keys
its validation task on the exact revision/entity/item tuple in addition to the
cache generation. Snooze release or delivery timing convergence therefore
disables actions only until the new tuple's facts are fetched, without forcing
the operator to navigate away and back.

**Confirmed and fixed in the generic-intake and snooze-authentication review:
raw run-proposal cards could bypass atomic admission, and reconstructed snooze
rows did not prove the command's reviewed snapshot.** Generic Signet intake now
rejects run proposals after action-policy validation; the engine's atomic
admission remains the only path that can resolve current authority and bind the
carrier. Snooze creation requires an exact open-item command binding, while
reconstruction binds the immutable digest set, PR head, and phase-specific
version floor without rejecting legitimate later delivery or terminal
transitions. Rejected alternatives were synthesizing missing admission context
from caller data, and requiring an exact post-snooze version that concurrent
delivery updates can legitimately advance.

**Confirmed and fixed in the final authority review: declared-path scope was
bounded but still caller-authored, revision authentication re-entered the
proposal carrier gate through its historical command item, and the client
treated a snooze's documented hidden-item response as a failed refetch.** The
daemon now rejects a declared-path count that differs from the durable work-unit
declaration at admission, decision, persistence, and reconstruction. Component
count and the control-plane flag remain bounded review facts because the
declaration lacks the repository trust profile required to classify them;
guessing them from path spelling was rejected. Revision authentication uses the
structurally authenticated historical item record and independently checks its
command tuple, avoiding recursive evidence authentication. A successful snooze
404 now evicts the same-epoch cached card and settles as applied without a false
validation error. The review's mock revision-movement concern was rejected by
verification: complete union validation already runs before the actor-isolated
transition and revision increment, so malformed parameter combinations cannot
reach the alleged mutation.

**Rejected by verification:** strict decoding refused unknown and trailing
JSON, invalid UTF-8, and oversized bodies; the typed admission union refused
empty, contradictory, untrimmed, overlong, and malformed source identities;
column/body and content-digest tampering failed reconstruction; stale policy
bindings and unregistered opaque handles failed the current gate; changed content
under one occurrence key conflicted; distinct occurrence keys preserved
distinct instance IDs for identical content; and close/reopen retry preserved
the original instance. No finding required an accepted risk or owner waiver.

## Revisit When

- A second effect kind lands: re-evaluate whether the tagged union remains the
  clearest compile-time registry without introducing data-driven dispatch.
- A client needs richer proposal facts than the existing artifact metadata and
  bounded facts projection: extend the authenticated projection and generated
  consumers together rather than smuggling facts into evidence metadata.
