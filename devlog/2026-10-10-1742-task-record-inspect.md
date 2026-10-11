# A Task Record for Agents, Built in the Command

Work unit #1936, unit 1 (the task record). The issue body carries the
contract and the plan comment the design. This note records the two options
the plan rejected, the places the implementation left its plan, and what the
refute-first pass found.

## Rejected Options

**Built the record in `cmd/freesided`, over `internal/observe`.** `observe`
is where `follow` lives, and a second observation read looks like it belongs
beside it. Its containment test is an import allowlist that leaves the store
out on purpose. The record joins items, commands, deliveries, admissions, and
the timeline in one read transaction, so it needs the store. Widening the
allowlist would hand `follow` the store's whole method surface to save one
file. The record reads through `store.ReadTx` in the command package, where
the other store commands already live.

**Left four facts off the record, over adding store accessors for them.** No
existing accessor returns the device that sent a Stop, cancellations before
the latest one or hold history, check proofs listed by run, or token counts
in the same transaction. Each needs a new shared-package read, which is
contract work and not this unit's. The README lists the four so a reader
knows the absence is a limit of the record, not of the task.

## Where the Implementation Left the Contract

**An item's `reason` is left off (agent's call; the owner can reverse it).**
The contract asks for "the reason line the card shows" and, in the next
clause, says the record never holds claims, conversation messages, or
workspace identity. The plan included the reason and noted that planning had
not checked every writer. The refute-first pass checked them. A
`spec_approval` reason is the specification summary, the same string as the
item's Summary claim. An `agent_question` reason is the agent's question
verbatim. A `finding_adjudication` reason quotes the external reviewer's
finding. An execution failure's reason appends the driver's error, which can
name a host export path. The two clauses cannot both hold, and the "never"
clause is the one whose breach cannot be undone after a record is pasted
somewhere. The typed causes stay. Restoring the field is one line and a
golden update; restoring it for an audited subset of item types needs an
audit of every writer and a rule that keeps it current.

**A failed stop has no reason field.** The contract asks for a typed reason
on every failure, `null` where the store holds none. The store holds no
reason for a failed stop at all, only the acknowledgement's state and
evidence digest. The record gives those two and adds no field that could
only ever be `null`.

## Where the Implementation Left the Plan

**The run conclusion is the authenticated one, over `domain.ConcludeRun`.**
The plan named `ConcludeRun`. It classifies from milestones alone, so it
reports a block that a later accepted reevaluation resolved, and a completion
that nothing authenticates. `follow` reads the authenticated conclusion, and
two reads that disagree about one run would send an agent after a difference
between readers. The record runs the same three calls `observedb` does; a
test holds the two equal for every fixture run, the refused one included.

**Each run lists its own errors, by section.** The plan gave errors to the
timeline and the item list only. The contract says only an unknown task or a
failed transaction fails the command, and a run's reads can each fail a
store check. Each read is attempted on its own and a failure names its
section, so a null field means "not read" exactly when the errors say so.
The first version returned at the first failure; the review showed one
unreadable admission row in another task then hid every run's hold and
conclusion.

**The task section can be null too.** A task row the store refuses is a
broken task, not an unknown one, so the record reports `task_error` and
reads the rest. A "not found" still fails the command: the store gives that
answer both for an unknown id and for a task missing one of its own rows,
and no read tells them apart.

**Invocation statuses are the driver's last observations, not signet's
authoritative overlay.** The overlay is unexported and assumes an
authenticated observation; applied to a run that failed authentication it
dereferences a milestone that is not there. The record gives the raw
observation and says so. `conclusion.terminal_status` and the timeline stay
the authoritative outcome.

**The timeline read is a function of the caller's transaction.**
`signet.ReadTaskTimeline` is `GetTaskTimeline`'s body with the transaction
and the read time passed in. Nesting `Store.Read` is not safe, and one
revision for the whole record is the property an agent waits on.

