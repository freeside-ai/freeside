# Package Phase 1A Operations Without Duplicating Authority

Work unit: #238. Scope: `daemon/`, `scripts/`, and `devlog/`.

## Decision

Compose `setup`, `onboard`, and `doctor` from the existing Phase 1A
primitives. `setup` owns the non-root, owner-private state layout, the
create-once empty authority document, and the existing GitHub App manifest
conversion. It seeds registration authority only from the canonical converted
App identity. `onboard` imports the reusable project-image builder directly,
records a fresh live workflow audit for the requested branch and commit, and
uses `WorkflowAuditReviewForProfile` as the single review payload. `doctor` reads
the durable conformance declaration and live four-dimensional backup-health
source, then converges ordinary `system_health` items.

Rejected wrapper scripts and second implementations of audit, recipe, image,
or backup semantics. Those would let operational packaging drift from the
primitives already proven by #237, #305, #334, and #274.

## Installation Authority

Use the existing `installation-authority.json` pending envelope for native
installation and selected-repository expansion. Onboarding is the only public
writer of installation intent and binding state; setup writes only the
create-once document and newly converted registration. Replacement passes the
existing strict decoder and encoder under one store lock before the
owner-private atomic writer can replace the live file. The lock spans read,
validation, mutation, and replacement so onboarding cannot overwrite a
concurrent janitor quarantine with an earlier snapshot; the store's advisory
lock extends the same serialization across processes.

The janitor now publishes a separate in-memory `PendingReady` transition
signal after a complete pass observes the exact expected selected-repository
set. This signal grants no runtime authority: `AllowsRepository` remains false
through the review pass. The approved pass records the image and immutable
profile revision without activating it, promotes the exact document envelope,
then activates the profile as the final commit; a later janitor pass must
cover the trusted binding. The signal binds the epoch, durable revision, and
complete repository set and supplies GitHub's canonical installation ID when
the pre-redirect envelope necessarily recorded zero. The SQLite profile and
authority document cannot share a transaction. This recoverable order leaves
no active profile when promotion fails; an interruption after promotion can
leave a binding without an active profile, which grants no executable work and
requires a new review to complete activation. The approval digest binds the
exact pending envelope, which promotion consumes, so accepting the old digest
after that boundary would also accept it for a replacement intent. The normal
path still has exactly one Freeside review; only this crash window repeats it.
Rejected promoting from flags, callback state, or a successful HTTP status,
because each would trust a caller or partial remote observation instead of the
janitor's exhaustive canonical comparison.

The CLI implements the restart-safe `--resume` path. No callback listener is
added in Phase 1A; a callback is only an acceleration in the plan, while
`--resume` is the authority-independent recovery path.

Setup creates the authority document with an exclusive owner-only write.
It inspects the keystore first and creates the empty document only when no
credentials exist. Replays strictly decode the existing file and never replace
it. A newly converted App receives epoch 1, revision 1, its canonical returned
owner as the sole trusted owner, and an explicit empty installation list.
Existing credentials without matching authority are not auto-initialized or
even given an empty document: an empty list authorizes the janitor to remove
every observed installation, so recovery requires an explicit
operator-authored migration rather than an inference. The one exception is an
interrupted conversion whose credential record atomically carries setup's
pending-authority marker. A retry may finish that sole matching registration
only while the pre-conversion authority document remains valid and empty;
ordinary credential consumers reject the marked record, and neither an
unmarked credential nor a nonempty authority document infers recovery. The
marker is removed only after the exact canonical registration has durable
authority.

Project-image failure cleanup checks image-reference ownership globally, not
only within the repository being onboarded. A content-addressed reference may
already back another repository's durable row, so repository-local ownership
would authorize destructive cleanup of shared state.

## Review and Activation Ordering

The first onboarding pass mints a repository-scoped, read-only workflow-audit
token only after the current authority document and janitor agree on the exact
trusted or pending installation. Cache hits repeat that gate. The normal
publish permissions and active-profile mint gate remain unavailable before
trust activation. The existing GitHub workflow auditor resolves the requested
branch before and after collection; onboarding requires the resulting commit
to equal the project-image commit before retaining the audit.

