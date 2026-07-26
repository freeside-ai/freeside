# Publication Recovery Boundaries

Work unit: #299. Scope: `daemon/`, `devlog/`.

## Decisions

**Bind authorization to the complete ordered evidence snapshot.** The domain
owns one canonical JSON-and-SHA-256 digest function for `[]Artifact`, and both
the engine producer and Publisher consumer use it. Comparing only repository,
head, recipe, trust profile, or artifact blob digests would permit a
structurally different evidence record to travel under an authorization that
never approved it.

**Finalize only a result returned by the already-gated publication call.**
Publisher binds that result to its own shared-store intent and records the
outcome atomically with intent dispatch. The final transaction reloads and
compares every committed intent field, so a substituted repository, base,
head, or authorization cannot be copied into a successful outcome. Recovery
of an unknown prior effect continues through the ordinary drain and repeats
current trust, evidence, and authorization gates before external convergence.

**Retry durability barriers after convergence.** An immutable blob or
checkpoint already present at its final name does not prove the parent
directory entry survived an earlier fsync failure. Retry paths therefore
repeat the directory barrier before allowing durable database state to depend
on that content.

**Make the recovery resolver reconstruct the head transport.** A durable
publication intent may exist before a locally re-authored candidate head has
been uploaded. Recovery therefore requires the engine to return both the
candidate and its idempotent transport callback, then repeats that callback
after the gates and before forge convergence. Treating transport as optional
would make the intent-only crash window unrecoverable.

**Archive v1 authorizations instead of guessing their missing evidence
binding.** Migration 0012 is the first code that can create v2 authorizations,
so it moves every pre-0012 row to a legacy audit table instead of trusting a
caller-supplied JSON field as a version marker. A fresh verification can then
record v2 for the same head and profile. Old pending
intents bound to those archived decisions move to an explicit `quarantined`
outbox state: their audit rows remain, but they cannot enter the active
recovery scan or block later v2 publications. No safe migration can infer
which evidence their v1 authorization approved. Malformed authorization JSON
is archived by the same fail-closed rule rather than aborting daemon startup.

**Re-gate recovered evidence and reserve publication markers at admission.**
An outcome row's persisted eligibility bit is historical metadata, not current
authority; loading the outcome repeats the complete artifact gate against the
current recipe set before returning it. Its repository, base, head, and pull
request number are also re-bound: the pull request number is accepted only
after a read-only forge observation finds exactly one marker-and-coordinate
match for the identity. This outcome predicate accepts an open or completed PR;
only active convergence treats human closure as a conflict.
Publication-marker-shaped prose is rejected through one shared validator,
allowing callers to refuse it before they commit immutable workflow state.

## Verification Findings

Regression tests substitute individually eligible evidence metadata under an
otherwise matching authorization and confirm refusal before intent or forge
effects. Additional tests cover recovery transport after an intent-only crash,
same-call result finalization without a second gate, recovered-intent
divergence, rejection of unknown outbox status, v1 archival followed by v2
replacement with pending-intent quarantine, complete-intent finalization,
install-success/fsync-failure retries, and janitor sibling coverage. Outcome
loading after recipe revocation or persisted coordinate/PR substitution and
admission of marker-shaped prose fail before returning or committing trusted
state.

## Revisit When

Evidence snapshot encoding changes. That requires a versioned digest contract,
not an uncoordinated change to either the producer or publication gate.
