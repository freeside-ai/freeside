# Remediation Review Loop

Issue: #842

Chose a derived implementation-role stage and an engine-private, head-bound
remediation input artifact over a new execution or domain contract. The
existing stage driver already accepts prior artifacts, while the adjudication
digest, routed finding IDs, source head, and derived invocation identity make
the dispatch replayable and effectively once.

The remediator still starts from the admitted exact-base checkout, so the
daemon includes a binary, full-index diff from that base to the bound candidate
in the private input envelope and instructs the driver to apply it before
editing. Passing only adjudication metadata was rejected because the candidate
commit is not necessarily reachable through the remediator's provider-only
egress; that would silently discard the implementation being repaired. The
authenticated blob is strict-decoded and canonicalized again at admission,
including the patch and instruction, and its encoded size is capped by the
production per-input limit.

Chose to preserve the run-scoped publication-task key and replace its payload
through two checked outbox promotions in one store transaction. A temporary
kind is never committed independently, so export acceptance exposes either
the prior candidate or the successor, while key inversion and quarantine
attribution remain unchanged. Head-scoped task keys were rejected because
they would allow multiple current candidates and break run attribution.

Chose head-scoped immutable verification checkpoints over rewriting the prior
run checkpoint. Inbox facts are write-once, and a distinct key plus an
explicit head field preserves the old evidence while ensuring the successor
head runs a new verification invocation.

Chose the imported evidence claim labeled
`freeside.remediator_pushback` as the structured pushback carrier over a new
`ExecutionExport` field. The engine revalidates claim provenance, exact
producer invocation and head, content digest, strict JSON shape, and
the adjudicated finding set. A valid pushback or an unfingerprintable identity
parks an explicit review dispute and never produces `fixed`. Malformed labeled
pushback also parks instead of being ignored: at this returned-object trust
boundary, treating a malformed attempt as absent could convert contradictory
agent output into an automatic fixed disposition. This deliberately chooses
the issue's fail-closed objective over the implementation plan's weaker
malformed-as-absent suggestion.

An unchanged remediation export is a distinct no-op outcome, not a candidate
replacement. Export acceptance durably marks that shape on the existing
run-scoped publication task, whose head remains the prior candidate. The replay
boundary then reconstructs the evidence channel before it authenticates the
pushback bytes; those bytes are not available safely at raw export acceptance.
A valid claim must cover the complete persisted `RouteRemediate` set for the
round. Missing, malformed, misbound, duplicate, outside-set, or partial claims
all fail closed to the same deterministic review-dispute item and terminalize
the task, so none can retry forever. The no-op path starts no verification or
review successor and can only require human attention. Treating an unchanged
head as an ordinary remediated candidate was rejected because later review
absence on identical content cannot prove `fixed` and would make reviewer
nondeterminism publication authority.

Chose to prove `fixed` only by a later independent review record with the same
base and a different head. Re-emitted fingerprints remain undispositioned and
enter fresh adjudication with structured dissent; earlier adjudication facts
are immutable. An allowlist rejection during replay becomes an explicit
import-path dissent item and terminalizes the rejected remediation candidate,
instead of retrying the same invalid export silently.

The refute-first pass changed three recovery details before handoff. First,
remediation attention and dissent are reconstructed from the persisted review,
authenticated request, findings, and imported claims before the clean-record
fast path; otherwise a crash after the review transaction could lose the
operator dispute. Second, a mixed batch with remediator pushback parks with the
exact pushed-back finding IDs instead of attaching that reason to unrelated
re-emitted identities. Third, checkpoint v2 keeps its head-scoped key, while
initial-candidate recovery accepts the old v1 run-scoped key after revalidating
the authorization's exact task head and verification invocation. Remediation
heads never use that legacy fallback.

