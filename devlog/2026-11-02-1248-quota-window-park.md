# A Quota Refusal Parks On Durable Facts, Not The Hold Row

#2421 parks a task on a provider quota refusal until the refused window
resets (plan §4, §5.4, revision 103). It adds the `quota_window_exhausted`
hold reason, its typed detail, and an optional quota refusal on
`exec.StageResult`, so the note is owed as a contract change. These choices
outlast the unit, because the router (#2393) and collection (#1716) build on
the same mark.

## The Gate Reads Facts, The Hold Row Shows Them

The plan comment gated the parked attempt's start on its hold row. Chose to
gate on durable facts instead: the attempt's reason, its parent run's stored
terminal (which carries the refusal), the parent's admitted identity and so
its pool, and the pool's record. The hold row is a projection: any milestone
clears it, and another hold reason replaces it. Gating on it would let a
parked attempt start before its reset after an unrelated write. The row still
carries the pool, window, and reset, so the operator sees the same thing the
gate reads.

The attempt reason is free text an operator's reattempt can also carry, so
the gate holds only on a refusal it can read. A parent whose terminal names no
refusal, or whose admission names no pool, lets the attempt start. The
fresh-context review found that failing loud there instead returned out of the
dispatch pass and starved every later intent.

## A Park Needs A Reset Ahead And A Pool That Can Hold The Mark

Chose to raise today's `execution_failure` card, and park nothing, in four
cases the plan left open:

- **Not the root invocation.** A refused remediation round or operator
  feedback keeps the card. A fresh attempt would discard the run's reviewed
  work, and the gate reads only the root's terminal.

- **No reset ahead.** Neither the refusal nor the window's current reading
  names a reset after now. A reset already reached would start the attempt at
  once and loop on the next refusal.
- **No enrollment on the pool.** A pool with no enrollment has no collector
  to own a record, so no mark can be written and nothing could clear one.
- **No attempt to park.** A run outside a campaign or not its latest attempt,
  a closed task workflow, or a stopped task cannot allocate a next attempt.
  The check runs before the allocation, which would otherwise fail the whole
  terminal transaction.

The mark is still written whenever a reset and an enrollment exist, since it
is a fact about the pool whatever happens to the task.

## The Park Happens In The Terminal's Transaction

The refused terminal, the window's mark, the next attempt, and its hold
commit together. The plan's risk was that the attempt allocation might not
see its parent as final inside that transaction; it does, because the
terminal milestone is appended first and reads see the transaction's own
writes. No crash window leaves a refused run with neither a card nor a next
attempt.

Revisit when the router (#2393) chooses another pool on a refusal: the park
then competes with a reroute, and the gate's single-pool read no longer
covers the choice.

## The Park Reads The Pool Usage Record

#1716's containment test landed after this unit was written. It lets only
listed files name the pool usage record's readers. The park reads the
record twice: for the reset when the refusal names none, and at dispatch,
just before admission, for the window's mark.

Chose to list `quota_park.go` as a reader instead of reworking the park.
Plan §4 (revision 103) takes the reset from the refusal or a reading and
resumes early when a fresh reading clears the mark. The mark lives in the
record, so a park that never reads the record can do neither. #1716's own
containment commit expected the quota park to take the readers up. Routing
the read through `usagecollect` would hide it from the test without
changing it.

This leaves the plan in conflict with itself. §5.4 says the record has
"one reader above admission: the router" and that "admission never reads a
reading", which excludes the park. The park only delays an attempt on the
line it already has and selects no agent or credential, so it doesn't do
what §5.4 guards against. #2493 asks the owner to reconcile the two
sections. The owner left the choice of fix to this unit.
