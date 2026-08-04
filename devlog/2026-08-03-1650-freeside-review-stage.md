# Freeside Review Stage: Exact Candidate Authority

Work unit: #427. Scope: `daemon/`, `docs/`, `devlog/`, `scripts/`.

## Decision

**Chose the plan's PR-anchored review shape over resolving the open review-anchor
fork in this implementation unit.** Production still verifies and publishes the
daemon-authored candidate first, but it cannot create `ready_for_final_review`
until a Freeside-invoked `ReviewSource` records a clean pass for the exact
admitted base and currently published PR head. A target-base advance, PR-head
advance, retarget, or close makes that evidence stale and creates durable
review-dispute attention.

**Chose a versioned trust-profile migration over preserving the falsified
native-trigger modes as current enum members.** `ReviewMode` now admits only
`freeside_invoked`, and the canonical trust-profile encoding advances to v6.
Profiles approved under `auto` or `framework_triggered` fail the existing
digest/reconstruction gate and require owner re-approval; silently translating
an immutable approval would assert authority the owner never granted. Native
GitHub review remains observational forge evidence and cannot construct the
new authoritative `ReviewRecord`.

**Chose outcome-before-cleanup with disposable post-start recovery over
adopting a local Codex process after daemon restart.** Each pass snapshots the
exact candidate into an independently owned read-only volume and records its
request, started topology, collected result or classified failure, cleanup,
and readiness as separate durable facts. A restarted daemon may collect a
stopped container, but it aborts a still-running one as transient because the
daemon-owned CONNECT proxy died with the old process. Cleanup authenticates
every recorded owner and is idempotent across partial deletion; `Poll` cannot
expose an outcome until teardown is complete.

**Bound the authoritative pass to the complete effective reviewer
configuration.** The deployment computes a canonical digest over the pinned
review and observer images, topology version, workspace shape, provider
endpoints, model and reasoning effort, auth authority, instruction content,
and cost owner. The source independently reconstructs that digest, every
result and record carries it, and the engine compares it with the admitted
trust profile before requesting or accepting a pass. Credentials and host
paths remain outside the digest.

**Chose durable ambiguity attention for a non-empty finding batch in the Wave
4 minimal loop.** The classifier, convergence/yield policy, and automatic
remediation-head re-review are assigned to Waves 5 and 6 by plan revision 26.
Until those contracts exist, Freeside cannot truthfully decide that a finding
is material, fixed, or still productive to pursue. A finding batch therefore
records immutable raw findings and a `review_diminishing_returns` item instead
of silently guessing a remediation policy. Clean passes proceed; transient
infrastructure failures retry with durable exponential backoff; configuration
or quota failures create dispute attention; contradictions stay durable and
fail loudly.

## Rejected Alternatives

- **Treat GitHub-native Codex review as the gate.** The live-run evidence on
  #427 disproved an App-visible trigger and re-trigger path.
- **Trust the review container's workspace or result fields.** Ward re-observes
  the detached clean head and tree through a separate networkless container;
  the source validates structured output, and the engine rechecks the request,
  result, base, published head, and finding/run bindings before persistence.
- **Keep a running review alive across daemon restart.** Its proxy endpoint is
  process-local, so adoption would present dead egress as a live review.
- **Delete topology before recording the outcome.** A crash would lose the only
  terminal account and could repeat paid review work without evidence.
- **Treat a persisted request as proof that workspace preparation began.** A
  crash can occur immediately after request persistence, so preparation has
  its own pending-to-final durable transition and is reconstructed before any
  launch intent exists.
- **Automatically remediate every first-pass finding.** Before the planned
  classifier and convergence policy, that would turn unclassified model output
  into write authority and invent stopping semantics outside resolved policy.

## Refute-First Verification

The destructive and returned-object boundaries were tested against their
failure cases, not only their success paths:

- reordered and duplicated finding identities cannot drift the stored pass;
- result/failure terminal accounts are mutually exclusive in application code
  and SQLite triggers, including different invocation IDs for one run/round;
- unknown fields, trailing JSON, malformed completion digests, stale base/head
  bindings, duplicate findings, and non-UTC completion times fail closed;
- a crash after collection and after partial topology deletion converges on no
  leaked runtime objects before exposing the result;
