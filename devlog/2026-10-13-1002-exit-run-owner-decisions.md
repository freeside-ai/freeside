# Exit Runs Defer by Default and Leave Blocking and the Merge to the Owner

Three owner decisions from the gh-imgup#116 exit run for #1613 (Ben
Nelson-Weiss, in the run's session, 2026-10-13) change the exit-run skill.
The run's record is the exit-run comment on #1613.

## Findings Default to Deferred

Owner: "I think we can default to deferred in general- even if the ticket
says deferred doesn't mean we can't choose to block on it." And on wording:
"I think maybe we can list those findings as something like, \"suggested to
block on\"".

Chose a Deferred proposal with a "suggested to block on" marker over the
spine proposing Required. The spine had proposed Required for two of this
run's findings under the old rule (a defect that contradicts the tracker's
stated outcome is Required whatever its severity). The owner reversed that:
what blocks exit is the owner's call, and a `deferral` label does not
prevent it.

- The old test survives as the test for the marker, so the owner still sees
  which findings contradict the stated outcome.
- Required and Investigate remain dispositions, adopted by the owner only.
- The marker is a note on a Deferred finding, not a fourth disposition.
  This is the spine's reading, stated to the owner and not contradicted.
- A consequence the spine drew: a proposal now has no exit set, so the
  comment's start order is written when the owner adopts one.

Rejected: keeping Required as a spine proposal (it pre-empts the owner's
decision and reads as decided on the tracker); a fourth disposition (the
tracker body's rules key on Required, Investigate, and Deferred, and a
marker changes none of them).

Revisit when the owner adopts the suggested group unchanged for several
runs running, or when a suggested finding is deferred and then stops the
next run.

## Ready-Card Actions Are Not Exercised Every Run

Owner: "I think we can stop exercising the ready card actions every time-
they've worked every time so far."

Chose exercising them only when a tracker gate names one, a prior run left
a gap on it, or it changed since it last ran live, over the per-run
obligation the skill added after two Wave 7 runs lost the evidence to an
early merge. The live record behind "worked every time" is uneven, and the
skill now says so: view PR and mark seen have run on real cards on both
clients, return to agent has run live once (#1445 run-113), and dismiss has
never run on a real card and rests on its engine and client tests.

Revisit when those actions or the ready card change, or when a gate names
one.

## The Owner Merges Every Real-Work Pull Request

Owner: "Also, I _always_ merge real work PRs at this point in time."

The skill no longer lists the merge as an operator step to negotiate. The
spine collects the pre-merge evidence, hands the owner the link, and waits;
it never merges a real-work pull request and does not ask who will.

Revisit when Freeside gains a self-merge policy for real work.

## Decided Elsewhere

The owner's decision that one refused adjudication entry should not park
the whole batch ("Each finding should be acceptable or deniable") is carried
by #1990, which owns its plan change and its own note.