The review-fix refute-first pass widened two initially narrow repairs to close
their classes. Remediation commit-author authentication now reconstructs the
stored run, stage, review, adjudication routes, deterministic input artifact,
finding ownership, artifact-only initial invocation, and original production
marker in one read snapshot; start, live import, and terminal replay all derive
the App author only from that original marker. The driver-limit finding also
applied to initial production, so the daemon now runs the real Claude
materializer and renderer before recording every production admission and
again before replaying an older recorded attempt. Copying the renderer's 31
KiB limit into the engine was rejected because it would drift from UTF-8,
aggregate-input, prompt-role, and shell-quoting constraints. Refutation found
no remaining bypass after the sibling marker and admission-path sweeps.

The no-op refute-first pass confirmed that imported claim metadata alone does
not carry pushback bytes; the durable artifact blob is opened only after the
hostile importer has revalidated the evidence manifest, media type, provenance,
and digest. Refutation rejected ordinary same-head review, partial route
coverage, and missing-claim retry. Integration recovery now proves the same
stored no-op replays to one dispute without a second verification, review,
candidate head, or forge effect. The changed-head and no-op paths both retain
the authenticated claim on their dispute item so downstream consumers receive
one pushback record form.

A later production-shaped refutation invalidated commit identity as the no-op
boundary. The stage driver assigns every invocation a fresh commit date, so an
unchanged remediation tree normally produces a different commit SHA and could
enter nondeterministic re-review. Change detection now happens only after hostile
import: the daemon re-authenticates the prior reviewed candidate's immutable
verification checkpoint and compares its digest-bound imported tree with the
new daemon-derived tree. An identical tree, regardless of commit SHA, takes the
terminal refusal/dispute path; a missing or contradictory source checkpoint also
terminates in dispute instead of entering review. Request-time SHA flags and the
later-review SHA-inequality echoes were rejected because commit metadata is not
content identity. Commit SHA comparisons remain only where they bind an export,
request, review, checkpoint, or provenance record to the exact commit it names.
The fresh-context refute-first pass confirmed two recovery gaps before the fold:
v1 tasks must continue decoding and byte-identically replaying the retired
`remediation_noop` field, and authorization or image reconstruction
contradictions must take the same terminal source-identity dispute as a missing
checkpoint. Both were accepted and fixed. Refutation rejected fresh-timestamp
bypass, changed-tree misrouting, a remaining SHA-based content decision, a
loose v1 checkpoint fallback, successor work after identical trees, and loss of
pushback coverage; focused engine and integration verification disproved each.

The final review refutation found that delivery terminalization must distinguish
durable shape refusal from operational materialization failure. Only an error
the real materializer classifies as `ErrProductionInputUndeliverable` now ends
the invocation; unavailable blobs and other transient faults remain pending and
retryable. A fresh deterministic refusal creates the ordinary
`execution_failure` attention and no attempt or successor. A pre-existing
admitted attempt records the standard immutable `ExecutionOutcome`, which both
reconstructs the refusal after restart and releases its identity-capacity slot.
The proposed engine-private remediation-failure inbox record was rejected: it
would have consumed capacity indefinitely and could have suppressed a legitimate
driver result after a crash between `Start` and dispatch bookkeeping. Recovery
therefore inspects first and revalidates delivery only when the driver proves the
invocation unknown.

The same pass moved remediation quarantine release behind authentication of the
entire stored transition, not just the request envelope. Missing or contradictory
run, invocation, stage, artifact, adjudication, finding, publication, or input
blob bindings quarantine only the attributable run; process-local configuration
absence and transient I/O stay loud. Repair retires that exact quarantine class
only after full reconstruction succeeds. The candidate patch is bytes, not
text: JSON carries it through the standard `[]byte` base64 encoding so binary
diffs containing invalid UTF-8 round-trip and apply without replacement bytes.

The final publication-boundary refutation found one later consumer outside the
pending remediation scan: after export, the pending publication task owns a
dispatched remediation marker that the dispatch scan no longer visits. The task
scan now authenticates both the original production marker and, for remediation
producers, the full dispatched remediation transition before replay. Missing or
contradictory durable state uses the existing per-row marker quarantines and holds
only that run; notices retire only after the complete chain reconstructs, while
unrelated work continues. The same pass narrowed source-tree recovery: explicit
checkpoint absence, malformed data, and authorization or image binding
contradictions remain terminal source-identity disputes. Concrete operational
filesystem, SQLite, and completed-transaction failures remain untagged and
retryable, while cancellation remains the context error. The sibling
marker-consumer and commit-identity sweeps rejected a new marker subsystem and
any return to SHA-based content comparison.

