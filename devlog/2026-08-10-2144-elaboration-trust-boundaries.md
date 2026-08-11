# Elaboration Trust Boundaries

Chose a separate pre-approval elaboration run and implementation run over
retargeting one run after approval. `domain.Run.SpecDigest` is immutable, so
the elaboration run binds the imported work-item artifact while the
implementation run is created only from the final specification artifact that
won an exact digest-bound approval. Rejected a new persistence table: the
existing run, invocation, artifact, inbox/outbox, attention, command, and
resolved-policy records already express the state machine durably. Revisit the
two-run shape if the domain gains a first-class workflow container distinct
from an approved-spec execution run.

Chose daemon-fetched research artifacts over giving the credential-bearing
agent general web egress. The elaborator runs under `provider_only`; its typed
output can request HTTPS URLs, but only the daemon fetcher reaches them. The
fetcher accepts exact configured origins, rejects IP literals and credentials,
disables ambient proxies, resolves and dials public addresses, rechecks every
redirect, strips credential-bearing headers, and enforces the resolved-policy
response bound. It stores the request URL and purpose, final URL, status,
content type, and base64 response bytes in a digest-addressed envelope. A
replayed invocation ordinal authenticates and reuses that envelope instead of
refetching mutable web content. Revisit when policy needs path-level or
content-type constraints, or when a production network boundary can enforce
the same origin and address decisions outside the process.

Chose exact command and artifact re-gating at the approval consumer over
treating an item's resolved status as approval. The engine accepts exactly one
concluding `approve` command whose artifact digest equals the current full-spec
claim. The approval item does not offer `discuss` because elaboration runs do
not own the generic conversation lane; offering it would durably enqueue work
that no dispatcher can execute. A `request_changes` command requires one text
comment, supersedes the reviewed item, and becomes an immutable daemon research
artifact supplied to the next elaborator invocation together with the prior
spec. The replacement item shows the textual diff and the agent's claimed
comment addressals. Dismissed, expired, stopped, stale, or mismatched decisions
never create an implementation run.

Chose a dispatched outbox marker as the durable reservation for each future
implementation-run identity. This prevents two elaboration runs from claiming
the same implementation run before either approval creates it, without adding
a persistence table. Replay reconstructs the driver result even when an inbox
row already exists, then authenticates admission and collapses only at the
transactional consumer after comparing the stored terminal bytes. Every later
iteration also re-binds its implementation identity, source, policy,
publication, and work-unit declaration to the initial claimed request; an
inbox or outbox row alone therefore cannot suppress or retarget validation.
Elaboration iterations, fetch requests, allowlist entries, response bytes,
specification fields, addressals, and human revision comments all have explicit
caps so durable replay cannot amplify unbounded agent or policy input.

The refute-first pass enumerated case, port, scheme, userinfo, IP-literal,
suffix, fragment, redirect, response-size, and persisted-ordinal retargeting
inputs at the fetch boundary. It also exercised malformed and lost stage
results, active-time cancellation, iteration exhaustion, repeated approval
reconciliation, approval-wait consolidation, request-changes revisions, and a
real driver/engine reconstruction between research and specification. Confirmed
failures were a persisted research ordinal initially refetched after a crash,
an inbox marker able to suppress result reconstruction, and two elaboration
runs able to reserve one future implementation identity. Automated review
also confirmed that the initial implementation decoded the driver's
human-readable summary even though production persists the typed Claude result
only in the transcript evidence artifact, and that the approval card offered
an unserviceable generic discussion action. The next review round confirmed
three more cross-boundary failures: both durable elaboration markers were
absent from backup-closure registration, expected research refusals escaped as
workflow-loop errors instead of terminal outcomes, and independently valid
field maxima could encode a result above the aggregate protocol limit.
Recovery now authenticates the
artifact, envelope, digest, original request, final
allowlisted URL, and response bound before reuse; acceptance reconstructs and
re-gates the result, matching existing durable payloads exactly. The engine
extracts one successful typed result from the authenticated transcript channel,
using the live fake stream or the single persisted production artifact, and
never interprets the summary as protocol data. Submission reserves the future
identity atomically, and backup closure authenticates both its pending
invocation marker and dispatched reservation marker. Agent or remote research
failures become durable execution-failure outcomes while store, reconstruction,
and daemon-cancellation errors remain retryable. Encoding enforces the same
aggregate byte limit as decoding. The production dialer additionally
rejects shared, documentation, benchmarking, translation, and other special-use
address ranges, not only private and loopback addresses. No tested stale
approval, unbound spec, duplicate implementation start, forged terminal
suppression or retargeting, or live-web dependency survived the final pass.