- retryable cleanup or ready-mark failures leave the collected outcome hidden
  but recoverable, rather than converting it into an immutable terminal
  failure;
- restart from an absent preparation, a pending preparation with a partial
  volume, and a finalized preparation with no launch intent all converges on
  one authenticated launch, rebuilding only the partial volume;
- swapped invocation IDs and reviewer-configuration digests fail the returned
  object gate independently of source-side verification;
- runtime workspace preparation failures remain transient while invalid
  deployment shape stays a configuration failure; and
- a restart while Codex is running records a transient failure, authenticates
  and reaps the disposable invocation, and leaves the next policy round free to
  retry.

Rejected-by-verification: candidate-volume cleanup originally keyed workspace
ownership by the review run ID and cleanup originally assumed every resource
still existed. The final binding now carries the independent workspace-source
identity, and cleanup treats proven absence as convergence while refusing
foreign or unprovable replacements. The final review outcome boundary also
revalidates reconstructed request and outcome bodies before use, frames each
completion-evidence part by length before hashing, and leaves attended Claude
composition independent of production review credentials; each closed a
corruption, collision, or unrelated-mode regression found during the full-diff
pass.

The first automated refute-first review confirmed five gaps in the initial
implementation: the trust-profile configuration digest was not yet connected
to the realized reviewer, the collected outcome could be stranded by a
cleanup error, the request-to-preparation crash window was unjournaled, the
result invocation ID was not checked, and runtime preparation failures were
misclassified as configuration. All five were reproduced as focused tests and
closed by the final design above.

The second review found three more class variants: default submission run IDs
could exceed ward's runtime-name bound when embedded in review IDs, the
restart-abort branch had not inherited pending-on-cleanup-error behavior, and
finding IDs were candidate-scoped rather than invocation-scoped. Review IDs
are now bounded hashes of run and round, every post-collection cleanup branch
keeps the durable outcome pending on error, and finding identity includes the
invocation. Focused regressions cover each boundary.

The third review exposed the remaining shared-wrapper problem: operational
runtime and journal failures could still be mistaken for authenticated
contradictions, including during preparation, inspection, collection, and a
partially failed launch recovery. The final boundary uses an explicit
operational sentinel, keeps transient starts on their original invocation,
and retries observation I/O while preserving fail-closed conformance results.
It also binds each normalized result and finding set to the authenticated raw
collection digest, then recomputes that evidence after store reconstruction so
a partially rewritten outcome cannot become a clean pass.

The fourth review completed that classification sweep at teardown and
persistence: outcome-journal writes now retain transient retry semantics;
runtime cleanup I/O is explicitly operational while foreign, duplicated,
missing, or unprovable durable topology is a contradiction; and reconstructed
review records re-run the current domain validator before readiness can consume
them. Focused tests cover the outcome-write and cleanup split plus a
completion-evidence body tamper that preserves every indexed column.

The fifth review closed the corresponding engine transitions. Transient
request, inspection, and final-verification errors now keep the same invocation
pending instead of creating an immutable failure and abandoning its topology;
terminal transient outcomes still record a failed round and advance under
policy. Readiness also re-gates the currently approved verification recipe
after the review completes. A durable ready item remains recoverable after
later revocation, but a publication outcome or clean review alone is not
authority to create a new ready item under revoked trust.

The sixth review found that same-invocation preparation retry still depended
on a per-reconciliation checkout that the engine deleted on return. Review
requests now bind to a deterministic, run-owned workspace retained under the
publication work root until the invocation records a terminal pass or failure.
Retries and restarts reuse that exact seed, while terminal cleanup derives the
path from the bounded invocation ID and refuses non-directory or symlink
replacements. Regressions prove the seed survives a transient post-request
failure, is removed after convergence, and cannot redirect cleanup through a
foreign symlink.

The seventh review closed four reconstruction and classification boundaries.
A restarted volume leaser now reconstructs a transferred lease only from one
container that proves the expected owner and exact two-volume attachment;
otherwise it refuses the topology as foreign. The engine also recomputes a
canonical authority digest over the current run, round, repository, candidate,
retained workspace, and complete verification evidence, then requires the
source to match its reconstructed request before accepting a result. Runtime
and journal failures throughout launch now retain their ward-check context but
carry an operational sentinel that keeps them transient, while authenticated
shape mismatches remain conformance failures. Finally, reconstructed review
failures re-run the domain validator before their indexed columns are trusted.
Restart, request-swap, transient-launch, and body-tamper regressions exercise
each boundary.

