# Review Gate File

Decision record for #558, which moved the §7 review gate out of
`daemon/internal/engine/production_publication.go` into
`production_review_gate.go` with no behavior change. The owner set the
boundary in the issue contract; the agent executed it.

## The Boundary Is the Gate, Not Everything That Mentions Review

Chose to move the gate's own machinery over every function whose name
mentions review, because the gate is the one cohesive part of the file
that the publication path only calls into. What moved: gate state and its
constants, review request construction, the review workspace lifecycle,
`reconcileReviewGate`, failure classification and retry backoff, review
configuration parking, review attention and escalation, and review
admission. Together they were 2,163 of the file's 8,216 lines.

What stayed in `production_publication.go`, and why:

- **`exactBaseReviewInstructions`** composes the instructions the
  publication path hands the gate; it is input to the gate, not the gate.
- **The readiness functions** (`productionReadinessResolutions`,
  `currentReadinessVerdict`, `assertReviewedCandidate`,
  `holdReviewConfigurationMismatch`, `adoptedReviewProfileDigest`)
  reconstruct readiness at publication time. They read the gate's results
  but run on the publication path, so they belong with it.
- **The item-ID helpers** (`productionReviewItemID`,
  `productionReviewHardLimitItemID`) key attention items for the task as a
  whole, alongside the file's other item-ID helpers.
- **`reconcileTask`** already only calls `reconcileReviewGate` and switches
  on its four states, so it needed no change.

Chose one PR over the 1,000-line budget over splitting the move in two,
an owner decision: the moved lines are checked by byte comparison rather
than read, and two PRs would double the comparison and review without
reducing what a reviewer weighs.

## The New File Is Tier A

Chose to list `production_review_gate.go` under "Publication decisions in
the engine" in the AGENTS.md Tier A paths, because the gate decides whether
a candidate reaches publication at all. Before the move it was Tier A only
by sharing a file with the publication replay; after it, it needs its own
entry or a later change to it would escape the Tier A bar.

## Refute-First Evidence

For a verbatim move, equivalence is the claim to refute, and two byte
comparisons against the base revision measure it:

- The new file, after its package clause and import block, equals the two
  base blocks in order, with nothing else.
- `production_publication.go` equals the base file with both blocks
  removed, except the two imports the move left unused (`diffscope`,
  `gitrun`).

Neither block declares a package-level `var` or `init`, so the move cannot
change initialization order. The only file-name-keyed reference outside
Markdown, the hold-exit enumeration in `hold_exit_enumeration_test.go`,
follows `reconcileReviewGate` to the new file with its reason unchanged.

Revisit when the readiness reconstruction grows enough to stand as its own
file, or a later change makes the gate call back into publication helpers
that stayed behind.
