# Review-Loop Bounds

Chose to keep the current `agent-setup` managed convergence text and bind it
to Freeside's existing review policy in project-specific guidance, rather than
forking the managed block or changing the plan. The plan makes round counts
emergency brakes and requires policy exhaustion or ambiguity to produce a
durable `AttentionItem`; the managed thrash rule is an additional stop
condition, not a replacement for that bound.

The rejected alternatives were to leave the canonical wording unqualified,
which could permit an unbounded blocker loop, or to edit the shared managed
block, which would make this project drift from the current skill. Changing the
plan would be a separate material policy decision outside this sync.

The review finding that prompted this clarification was confirmed against
`docs/plan.md` §§2 and 7. The project-specific rule now preserves both
yield-driven remediation and bounded exhaustion/escalation.

Revisit when the managed canonical text explicitly describes project-policy
bounds, or when the plan changes the review-remediation contract.
