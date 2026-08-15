# Reviewer Configuration Preflight

Issue: #532

Prior decision: `2026-08-14-2308-review-configuration-recovery.md`

## Decision

Refuse a new unattended execution admission before recording or starting its
attempt when the repository's latest activated trust profile pins a review
configuration digest different from the daemon's effective digest. Treat this
as mutable operating-state policy, so the pending intent remains resumable
after the operator activates the review-configuration-only profile revision
used by the existing adoption flow.

Surface the same ambient mismatch through the scheduled doctor startup pass.
The doctor compares the effective digest with only each repository's latest
activated profile, not in-flight runs pinned to historical revisions. The
per-run admission gate remains authoritative for those runs, while stale
historical profile encodings cannot poison ambient health or block their
documented re-approval recovery. If a current activated profile itself uses a
stale encoding, doctor reports that repository as unhealthy without aborting
startup, so the operator can still record the re-approved current revision.

Keep the mismatch recoverable rather than making daemon startup fatal. The
operator's recovery action is implemented inside the running daemon; refusing
startup would make the approved recovery path unreachable. The doctor files a
blocking `system_health` item, and the admission gate independently prevents
implementation spend before that item exists or across its read/write race.

## Rejected Alternatives

- Rejected a startup-fatal configuration check. It would brick the in-daemon
  profile activation and adoption path established by #611 and #788.
- Rejected checking every historical profile revision in doctor. Historical
  revisions may legitimately disagree with the current daemon and are handled
  at their run-scoped re-gates; only the latest activation is ambient policy.
- Rejected moving adoption semantics into the admission layer. Adoption
  records authorize recovery from one exact parked failure, while admission
  needs only the current activated profile that the adoption flow already
  promotes.

## Verification Findings

The existing startup composition computes the effective review digest before
constructing both the engine and the doctor pass. The store already validates
the selected active profile on reconstruction; the new cross-repository query
reuses that boundary and selects one latest activation per repository.

Independent refutation found that returning a stale current-row decode failure
as a doctor source error would abort startup, while treating every profile read
error as a finding would mask database failures. The store inspection now
separates query and scan errors, which still propagate, from deterministic
per-row reconstruction errors, which doctor reports as blocking health. The
inspection is anchored on activation rows with a left join, so an orphaned
current activation is reported rather than disappearing from the health set.

Automated review then found that a repaired profile could remain held by the
previous doctor item until the next scheduled pass. The unattended engine now
runs a narrow review-configuration convergence under its effective runtime
digest before each dispatch pass, so repaired activation clears stale blockers
from any doctor project while the per-admission comparison remains the
authoritative race-closing gate. The daemon serializes that narrow pass with
its scheduled full doctor pass, preventing opposite profile snapshots from
racing their item transitions or surfacing a stale-write loop failure.
The final automated pass found the remaining cross-repository activation gap:
a different repository could drift after the pre-dispatch health refresh. The
admission write transaction now re-gates the full activated-profile set under
the same runtime digest before recording either a fresh or replayed unattended
start, so activation and admission serialize at the authoritative boundary.

Refutation rejected running that convergence from the onboarding CLI: its
review digest is caller-supplied profile input rather than the running daemon's
effective configuration, and a fixed project ID could leave another doctor's
global blocker open. Keeping the refresh in the runtime composition closes
both gaps. Automated review also found and closed the related operator-doc gap:
the one-shot unattended doctor example now names the required effective digest
and its startup-log source.

Revisit when review-configuration recovery moves outside the daemon, or when
the control plane can safely expose a startup-fatal check without blocking its
own repair action.