The eighth review found two terminal-state gaps. A transient launch that had
already recovered and closed its intent was deleting the durable candidate
workspace that the same invocation needed for its retry, so transient launch
classification now retains that workspace until a terminal outcome owns its
cleanup. It also found that failure outcomes lacked the invocation identity
already required of success results. Every durable outcome now carries and
validates that identity, including at the SQLite adapter seam, so a swapped
valid failure cannot be reported for a different invocation.

The ninth review found that a review-findings escalation advertised actions
whose effects the completed publication workflow cannot execute, and that a
terminal launch refusal could be recorded while its candidate cleanup was
still incomplete. Escalations now offer only their executable acknowledgement;
launch cleanup independently controls the result class, retaining the same
invocation for transient cleanup and surfacing authenticated cleanup
contradictions.

The tenth review fixed the last two boundary placements and made both
explicit. A collected result that is schema-valid but semantically invalid is
a content contradiction inside a healthy topology, so it now persists as a
durable contradiction outcome and completes authenticated cleanup before Poll
exposes it; only topology contradictions (conformance failures observed while
inspecting, collecting, or cleaning) remain loud without automatic teardown,
because there the topology itself can no longer be trusted to tear down. The
engine also verifies its recomputed request-authority digest before Inspect,
not only after Poll, because Inspect's restart-recovery window relaunches a
paid credential-bearing review from the decoded request row; a
rewritten-but-valid row now fails closed before it can prepare or start
anything. An independent refute pass withstood both fixes and confirmed the
first-request, transient-journal, and steady-state re-verification paths are
unchanged.

The eleventh review closed the gap the tenth's pre-Inspect gate opened: a
rejection terminalizes reconciliation before Inspect, so a request rewritten
after its review had already started would have stranded the running
credential-bearing topology forever. A rejected request now reconciles what
the original request started before the contradiction is reported: from the
moment the durable binding exists (every intent state from prepared onward),
the invocation is aborted through the durable outcome/ready protocol,
authenticating teardown purely against intent, binding, and ownership
labels, never any row the rejected request could have influenced; a
never-started or closed invocation reaps at most its prepared workspace
volume; only the pre-binding preparing state stays loud without teardown,
because no durable binding exists yet to authenticate one — the recorded
topology-contradiction boundary. A refute pass on the fix moved two more
trust bits inside the boundary: the workspace reap re-pins the stored
binding to the invocation's deterministic volume name before deleting (a
rewritten binding must not redirect deletion at a sibling's volume), and
the rejected-path cleanup ignores the row's decoded abort bit, always
aborting, since a flipped bit against a running container would refuse
teardown forever. The corrupt-row read-path variant (adapter flattening)
remains deferred to #491 by the earlier decision.

The twelfth review generalized that re-pinning across the whole teardown
boundary: cleanup previously trusted the stored binding's workspace fields
and the stored intent's resource names, so a consistently rewritten row
could redirect deletion at a sibling invocation's prepared volume (using
the sibling's own valid ownership evidence) or fake convergence by naming
resources that never existed. Every teardown and observation target is now
re-derived from the engine-supplied run id (`validateIdentity` on the
intent; workspace source and volume pinned to the run-derived name on the
binding and in `CleanupCodexReviewWorkspace`), and the launch contract
narrowed to self-sourced, run-derived workspace volumes, because a
workspace identity that cannot be re-derived from the run id can never be
authenticated for cleanup under a rewritten-journal threat model. The
production source always launched self-sourced; the generality was
test-only and is gone.

The thirteenth review closed the last silent seam on that boundary: terminal
cleanup wrapped every volume-lease recovery failure as operational, so the
leaser's authenticated refusals (foreign or unprovable owner, or a lease
still transferred after the container was verified absent) retried quietly
forever instead of surfacing. Those two refusals now classify as loud
conformance contradictions at the cleanup site, matching the launch path's
existing branch; genuine runtime I/O failures stay transient and retryable.

