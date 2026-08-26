# Require Explicit Tracker Collector Provenance

Chose an explicit `--direct` operator assertion over inferring a direct
session-contained unit from zero attributed closing issues. An empty attributed
set cannot distinguish a prompt-backed direct assignment from an issue-backed
unit whose issue was manually closed, whose closing keyword was removed, or
whose pull request merged into a non-default branch. Without the assertion the
collector now records `AMBIGUOUS unit-origin`; an assertion that contradicts an
attributed closing issue fails hard.

Chose the latest GraphQL `ClosedEvent.closer`, bound to the requested pull
request and exact merge commit, over trusting `closingIssuesReferences` as
closure attribution. Closing-keyword references are candidates only and can
survive a closure they did not cause. The returned-object gate rejects absent,
malformed, unknown, incomplete, and cross-page inconsistent identities, while
an explicit null or different valid closer remains unattributed evidence.
Issue identity and stamps are likewise bound to the first closing-PR page so a
pagination race cannot mix relationship evidence from different issue versions.

Rejected preserving issue #935's older inference that zero closing issues
proves a direct unit. Current AGENTS provenance policy requires the prompt-backed
work contract that forge absence cannot establish. Also rejected making every
zero-attribution run permanently ambiguous, because an explicit operator
assertion can carry the missing provenance without inventing forge evidence.

Chose exactly one **Implementation order** section as the minimum structural
evidence that an issue containing a unit checkbox is a tracker. A repository-wide
checkbox scan otherwise nominates ordinary work-item acceptance lists as
containing trackers. Invalid numeric checkbox and relationship references now
remain ambiguity evidence instead of collapsing to issue zero or disappearing;
marker claim IDs must likewise parse as positive 64-bit integers, and a
planning-reservation marker binds exactly one valid `Plan: #N` line to its
enclosing issue rather than making the marker alone authoritative.

Revisit when the work-unit or pull-request contract gains a durable forge field
that authoritatively distinguishes prompt-backed direct assignments from
issue-backed units, or when the repository's tracker-format contract stops
requiring an Implementation order section.
