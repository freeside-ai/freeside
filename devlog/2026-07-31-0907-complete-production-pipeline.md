# Complete the Production Pipeline

Work unit: #411. Scope: `daemon/cmd/freesided`, `daemon/internal/engine`,
`daemon/internal/exec/claude`, `daemon/internal/integration`,
`daemon/internal/projectimage`, `daemon/internal/ward`,
`scripts/run-real-work.sh`, and this note.

## Decision

**Chose a dedicated durable production-publication workflow over extending the
attended fake workflow.** A completed Claude result now queues exact replay,
clean verification, and publication work. The production terminal record is
written after either `PublishExecutionAfterGateAndFinalize` has converged the
PR and the ready item exists, or a definitive trust refusal has durably
surfaced `publish_blocked`; a pending external-effect intent keeps the task
runnable instead. The publication invocation is reserved in the original
submit transaction and promoted only by the execution-bound API. The ordinary
`Candidate` API remains confined to the attended fake lane.

**Chose an atomic content-addressed replay task over retaining the released
directory or widening the shared execution contract.** Before
`ExecutionExport` can commit, Claude copies both manifests, every repository
and evidence blob, and the optional commit plan into the durable blob store.
The command-layer adapter then commits the export, publication reservation,
and role-bearing publication task in one SQLite transaction. Production
reconstructs a fresh exact-base checkout from that task, reruns the hostile
importer with the immutable admission-bound policy, and requires the resulting
head to equal the write-once export. Keeping the random handoff directory was
rejected because the driver deliberately removes it; adding replay fields to
the shared domain/store export record was rejected because the existing
outbox task and backup extractor can own the same closure without a new
cross-component schema contract.

**Chose immutable execution replay followed by current publication gates.** A
restart reconstructs import options from the recorded admission, resolved
policy, and admission-bound trust-profile revision. Mutable daemon configuration
can stop new or not-yet-accepted work but cannot retarget terminal replay. The
publisher still re-gates verifier artifacts under the configured approved
recipe set, requires the current trust profile and fresh workflow audit, and
authenticates the unattended admission/export/run/head join when it settles
the reservation. This follows #318's terminal-authority decision while keeping
current publication authority current.

**Chose a ward-owned project-image room over `ProcRoom` or a second container
lifecycle.** Each preparation or recipe command runs in the admitted
digest-pinned image with `--network none`, the fresh verifier workspace as its
only mount, and only fixed non-credential environment entries. The room uses
ward's unpredictable ownership label and fresh evidence rules to reap its
container after success, failure, or cancellation. `ProcRoom` was rejected
because it cannot deny network or contain escaping descendants; raw `--rm`
alone was rejected because interruption can leave a container whose ownership
and deletion were never proved.

**Chose the admitted project image as the durable external-recipe source.**
Onboarding already copies the exact approved recipe bytes into the
digest-pinned image and records their digest in `ProjectImage`; a production
repository is therefore not required to carry `.freeside/verify.json`.
Before verification, the room now extracts the fixed embedded file in a
networkless container with no workspace mount or credentials, caps the output,
and checks its digest against the durable image binding. The engine repeats
that digest check before any recipe command can run. Rereading the original
operator path at daemon startup was rejected because that mutable ambient file
is an onboarding input, not durable authority; falling back to the base or
candidate tree was rejected because either would replace the approved recipe
after the image was admitted.

**Chose a durable verification checkpoint over rerunning successful recipes
after every later crash.** The checkpoint records the reconstructed import
account, verifier artifacts, project image, and candidate authorization in one
store transaction. Every retry still reimports and compares the exact account,
reloads the immutable source bindings, verifies each artifact blob and stored
authorization, and then enters the publisher's current gates. This prevents a
PR-creation or ready-write crash from rerunning arbitrary project tests while
also preventing the checkpoint from becoming unverified authority.

