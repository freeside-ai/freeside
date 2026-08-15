# Exported Recovery Uses Admission-Bound Import Policy

Issue #778 chose the immutable `ImportOptionsRecord` policy for a surviving
`phaseExported` tree over current `ImportOptions` policy. The gate has already
released the tree and stopped the agent, so this path can only validate and
import already-produced bytes; current policy still applies later at the
publication boundary. Live imports and recovery that can still resume a
running invocation continue to use current policy.

The implementation-plan assumption that removing the recovery
`AuthenticateStart` branch was sufficient was incomplete. A refute pass found
that `finish` independently called current `ImportOptions`, which would strand
a surviving unrecorded export under the same drift. Removing only the first
gate was rejected for that reason. Using admission-bound options for live or
running recovery was also rejected because those paths can still start or
resume work and must retain the current-policy gate.

Automated review then found that one `phaseExported` value could not safely
mean both the crash-only admission replay and a retry after live or running
recovery had already attempted current-policy import. A current-policy refusal
left the phase unchanged, so the next inspection could retry with admission
options and bypass that refusal. The durable state machine now advances to
`phaseImportPending` before either current-policy import attempt. That phase
describes the retry, while an independent trusted marker now requires current
start and import policy on every retry; only an exported recovery with no such
marker uses admission-bound options. Existing `phaseExported` files remain
admission-bound because their history is ambiguous and issue #778 explicitly
requires those pre-upgrade recovery records to converge. Terminalizing all
import-option errors was rejected because the authority port intentionally
mixes mutable-policy holds with retryable operational failures.

A second review pass found the same bypass class at the phase-transition
write boundary: `advance` changed only its local copy when the
`phaseImportPending` write failed, while the durable file stayed
`phaseExported`; without retaining that newer copy, the next inspection again
fell back to admission policy. Both the live and running-recovery transitions
now retain the failed `phaseImportPending` write in their existing in-process
retry session, and inspection persists it before reconstruction can choose an
import policy. The widened class is every transition from the admission-bound
phase into a current-policy import, including persistence failure before the
policy call.

A third review pass showed that the phase split alone still trusted decoded
private state to select weaker policy. Changing only `import_pending` back to
`exported` preserved every authenticated export fact but selected admission
options. Local format versions, companion files, and booleans were rejected:
the same actor or corruption that changes the private intent can remove or
rewrite them. The durable distinction is now a standalone, write-once
`current_import_starts` store record bound to the invocation's immutable
admission. Live and running-recovery paths record it before the private phase
transition or any current-policy import call. Recovery reconstructs and
validates that record at the store boundary, then uses its presence alone to
require current policy regardless of decoded phase. Authentic legacy and
crash-only `phaseExported` records have no marker and retain the admission-
bound convergence required by #778. This adds one internal schema record
rather than weakening the private-state trust rule or abandoning convergence.

A real-Git recovery regression verifies that a crash-only surviving released
tree reaches a durable completed export under current-policy drift without
another agent handoff. A live refusal regression and a blocked running-recovery
import verify that `phaseImportPending` never falls back to admission policy.
The live refusal regression also rolls the private phase back to `exported`
and verifies the trusted marker still forces the same current-policy refusal.
Store tests cover marker binding, write-once convergence, migration, and
column/body reconstruction. Gone trees and already-durable failed, canceled,
and lost outcomes are covered separately.

The required fresh-context adversarial pass found no surviving issue. It
specifically disproved fallback in either crash gap, phase-rollback bypass,
ambiguous-write divergence, silent marker retargeting, terminal-record
stranding, missing production wiring, and restore-order foreign-key failure.
Those rejected hypotheses are recorded here so a later review needs new
evidence rather than re-raising the same trust-boundary concerns.

Revisit when `phaseExported` can resume agent work, `ImportOptionsRecord` no
longer reconstructs the exact admitted import policy, the store no longer
provides independent current-import-start authority, or publication stops
applying its independent current-authority gates.
