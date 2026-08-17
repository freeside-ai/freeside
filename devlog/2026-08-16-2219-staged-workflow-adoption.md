# Adopt Two Work-Unit Stages

Chose planning and implementation as Freeside's only first-class work-unit
stages because each has a distinct activation, mutation boundary, durable
handoff, and finish line. Review convergence remains part of implementation;
the human merge decision and post-merge reconciliation remain gated transition
phases. Treating those phases as additional stages would add vocabulary
without adding a distinct authorization boundary.

Kept the six stage fields and the compact post-merge record in AGENTS.md while
placing single-plan, conflict-guard, and tracker-refresh procedure in
`docs/coordination.md`. Inlining those mechanics would load volatile procedure
into every agent session and blur the boundary between binding gates and their
implementation.

Rejected mandatory planning for every issue. Directly assigned implementation
can still start from an authoritative issue contract; a planning comment is
required input only when the planning stage actually ran. Also rejected local
or transient plans as handoff authority: the issue contract wins over its plan,
and a completed plan never authorizes implementation.

Clarified the same preservation for the stage records: direct,
session-contained assignments remain an implementation activation path; a
scheduled issue keeps its existing pickup path after planning; and direct work
has no containing tracker to refresh after merge. These are pre-existing
workflow cases, not new authorization doors.

Planning must also reserve its issue while it changes the authoritative
contract. The reservation is written only inside the full conflict guard,
blocks implementation pickup or claim for that issue, and is revised in place
to the completed plan or a release marker. Treating planning as simply
claim-free left a scheduled implementer able to start against the old contract;
turning the reservation into a claim would have incorrectly created a new
authorization door.

Adding an `exclusive-with` relationship is another occupancy transition. Its
guard now queries both proposed endpoints' claims and reservations and rejects
the edit when either is active; otherwise a new relationship could be published
while a pre-existing planner escaped the old direct-conflict set.

The endpoint guard distinguishes the planner's own reservation from foreign
occupancy. The planning transaction necessarily holds its own reservation
before editing the contract, so that one record is permitted; any claim or
other reservation remains a blocker.

The binding AGENTS exclusivity gate mirrors that distinction. Leaving the
reservation-aware rule only in procedure would let the binding gate authorize
an active cross-unit relationship edit.

The guard is transaction-wide when a plan changes Dependencies. It discovers
and guards every containing tracker and projection input before the issue edit,
holds that protection through all tracker repairs and verification, and rejects
the entire mutation set on any conflict. A per-resource check could otherwise
commit the authoritative contract while leaving a tracker projection stale.

The reservation is an occupancy boundary, so its first local formulation was
too narrow. It now applies across the same direct `exclusive-with` set as
claim arbitration, and it has a 48-hour forge-timestamp expiry with an
owner-authorized, guarded recovery path. This widens the reservation class
from a local marker to a lease-like conflict record without making it an
authorization door.

Adopted the canonical stage-aware finish-line wording with the project record.
Without it, the generic implementation checklist would apply to every work
session and contradict planning's no-branch, no-PR boundary.

Rejected the planned reread-only fallback after verification against the
reusable planning skill showed it cannot detect a lost update between the read
and an unconditional forge write. Planning therefore fails closed to
ready-to-post artifacts when the platform lacks a full-surface conditional
write or verified exclusive-writer boundary. Supplying that boundary is
actionable follow-up work rather than a weakness to hide in this adoption.

Follow-up: #813.

Revisit when the Phase 1B scan initiator creates a genuinely distinct
activation and durable handoff, or when another recurring phase demonstrates
its own mutation boundary and finish line rather than merely extending review,
merge, or cleanup procedure.