**Smaller choices.** Item times are spelled `created_at` and `decided_at`,
as the item spells them. The image is the admission's digest-pinned
`image_ref`. `-approved-recipe` applies only with no daemon. The index's
command lines use `PATH`-style placeholders: angle brackets come out of the
JSON encoder as `<`.

**The index lists the commands that print stored state as JSON and write
nothing.** That is `inspect`, `inspect task`, `follow -snapshot`,
`comprehension`, and `auth list`; the first version left `auth list` out and
the review caught it. `doctor` writes. `preflight` and the review pass of
`approve-shadow-review` print a host check and a proposal, not stored state.
`publication-formats` prints build constants and takes no database. Listing
`auth list` puts no identity in the index, only the command's name.

## Refute-First Findings

An independent reviewer tried to break eight claims about the change.

Confirmed and fixed:

- **`reason` carried agent, reviewer, and driver text.** Left off, above.
- **One early return per run hid facts that read fine,** and a null hold,
  conclusion, policy digest, or cost meant both "none" and "not read". Fixed
  by the per-section errors.
- **A refused task row failed the whole command.** Fixed by `task_error`.

Confirmed and allowed:

- **The timeline carries a display name an agent may have written, and an
  operator's reattempt reason.** The contract asks for the timeline a client
  gets, unchanged; the README names both.
- **An error message is the store's text, and a few checks quote the value
  they refuse** (a decision's question, a display name). It takes an invalid
  stored row to reach one, and the message is what says which check failed.
- **A failure the store does not type prints as kind `other` and the command
  exits 0,** a database error included. `store.IsRowVerdict` draws the
  store's line between a row's verdict and an infrastructure failure, but
  the reviewer reached a check that fails with a raw `sql: no rows` (a run
  removed from its task's membership), which that line calls infrastructure.
  Failing on it would drop the whole record for a broken task. The error and
  its message are in the output either way, so nothing reads as absent.
- **A run removed from the task's membership rows leaves the record
  silently.** Membership is what the store says it is; no other read lists
  a task's runs.
- **With no daemon the command creates `<db>.daemon.lock`,** as every store
  command does, and opens the store read-write. The read functions take a
  `store.ReadTx`, which has no write method.
- **`inspect` fails when the task list does.** It prints the store's error
  and exits 1. The contract's broken-task clause is about the record.

Disproved by a check:

- **The `signet` extraction changes behavior.** The reviewer compared the
  two versions: the same error chains, with the row-verdict wrap still after
  the task snapshot. `as_of` is now taken before the reads, and it is a
  stamp only.
- **The stall-notice matcher changed.** It is the old closure, and an
  invocation id that extends another's never matches.
- **The record's conclusion differs from `follow`'s.** The same three calls
  in the same order.
- **A second transaction is opened.** `ReadTx` holds one `*sql.Tx`, and no
  callee takes a `*store.Store`.
- **The control routes widen access.** They are `GET`, call `Store.Read`,
  and sit behind the socket's owner-only directory and peer-UID check.
- **An admission leaks workspace or auth identity.** The projection lists
  its fields; neither is among them.

## Known Limits

- **The item list is all or nothing,** and so is the admissions read. The
  store checks every row when it lists them, and one failing row fails the
  list for every task.
- **Cost and the revision.** Usage rows are written without advancing the
  store revision, so two records at one `as_of_revision` can differ in
  `billable_cost`.
- **No test reads through an unapproved recipe.** The fixture's evidence
  names none. The classification is tested on the error alone.
- **A membership row that names another task's run is believed.** It takes
  a hand-edited `task_runs` row. The record then reports `integrity` on the
  timeline, on that run's conclusion, and on the item list when the run has
  items, and still prints the run's admissions, cost, hold, and invocations.
  The store gives one verdict for every check on a run's row, so the command
  cannot tell a foreign run from a run whose row is broken some other way,
  and it keeps the reads that do not need the row.

Revisit when #1934's procedure or #1938's proof needs one of the four facts
left off, when the owner wants an item's reason on the record, or when the
store types the errors it now leaves raw.