The pass returns the complete retained audit evidence and a separate approval
digest bound to both the derived profile and the canonical registration,
installation, account, repository, and complete pending-envelope identity
(epoch, durable revision, authored installation, repository sets, mode, and
expiry). The v2 digest also binds the displayed image request: audited commit,
exact recipe-byte digest, base image and local build reference, registry
destination, image name and tag, DNS inputs, and build proxy. The applying pass
must supply that exact digest, and it repeats the live audit and installation
resolution first, so branch, installation, intent, recipe, or image-input drift
invalidates an earlier approval. Builder defaults for the image name and tag
are normalized before this display and digest, so an omitted CLI flag cannot
authorize a destination the operator did not see. Slice-backed recipe and DNS
inputs are copied into an immutable approval snapshot, and the builder receives
a second owned
copy, so it cannot rewrite the later returned-object comparison through Go
slice aliasing. The pass invokes and durably records the project-image result
before activating the profile, validates the returned object against the request
(including the one fixed preparation command), and reconstructs the exact
durable row before trust activation. Recipe detection uses the verifier's
hardened git plumbing against the same exact commit as the retained audit and
image. A failed or unrecorded build therefore cannot leave a trusted
repository without its required runtime artifact.

Effective token permissions and every audited allow-axis are copied from the
audit. Owner choices that an observation cannot decide remain explicit:
review configuration, commit plan, PR execution, message ruleset, and
protected-path widening. Phase 1A defaults are conservative.

The unattended proof harness now opts into `unattended` explicitly. Operational
commands keep `attended_dev` as their honest default; proof callers cannot rely
on the former implicit unattended behavior. All driver modes validate the
flag, even when the selected driver has no use for the value. The selected
mode also controls both admission gates: `attended_dev` requires ward's three
base handoff capabilities, while `unattended` additionally requires the two
full-suite proofs. Using the unattended floor for both modes would make the
permissive default unusable on a fresh installation and misstate the plan's
weaker attended runner class.

Attended imports resolve the currently active repository trust profile at the
import boundary. Their admissions deliberately remain unbound to a profile
revision, preserving the domain contract that only unattended execution
anchors the pre-execution revision. Letting import reject that valid nil field
was rejected because it made every successful default-mode export retry
forever. Unattended import still loads the exact admission-bound digest; the
attended path merely supplies the protected-path policy the importer requires
under its explicitly human-supervised mode.

## Doctor Convergence

Each doctor dimension has a stable item class. A failing episode creates one
open item, repeated scheduled checks reuse it, and a healthy pass transitions
it terminally to resolved. A later recurrence receives a fresh
revision-derived identity, preserving the item lifecycle instead of reopening
a terminal diagnosis. Concurrent duplicates converge to one open item, and
project scopes never clear each other's items. `attended_dev` is the command
default; unattended mode remains explicit and is reported from the actual
doctor configuration.

Conformance is healthy only when the latest durable pass names the exact live
backend-configuration digest. The standalone command requires that digest, and
the scheduled composition takes it directly from the constructed backend. The
standalone command also accepts the current approved-recipe set and passes the
same set to checkpoint inspection and store reconstruction.

The production daemon accepts that same repeatable approved-recipe input and
copies it into its store policy before constructing checkpoint health. Startup
and scheduled doctor passes therefore reconstruct publish-eligible
verifier/daemon artifacts under the active policy rather than an accidental
empty set. The real-work harness requires the onboarding-approved digest
explicitly; inferring it from persisted `publish_eligible` bits was rejected
because reconstruction is the boundary that must re-derive those bits.

The returned project-image reference is re-bound to the approved destination,
not merely checked for a valid digest and self-consistent ID. The accepted
repository portion is exactly the reviewed registry (or loopback registry
port) plus image name. Trusting a builder implementation to preserve that
destination was rejected because `ProjectImageBuilder` is a returned-object
trust boundary.

Before creating or promoting a selected installation, the command scans the
authority for every locally credentialed App registration. A repository
already trusted under another App is refused with the runtime resolver's
ambiguity class. Checking only the selected registration was rejected because
it could create durable state that every later token resolution refuses.
Promotion repeats that global ownership check inside the serialized authority
document update; the command-level check alone cannot close a race between two
onboarding processes. Setup replay likewise selects the one local App matching
the canonical operator account ID instead of treating unrelated local Apps as
an ambiguity; multiple Apps for that same ID remain fail-closed.

An operational-source error during a scheduled doctor pass remains daemon-fatal.
Unlike an unhealthy dimension, which becomes a durable item, a source error
means the daemon cannot truthfully report health. Stopping follows the existing
fail-loud treatment for a janitor that unexpectedly exits; revisit when the
supervisor contract defines a bounded retry budget that cannot hide a prolonged
blind spot.