**Kept production effects unattended-only while always composing durable task
recognition.** `attended_dev` disables automatic verification and publication,
so an attended daemon leaves a submitted production intent durable and pending.
It still composes the publication boundary in hold-only mode, including when no
recipe is currently approved, because an earlier unattended process may already
have atomically queued a task. That recognizer prevents terminal intake from
mistaking the durable task for a missing atomic checkpoint while performing no
verification or forge effect. A completion admitted by an older build under
attended mode is authenticated and recorded as terminal history without becoming
a publication task. Omitting the workflow entirely was rejected because a mode
downgrade then turned a valid queued task into a daemon-stopping invariant error.

**Removed the production rerun action until it has a durable consumer.** A
blocked production publication is terminal for this Phase 1A task: its outbox
row is retained but dispatched, while signet's generic
`rerun_trust_evaluation` acceptance only resolved the attention item and
neither requeued nor superseded the task. Leaving that control visible would
strand the run while claiming a retry. Keeping the task pending was rejected
because reconciliation would repeatedly re-enter verification and trust
boundaries without an accepted operator command. A later review-loop unit must
atomically consume the command and enqueue a fresh, command-keyed reevaluation
before the action can be restored.

## Refute-First Verification

Restart tests interrupt after verification, after publication, after the ready
write, and after terminal acceptance. Separate effect-boundary tests fail a
transport push and make PR creation succeed remotely while returning an error;
both retries converge to one branch and one PR. Negative tests make failed
verification, an export-head mismatch, or replay policy substitution reach no
external effect. The room tests prove fixed networkless argv, preparation
ordering, bounded output, ownership-gated cleanup, cancellation cleanup, and
refusal to delete a foreign runtime identity. Replay tests remove the released
directory and apply current policy drift before loading the immutable export
account.

The independent refute pass confirmed two gaps. First, trusting the driver's
returned `ImportOptions` would let a compromised replay loader weaken the path
policy while retaining the recorded head. Production now independently derives
the complete importer policy from the durable resolved policy and bound trust
profile and requires exact option equality. Second, a new run could converge on
an already-promoted publisher intent at its derived invocation key. Submission
now checks availability before claiming, in the same transaction that creates
the run, and a regression proves refusal leaves no run or dispatch row.

The refute pass also questioned why the optional commit-plan content address is
private driver replay metadata rather than part of `ExecutionExport`. This is
rejected as a candidate-integrity gap for this unit: the write-once export binds
the exact manifest and resulting Git head, while production reruns the importer
and requires that same head. Any plan change that alters commit grouping,
messages, parentage, metadata, or tree changes the head and is refused; a raw
serialization change with identical parsed semantics produces the same exact
candidate history and checkout. The task additionally snapshots the private
plan digest before verification, so later replay mutation is refused. A shared
first-class replay contract could still remove the private metadata, but it is
not required to authenticate the exact candidate.

Automated review found two P1 integration failures and both were confirmed.
The default attended composition could start a production run and route its
completion into an execution-bound publisher that requires unattended
authority, terminating the daemon. The publish-blocked item also advertised a
rerun action after its only recovery task had been dispatched. Regressions now
prove attended submissions remain pending without an admission or attempt, and
that a verification-blocked publication retains a dispatched audit row while
offering only inspect and stop and producing no effect on replay.

A later automated review found two more convergence gaps and both were
confirmed. Removing a project-image recipe from the current approved set was
misclassified as corrupted durable authority, which terminated reconciliation
instead of surfacing `publish_blocked`. Also, blocked publication dispatched
its task without recording the producing completion, so each poll re-enqueued
the same task through a client-visible write. Current recipe revocation now
blocks before an unapproved verifier can run and omits those untrusted
artifacts from the attention item; every definitive block records the
authenticated completed terminal before dispatching the task. Regressions
cover revocation before verification and after a durable checkpoint, a live
publication intent that remains recoverable rather than being hidden behind a
terminal task, zero forge effects, and a replay that leaves the server revision
unchanged.

