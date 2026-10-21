# Run `await-pr-review` at Handoff Again, on CI Only

The owner told the agent handing off PR #2124 (2026-10-21) to run
`/await-pr-review`, after AGENTS.md had told it to skip the skill. This
note revises one record choice in
`devlog/2026-10-21-0848-retire-codex-reviewer.md`: the project-level
override that banned the handoff review watch. The owner's decision there
stands: Freeside's PRs still have no automated reviewer.

The assumption that changed is the skill's behaviour with no reviewer. The
earlier note banned it because, finding no record, it scanned recent PRs,
identified Codex from its old reviews, and waited out its cap on every PR.
The installed skill now reads an owner's `none` reviewer record
(`references/detection.md` §declared-no-reviewer). With that record it
spawns no conductor, scans no past PRs, records no reviewer, and only waits
on the PR's CI checks. That meets the earlier note's "Revisit when"
condition, except that the skill still waits on CI rather than stopping.

Chose to invoke the skill as step 1 of Handing Off the PR over keeping the
ban. The managed handoff text already says to use an installed review-watch
skill, so removing the project-level override lets that rule apply unchanged. The
skill's CI wait also confirms a pass on two consecutive reads, which catches
the fan-in `required` check that appears only after its jobs finish.

Chose to add a `- **Reviewer:** \`none\`` line to `## Automated Reviewer`
over relying on the existing records. The section's bold **None.** isn't in
the record form the skill reads, and the skill says a landing record's
`Reviewer: none` alone means only that no reviewer is recorded, so it
doesn't skip the review exchange. Without the new line the skill would fall
back to scanning past PRs and find Codex again.

Not changed: the pre-push fresh-context review (§pre-push-review), and the
rule that stray Codex activity is no reviewer to record.

Revisit when automatic review is turned back on for this repository
(replace the `none` record with a full reviewer record), or when the skill
changes how it finds a `none` record.
