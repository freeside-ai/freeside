---
run: manual
stage: signet-action-policy
date: 2026-07-15
branch: feat/signet-action-policy
---

# Signet per-type action policy (issue #23)

## Decisions

**The plan's lists are allowed sets, enforced when an item crosses signet.**
Chose a signet-owned table plus `Service.PutItem` over adding type/action
coupling to `daemon/internal/domain`: domain remains the shared Action union,
while signet rejects any requested action outside the item's type-specific
set before a store transaction starts. An item may offer a subset of its
type's allowed actions; policy does not force every context to render the
entire list. The nine actionable Phase 1 types must offer at least one action.

**`blocked` is read-only.** Plan §4 names action lists for nine types and
§5.12 defines `blocked` only as a consolidation of external waits, so its
authoritative allowed set is empty. Rejected the saddle fixture's provisional
`acknowledge / snooze / stop`: those actions carry system-health, proposal,
and workflow-control semantics that the plan never assigns to an external-wait
summary. The existing domain and OpenAPI contracts require at least one
requested decision, so this unit pins and tests the policy but does not cross
the lane's declared paths to relax shared contracts. Follow-up: #96.

**Durable rows are re-gated when a command is submitted.** Chose current-policy
validation after the live item read over trusting that every row entered through
`Service.PutItem`: pre-policy data and internal direct-store writes otherwise
remain authorities for commands the current type policy forbids. Replay remains
first, preserving command-id idempotency; for a genuinely new command, policy
invalidity is judged before lifecycle, version, and implementation-support
checks, so even a pending action on an invalid row fails as invalid policy.

Revisit when plan §4 assigns a new type or action, or gives `blocked` an
explicit decision flow; update the table and its independent ten-type fixture
together.