The authorized real run against `freeasinbird/gh-imgup` issue #83 supplied the
approved external recipe and its admitted project image. Run
`run-545ee8551d91f2d42a0eee8157679d96117f4336995198fc570c6d2594305a62`
completed Claude inference and was correctly contained when
`npm test` left ignored `dist/` output outside the declared `src/**` scope; the
policy was not widened, and the revised task requires `npm run clean`. Run
`run-b76a45bf9b0d905e23d7040c857bf0770d271ee1d6fdce00701effc92d96a429`
then completed inference and containment but exposed that
production verification still hardcoded the base-commit recipe source, which
failed because gh-imgup intentionally has no in-tree recipe. This changed the
model from "missing live-run prerequisite" to "the production composer ignored
the already-admitted external recipe." The image-extraction decision above
closes that gap without adding mutable runtime input.

The image-recipe refute pass found that extraction preceded the verifier's
per-command timeout and inherited the daemon's long-lived reconciliation
context. A wedged runtime could therefore stall all reconciliation. Production
now applies a dedicated two-minute extraction deadline at the engine boundary,
while ward performs ownership-gated cleanup under a cancellation-independent
cleanup context. Regressions prove timeout reaches no preparation, recipe, or
forge effect and cancellation leaves no owned extraction container.

The next live startup failed closed before inference because Apple
`container run` writes image-start progress to stderr and the initial
extractor combined stderr with the recipe stdout before hashing. Recipe
extraction now captures only bounded stdout as authoritative bytes, while
independently bounding diagnostics; ordinary verification commands still
combine both streams in their evidence transcript. A regression reproduces
runtime progress on stderr and requires the extracted recipe to remain exact.

A later P1 review found two successive durability gaps. First, the publication
task retained replay blobs but not their role-bearing manifest, evidence,
commit-plan, and importer coordinates, so recovery after task enqueue still
loaded private Claude state. The task now snapshots and validates that replay.
Second, `ExecutionExport` and that task were committed in separate
transactions, so a crash between them still left SQLite unable to reconstruct
completion. The unattended export recorder now commits the export,
reservation, and fully validated task atomically after every referenced blob
is durable. Task insertion failure rolls the export back; an identical retry
converges both records. The engine authenticates a completed terminal from the
task plus immutable export instead of returning to private driver state.
Regressions remove the driver state immediately after the atomic commit,
converge one ready PR and an inert replay, verify backup closure from the task,
and prove a conflicting task leaves no export. This closes the supported
SQLite-plus-artifact recovery frontier without a pre-enqueue exception.

The final authorized live run
`run-f6e40c11f910bda2bb8b9f69f46f178417c5985a019ab06f7ceb8fcaeef956a9`
completed Claude inference, containment, image-bound verification, publication,
and ready reconciliation. It created open, unmerged gh-imgup PR #90 with one
generated commit, head `0bc272653a87379b2262f215b87dae99876f207a`, whose
parent is the configured immutable base
`6ab4e3dff2be53f74bde9b8b3150290775152f9f`. The target `main` ref had
independently advanced to `a70dc153153e6b2d40ed6f4ecd5f0991ba69ab7d`, so
the run exposed three production correctness gaps instead of establishing a
review-ready result: publication used generic hardcoded title/body, the first
publication intent did not require the fresh audit's target SHA to equal the
admitted base, and the import commit used the unassociated
`freeside-daemon <daemon@freeside.invalid>` placeholder identity.

**Chose explicit publication metadata, an execution-bound freshness gate, and
App-bot primary authorship.** The operator now supplies reviewer-ready title
and body plus the selected App bot's canonical public slug and numeric bot user
ID. Submission validates and snapshots those values with the run; neither the
agent specification nor the driver's returned summary is trusted to become
public PR content. Before execution and again before import, the daemon
resolves the canonical bot account from the same App registration selected by
the repository-scoped installation token and requires the durable attribution
to match; a syntactically valid identity for another App is refused. Before
settling a new execution publication reservation, the store transaction
requires the fresh workflow audit's target SHA to equal
the immutable admitted base and otherwise creates no intent or forge effect.
An already-committed intent remains recoverable after a later base advance,
because external effects may already have begun and abandoning that identity
would strand them. The App bot becomes the Git author and committer so GitHub
associates the commit with its visible principal and avatar; the slug and bot
user ID remain attribution only, while the App installation and token retain
all publication authority. Manually polishing or rebasing PR #90 was rejected
as the proof of these fixes because it would hide the defects in the pipeline
that produced it.

