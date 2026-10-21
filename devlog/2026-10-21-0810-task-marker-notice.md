# Give the Publication Lane's Task Check Its Own Quarantine Notice

Chose a separate notice class, `production-task-marker-quarantined-`, for a
publication task that disagrees with its run's remediation record while the
record itself authenticates. That refusal was raised under the shared
`remediation-marker-quarantined-` notice. The production lane authenticates
only the marker chain, found it sound, and superseded the shared notice on
every pass; the publication lane then minted the next occurrence. The Wave 8
exit run raised 89 notices in about two minutes, and every one notified every
paired device (#2102).

The new class follows the scope-notice precedent: a lane that makes a check
no other lane makes owns the notice for it. The production lane's release
never matches this prefix, so the notice stays open under one occurrence
until the publication lane reads a matching task and retires it. Marker
failures the production lane also sees (an unreadable or missing marker)
keep the shared notices.

## Rejected Options

- **Run the task comparison in the production lane too.** The production
  lane does not own the publication task. Reading it there couples the two
  lanes' decode and recovery paths to stop one lane from releasing a notice.
- **Deduplicate or rate-limit the notifications.** That hides the symptom.
  The notice record would still churn, and a superseded notice would still
  claim the hold had cleared.
- **Make release check which lane raised the notice.** The prefix already
  names the owner, and the scope notice shows the pattern. A provenance
  field would change the attention item shape for no added safety.

## Refute-First Findings

- **Disproved, a failure both lanes see moves to the new class:** every
  reclassified return sits after the run's transition has authenticated in
  the same transaction. Unreadable or missing markers still return before
  it, unwrapped. A bare store error or cancellation passes through
  unclassified and propagates, as before.
- **Disproved, the new notice can strand open in normal operation:** a
  re-entered task falls through to the release, the task row is one per run
  and is rescanned, and workflow closure expires every open item bound to
  the task regardless of prefix.
- **Allowed, hold-only daemon:** a hold-only daemon never runs the
  publication lane's pending scan, so it cannot retire the notice. Retiring
  it requires the comparison that lane makes, so a hold-only daemon cannot
  truthfully retire it. The scope notice behaves the same way, and the notice
  text says the hold clears when a daemon that can authenticate the task
  runs.
- **Allowed, hand-deleted task row:** only the unreadable-task notice is
  released when its row disappears. The task-marker notice then waits for
  workflow closure or an operator decision. That needs a manual store edit.
- **Allowed, wording:** an unreadable operator-feedback marker reached
  through the successor check now shows the task-marker text. It no longer
  loops, and the text still names a held publication.
- **Test genuine:** with the parent commit's engine files, the test sees
  four remediation notices in four passes; with the fix, one. It covers the
  publication comparison branch. The successor and producer branches share
  its record path and are untested here.

## Notes

After #2101's fix, the exit run's trigger (feedback returned after a
remediation) no longer reaches this refusal. The regression test reaches
it with a publication task whose title the remediation never recorded,
which decodes cleanly and fails only the lane's comparison.

Revisit when another lane starts making a check the production lane cannot
see, or when quarantine notices gain an owning-lane field.
