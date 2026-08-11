# Production Elaboration Boundaries

Chose to admit every fetched research artifact through the exact downstream
production input snapshot, materializer, and Claude prompt renderer before
committing the next elaboration invocation. The fetcher's raw response limit
alone is insufficient because base64 and envelope metadata expand the stored
artifact, multiple individually valid artifacts can cross the aggregate
materializer limit, and the rendered prompt has a tighter 31 KiB boundary.
Rejected widening either production limit: this work closes the workflow
against its existing delivery contract. A request that cannot fit becomes a
durable elaboration failure; store and I/O failures remain retryable. Revisit
when #698 introduces a prompt-mount contract or the production input limits
change.

Chose to persist ordered prior-artifact bodies inside new stage-driver intents
over reconstructing them from mutable external state. Their digests were
already admission facts, but the private driver dropped their bytes before
provider rendering, so research could influence the elaborator without ever
reaching Claude. Reconstruction hashes each persisted body against the
snapshot. A separate persisted marker distinguishes new intents from the
historical wire shape, which reproduces its original no-prior prompt; deleting
new bodies therefore fails closed instead of authenticating as history.

Chose the existing dispatched elaboration claim as an atomic production-run
reservation gate. Public production submission cannot enter a claimed
implementation identity, including exact replays after approval; only the
unexported approval consumer supplies a request that re-authenticates the root
claim and all fixed production bindings inside the run-creation transaction.
Rejected a caller-provided Boolean or exported bypass token because either
would turn approval authority back into a convention outside the transaction.
Revisit if implementation reservations gain a first-class persistence type.

Chose two startup-ingested prompt packages and stage-specific digest selection
inside admission. The persisted stage identity is already authenticated before
admission, so an exact elaboration stage selects the elaborator digest while
every other stage keeps the implementation digest. Both still occupy the
existing `PromptPackageDigest` field; adding prompt identity to the stage
snapshot, start spec, or driver contract was rejected as an unnecessary
contract change. The prospective research-delivery check uses the same
elaborator digest, preventing a prompt package and a valid research envelope
from being accepted independently but rejected together at dispatch.

Chose authenticated prior-artifact envelopes over positional inference. The
elaboration snapshot replaces each prompt-facing prior digest with the digest
of a daemon-authored JSON envelope that names its research, prior-specification,
or human-feedback role, binds the original artifact digest, and JSON-escapes
the body. This remains correct when a revision requests more research after
feedback, and it gives the prompt an unambiguous boundary even when fetched
content imitates renderer headings. No shared stage or driver field was added.
Research-role envelopes strictly reconstruct the fetcher's canonical storage
wrapper, decode and validate its Base64 UTF-8 payload, and expose readable
evidence alongside authenticated source metadata; the storage encoding itself
never becomes the agent's evidence surface.

Admission also appends a fixed no-edit, typed-output elaboration
contract to the stage's snapshotted vendor instructions. It is therefore the
final operator-host content in Claude's system instruction bundle and outranks
conflicting repository or operator prose; implementation stages keep the
original instruction snapshot unchanged.

Chose to keep `run_id`, `invocation_id`, and `stage_id` as aliases for the
future implementation identities in submit output, while adding explicit
implementation and elaboration identity fields. Existing real-run consumers
therefore keep observing the execution they verify, and new consumers do not
have to infer which lane an unqualified identity names. The real-run rig is now
documented as gated-unattended: it starts elaboration, waits while an operator
approves or revises the specification, then resumes verification against the
reserved implementation identity.

Chose deterministic quarantine for attributable malformed elaboration markers
over failing the global reconciliation pass. Both ordinary dispatch and the
global-hold observation path authenticate the stored marker and its referenced
state, record one execution-failure notice for the affected run, and continue
to healthy rows. Terminal inbox authentication now precedes driver inspection,
research recovery, and network access, so a replayed terminal performs none of
those side effects. Operational store failures remain loud rather than being
misclassified as durable corruption.