The independent refute pass confirmed and closed an attribution-spoofing gap:
syntax-checking operator-supplied slug and user ID alone could attribute a
commit to another legitimate App. The selected installation token now binds
the exact local registration before GitHub's returned bot login, positive ID,
and account type are trusted. Negative tests reject a different durable App
identity, an unknown selected registration, and wrong returned login, ID, or
type without reaching import or publication.

A final P1 review found that the rule preserving an already-committed
publication intent across later recipe revocation still returned a permanent
reconciliation error. The intent and recovery task were durable and correctly
caused no external effect, but the error stopped `Engine.Run` and invited a
supervisor restart loop. That state is now an idempotent high-priority hold:
the task and intent remain pending, no unapproved evidence enters the item,
and repeated reconciliation succeeds without duplicating the interruption.
Restoring trust resumes the same committed identity; after it converges, the
hold is superseded rather than left as a stale warning. The hold offers only
inspection, not `stop`: no cancellation transaction yet retires both durable
records, so presenting a concluding action would let later trust repair publish
against the operator's explicit decision. Abandoning or marking
the intent dispatched was rejected because an external effect may already
exist at this crash boundary and must remain recoverable.

The next P1 review widened that same rule to two adjacent durable states. A
publication outcome finalized before a crash now wins over later recipe
revocation: recovery authenticates the frozen authorization and live forge
coordinates before current trust blocking, completes the ready transition,
and omits the now-unapproved verifier artifacts from the new evidence snapshot.
If the ready item itself committed before the crash, recovery reads it only as
historical terminal state, re-authenticates its complete content against the
finalized outcome and checkpoint, and never presents that bypass as currently
approved evidence.
Conversely, a conflicting or foreign branch or pull request after intent commit
is a durable nonterminal hold, not a correctness error that restarts the
daemon. Reconciliation creates one inspection-only interruption, preserves the
intent and task without claiming completion, and resumes the same identity
after the external conflict is repaired. Overwriting the external resource,
discarding the intent, and treating already-finalized effects as a new trust
decision were rejected because each loses one side of effectively-once
publication.

The following review found two further recovery edges and one diagnostic
convergence defect. First, restarting in attended mode after an unattended task
was queued omitted the publication workflow and crash-looped terminal intake;
the hold-only recognizer above now preserves that exact task through a later
return to unattended mode without reading the recipe or causing forge effects.
Second, a finalized publication first writing its ready item after recipe
revocation redacts evidence, but a crash after that write reconstructed only the
historical evidence-bearing form and rejected its own redacted record. Recovery
now authenticates both exact canonical crash-frontier states: evidence retained
when the item committed before revocation, or evidence absent when first written
after revocation. It never writes the evidence-bearing form under revoked trust.
Finally, the one deterministic nonterminal hold now advances its version and
diagnostic when the blocking cause changes, while preserving delivery timing and
remaining byte-idempotent when the cause does not change. Cause-specific item
identities were rejected because the conditions are mutually exclusive views of
one pending publication and simultaneous open holds would misstate current work.

The live-run harness now classifies an open `publish_blocked` item as a durable
failed outcome with its daemon-authored reason. Previously the verifier fell
through to the generic missing-ready error, so the wrapper mistook a stable
block for transient progress, waited its full 40-minute cap, and then hid the
actual cause. The polling loop now stops immediately on the dedicated marker
and prints both verifier and bounded daemon diagnostics. Treating every missing
ready record as terminal was rejected because admission, execution, export, and
publication legitimately pass through that state while still progressing.

