# Bind Codex Re-Enrollment Recovery To Durable Evidence

Chose an immutable, credential-free re-enrollment operation header plus one
terminal outcome over a mutable state blob. The header fixes the identity,
lease fence, holder, and opening time; a successful outcome records only the
auth-store digest and access-token expiry, while failure records one value from
a closed classification enum. Provider responses, token bytes, and free-form
errors are deliberately outside the durable contract.

Chose a dedicated `resolve_reenrollment` command over treating
`acknowledge` or direct item resolution as recovery authority. The revoked
identity marker remains a `system_health` attention item, but it gains the
exact verified operation binding before offering the recovery command. The
signet transition re-derives the accepted command action, the immutable carrier
item, and the latest verified journal row in the same transaction that closes
the item.

The initial implementation assessment treated that action as already implied
by the plan's verified-recovery requirement. Review showed that the canonical
Section 4 action table still listed only the generic `system_health` actions,
so revision 30 now records `resolve_reenrollment` and its exact-evidence gate
explicitly. Leaving the table implicit would let its executable parity test
claim agreement with a contract that did not name the action.

The real-daemon convergence matrix then exposed a boundary-ordering defect:
its `system_health` policy probe could not construct the newly conditional
action without a binding, while wrong-type probes failed domain validation
before signet could return the policy verdict the matrix measures. Domain
validation now requires the binding only for a `system_health` item, leaving
per-type action policy at the signet boundary as intended; the dev-only harness
synthesizes a valid binding only for its allowed `system_health` probe. The
product path still projects bindings exclusively from verified journal state.

Chose latest-operation authority over any historical success. A pending or
failed newer operation keeps admission closed even when an older operation was
verified. Admission opens only after the exact marker occurrence is resolved
by a recorded `resolve_reenrollment` transition whose command carrier is that
same item; a human-only status change cannot clear the revoked identity.

Refute-first verification confirmed that all four binding coordinates
(identity, lease fence, auth-store digest, and token expiry) must be checked,
and that identity-scoped operation coordinates alone would permit a later
revocation occurrence, or an unrelated system-health item, to borrow that
evidence. The immutable operation header therefore also names the exact marker
item ID; begin, projection, resolution, and read reconstruction re-authenticate
that coordinate. A new revocation supersedes an open marker that already has a
verified operation or projected binding and creates a fresh occurrence, which
also prevents a stale projected binding from becoming a retry dead end.
Admission checks an authenticated open occurrence before consulting any
historical resolved transition, so a later revocation cannot be masked by an
older successful recovery.

Upgrade review found that the new strict shape would reject markers persisted
by the immediately preceding daemon version. Migration 0039 now authenticates
the full frozen legacy item against a recorded identity, its deterministic
marker ID, and a canonical numeric occurrence before normalizing the reason and
actions. The Go hook preserves arbitrary quoted identity IDs and all lifecycle
state, advances synchronized metadata exactly once, and leaves malformed or
near-match rows untouched so migration never turns resemblance into authority.
Terminal replay re-authenticates the persisted holder before treating an
identical payload as convergence, so a different holder cannot borrow another
invocation's completed operation.
Mutation tests reject every individual coordinate mismatch without closing the
marker, journal tests reject divergent terminal replays and newer pending
operations, and strict reconstruction rejects unknown or trailing persisted
JSON. A process-kill harness confirms that the pending header survives abrupt
termination and remains unusable as recovery authority after reopen.

A later review confirmed that applying the live-lease gate to every terminal
made `lease_lost` impossible to record after actual expiry, release, or
takeover. The accepted exception remains failure-only and original-holder-only:
verified outcomes and ordinary failures still require the exact live fence,
while `lease_lost` requires the current lease row to prove that the journal's
fence was released, expired, or superseded at the supplied completion instant.
An independent refute-first pass rejected the tempting `!HeldAt` shortcut
because it is also false before acquisition and, for a released record, before
release. It found one cross-row ordering gap, now closed in both mutation and
reconstruction by refusing completion before the journal opened. Boundary
tests cover the instant before and at expiry, release, and takeover, stale and
wrong-holder terminals, exact replay, and preservation of a newer pending
operation. The remaining caller-clock assumption is unchanged from the lease
contract: production callers must supply the daemon's trusted current instant.

Review of the completed contract also found two evidence-legibility gaps.
Verification now refuses an access-token expiry at or before its completion
instant, and reconstruction applies the same terminal invariant, so already
expired evidence can never become recovery authority. The operator detail
view now renders all four accepted recovery coordinates (identity, fence,
auth-store digest, and token expiry) beside the consequential resolve action;
the prior action-only presentation hid what the decision would bind.

Revisit when the authentication provider requires durable recovery evidence
beyond the current digest and expiry, or when journal retention needs a public
inspection policy.