Every scheduled doctor pass first reruns the full backend conformance suite,
then evaluates the freshly persisted result even when the suite reports an
error. Merely comparing the latest record's configuration digest would let a
same-configuration runtime regression retain stale healthy evidence
indefinitely. Setup or `-run-conformance` controls the startup pass; the doctor
schedule controls continuing proof.
Startup and scheduled conformance hold the janitor's latest completed coverage
snapshot stable only for the authenticated exact-base fetch. Retrying a token refusal after a
mid-pass coverage withdrawal was rejected: the retry would still race the next
pass and would obscure which grant snapshot the probe used. Ordinary execution
continues to observe the immediate fail-closed gate; only the operational
fetch delays reconciliation while it authenticates. The longer runtime proof
runs after releasing that coordination lock.

Doctor derives its capability check from the same mode-aware domain floor used
by admission reconstruction. In unattended mode, a passed conformance record
without networkless export or enforced provider egress remains unhealthy even
when its three handoff capabilities are present.

Cancellation observed after the scheduled pass is graceful shutdown, not an
operational-source failure; this prevents SIGINT or SIGTERM from racing the
long-running conformance suite into a false daemon-fatal result.

Installation intent and promotion checks use one injected clock end to end.
The CLI supplies the real clock, while orchestration tests supply their fixed
audit instant; no durable authority decision depends on the test runner's date.

The project-image builder accepts an explicit token source for onboarding while
the standalone public-repository command remains anonymous. Each canonical
identity lookup and the clone request obtains a fresh exact repository token
through the onboarding source, which revalidates both durable authority and the
janitor's latest grant observation even on a cached token. The builder rejects
tokens whose repository identity or permissions differ from the requested
read-only audit authority. Git receives the token only through its
per-invocation environment config; authenticated command output is dropped on
failure because it is remote-controlled and can reflect credentials. Both the
API and git paths reject redirects so the credential cannot cross endpoints.

Trusted onboarding re-reads the exact installation binding and rechecks the
janitor immediately before activating the profile. The long image build is an
authority-drift window, so the review-time binding and earlier token gates are
insufficient evidence at activation. Pending promotion retains its separate
exact-envelope recheck because its reviewed grant is intentionally not runtime
authority until promotion.

Onboarding gate reads coordinate with an in-progress janitor pass. The janitor
still withdraws ordinary runtime coverage before every pass, but its cycle lock
now spans withdrawal through publication; the onboarding-only view waits on
that lock and observes the pass's completed result. Preserving the old coverage
during a refresh was rejected because a stalled or failed pass must close the
gate. Letting onboarding read the deliberate mid-pass empty map was also
rejected because the 500 ms refresh cadence made audit and image fetches fail
according to scheduler timing rather than grant state.

## Verification

Focused tests cover private idempotent setup and symlink refusal; the corrected
fake-driver directory; create-once authority and manifest conversion; a
pending-gated read-only audit mint whose cache is invalidated by authority or
janitor drift; review-before-build and build-before-activation;
installation-envelope expansion, promotion, and changed-coordinate refusal;
validated atomic authority replacement; separation of pending readiness from
runtime authority; exact project-image preparation-command validation; and
unhealthy/repeated/cleared doctor convergence.

The refute-first pass confirmed and fixed two blockers before commit: setup
created empty authority before detecting pre-existing credentials, and the
approval digest did not distinguish replacement pending revisions with the
same installation coordinates. Regression tests now prove the former leaves
the authority path absent and the latter rejects the stale approval. The same
pass found no additional issue in exact read-only token scopes, returned grant
validation, or fixed preparation-command validation. Automated review then
found that the attended default still selected the unattended capability
floor; a mode-table regression now pins both the engine and persistence floors.
It also found that the scheduled doctor reused a same-configuration
conformance pass indefinitely; an ordering regression now proves every
scheduled pass refreshes conformance before evaluating health, including the
failure path. A later pass found the one-time conversion key could become
durable before authority initialization and that scheduled cancellation could
surface as failure. Setup now persists the pending-authority marker before key
or metadata in the same credential swap, rejects it from runtime consumers,
and finalizes it only after exact durable authority; scheduled cancellation is
graceful only when the context is actually canceled. The refute-first pass
rejected raw-code recovery and late-marker ordering before confirming the
final crash-state matrix clean. A later credential-boundary pass rejected
putting the installation token in the clone URL or command arguments and
retaining authenticated git output in errors. Tests instead pin environment
only injection, exact read-only token validation, credential-free errors, and
authority and janitor drift rejection before activation. Live GitHub conversion
and token minting, plus the Apple-container build, remain external manual
verification surfaces.

Revisit when Phase 1B adds a callback transport or a second recipe ecosystem.
Neither should widen the existing authority boundary implicitly.
