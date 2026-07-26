# Durable Run Record Contract

Work unit: #301. Scope: `daemon/internal/domain`, `daemon/internal/exec`,
`daemon/internal/store`, `daemon/migrations`, `daemon/internal/engine`,
`daemon/internal/integration`, `devlog/`.

## Decisions

**Two write-once records per invocation, not an extended attempt and not one
refinable row.** `ValidateRunTransition` makes a recorded `Attempt`
byte-immutable and `TestRunFixedBindingsAndHistory` pins that, so carrying the
new fields on the attempt would have meant relaxing a one-line structural
invariant into a field-wise rule every future field must remember to join;
worse, the export facts arrive after Start, so an attempt carrying them could
only be written once the stage finished, leaving nothing durable during the
window a crash is most likely. A single forward-only `ExecutionRecord`
upserted through both phases was rejected for the other half of the same
reason: admission facts and settlement facts would share a mutable row, and
only a field-by-field transition validator would stop the settlement writer
from rewriting the audited admission class. `ExecutionAdmission` and
`ExecutionExport` are separate `putImmutable` rows instead, so "the class is
never rewritten" needs no validator to maintain, and an admission with no
export is the in-flight state #303's adopt-or-rerun needs to see.

**Owner decisions of 2026-07-26** (taken in the planning pass for this unit,
all four as recommended): the engine wiring is in scope, so #39's acceptance is
exercised rather than only declared; the auth-store mutation lease ships
store-enforced rather than as vocabulary; `BaseRevision` is frozen while
`Workspace` stays an opaque string for #302; and the publication link reuses
#308's reservation join instead of recording a second binding.

**The capability vocabulary moved to `domain`, and `exec` aliases it.** The
persisted snapshot has to live where `store` can see it and `domain` cannot
import `exec`. A parallel enum plus a conversion was rejected: it needs a
drift-parity test to catch a seventh capability added on one side, where a type
alias has one registration point and cannot drift. `CapabilitySet` stays a
runtime map and stays out of JSON, exactly as its comment said; the persisted
form is `CapabilitySnapshot`, a sorted, deduplicated slice, so one declaration
has one byte form. That matters twice over: the golden convention bans map
fields, and a content-addressed identity over a map would depend on iteration
order.

**Identity is a content address, and its limits are the point.** The admission
recomputes its own id in `Validate`, as `CandidateAuthorization` does, so
`decode` refuses a body that was edited without recomputing the digest. This
catches partial corruption and a careless edit; it does not defend against an
actor with full write access to the database, who can recompute the digest.
That is why the re-gate is separate and is the load-bearing half: a snapshot
admitted under a weaker floor stops reading as admissible the moment policy
raises the floor, and a §5.7 waiver is checked against both the repository ID
the operator configured and the one the repository's approved trust profile
binds, so a self-consistent forged waiver still buys nothing. A test writes
exactly that forged row and asserts the refusal.

**The re-gate never consults the live backend.** §5.3 fixes capabilities at
spawn and #39 froze the snapshot so a later backend change cannot rewrite the
admitted class retroactively. Re-reading `Capabilities()` at reconstruction
would make every historical record unreadable after a legitimate conformance
re-run: an audit trail turned into a liveness check. The floor is the live
authority instead, and a mode with no configured floor admits nothing, since
an unconfigured floor is not an empty floor.

**`StartSpec` gained no `Validate`.** A complete spec only ever comes from
`StartSpecFromAdmission`, whose input has already been validated. A second
validator in `exec` would have to restate `domain`'s vocabulary rules without
being able to call its unexported `valid()` predicates, so either the
predicates get exported against the convention or the two definitions
eventually disagree. A reflection test fails if a field is added to the spec
and left unmapped, which is the drift this actually risks. The exec golden
table now tolerates a fixture with no validator.

**Admission is configured on the engine, not implicit.** The 1A.0
conversation-turn path runs no VM, seeds no workspace and pins no image, so it
has nothing truthful to record; a default environment would forge exactly the
audit fact the record exists to make trustworthy. `cmd/freesided` is therefore
left unconfigured and #237 supplies the real environment. With an admitter
configured the invariant is structural: the admission and the attempt append
share one transaction, so neither can exist without the other.

