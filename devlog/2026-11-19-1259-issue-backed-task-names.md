# Issue-Backed Task Names and Finished Outcomes

**Decision (owner, 19 Nov 2026, #3456, finding F39 from #2382's exit
run):** a task submitted as one issue's URL is issue-backed like a
label-intake task. It takes the issue's title as its name, the API's
`Task.issue` names the issue for both kinds, and each Finished row draws
how its task ended. The owner saw two abandoned attempts at #2968 under
different agent and specification titles and read them as unrelated tasks.

## Issue-Backed by the Submitted Text

Chose "the submitted text, trimmed, is exactly one canonical issue URL in
the project's managed repository" over parsing an issue reference out of
any text. It is the rule `engine.TaskSourceIssue` already applied for
research, so the namer, triage, and the new field agree on one test. A URL
for another repository is not issue-backed: the daemon's token reads only
the project's repository, and a foreign issue would name a task with text
nobody verified.

The submitted issue is a separate stored field (`Task.SubmittedIssue`), not
a rewrite of the task's `work_item_artifact` source. The source records how
the task arrived and keys intake; the issue is descriptive and grants
nothing. Stored tasks are not backfilled: a task submitted before this
change keeps its name and gets no issue. A label-intake task already
carries its issue, so one still running takes the issue title at its next
dispatch. The implementation accepted that rename as the point of one name
per issue, rather than adding a cutoff to spare tasks already running.

## Name Precedence: Operator, Issue, Specification, Agent, Identifier

Chose to let the issue title replace a specification title, before or
after approval, over freezing names at approval as `docs/plan.md` §5.12
did. The point is that every task for one issue carries one name, and
specification titles differ from run to run, which is how F39's two
attempts came to read as different work. Only an operator name outranks
the issue title. The issue title, like any name, grants no authority.

The title is read once through the research fetcher, under the namer's
own artifact id and purpose, so a retried read recovers the same fetch.
A blank title, or one the credential scan refuses, falls back to the agent
namer. So does a failed read for a submitted task, whose text the agent
can name without the issue; the issue title replaces that name when a
later dispatch reads it. A label-intake task's agent name needs the same
issue, so it waits for that retry.

## Finished Outcomes, Drawn and Spoken

Finished rows draw the outcome as a timeline marker before the name:
filled `diffAdd` green for `finished`, filled `waxText` for `stopped`, a
hollow ring for `abandoned`. VoiceOver speaks the outcome, since the first
two differ by color alone. An issue-backed row adds a context line of
project and issue number in the face Working rows use. iPhone draws the
same row.

This reverses "The status those lines drew ... is spoken but no longer
drawn" in `devlog/2026-11-11-0830-sidebar-sections-at-foot.md` for the
outcome. F39 is the condition that note's Revisit When names. The recorded
status word is still spoken, not drawn, when it says more than the outcome.

`diffAdd` now has two uses beyond diffs: the task timeline's stage-start
marker and this finished marker.

## Declined

- **An outcome keyword line or trailing chip** on each Finished row. The
  marker says it in less width.
- **`First try` and `Retry N` labels.** The API holds no link between
  tasks, so the app would have to infer them from submission order.
- **Folding earlier tasks under the newest one.** It would put finished
  tasks under Working and break Finished's count.
- **A declared "resubmission of task X" link.** It is a daemon and API
  change of its own; the shared issue, title, and markers already say "one
  issue, two abandoned".

Revisit when an operator still can't tell attempts at one issue apart, or
when tasks gain a declared link to one another.