The fourteenth review found three adjacent production seams that the earlier
fake-journal and schema-valid collection regressions did not exercise. A
decoded or invalid persisted request now carries an explicit rejection
sentinel from the SQLite adapter so every ReviewSource read path routes it
through authenticated rejected-request teardown instead of flattening it into
transient I/O. Missing or invalid files from an already-authenticated stopped
review container now persist a contradiction outcome and complete teardown,
while archive transport and filesystem failures remain retryable. Finally,
pre-start recovery preserves the operational sentinel across journal, lease,
runtime list/inspect/delete, transferred-start, network, release, and close
paths, while foreign ownership and malformed durable identity stay loud. A
fresh-context refute pass confirmed all three findings as blockers and widened
the fixes to the adjacent production paths; focused regressions cover the
adapter seam, malformed raw-output classes with a cleanup retry, runtime-list
recovery, and lease-release recovery.

The fifteenth review closed the remaining row-content versus storage-I/O
classification seam. A temporary binding read now remains operational and
retryable, while a malformed binding is a loud conformance contradiction and
cannot authorize teardown. Persisted outcomes have their own rejection
sentinel and run authenticated abort cleanup before the contradiction becomes
terminal, including retrying an interrupted cleanup without trusting the
corrupted row's ready or abort bits. The same classification sweep covers
intent, workspace, binding, request, and outcome reads and mutations: decoded
content and immutable-row conflicts stay conformance failures, while storage
and transaction failures stay operational. Every consumer preserves that
distinction instead of joining an operational sentinel onto a content error.
A fresh-context refute pass also found state branches that preceded full
launch-intent identity validation; launch, observation, and rejected-request
reconciliation now validate the complete deterministic resource identity
before trusting even a `started` or `closed` state.

The sixteenth review corrected the recovery interpretation of the runtime
coordinator's transfer sentinel. A restarted coordinator can prove only that
the exact owner and review container attach both protected volumes, not that
`Start` ran; the same evidence therefore appears after a crash during
`preparing` or `prepared`. All valid pre-handoff states now authenticate and
reap that attachment before reacquiring the lease and completing ordinary
cleanup. `started` remains ReviewSource-owned, while foreign holder, volume
set, target, fingerprint, or owner evidence still refuses without deletion.
The refute-first pass rejected moving `starting` earlier or requiring a
running-state observation, because either would falsify the journal state or
leave the created-but-stopped attachment stranded.

The seventeenth review moved outcome authority outside the rewritable JSON
body. Completion evidence alone was only self-consistency: a rewritten result
could remove findings and recompute the unkeyed evidence from fields in that
same body, allowing a schema-valid clean pass. Migration 0029 now stores a
SHA-256 digest of the exact canonical outcome bytes in a separate immutable
column, written atomically with the body and retained across the
`collected`-to-`ready` transition. Reconstruction verifies that authority
before decoding; a mismatch uses the rejected-outcome path, so neither Poll
nor the engine can turn rewritten findings into readiness. The full-body
binding also covers failure payloads. The refute-first pass confirmed this is
sufficient under the repository's established extracted-column trust model;
coordinated arbitrary writes to both body and authority remain outside that
model and would require a keyed external authority.

The eighteenth review generalized that authority to every opaque journal
body. A validly rewritten intent could otherwise replace owner tokens and
resource fingerprints with observations from foreign same-name objects; a
consistently rewritten binding would pass the in-body cross-checks and turn
those forged claims into deletion authority. Workspace ownership had the same
independent risk. Migration 0029 now stores and verifies a body digest for
workspace, intent, binding, request, and outcome rows. Immutable inserts bind
both values; workspace finalization and every intent transition update digest
and body in the same compare-and-swap, and closed-intent reuse validates the
old authority before replacing it transactionally. No adapter decodes or
mutates a row until the external digest matches. The destructive regression
proves an intent-authority mismatch reaches no runtime deletion call. The
refute-first pass preferred this uniform binding over normalized ownership
columns, which would shadow a mutable resource set while leaving other
security-relevant fields rewritable.

