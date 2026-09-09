# Publication Replay And Completion Time

For #1258, chose to accept a nonzero replay commit date at or before export
completion, instead of requiring equal timestamps. This completes the
separation recorded in [Export Completion Time](2026-09-08-1901-export-completion-time.md):
the driver pins the commit date at invocation start and records completion
after the returned handoff. Equality rejected normal live exports at the
atomic export/publication-task boundary.

The same chronology check applies when recording, reconstructing, and
authenticating terminal publication. Existing exports with equal timestamps
remain valid. Rewriting the replay date to completion was rejected because
it changes the candidate commit and breaks deterministic head reconstruction.
Neither timestamp is rewritten, and no schema or stored-row migration is
needed.

Chronology does not authorize publication. Identity, admission, manifests,
commit plan, approved policy, and exact reconstructed head remain separate
gates. A changed commit date can satisfy chronology but must still reproduce
the immutable export head before verification or publication can run.

The refutation pass found no additional defect. A regression test disproves
the concern that a chronologically valid altered commit date could publish:
head reconstruction refuses it before verification or forge writes. Existing
coverage rejects a date after completion. Equal and later completion cases
exercise atomic recording, backup restoration, and terminal reconciliation;
successor tests retain the expected-old-head update gate.

Revisit when execution no longer pins commit identity at invocation start,
or export completion is given a different durable lifecycle meaning.
