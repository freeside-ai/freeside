# Unit Sizing Counts Authored Lines and Admits a Contract's First Consumer

Two revisions to docs/coordination.md (Unit Sizing). Both amend
`devlog/2026-08-18-0818-unit-size-budget.md`. The owner supported both
directions in a workflow assessment on 2026-10-02 and decided their bounds
the same day, including that they land as one unit because they edit the
same seams list and the same decision. The assessment is not committed;
#1728 and this note are the record. The policy wording is the agent's and
is open to the owner's review.

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

## Chose a Combined Contract Unit Over Always Splitting

Chose to let a `kind:contract` unit carry its first behavioral consumer,
over always splitting a new field from the behavior that uses it. The
always-split seam cost two setups, two handoffs, and an intermediate pull
request with no usable behavior.

**Changed assumption: the contract-first seam had no contract-first
evidence.** The 2026-08-18 note measured a persistence-first case: #741
landed the migration and store, then #742 and #743 consumed it. The
contract-first seam rested on that evidence. No contract-first split was
measured.

**The split stays the default.** It still wins when the parts are each
valuable alone, when the contract serves several consumers, or when
splitting isolates design uncertainty. The exception needs two conditions:
the contract change and the consumer can be understood and verified
together, and no other contract unit is waiting for the slot.

**Bounded by slot availability, not by diff share** (owner, 2026-10-02).
Rejected bounding the consumer by its share of the diff. A contract unit's
open pull request is an active claim that never expires and counts toward
the cap of two (`devlog/2026-10-05-0904-contract-units-no-merges-after.md`).
A combined unit stays open longer than the contract alone would, so what
it costs is a slot other contract units can't use.

**"Waiting for the slot" is the agent's reading,** open to the owner's
review. Another open contract unit is waiting when it is scheduled, about
to be scheduled by the wave planning in progress, or holds a planning
reservation, and the combined unit is what would hold it back. That covers
a `starts-after` on the combined unit, an `exclusive-with` pair, a pair
the spine has not assessed as independent with the consumer included, and
a unit that only the cap would hold back because the combined unit holds
the slot it needs. An independence record assessed the contract change
alone, and the fold withdraws it, so a pair counts as clear only once the
spine has assessed it with the consumer included. A unit with an active
claim has already started, so it is never the one waiting. The reading
uses only states the contract regime already defines, so it adds no new
record and no new gate.
It is checked where a unit is shaped, at wave decomposition and at
planning, because Unit Sizing is decomposition guidance and not an
authorization door.

**Rejected "listed on an open tracker" as the sign of waiting.** #1728's
plan proposed it, together with "a dormant deferral is not waiting". The
two disagree on an ad hoc tracker, which lists unscheduled deferrals: #1616
lists #1425 and #1600, both `deferral` with no milestone. Chose scheduling
and the planning reservation, the states the contract gate already uses to
decide whether a unit counts. Wave planning publishes a wave's milestones
and listing together at its end, so a unit it is about to schedule counts
too; otherwise the check at wave decomposition would see none of the wave's
own contract units.

**Rejected a count of units against the cap.** The cap test could count
the combined unit, the units with active claims, and the candidates ready
to start, and compare the total with the cap. Every wording of that count
gave a wrong answer in some case: it counted a claimed peer twice, or read
the combined unit's own wait for a slot as another unit's. Chose a causal
test: a candidate waits when only the cap would hold it back and the
combined unit holds the slot it needs. The spine applies it with judgment
at wave decomposition and at planning, so it needs no arithmetic.

**The combined unit stays a contract unit.** It keeps the `kind:contract`
label, contract review, regenerated-output verification, and the rule that
every further consumer waits for its merge. Lane work still makes no
shared-package edit in passing.

## Revisit When

- Units under the authored-line budget that carry large regenerated output
  draw review rounds the way over-budget units did. That would mean
  regenerated lines cost more review than this note assumes.
- A file's regeneration command stops being documented or stops reproducing
  the file from tracked inputs. The file is then authored and counts.
- A later wave can compare combined contract units with split ones on the
  owner's metric: complete feature slices delivered, repeated setup and
  verification cycles, and time to usable behavior.
- Combined units hold contract slots while other contract units queue. The
  slot condition is then too weak, or is not being checked.