The nineteenth review closed two readiness gaps at the source/engine boundary.
Successful structured output must now carry one present, non-null `findings`
array; JSON's nil-slice equivalence and last-key-wins decoding can no longer
turn an omitted, null, or duplicate field into a clean pass. Transient
same-invocation start and observation failures now remain non-terminal but
reach the engine's existing per-run retry schedule,
instead of repeating workspace reconstruction or runtime operations on every
100 ms reconciliation tick. The delay keeps the current invocation and uses
the same round-based exponential bound as terminal transient retries.

The twentieth review extended the authority over every separate lifecycle
state. Intent and outcome digests now bind `state + body`; every intent
transition and the outcome's `collected` to `ready` transition verify the old
authority and atomically update state, digest, and body. A rewritten ready bit
therefore rejects the outcome and enters authenticated rejection cleanup
instead of bypassing teardown. This also prevents a forged closed intent from
authorizing `BeginCodexReviewIntent` to delete its binding and restart it. The
other opaque rows have no mutable state column, so no sibling transition
required the same change.

The twenty-first review separated a durable terminal source failure from
retryable observation I/O. `Poll` now joins `ErrNoResult` onto a persisted
failure outcome, preserving its configuration, quota, transient, or
contradiction class while marking that the invocation is finished. The engine
records that terminal failure and advances through normal round backoff;
transient errors without the no-result sentinel continue retrying the same
invocation. This prevents a ready transient outcome from being polled forever.

The twenty-second review made review cleanup independent of the daemon's
current operating mode. Attended startup now composes a cleanup-only
reconciler that enumerates intent IDs without filtering on unauthenticated
state, then verifies each state-and-body authority independently. One corrupt
row cannot starve later cleanup: per-intent failures are joined only after the
remaining candidates converge. The reconciler retries pre-handoff teardown,
including the candidate workspace, and aborts a started review after durably
recording the lost process-owned proxy. A rejected outcome is reported only
after authenticated cleanup. The reconciler carries no reviewer credentials,
prompt, instructions, model configuration, or launch path. Terminal outcomes whose
topology closed before their ready transition are also enumerated and retried.
Cleanup authenticates the exact durable owner, creation fingerprint, command,
and environment rather than requiring the current deployment's reviewer image,
so an image rotation cannot strand an older owned credential container while a
foreign same-name replacement still refuses deletion. The refute-first pass
confirmed mode-downgrade recovery as blocking. It also confirmed that review
disputes advertised an `adjudicate` action Signet rejects before transaction;
completed disputes now offer only the executable `discuss` and `stop` actions.

The twenty-third review moved cleanup-only recovery ahead of both operating
modes. Restricting it to attended hold-only startup left an unattended restart
free to terminalize a changed reviewer configuration before inspecting the old
invocation. Each production workflow now retries recovery before any task may
launch, inspect, or terminalize a review, then disables the startup gate after
successful convergence so it cannot abort a review launched by the current
process.

The twenty-fourth review added the earlier crash window before a launch intent
exists. Startup recovery now enumerates workspace-binding identities
independently and reaps an authenticated prepared candidate volume only when
no launch-intent identity exists for that run. A present intent, even one whose
recovery failed, retains ownership of its workspace and prevents this orphan
pass from racing or compounding active-topology recovery. Refute-first review
found that raw intent keys could not safely establish absence, so any intent
that cannot be authenticated now blocks the orphan pass while other valid
intents still converge. It also found that deletion needed to compare the
workspace table's separately stored volume column; the binding removal now
uses a full body, digest, and volume compare-and-delete.

The twenty-fifth review applied the same reset to pre-start intents. Recovery
first proves the workspace absent, then closes the intent, then removes the
binding by compare-and-delete. A crash after the close remains recoverable:
closed intents do not suppress the orphan pass, so the next startup finishes
the binding reset before allowing the retained invocation to relaunch.

The twenty-sixth review extended external body authority to the terminal
review account itself. Both `ReviewRecord` and `ReviewFailure` now store and
verify a digest outside their canonical body, so a partial rewrite cannot
fabricate reviewer configuration, completion evidence, candidate identity, or
failure provenance while leaving the indexed columns plausible.

Revisit when Wave 5 supplies finding classification or Wave 6 supplies the
resolved convergence/yield contract; that is the point where a non-empty local
finding batch can safely route into automatic remediation instead of ambiguity
attention.
