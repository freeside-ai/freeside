# Unit Sizing Counts Authored Lines

A revision to docs/coordination.md (Unit Sizing) that amends
`devlog/2026-08-18-0818-unit-size-budget.md`. The owner supported the
direction in a workflow assessment on 2026-10-02 and decided its bounds the
same day. The assessment is not committed; #1728 and this note are the
record. The policy wording is the agent's and is open to the owner's review.

## Chose Authored Lines Over Every Changed Line

Chose to count authored changed lines against the budget, over counting
every changed line, because a regenerated line does not cost a reviewer
what an authored line costs.

**Changed assumption: regenerated output was counted as review effort.**
The 2026-08-18 amplifier list named golden regeneration and the generated
app client as reasons to presume a unit over budget, and its evidence table
bucketed pull requests by total lines added. Both treat a regenerated line
as equal to an authored one. A reviewer weighs an authored line, because
someone decided it. For regenerated output the reviewer confirms that the
command reproduces it, then scans the diff for change the authored lines
don't explain.

**Excluded from the count, not from review.** #1728's objective says a
reviewer verifies regenerated output "by rerunning that command, not by
reading it". The policy text stops short of that. `daemon/README.md` has a
golden's diff reviewed, because that diff is where an unintended shape
change shows, and rerunning the command only proves the golden matches the
code. This is the agent's correction, open to the owner's review.

**One test separates the two.** Output is regenerated when a documented
command reproduces the file from tracked inputs. Everything else is
authored, including a fixture edited line by line. Regenerated output is
declared beside the estimate, so its verification stays visible, and left
out of the count.

**The text says "authored", not "handwritten".** Agents write most of this
repository, and "handwritten" reads there as "typed by a person". A
planning clarification on 2026-10-05, recorded on #1728, fixed the meaning:
authored by an agent or a person.

**The presumption stays for the amplifiers that add authored lines.** A
unit touching two or more of a new migration, a sync-carried contract
field's schema edit, and new mock state is still presumed over budget. Only
the regenerated entries left the list. This is the agent's reading of the
owner's direction.

## Kept 1,000

The owner kept the figure. Rejected a new line cutoff: the assessment
sampled 40 recent pull requests and saw findings drop in every size bucket,
including under 500 lines, so the sample supports no new universal cutoff.
The sample's data is not committed, and this note records only that
conclusion.

**The budget is looser than the one the 2026-08-18 table measured.** 1,000
authored lines admits a larger total diff than 1,000 total lines. The table
in that note is evidence for the old count, not for this one.

## Revisit When

- Units under the authored-line budget that carry large regenerated output
  draw review rounds the way over-budget units did. That would mean
  regenerated lines cost more review than this note assumes.
- A file's regeneration command stops being documented or stops reproducing
  the file from tracked inputs. The file is then authored and counts.
