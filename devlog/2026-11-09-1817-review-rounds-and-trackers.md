# Exit Reviews Run in Rounds and on Any Tracker

Work unit: a direct owner assignment, no issue. Mandatory note: it
records owner choices made in chat on 2026-11-09. The rules live in
`.claude/skills/review-wave/SKILL.md`; this note records why. It revises
the single-review stop that skill carried since
`2026-08-04-0823-review-wave-skill.md`.

The same change brought the skill in line with rules that had already
changed and carry their own notes: no lanes and no contract ordering
(`2026-10-26-1505-start-freely-validate-at-landing.md`), the Assurance
Tiers (`2026-11-03-1442-assurance-tiers.md`), and mechanism at its point
of use (`2026-11-03-1441-plan-content-rule.md`).

## Decision: A Tracker Can Be Reviewed in Rounds

The owner decided that an exit review can run more than once on a
tracker. The skill used to stop when a findings summary was already
there, treating a second prompt as a mistake.

Chose rounds over the single-review stop because the fixes a review
produces are merged code no fresh reviewer has read. Each round marks
its two comments, so the next run counts rounds from the markers instead
of guessing from headings.

The agent chose the later round's default scope, and the owner can ask
for a full review instead: the PRs that closed earlier findings, units
added since, and partitions an earlier round reported as sampled or not
read. Reading every unit again each round would spend most of the
reviewer's effort on code an earlier round already cleared.

## Decision: The Remediation Proposal Keeps Its Tracker Shape

The agent proposed replacing the tracker-shaped proposal comment with a
short list in the findings summary. Its evidence: the three fixes Wave
8's proposal selected (#1707, #1755, #1833) never reached #1613's body,
and the merge refresh maintains tracker bodies, not comments.

The owner rejected that: some findings need a tracker. The proposal
stays in tracker shape so the owner can adopt it as one, and a later
round's proposal carries forward the units still open, so the newest
comment is the one to read.

## Decision: Any Tracker Can Be Reviewed

The owner accepted that the skill takes a tracker issue number, as
exit-run does, and that a wave run reports the ad hoc trackers that
closed with no review.

The evidence: of the 259 PRs merged from 2026-10-28 to 2026-11-09, 43
closed Wave 9 units, 71,000 of 240,000 added lines. A review bound to
wave units reads less than a third of what lands.

The plan is unchanged. §11 still requires the review only at a wave's
exit; an ad hoc tracker's review runs when the owner asks for one. A
review of a closed tracker posts comments only, which the agent read as
consistent with "a closed tracker is never reopened or mutated": the
body and state stay as they were.

Rejected: reviewing everything merged since the last audited commit. It
covers all merged work, but it is too large for one reviewer and would
change the unit §11 reviews.

## Decision: One Prompt

The owner chose one prompt over one per partition. Wave 9 is 43 PRs,
more than one reviewer reads closely, so the prompt sets depth by
Assurance Tier and requires a coverage statement per partition. A later
round picks up what the statement reports as sampled.

Revisit when coverage statements keep reporting Tier A partitions as
sampled or not read.
