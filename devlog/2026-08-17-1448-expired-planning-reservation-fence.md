# Fence Expired Planning Reservations

Chose reservation expiry as the revocation point for planning-write authority,
not merely the point that frees occupancy for a successor. Otherwise the
expired planner could continue a guarded mutation while a successor recovered
the reservation and began another planning transaction against the same issue.

The [staged-workflow adoption decision](2026-08-16-2219-staged-workflow-adoption.md)
established the 48-hour reservation and owner-authorized recovery, but assumed
that expiry unambiguously fenced the holder. PR #814's review showed that the
written contract only described when a successor could proceed; it did not
revoke the original planner's authority or bind the deadline into each write
and its verification. This change makes that missing safety boundary explicit.

Chose a pre-write margin fence: every planning transaction treats its
reservation deadline as a guarded input, rereads it immediately before each
write, and issues that write only when enough margin remains to complete both
the write and its post-write verification before expiry. When the margin is
insufficient, the original planner uses the complete guard to release and
replace its own reservation before writing. An expired original planner and a
successor use the same guarded recovery path, with existing arbitration
deciding races.

The owner accepted that the residual timing race can leave a visible mutation
whose verification finishes after expiry. That mutation is visible but
unverified: the original planner stops writing, claims no planning finish line,
and posts one recovery-only issue comment with the exact partial state so the
successor's complete guarded reread and recovery includes it. The report is the
sole post-expiry write and does not continue planning authority. Reporting
preserves the evidence needed for recovery without pretending the expired
session can undo a forge mutation already made visible.

Rejected allowing an expired original holder to continue when no successor is
yet visible. That would make expiry a race-prone observation rather than a
fence. Also rejected permanently excluding the original planner after expiry;
owner authorization plus the guarded recovery path supplies fresh authority
without privileging either contender.

Rejected requiring atomic commit visibility or rollback in this unit. GitHub
cannot provide either across the issue, plan comment, and tracker projections,
and conditional-write machinery remains the explicitly separate scope of
#813. Post-write rejection alone would only rename a visible partial mutation,
not reverse it.

Revisit when #813 supplies a guarded conditional-write mechanism, or when the
forge can atomically lease and condition every resource in a cross-issue
planning transaction, so visible-but-unverified recovery can be replaced by a
stronger native fence.
