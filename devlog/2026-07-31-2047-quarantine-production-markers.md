# Quarantine an Unreadable Production Marker, Never the Daemon

Work unit: #424. Scope: `daemon/internal/engine`, `daemon/internal/integration`, and `devlog/`.

## Decision

Chose a per-run, daemon-local quarantine (classify the failure, hold that one
run out of the production lane, record a durable notice, continue the pass)
over three alternatives: failing loud as before, marking the outbox row
`quarantined` in the store, and raising a blocking `system_health` item.

The failure this closes: `ownsProductionRun` re-authenticates a marker for
every stored run on every pass, and `Engine.Run` ends the daemon's loop on any
`Reconcile` error, so one unreconstructable row killed the daemon on every
start. That is exactly the legacy-row failure mode #418 removed, arriving
through a different door: a downgrade past a marker version a newer daemon
wrote. Failing loud is the right answer for broken *owned* state that a
human must repair; it is the wrong answer for state a later binary reads fine,
because the daemon cannot survive long enough to be upgraded in place.

**Quarantine is this daemon's classification, not a stored status.** The store
already has an `outbox` `quarantined` status (`daemon/migrations/0012_*.sql`),
which was the obvious mechanism and is wrong here. Its semantics are "preserve
for audit, remove from active recovery" — permanent. The dominant cause of an
unreadable marker is a *reversible* one: this binary is older than the row. A
durable status change would strand a marker that is authentic under the daemon
that wrote it, converting a temporary downgrade into permanent data loss. The
pending row is therefore left untouched, and a re-upgraded daemon picks the run
back up with no repair step.

**The notice is `execution_failure`, run-scoped, not `system_health`.**
`system_health` is the better fit for the *nature* of the fault (a mechanical
daemon-level diagnostic), and it was rejected on effect: any open
`system_health` item without a supersession blocks all unattended admission
(`daemon/internal/store/unattended.go:187-208`). One unreadable row would then
hold every unrelated run's dispatch — a quieter wedge, but still a wedge, and
the acceptance criterion is explicitly that unrelated runs keep reconciling.
A run-scoped `execution_failure` item confines the blast radius to its run.