A later P1 found one remaining mode-downgrade crash frontier before that queued
task exists. The Claude adapter selected its export persistence path from the
daemon's current operating mode, so an unattended invocation recovered after an
attended-mode restart could commit an export without atomically committing its
publication task. Export recording now authenticates the invocation's immutable
admission and selects the attended or unattended boundary from that recorded
mode. Current daemon configuration is no longer an input to this decision. A
regression reopens the store with only the attended admission floor, obstructs
publication-task insertion, and proves that the historical unattended
admission still reaches the atomic boundary and rolls back its export. The
atomic transaction authenticates immutable admission history while retaining
the exact production marker, run, profile, policy, export, and replay binding
checks; mutable trust is re-gated before any publication effect. Treating the
restart mode as authority was rejected because it does not own the
already-admitted attempt and cannot safely downgrade its crash recovery
contract.

The next exact-head review found that definitive blocked completion had the
same successor-state recovery requirement as ready and walking-skeleton
terminal items. If the blocked item committed, an operator resolved it with
`stop`, and the daemon crashed before recording the production terminal or
dispatching the task, replay attempted to restore open v1 and failed stale
forever. Production blocked completion now accepts only a current item that is
either byte-identical or a valid version-advancing terminal successor of the
exact expected item; incompatible changes still fail closed. A crash-sequence
regression resolves the item between its commit and terminal persistence, then
proves restart retains the operator's decision, completes the task, and
converges. Overwriting or reopening the resolved item was rejected because the
operator's terminal decision is durable authority, not transient workflow
presentation.

A later P2 found the complementary finalized-success ordering edge. If a ready
item committed and the daemon crashed before terminal/task cleanup, a later
external branch or pull-request conflict was consulted before the ready item;
recovery could then create a simultaneous open blocked hold. Reconciliation now
authenticates an existing ready item against the immutable publication outcome,
authorization, task, and checkpoint before consulting mutable forge state. Only
that exact durable terminal state bypasses a later live conflict; an outcome
without a compatible ready item still requires live forge verification and is
held on conflict. A regression removes the published branch after the ready
write and proves restart retains the sole ready item, creates no blocked hold or
new effect, completes terminal/task cleanup, and converges. Treating a durable
ready state as provisional was rejected because it had already crossed the live
verification boundary before its commit. The reciprocal integrity check is
explicit: a ready item without its verification checkpoint or finalized
outcome fails closed instead of falling back to external recovery; adversarial
deletion regressions prove either case creates neither a hold nor a new effect.

The same review found two hold-lifecycle defects. A successful retry used to
supersede its durable hold before writing ready, so a crash in between could
leave neither an open hold nor a terminal success that later external conflict
recovery could use. Ready now commits first; only then is the hold superseded,
so every crash frontier retains at least one authoritative open or successful
state. Regressions crash between those writes, preserve open ready successors
and operator-resolved ones, introduce a later external conflict, and prove
ready-first recovery supersedes the hold without another effect or decision
rewrite. Separately, an unchanged hold used to drive the full reconstruction and
forge path on every 100 ms daemon pass. The production workflow now applies a
30-second per-process retry window after each held attempt, while restart still
gets one immediate repair probe; a regression proves rapid reconciliation does
not fetch again and a later due attempt recovers. Reconciliation also prunes
deadlines for tasks another worker has completed, so the pacing map cannot grow
for the daemon lifetime. A schema addition solely for retry timing was rejected
because the durable hold remains the authority and a process-local pacing
window is sufficient to stop the hot loop without making repair timing part of
the workflow contract.

The next exact-head review exposed two upgrade boundaries that must preserve
historical authority rather than reinterpret it. Released production markers
contain only their derived invocation, run, and stage identities; requiring
the new publication object while scanning every durable run would wedge
terminal history and backup closure after upgrade. The marker is now explicitly
versioned for new submissions, while the exact canonical released v1 shape and
the exact unversioned publication-bearing preview shape reconstruct under
separate formats. V1 retains its admitted export-and-terminal lifecycle but
cannot supply public PR content, claim App attribution, reserve a publisher, or
create a publication task. Attaching v2 metadata by retry is rejected without
rewriting the v1 row. A migration that invented publication authority or
quarantined otherwise authentic work was rejected: execution and terminal
authority remain reconstructable, but publication authority does not. A
completed v1 terminal can recover without the driver's private state only when
the dispatched marker, immutable admission, and independently durable execution
export bind the same invocation, run, base, and head; without that export it
retains the driver's adversarial comparison rather than trusting an inbox row
that could suppress live work.