The boundary recurrence after that repair invalidated the publication task's
producer as remediation-lifecycle authority. Before export, a dispatched
remediation transition exists while the task correctly still names the prior
candidate producer; after export, the same task field names the remediation
producer. The replacement boundary derives the active transition from the
highest persisted remediation stage and authenticates the complete production
and remediation chain in one read snapshot. A fresh-context refutation found
that row reconstruction alone omitted the referenced remediation input blob, so
the shared boundary now also opens, digest-verifies, strictly decodes, and
canonical-compares that daemon-authored input before release or publication.
The same refutation then exposed a stale caller snapshot: ownership could see an
older remediation round while a concurrent publication pass had already
persisted its successor. The boundary therefore reloads the run inside its own
store snapshot before selecting the highest round. Both production-run
ownership and publication use that one reconstruction boundary. Acceptance-
before-publication, pre-export dispatch, post-export replay, later remediation rounds,
and restart all agree without another lifecycle-specific guard. The task
producer remains only where it binds the candidate, replay, verification, and
publication identities. A first focused refutation caught the acceptance scan
reaching a corrupt dispatched marker before publication; sharing the boundary
with production ownership closed that unrelated-run starvation path. The
task-field guards and a generalized marker subsystem were rejected because
both would preserve the same false authority split.

Artifact-backed remediator pushback now distinguishes deterministic content
refusal from operational storage failure. A body beyond the claim-text limit
is malformed pushback and a digest mismatch is unauthenticated evidence; both
use the existing terminal review-dispute attention path, which preserves the
claim and cannot declare a finding fixed. Quarantine was rejected for digest
mismatch because quarantine denotes durable daemon state that can be repaired
and retried, while contradictory imported agent evidence requires operator
disposition. Artifact open, read, and close failures remain ordinary retryable
errors.

A #911 re-review member closed the symmetric input-side boundary. A candidate
whose binary diff pushes the marshalled remediation input past the production
per-input limit is a deterministic preparation refusal that fired raw
`strictjson.ErrLimitExceeded` during finding adjudication, before any
invocation, stage, admission, or outbox intent exists. That raw error was
non-retryable and lane-fatal: it stopped every production publication and left
the task pending without attention. The dispatch-phase delivery-refusal
terminalizer could not classify it because that path requires an existing stage
and admission. Preparation now surfaces the first-class
`ErrRemediationInputUndeliverable` sentinel, and finding adjudication raises a
durable per-run `execution_failure` escalation at a distinct deterministic
identity, then parks and advances the lane. The escalation reuses the
dispatch-phase failure type and its direct acknowledge-only write, since the
signet action policy excludes acknowledge for that type. Reusing the round
review-item identity was rejected because its type is immutably bound to the
routing decision and would collide; adding another guard at the unreachable
dispatch terminalizer was rejected as the wrong boundary.

The re-review then found the retryable-versus-terminal split had not been
applied to checkpoint-artifact blob verification. Both the durable production
checkpoint authenticator and the remediation source-tree reconstruction
unconditionally joined a `verifyFakePublicationBlob` failure with a terminal
sentinel, so a transient open, read, or close fault permanently disputed an
otherwise valid run instead of retrying. A shared `retryableOrTerminal` helper
now keeps operational blob faults retryable and reserves the durable sentinel
(`ErrParentKeyMismatch` for production, `errRemediationSourceIdentity` for
remediation) for a digest mismatch or an absent blob, matching the adjacent
checkpoint, authorization, and image store-read classification. Both blob
call sites in that boundary were swept, not only the cited one.

Revisit when remediation needs a shared wire contract, more than one current
candidate per run, a retryable operator repair for import-path dissent, or the
adjudicator-model unit (#843) defines a durable dissent record beyond the
current engine-private carrier.