**Stop is the only action offered.** Retry re-enters the same failed
reconstruction; `retry_with_capabilities` has no honoring machinery;
`discuss` rides a conversation channel a production run does not have; and
`acknowledge` is not in signet's allowed set for this type
(`daemon/internal/signet/policy.go:24-26`). Stop is the boundary's concluding
action and the one an operator can actually take. Note that the sibling
`productionFailureItem` (#418) offers `acknowledge` for the same type, which
signet's policy would reject; it is written through the store rather than the
service, so the mismatch is latent. Left alone: it is pre-existing and outside
this unit's scope.

**The reason is a fixed daemon-authored string.** The decode error quotes the
stored version, kind, and identities, which are the untrusted bytes at this
boundary. It stays in the daemon's error path; only the classified reason and
the run subject reach the operator-facing item, following the ward/finding rule
("never echo the attacker-controlled path in the reason").

**Attribution is by key, never by payload.** The dispatch loop knows only the
outbox row, whose payload is precisely what failed to reconstruct, so the run
is derived from the row's idempotency key (`productionRunIDFromInvocationID`).
A row whose key this lane could not have filed, or whose run the store does not
have, names no run to quarantine, and stays a loud failure: there is nothing to
hold out of the lane and no subject to file a notice under.

**A notice is retired on every path where its hold ends**, not only the
obvious one: the marker or task reconstructing again, and either row being
removed, which is the other repair an operator can make. A high-priority item
still claiming a run is held, while that run publishes, is worse than no item:
it trains an operator to ignore the class. The retirement supersedes rather
than resolves (nothing was decided; the condition stopped holding), following
`supersedeBlockedHold`.

**The notice is retired when the marker reads again.** An open, high-priority
item asserting a run is held, while that run publishes after the upgrade, is
worse than no item: it trains an operator to ignore the class. The ownership
scan and the publication lane both supersede it (not resolve: nothing was
decided, the condition stopped holding), following `supersedeBlockedHold`.

## Scope of the change

Every marker reader inside the reconcile loop takes the same route, because
the same downgrade wedges each of them: the dispatch loop (`invocation.go`),
the ownership scan (`ownsProductionRun`), and the publication lane. Fixing
only the scan named in the issue would have left the daemon dying on the
others.

The publication lane needed two changes, both from the refute-first pass.
`reconcileTask` never reads the marker (it re-gates its authority from the
store), so a first draft's hook there was unreachable *and* the lane would
have published a run the other two paths had just quarantined — a real PR for
a run whose notice claims it is held. The lane now authenticates the marker
per task. Its sibling row, the publication *task*, wedges identically under a
downgrade (`validate()` hard-requires the current task version), so it is
quarantined on the same route, under its own item id: the marker's notice is
retired when the marker reads again, and a task hold must not ride that
identity.

## Residual gap: startup still dies on a downgrade

**This unit does not make a downgraded daemon start.** The checkpoint
artifact-closure scan (`daemon/internal/store/local_backup.go:487-497`) runs
every registered outbox payload extractor over the live database, returns an
extractor error verbatim, and `newDaemon` fails on it — before `Engine.Run`
exists. `ProductionInvocationBackupPayloadDigests` is one of those extractors,
so an unreadable marker fails startup, the initial doctor pass, and every
scheduled checkpoint. The orphan-adoption pass (`cmd/freesided/main.go:494`)
reads the marker too and also stops startup by design.

That fix is not a marker classification. The scan's digest set is bound into
the checkpoint's `ArtifactManifestDigest`, so tolerating a row means deciding
what an incomplete closure does to checkpoint production and restore
verification, and the same intolerance applies to every registered kind and to
any unregistered one. It is its own unit, filed as #430, and it fails
*closed* today (the daemon refuses to run), which is why this unit ships
without it rather than widening into store-level backup policy.

## Verification

Refute-first pass (mandatory: returned-object trust boundary), two independent
lenses: one on trust-boundary correctness, one enumerating surviving wedge
paths.

Codex review, round 1 (three findings). Accepted in part: `Stop` is offered
with no machinery that could honour "keep this run stopped", so both reasons
now state that the hold ends by itself when the row reconstructs. Declined the
mechanism Codex asked for, gating dispatch on the notice's terminal status:
`Stop` concludes an item and does nothing else for every item in this lane
(`signet/service.go:296-320`), and no run-terminating machinery exists in
Phase 1A, so keying resumption off one notice's status would invent an
authority bit no other item has, out of a decoded item's own state. Accepted
in full: both retirement gaps (a repaired task row, a removed marker row),
which the enumeration above now covers, plus the acceptance scan's own reader
of the task row, which the same downgrade wedges.

Codex review, round 2 (three findings, all accepted). A concluded notice is
history, not a record of the current hold, so a recurring quarantine now opens
its own occurrence: a terminal `item_status` is final, and reusing the
identity left a re-quarantined run held behind a superseded item. The two
lifecycle writes also converge on a lost race instead of erroring, which
matters more here than usual: an error from either would end the reconcile
loop this unit exists to keep running. A lost create race is accepted only
after re-reading and checking the stored notice really is this run's.

Codex review, round 3 (two findings, both accepted, final push): the task
notice is now retired by the acceptance readers as well, since the pending
scan that also retires it returns early under a hold-only publication lane;
and the ordinary replay path re-checks the stored open notice instead of
trusting the identity it was found under, rewriting its reason when the hold
changes class.

Codex review, round 4 (three findings, all accepted; one blocking). The
blocking one was self-inflicted by round 2's occurrence identity: a run id is
validated only as non-empty, so appending the occurrence made run "foo"'s
second notice collide with run "foo-2"'s first, and the mismatched subject
that produces is an error on the path whose whole purpose is to keep the
reconcile loop running. The occurrence now sits between the class prefix and
the run id, which is injective over the pair. The cap on that history is gone
with it: bounding the walk forced a choice between failing (the same wedge)
and holding a run behind no current notice, while the history itself grows
only when an operator repairs the run. The task notice is also retired when
its row is absent, in both acceptance readers, with the ambiguous three-lookup
not-found disambiguated so a missing admission record is never read as a
repair.

Codex review, round 5 (one finding, accepted in part, final push): the release
path now checks that the item under its predictable id is this hold's own
notice, by row class as well as subject, before concluding it. It skips rather
than errors on anything else, which is the deliberate asymmetry with the
recorder: a divergent item there means the hold cannot be recorded and the run
would be held silently, which must surface, while here it means someone else's
item sits at this id, and failing to retire a notice this lane does not own is
harmless where erroring would end the loop. Declined the project-id half of
the ask: the subject already binds the item to the run, a run's project is
immutable, and the recorder enforces the pairing at creation, so the check
would only differ for a writer that forged the id, the subject, and one of the
three reasons, which nothing but this lane writes.

Codex review, round 6 (one finding, accepted): the accept path compares the
whole reconstructed notice against the canonical one, normalising only the
lifecycle fields a decision or delivery advances, and repairs a drifted row
rather than accepting it. Three rounds had each named a different subset of
fields to authenticate, which is the shape of the problem: a subset check can
only ever authenticate the fields someone thought to list, so the enumeration
was replaced with a whole-shape comparison that cannot omit one.

Codex review, round 7 (one finding, accepted): the marker version is now
classified before the strict decode. A newer version normally *adds* a field,
and `DisallowUnknownFields` rejected the payload before the version was ever
read, so the downgrade this lane exists to survive was reported as a malformed
marker: two reasons that cannot discriminate in the common case are one
reason. The classifier is not an acceptance path, and every payload it passes
over still meets the unchanged strict decode and its gates, so it can only
decide which refusal an operator reads.

Codex review, round 8 (one finding, accepted; exchange closed here). The
newer-daemon diagnosis now requires a version in this lane's own namespace
that orders after the release this binary implements, so a corrupt or
foreign version string stays an unreadable marker instead of being told an
upgrade repairs it. The imprecision predates this unit (the decoder's
`default` branch classified every unequal version the same way); the
two-reason notice is what made it operator-visible. The namespace, the
release number, and the released version string are pinned to one another by
a test, since the classifier now derives the diagnosis from the first two.

This closed the exchange under the owner's stop condition: eight rounds, and
the first whose findings were all non-blocking hardening against a corrupt
row rather than a failure the feature is for.

Confirmed and fixed: the publication lane's unreachable hook and the
publish-a-quarantined-run regression behind it; the unreadable publication-task
wedge; the never-retired notice; a quarantine store fault that dropped the
authentication failure that caused it.

Confirmed and filed as #430, not fixed here: the startup checkpoint-closure
wedge above.

Rejected by verification, so they are not re-raised: no decoded marker field
reaches an item, a lookup, or a decision (both live callers pass store-read
`run.ID`/`run.ProjectID`, and attribution is key-derived); no released version
decodes where it did not before (the only `decodeProductionRequest` change is
a `%w` on the unsupported-version error); neither sentinel can be produced
outside marker authentication, so no unrelated error is swallowed as a
quarantine; the notice's reasons are two fixed constants and its identity
carries only a store-resident run id; the deterministic item id plus
single-transaction `PutItem` make a swallowed `ErrStaleWrite` a concurrent
identical create.

Accepted by decision: a production-kind row whose key this lane could not have
filed, or whose run the store does not have, still fails loud. There is no run
to hold out of the lane and no subject to file a notice under, and the
codebase's rule for broken owned state is to fail loudly.

Mutation check: with the quarantine branches disabled in place, all five new
and revised tests fail (engine unit tests and the end-to-end downgrade
regression); with them restored, the suite is green. The regression seeds a
future-version marker beside a healthy production run and asserts the healthy
run still executes, verifies, and publishes in the same pass.

Released marker versions keep their exact canonical decode: classification
wraps `decodeProductionRequest`'s existing errors and adds no tolerance.

## Revisit when

Three consecutive review rounds landed on the same class: the notice's
lifecycle is maintained by hand at each row reader (create here, retire there,
re-check at that read), so every new reader is a new place to forget it. It
is correct at each of the current readers and covered by tests, but the shape
invites the omission. Revisit when a fourth reader of a production row
appears, or when the lifecycle needs a third state (round 4 added a fourth
and fifth retirement path, and its blocking finding was a defect in the
identity round 2 introduced, which is the same signal one level up): at that point the notice
should be converged once per pass from the authoritative condition, with the
readers only consulting whether the run is held.


A marker version is actually retired, or the store gains a real forward-version
policy. Quarantine assumes "this binary is too old" is temporary. A deliberately
withdrawn version would want the durable `quarantined` outbox status this note
rejects, plus a migration, rather than a per-pass classification.