Refute-first verification exercised the boundaries from both sides: an exact
4 MiB encoded envelope survives fetch, recovery, materialization, and prompt
delivery; base64 expansion, aggregate input above 32 MiB, a rendered prompt
above 31 KiB, and oversized response metadata all fail before provider start.
It also confirmed that malformed owned rows are quarantined on both ordinary
and held reconciliation paths while transient store failures stay operational,
and that pending, replayed, mismatched, and concurrently raced direct
production submissions cannot bypass the elaboration claim. Historical stage
intents reconstruct without prior bodies, while removing bodies from the new
persisted shape fails authentication.

Fresh-context review also found four integration gaps in the preserved
implementation. Confirmed and fixed: a missing elaboration claim could fail
open, an approved specification could be duplicated in the provider prompt,
and malformed already-dispatched markers could escape quarantine. The
real-work rig's unqualified invocation identity would also have followed the
elaboration invocation instead of the future implementation; explicit lane
identities and the compatibility aliases close that consumer break. The
complete seam now exercises source, research, specification, requested
revision with addressals, approval, and implementation admission while the
real Claude prompt validator observes the stage-specific package each time.

A prompt-specific refute-first pass found two further fail-open edges. Binary
HTTP response bodies could survive fetch and later fail as an operational
reconstruction error; fetch and recovery now classify non-UTF-8 content as a
durable research failure. Revision addressals were also prompt guidance rather
than an enforced result invariant. The acceptance boundary now requires no
addressals without feedback and an exact one-to-one comment match for every
accumulated human-feedback block before it persists or auto-approves a
specification.

Automated review found that an auto-approved terminal and its implementation
creation are separate durable operations. Reconciliation now authenticates a
terminal that records a produced specification but no approval item, and
retries its deterministic implementation transition until it commits. The
terminal is not treated as proof that the downstream run exists. Revisit if
the terminal and implementation can be committed in one transaction without
coupling the lanes' independently durable histories.

Chose compatibility over retroactive migration for exact submissions created
by the pre-elaboration production-only intake. A replay returns that existing
run only after it re-authenticates every durable binding, including the
production marker, publication, artifacts, policy, invocation, and optional
work-unit declaration. It never mints a late elaboration reservation, because
that would claim an implementation that may already be executing or terminal.
The directly coupled plan text now records the new submit semantics as a
material feature change.

The shared Claude execution authority now authenticates an elaboration
dispatch marker against its admitted run and deterministic elaboration stage
at start, live import, and recorded-import replay, without requiring the GitHub
App commit-author attribution that belongs to production implementation.
Implementation retains the existing publication-author authentication at all
three boundaries. This keeps the research-only lane unable to publish while
allowing real unattended elaboration to complete its typed transcript handoff.

The canonical real-run verifier now registers the same elaboration invocation
and implementation-claim backup extractors as the daemon checkpoint verifier;
otherwise the durable intake rows appear as closure gaps after an otherwise
successful implementation. Exact submission replay also tests for any current
elaboration intake state before invoking pre-upgrade compatibility. A complete
current intake is reconstructed through `SubmitElaborationRun`, while partial
current state follows that path and fails closed rather than being returned in
the legacy production-only result shape.

Elaboration creation now records its `run_submitted` milestone atomically with
the run, initial invocation, and implementation claim. The explicit
`elaboration_run_id` returned by submit is therefore observable through
`freesided follow` throughout research and specification approval, before the
reserved implementation run exists; exact submission replay does not invent a
second observation instant.

Only the versioned directive in the digest-authenticated elaborator prompt
package enables Claude's text-prior renderer, and elaboration admits only
daemon-constructed envelopes to that role; opaque conversation attachments
remain outside it. Durable refusal to
deliver a requested revision is also final for that deterministic next
iteration: the failure item's existence gates every later replay, including
after the operator resolves it with stop or daemon prompt validation changes.
