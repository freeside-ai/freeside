# Preserve The Approved Visual Hierarchy And Existing Behavior

The owner chose a focused improvement to Freeside's information hierarchy
over a redesign. Four rounds of native Mac/iPhone comparisons settled the
accepted treatments. All four V4 proposals were marked Keep for both
platforms; earlier approvals also retain serif inbox summaries, the task-hold
callout, long-message expansion, empty labeled forms and an on-demand
Unverified explanation.

The [implementation handoff](../docs/design/visual-audit-2026-10-02/README.md)
preserves the exact review records and selected visual references. Its stable
decision IDs connect accepted treatments to information-preservation rules
and verification. Earlier rejected experiments, especially condensed task
rows and serif section headings, are not implementation inputs. The original
keyword headings and the existing design tokens remain the baseline.

The owner approved more prominent decisions and quieter supporting details.
That changes presentation, not authority: claims remain labeled, technical
provenance stays reachable, and failed/waived/stale states remain visible.
Finding cards show the actual message and proposed outcome together; the
bulk approval action retains its current command scope. The review did not
approve new per-finding commands or a new meaning for dispute approval.

The current specification explicitly puts the final-review PR link last and
imposes recommendation/fact ordering that differs from the accepted design.
Those presentation rules need a separate material-document review before
dependent implementation. The handoff records the intended deltas without
silently changing the active specification or trust contract. Likewise,
screenshot approval does not relax the existing 520pt action-region budget
at a 560pt Mac card width; a conflict needs measured evidence and an explicit
owner decision through #1141.

Copying the cumulative prototype diff was rejected because it includes
audit-only switches, fixture injection, rejected experiments and a base older
than current main. Recreate the approved treatments in current shared views,
preserve newer behavior, and inspect unchanged screens that use those views.
The owner reviews a runnable native mock build in addition to screenshots;
pixel tests alone cannot establish a better reading path.

Revisit when the available decision data or action contract changes, when
native accessibility testing contradicts the accepted layout, or when real
content cannot satisfy both the approved hierarchy and an existing measured
layout budget. Any revision should name the affected decision and preserve
the remaining approvals.

Follow-up: #1724.
