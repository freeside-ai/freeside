# Keep the Tracker Label's Resolver Role in tracker-format.md

The agent chose this during the 2026-11-18 agent-setup sync. The owner
reviews it in the sync PR.

## Decision

Chose to refresh `docs/tracker-format.md` from agent-setup's canonical copy
but replace one sentence of its new `label` section. The canonical text says
"No tooling selects a tracker by label or title text; the project's records
and the requests agents receive name the trackers to read." Freeside's copy
says instead that the §11 resolver finds the wave tracker by the `tracker`
label and a milestone, and that no tooling selects a tracker by title text.
The other option was a verbatim copy.

## Why

The canonical sentence is false here. free-skills commit a819eae added it
on the finding that no tooling looks a tracker up by label, which holds for
the shared skills but not for Freeside: `docs/coordination.md` (§Work-Unit
Issues and §Tracking Issues), `docs/plan.md` §11, and the `plan-wave` and
`review-wave` skills all resolve wave state over open issues carrying the
`tracker` label and a milestone, and `scripts/trackercollect` queries open
issues by that label. A verbatim copy would have the format file contradict
the documents that cite it.

## Effect on Later Syncs

agent-setup's update mode will report this file as drifted on every run,
in that one sentence. That drift is this decision, not staleness: refresh
the rest of the file and keep Freeside's sentence. `docs/coordination.md`
(Tracking Issues) names the exception beside its "copied verbatim" rule.

Revisit when Freeside's resolver stops reading the label, or when the
canonical format stops claiming that no tooling selects by label.