**The lease is enforced, not declared.** #308's own argument applies verbatim
with `auth_store_mutation_lease` substituted: shipping only the boolean leaves
every future ward writer to remember to serialize, with nothing in the schema
or the store to notice when one does not, and the failure mode is credential
corruption rather than a retry. One row per identity makes "at most one holder"
the primary key; a takeover bumps a fence, so a holder that stalled past its
expiry presents a fence the row has left behind and is refused on both renew
and release. #303 may reshape *what counts as a mutation*; it does not reshape
"one live holder per identity", which is what ships here.

**`RefreshStrategy`'s members are invented.** §5.4 names the field and not its
vocabulary. Two members cover the 1A distinction (the daemon drives a refresh,
or something outside it owns the store); widening is a `kind:contract` change,
recorded on the type.

**Expiry is compared in Go.** `store/trust.go` already learned that
RFC3339Nano trims trailing zeros, so the text column does not order
lexicographically. The lease keeps a text column for audit and an integer
column for comparison, and liveness is always `HeldAt(now)` with a
caller-supplied instant: a row saying "held until T" is a claim about a clock
the store does not have.

**No backfill in either migration.** No attempt executed before this contract
has a truthful capability class, credential mode, egress profile, or base
identity, and no identity declaration existed at all; synthesizing either would
forge an audit fact. This is #308's reasoning for its own no-migration
decision. A pre-0014 attempt has no admission row, which every reader reads as
unadmitted.

## Verification Findings

Every commit builds and passes the daemon suite on its own; `go vet` and
`golangci-lint run` are clean over the whole module.

A refute pass replaced `gateAdmission` with a bare `Validate` and re-ran the
re-gate tests: all four failed, so they measure the gate rather than the
record's own validator. Restoring it returned the suite to green.

**Confirmed and fixed during the work:** the first draft gave `StartSpec` a
`Validate` that called `CredentialMode.Valid()`, which does not exist and
cannot without exporting a predicate the domain deliberately keeps unexported;
the spec's validity now comes from its admission instead. The engine's attempt
identity was derived by string concatenation in three places and is now one
`attemptIDFor`, because the admission has to name the same attempt the append
creates.

**Rejected by verification, so they need not be re-raised:** the attempt
cross-check does not need repeating on read, since a run's attempts are
append-only and one that existed at write time cannot stop existing; and the
`auth_identity_id` foreign key does not need to be `NOT NULL`, because a
clean-verification stage legitimately reaches no provider (the egress profile
decides, and `Validate` binds the two).