Definitive `publish_blocked` recovery is likewise authenticated before mutable
recipe approval is reapplied. Once the exact evidence-bearing or evidence-free
blocked item commits, a later recipe revocation cannot cause replay to propose
a different reason or item and fail against its own durable result. Recovery
accepts only the daemon's exact definitive reasons, decisions, task bindings,
and canonical evidence variants, reconstructing the admitted recipe solely to
authenticate that historical item; nonterminal trust holds still re-enter the
live mutable gate. Open and operator-resolved crash regressions prove recipe
revocation preserves the original item, records terminal completion, and
causes no forge effect. Rewriting the item under current trust was rejected
because it would replace already-committed diagnostic and operator authority.

The corrected live proof then exposed a 30-second runtime race in the App
authority loop. Each installation-janitor cycle deliberately withdraws its
previous coverage before refreshing GitHub state; Claude recovery happened to
re-authenticate the current intent during that bounded interval and the engine
treated `ErrJanitorInactive` as corruption, terminating the daemon. Janitor
inactivity is now classified with the other mutable current-policy refusals:
the attempt and durable intent remain unchanged, and a later reconciliation
retries after the janitor publishes fresh coverage. App identity mismatches,
malformed records, and immutable binding failures remain fatal. Extending stale
coverage across a janitor pass was rejected because a stalled or failing pass
must withdraw authority immediately.

That race was one instance of a broader missing fault-containment boundary:
every per-task production-publication error escaped `Engine.Reconcile`, so a
DNS failure, a slow container runtime, a forge outage, or a local filesystem
fault terminated the daemon. Reconciliation now positively classifies failures
at environmental boundaries, retains the immutable task, and applies the same
bounded retry window used by durable holds. Permanent external refusals become
durable holds while the reconstructed task account is still available. Only
durable-state contradictions, explicit crash seams, and unknown untyped errors
escape; malformed checkpoints, intents, image bindings, and replay blobs are
tagged as contradictions so corruption cannot turn into an immortal pending
task. A repaired nonterminal hold that reaches a definitive blocked result
advances the same deterministic item identity, preserving timing and
conversation metadata instead of colliding with its earlier version.
Enumerating each external call-site failure as a separate daemon fix was
rejected because the invariant belongs at the per-task reconciliation boundary.
The separate synchronous head-of-line cost remains follow-up work.

The attribution gate also had a hot-loop defect outside that publication-task
boundary: current-intent inspection re-authenticated the App bot every 100 ms.
The first start or recovery authentication in a daemon process still re-reads
current token and keystore authority, but its success is cached for that exact
invocation, repository, and durable attribution while the running intent is
polled. The bounded cache drops the entry after import revalidates current App
authority; a changed start binding also misses, and process restart forces a
fresh recovery check. The shared resolver separately caches only the validated
GitHub bot object for the exact registration, installation, repository, slug,
and installation-token lease. Thus a new invocation always rechecks current
authority, an unchanged binding avoids another `/users` request, binding change
or token renewal refetches, concurrent cold calls coalesce, and failures are
never cached. Historical `ImportOptionsRecord` reconstruction no longer needs
live GitHub state: the actual import already authenticated the author, while
the later publisher independently re-gates current publication authority.
Repo-wide hot caches and durable caches were rejected because either could
authorize a new invocation after the App binding changed. Refute tests cover
per-invocation hot reuse and eviction, explicit import revalidation,
installation invalidation, token-lease expiry, concurrent cold calls, and
non-caching of untrusted returned bot fields. The live verifier now likewise
distinguishes an open publication hold from the superseded repair history
retained after a successful recovery.

Revisit when per-run ward configuration replaces the manually configured path
boundary, or when the shared execution contract gains a first-class immutable
replay object that makes the private Claude metadata redundant. Restore the
production rerun action only when a command-to-engine reevaluation transaction
exists with crash and duplicate-submission coverage.

Follow-ups: #419, #423, #424, and #425.
