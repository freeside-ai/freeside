---
run: manual
stage: coordination-policy
date: 2026-07-13
branch: chore/deferral-issue-metadata
---

# Devlog deferral escalation

- Escalation uses two-way provenance through the issue form's required single-line source field and the devlog item's `-> Refs` marker, owner-lane routing, `kind:contract` precedence for shared-package needs and nature-based `kind:*` plus `deferral` otherwise, `needs-human` without a lane for maintainer-only work that returns by fiat for verification and a devlog-backed closure PR, no milestone or status label, ordinary PR closure, one scheduled predicate (milestone plus tracking listing), blocker exclusion until scheduled or actively claimed, and dependency-chain insertion before contract-deferral pickup.
- Pickup requires either spine scheduling or human fiat, except that sweeps skip `needs-human`, which remains unmilestoned and fiat-only after maintainer action; sweeps run at every planning session while waves exist, at later phase boundaries, or ad hoc, and the queue stays dormant between sweeps until the Phase 1B scan initiator replaces human cadence.
- Labels `deferral` ("origin: escalated from a devlog queue item; unmilestoned = unscheduled") and `needs-human` ("maintainer-only action; never agent-selected") were created on GitHub.