**Accepted by decision:** the admission gate's floor is process-global,
inheriting the caveat `Options.ApprovedRecipes` already carries; a per-run
policy resolver is the real answer and has no source yet (#313). The
digest-pinned image rule now exists in both `ward/spec.go` and `domain.ImageRef`
(#312), and `engine.OperatingModeAttendedDev` remains a bare string beside
`domain.OperatingMode` (#311). No operator configuration surface sets the §5.7
waiver end to end yet (#314).

## Review Findings

The automated reviewer raised three P1s on the first pass, all real, all fixed
in place.

**A dispatch replay started under an admission that was never stored.** When
the attempt-and-admission transaction commits but the daemon stops before
`Start` or `MarkOutboxDispatched`, the next pass rebuilds an admission with a
new instant, so a new identity; `recordAttempt` exits through `errReplay`
before writing it, and dispatch then built the start spec from that unpersisted
value. The driver would have been started under an admission id no reader can
reconstruct: the exact failure the record exists to prevent, in the exact
window it exists to cover. `recordAttempt` now returns the *effective*
admission, loading the stored one on the replay branch, and fails closed when
an already-recorded attempt has no admission rather than inventing a class for
it. The fixed clock in the first round of tests is why this passed review-free:
it made the rebuilt id identical to the stored one, which a real clock never
does.

**Acceptance never consulted the admission.** `acceptAttempt` inspected and
collected from the attempt alone, so a floor raised while an attempt was in
flight was enforced on every read of the record and on nothing that mattered:
the result was accepted and the workflow advanced. Acceptance now loads the
admission first, which runs the store's reconstruction gate, and refuses
loudly. An attempt with no admission record is still accepted, because
admission is configured and work started before it was configured has no
audited class to re-gate; wedging it would punish existing runs for a contract
that did not exist when they began.

**The §5.7 waiver was not bound to the repository the run targets.** The gate
compared the record's waiver id against the operator's configured id and
stopped there, so a waiver configured for repository 42 authorized any run
whose record repeated the number, whatever repository it actually ran against.
`BaseRevision` now carries the canonical numeric `RepositoryID` beside the
name, the domain gate requires the waived id to equal it, and the store
additionally requires the repository's approved trust profile to bind that name
to that number. Both halves are needed: the record's name and number are
caller-supplied, so without the trust-profile check the pair is self-asserted.
The canonical-identity move follows #261, which established exactly this
binding for mint audits.

Nothing was declined. A refute pass then neutered each new guard in turn and
confirmed the regressions fail without it.

Round two found the half of the replay fix that was still wrong, plus one P2.

**The replay fix consulted the configuration instead of the record.** The first
fix loaded the stored admission only when *this* process was configured to
admit, so a restart that had lost the configuration between the commit and
`Start` skipped the lookup entirely and started the attempt with run and stage
ids alone, dropping the image, base, credentials, egress profile, and admission
id it was admitted with. An attempt that was admitted stays admitted, whatever
the current configuration says, so the lookup is now unconditional: a stored
record is used and re-gated whenever one exists, the unbound spec is used only
when none does, and a configured admitter with no record still fails closed.

**One class, three instances, finally fixed structurally.** Round three found
the same rule broken a third way: `admitAttempt` ran *before* replay detection,
so a restart whose backend no longer cleared the engine floor refused a pending
invocation outright, permanently, even though its stored admission was fine and
still passed the store's gate. Rounds one and two were both patches to a branch;
this one is the structure. Dispatch now consults durable state first (the run
snapshot it already holds says whether the attempt exists) and runs the live
capability gate only for an attempt that does not exist yet, so nothing about a
recorded attempt depends on what this process could admit now.

The one place configuration still decides is deliberate and is a different
question: when an attempt exists with *no* admission record, there is no
durable decision to honour, so an engine that admits nothing starts it as it
always did while a configured one fails closed rather than starting unaudited
work. That is about the absence of a record, never about overriding one.

**An empty capability floor was rejected as a misconfiguration.** The
persistence boundary distinguishes a missing floor (unconfigured, admits
nothing) from a present-but-empty one (configured, no minimum), and
`WithAdmission` is itself the presence signal, so refusing `len(floor) == 0`
made the engine unable to express a policy state the store accepts.

**A lease renewal could shorten its own window.** `Validate` only required the
expiry to follow the acquisition instant, so a delayed or reordered renewal
carrying an earlier expiry was accepted: the caller was told it held a lease
that had already lapsed, or whose window another holder could take sooner than
it expected. Renewal now refuses any expiry that is not in the future or that
moves the current one backward, with an exact replay still idempotent. The
class sweep found the same hole on acquisition, where a window ending before
`now` was accepted; both are refused with `ErrLeaseWindowRegresses`.

Round four found four more, two of them fresh consequences of the fixes above.

**A refused reconstruction read as an absent record.** The acceptance path
treated `ErrNotFound` as "this attempt predates admission, carry on", but the
gate can raise a not-found of its own: a waived admission whose approved trust
profile is gone. That refusal was therefore read as a legacy attempt and its
output accepted, defeating the re-gate exactly where it fails closed. Absence
is now a boolean rather than an error class:
`LookupExecutionAdmission` reports presence separately and every error is a
failure, both engine call sites use it, and the untrusted-waiver refusal
carries its own sentinel so it can never be mistaken for a missing row again.
The general lesson is the same one the configuration class taught: a caller
should not have to classify an error to learn a fact the API can state.

**An admission could name digests the run is not bound to.** The attempt
cross-check verified run, stage, and attempt but not the spec and policy
digests, so a caller supplying different ones was persisted and the driver was
later started from them. Both are immutable run bindings and are right there in
the transaction, so they are now compared against the run rather than taken on
the caller's word.

**The auth-identity column was never cross-checked.** Reconstruction scanned
neither the nullable `auth_identity_id` column nor compared it with the body,
so a tampered or imported row could name one identity in the column the foreign
key constrains and another in the body the daemon reads. The column is scanned
and compared now, like every other extracted field.

**Auth-identity revisions could move backward.** `recorded_at` is documented as
ordering revisions, but the upsert ignored it, so a delayed older measurement
landing after a newer one reinstated a superseded `MaxParallelExecutions` —
raising concurrency past the latest safe result, which is the direction that
matters. A strictly older revision is now `ErrStaleWrite`; an identical instant
still converges.

Round five found four more, and the shape of them says the boundary was
verifying the wrong half of what it holds.

**Only a waived admission was anchored to a trust profile.** §5.7 lists a trust
profile among the conformance an unattended run requires, and the gate consulted
one only when the waiver was present, so an ordinary unattended admission could
name a repository with no approved profile, or a self-asserted numeric id, and
pass both recording and reconstruction. The profile check now covers every
unattended admission as well as every waived one; `attended_dev` is untouched,
since §5.7 admits the weaker class there.

**The input digest was still taken on the caller's word.** The run's spec and
policy digests were compared to the run after round four, but the digest naming
what the stage ran *against* was not, so a writer with correct run, stage,
attempt, spec, and policy bindings could still substitute it and the driver
would be started from the substitute. The agent invocation record is the
durable statement of what a turn was given (§5.14), so the digest is recomputed
from it. An invocation with no such record is left alone: not every stage kind
binds inputs that way, and there is nothing to compare against for one that
does not, so this closes substitution wherever a binding exists, which today is
every dispatched invocation.

The general rule these two share, and the one worth carrying into #237: any
field of the record that has a durable counterpart is verified against that
counterpart, never accepted because the caller is in-process. What remains
caller-supplied is exactly what has no counterpart yet (backend name, image
ref, base ref and SHA, workspace).

**An export could predate its own admission.** Nothing compared the two
timestamps, so a record could say the handoff happened before the attempt was
admitted. That is not clock skew to tolerate; it is an audit trail that reads
backwards, and it is refused.

**An identity revision could be replaced at an equal timestamp.** The staleness
check used `Before`, so two declarations sharing a `recorded_at` — a reused or
coarse stamp — let a divergent body overwrite the stored one with no ordering
evidence at all, which is the same superseded-limit restoration the check
existed to stop. Equality now admits only a byte-identical replay.

Round six found two, and one of them is only partly fixable inside this unit's
scope.

**The acceptance re-gate is taken outside the transaction it protects.** The
engine checked the admission, then ran `Inspect` and `Collect`, then asked
signet to accept; a trust profile retired during that I/O left the verdict
describing the past. The check now runs immediately before the accept call,
which narrows the window from "across two driver round-trips" to "between two
adjacent store operations", and that is as far as this unit reaches: the
accepting transaction belongs to `signet.AcceptAgentCompletion`, and closing
the race properly needs a pre-commit hook on that API. `daemon/internal/signet`
is another lane's territory, so it is filed as #316 rather than taken in
passing. Stated plainly: this remains a race, and the residual window is one
accepted conversation turn under a profile retired in the last few
milliseconds; publication has its own trust checks downstream.

**A delayed acquisition could reach back past a released lease.** `HeldAt` is
false for a released lease at *any* instant, so an acquisition carrying an
older `now` took over unconditionally and installed its own, possibly still
future-dated, window over a generation that had already come and gone —
blocking the holders that follow. An acquisition instant that predates the
current generation's own acquisition or release is now refused.

Round seven found two, both P2, and both are consequences of earlier fixes
rather than new ground.

**A release could land outside the window it ends.** The holder and fence
matched, so a delayed release stamped past the expiry was recorded — and the
round-six acquisition rule then made that stamp harmful, since an acquisition
whose instant precedes the current generation's release is refused. An
accidental far-future release would have blocked every takeover until it
passed. A release must now fall inside the live window, which is
`HeldAt(releasedAt)` and nothing more.

**The export's audit facts rested on the body alone.** `EvidenceManifestDigest`,
`CommitPlanPresent`, and `ObservedBaseSHA` had no extracted columns, and unlike
the admission the export carries no content address, so editing the body
changed what the record says about the handoff with nothing to catch it. All
three are extracted and cross-checked now. The alternative was to
content-address the export as well; the columns are the cheaper answer and the
one the store's own convention already uses everywhere else, and neither
defends against an actor who rewrites row and body together — that limit is the
same one recorded above for the admission.

Round eight found three, and between them they finish two sweeps rather than
opening new ground.

**Enum validity was treated as authorization.** §5.7 requires an approved
credential mode of an unattended run, and the gate never consulted one: a
record naming the Phase 2 `api_key_isolated`, or the trusted-inputs-only
`local_trusted`, passed because the token was spelled correctly.
`AdmissionPolicy` now carries the approved set and an unattended admission is
held to it, with an empty set approving nothing. `attended_dev` is deliberately
exempt, on the same reasoning as the trust profile: the plan admits the weaker
class there, and the dev loop is where an unapproved containment gets exercised
on purpose.

**The identity's own declaration was half-authenticated.** `GetAuthIdentity`
compared the provider and the lease flag, both extracted columns, and took the
rest of the body on trust, so a partially edited row could return a larger
`MaxParallelExecutions` than anyone measured. Every declared field is an
extracted column now. With the export (round seven) that completes the sweep:
across all four record types, every persisted field is either covered by a
content address or extracted and compared.

**A lease could be released after expiry through reconstruction.** Round seven
stopped the release *path* from recording an out-of-window instant; validation
still accepted one, so a malformed or imported row could carry a far-future
release and recreate the same takeover blockade from the read side. `Validate`
now requires a release to fall inside its window. That completes the other
sweep: every impossible timestamp ordering these records can express is
rejected at validation, not only at the write that would have created it.

Round nine found two, both P2, both about a value being the wrong thing rather
than a gate being missing.

**The input digest addressed the record, not the inputs.** It marshalled the
whole `AgentInvocation`, id included, so re-running identical inputs under a
new daemon-assigned id produced a different digest — the opposite of what a
content address is for, and it would have broken exactly the cross-run audit
comparison the field exists to enable. It now hashes a versioned canonical
shape carrying only the bound inputs. Input order is preserved rather than
sorted: that order is how the invocation recorded its binding, and turning the
list into a set is a semantic change no finding asked for.

**The admission configuration aliased the caller's values.** `WithAdmission`
kept the floor slice and the environment's two pointers by reference, so a
composition reusing them could weaken the capability gate or retarget the
credential and waiver bindings after `engine.New` returned. Both are detached
when the option is applied. This is the same discipline the domain
constructors already follow, applied one layer out at the boundary where
configuration becomes live.

Round ten found the last real gap in the trust binding, plus a mode rule that
should have been there from the start.

**The repository id cannot stand in for the profile revision.** Activating a
revised trust profile for the same repository keeps its numeric id, and the id
was the only profile field the gate compared, so a run admitted under a retired
revision kept passing after the operator replaced it — the revision bound the
next run and not the work already in flight, which is the opposite of what
approving a revision means. `ExecutionAdmission` now records the exact
`TrustProfileDigest` it was admitted under (required precisely where a profile
is required, forbidden elsewhere), and the store compares it with the current
activation. The engine reads the active digest at admission rather than taking
it from configuration, because activations happen at runtime and a configured
value would name whatever was current at daemon start; the store re-checks on
write, so a revision landing in between fails closed instead of being recorded
stale.

**A waiver could be claimed in a mode not eligible for it.** §5.7's exception
is about unattended backup health, and nothing stopped an `attended_dev` record
from carrying one — an evidence-grade claim on an operator exception the mode
cannot use. `Validate` now refuses it, alongside the profile-digest rule, in
one `validateTrustBinding` that says which trust claims belong to which mode.

Round eleven found one P2, at the far edge of the lease window.

**The window had no lower bound.** `HeldAt` asked only whether the instant
preceded expiry, so a holder whose clock had regressed named a moment before
its own generation began and still read as holding it — enough to renew a
generation that did not exist at the instant supplied, with no takeover and no
fence bump. A lease is held throughout `[AcquiredAt, ExpiresAt)` and nowhere
else now. The acquisition and release paths already refused pre-generation
instants explicitly; this closes the same hole in the predicate they all share,
which is where it belonged.

Round twelve found two, and the P1 is the one finding in this whole exchange
that the plan text answers outright.

**Unattended admission could present no backup authorization at all.** §5.7
gates unattended running on backup health, and its Phase 1A.2 exception says in
so many words that "admission without the waiver fails closed as before". The
gate enforced the waiver's *contents* whenever one was present and never asked
for one, so a run with a trusted profile, an approved credential mode, and the
full capability class was admitted on no backup evidence whatever. Unattended
now requires the waiver, which is the only backup authorization this build can
present: the encrypted checkpoint that would supply the ordinary path is #305,
and the build that carries it must reject the waiver outright, which is how
§5.7 retires the exception. Two honest limits, recorded at the gate rather than
implied away: the waiver covers only the encryption dimension, and checkpoint
currency, artifact closure, and restore-test age have no source in the tree to
evaluate against.

**The revision instant was the last unauthenticated field.** `recorded_at` is
what `requireForwardRevision` compares against, and it lived only as a column,
so moving it backward would let a superseded declaration overwrite the current
one and moving it forward would block legitimate revisions — the very attack
the ordering check exists to stop, reachable by editing the thing the check
trusts. The identity is now persisted as a package-private record carrying the
instant inside the validated body, cross-checked against the column like every
other field. That is the eighth round in a row where the answer was "authenticate
what the check trusts", and it is now true of every field of every record here.

Round thirteen found two, and one of them is the first remedy in this exchange
worth declining.

**An admission naming an unreadable identity still reconstructed.** The foreign
key proves the identity row exists; it says nothing about whether the
declaration in it decodes. A row that kept its key and columns but held a
malformed body therefore let the admission read succeed, and a replay would
dispatch under credential state whose concurrency, refresh, and snapshot
declaration had failed reconstruction. The gate now reads the identity through
its own reconstruction, so the admission fails closed with the identity's own
error.

**Declined: binding the lease holder to a recorded agent invocation.** The
observation is fair — a holder naming no real execution cannot be traced, and
occupies the identity until expiry. The remedy is what does not fit. §5.4
serializes "refresh, login state, configuration writes, and store replacement"
per identity, and the last two are daemon or operator actions with no agent
turn behind them; requiring an `agent_invocations` row would grant the lease
only to the one case that has one, and #303 (which consumes this vocabulary for
exactly a credential refresh) has not landed to say otherwise. This is the same
judgement as the input digest two rounds earlier: verify against a durable
counterpart where one must exist, and do not invent the requirement where it
need not. The limit is documented at the field rather than left implicit, and
if #303 establishes that every holder is a recorded invocation, the foreign key
is a one-line migration then.

Round fourteen split the same way: one half belongs here, the other does not
exist to be built yet.

**Every waived admission now surfaces its degraded posture.** §5.7 asks for two
things of a waived admission and I had implemented one: the waiver was recorded
in the audit record but nothing told an operator that unattended work was
running on the temporary exception. A `system_health` item is raised in the
admitting transaction, keyed deterministically by invocation so a replayed
dispatch converges on one notice rather than accumulating them. §5.7's
supersession rule — the validated waiver configuration overriding the item's
blocking state, so the notice is visible without blocking the admissions the
waiver exists to permit — is §4 attention semantics that signet owns; this
raises the visible notice and does not model the blocking rule.

**Filed, not built: the non-waived backup-health dimensions (#317).** §5.7
waives only encryption; checkpoint currency, artifact closure, and restore-test
age still gate. They are not enforced, because no queryable backup-health
signal exists — making one is #305's stated acceptance, and this unit's
non-goals exclude the encrypted checkpoint. The two available alternatives were
both worse than filing: gate on a signal that does not exist and every
unattended admission fails, contradicting the owner's decision to schedule #305
after #237; or invent #305's health semantics inside a contract unit that
excludes them, and have #305 inherit a definition it never chose. So the gap is
named at the gate, in the note, and in an issue with its own acceptance, rather
than papered over with a comment that reads like completeness. An unattended
run can currently be admitted against a stale local checkpoint; that sentence
belongs in the PR, not only here.

Round fifteen caught two places where this unit's claims outran what it
enforces. Both are worth recording as claims, not just as fixes.

**The chain-walk test was tautological.** It set a local `intentSourceHead`
literal to the same value it had given the export fixture and then asserted
they matched, so it proved nothing about anything. Worse, it was the evidence
behind a stronger claim than the code supports: nothing outside these APIs
writes an `ExecutionExport` yet, so no production path holds a publication
intent's `SourceHeadSHA` against the recorded head. The record makes that join
*expressible*; it does not enforce it. The test now asserts the durable half it
can (run → attempt → admission → export, with the export's bindings checked
against the admission it names), says at the top what it deliberately does not
cover, and the enforcement is #318. Simulated coverage is worse than a stated
gap: the gap invites the work, the simulation retires it.

**An offered action nothing could honour.** The waived-posture notice offered
`stop_unattended` because signet's policy allows it for the type — but the
admission environment is fixed at engine construction, so an operator would see
the stop succeed and the next reconciliation would admit unattended work again.
The notice now offers `acknowledge` alone, and #319 carries the durable mode
transition that makes the other action honest. Offering a control the system
cannot honour is a lie told through the UI, which is worse than an absent
control.

Round sixteen produced two more real gaps and no in-scope fixes, which is the
signal this unit has reached its edge.

**A capability snapshot is not proof the backend earned it (#320).** The gate
compares the recorded class against the floor, never against what the named
backend was proven to declare, so an over-claiming writer is not caught. There
is nothing to compare against: ward proves conformance at runtime and nothing
persists the result. The re-gate keeps a snapshot honest about policy drift,
which is what #39 and #52 asked of it; it was never proof of provenance, and
the comment at the gate now says so.

**No blocking-health check (#321).** §5.7 lists "no blocking `system_health`"
in the unattended conformance set, and the gate never looks at attention items.
The rule is not "any open item blocks": §5.7 has the validated waiver
configuration supersede the blocking state of the very notice this unit raises,
and that §4 supersession does not exist in the tree — the only supersession
today is item versioning. Encoding it here as an id-pattern exemption would
make a §4 rule into a string convention that breaks on the second superseding
condition.

**Where this unit stops.** Rounds fourteen through sixteen were all the same
exercise: the reviewer walking §5.7's unattended conformance list against a
contract unit that owns the record and the gate but not the signals. Backup
health is #305's to produce (#317 to enforce), backend conformance is ward's to
persist (#320), blocking-health supersession is §4's to define (#321), and the
publication head comparison needs a producer that does not exist (#318). Each
finding is true; none is closable here without inventing another unit's
vocabulary, which is the over-reach the 2026-07-24 chain amendment decomposed
out of #236. The gates now name what they do not enforce, the PR carries one
consolidated statement of it, and the issues carry the acceptance.

## Revisit When

`Workspace` becomes structured. #302 owns the ward-side shape; promoting the
opaque string is golden churn plus a `StartSpecFromAdmission` field, not a
contract break, and the frozen `BaseRevision` half (repo, canonical repository
id, ref, resolved commit) is what the publication chain binds to either way.

The admission encoding version changes. Every recorded admission's identity is
derived from `freeside.execution.admission/v1`, and `Validate` recomputes it,
so a bump invalidates every stored row at once. Unlike #308's reservation, the
rows are audit history rather than in-flight state, so the migration question
is whether to re-derive or to archive: decide it with the bump, never after.

A second stage kind needs its own environment. `AdmissionEnvironment` is one
per engine today because one stage kind dispatches through it; a run whose
stages differ in credential mode or egress profile needs the environment to
come from resolved policy per stage, not from the composition.
