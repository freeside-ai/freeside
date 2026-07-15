# Selective decision notes, High-assurance profile

Chose the selective decision-note model over the per-session bookend
protocol because the queue mechanics duplicated the issue tracker and
drifted: three escalated items (issues #18, #27, #52) never received
their canonical `-> Refs #N` source markers, later entries used
non-canonical "tracked by #N" spellings a `-> Refs` grep misses, and
one entry asserted a full sweep the corpus contradicted. Status kept
in two places converges on the wrong one; issues and git are now the
only sources of active work state, and notes record only why.

Decider: owner, via the agent-setup sync instruction (upstream
free-skills commit 94c46442a906c21dbabf10598bc87108f4fa698b). Rejected:
Standard profile (no notes; Freeside's contract/safety surfaces need a
durable why-record) and plain Decision-log (trigger judgment alone was
judged too permissive for those surfaces). The High-assurance
mandatory-note list lives beside the managed devlog block in AGENTS.md.

The migration is recorded as plan revision 8 (§13 plus
docs/history/decisions.md): §9's comprehension line held the
cadence-split model as a logged decision spanning revisions, and the
plan forbids §13/history drift, so replacing the sentence without a
revision entry would leave revision 7's log asserting the retired
model (Codex review finding; recording chosen over reverting §9,
which would have kept stale guidance live).

The 28-entry corpus (2026-07-08 through 2026-07-14) is frozen history
per devlog/README.md; the one-time audit of its apparently open queue
items found every actionable item already tracked (#18, #24, #27, #52,
#58), dispositions recorded in this unit's PR body. No markers were
backfilled: mutation of historical entries is removed, and the open
issues are the drain records.

Revisit when: a class of consequential decisions is discovered to be
leaving no trace under the selective triggers (e.g. re-litigated in
chat because no note existed), or the mandatory-note list misfires
often enough that routine work is writing notes.
