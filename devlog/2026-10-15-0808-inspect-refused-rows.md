# Inspect Reads a Stopped Root Row by Row

Work unit #2042. The issue body carries the contract and the plan comment
the design. This note records why the item list degrades row by row while
the timeline does not, the trust argument for a refused row, the options
the plan rejected, and what the refute-first pass found. The record
command itself and its earlier decisions are in
`2026-10-10-1742-task-record-inspect.md`.

## Decisions

**Items degrade row by row; the timeline stays fail-closed.** The record's
item list now reads through `store.ReadTx.ListAttentionItemRows`, which runs
the same reconstruction gates as `ListAttentionItems` but reports a refused
row in place. The timeline is `signet.ReadTaskTimeline`, the body of the
paired client's `GetTaskTimeline`, and the record prints it unchanged; making
it tolerant would change what the client sees, which the contract rules out.
So on a stopped root read with no recipes, `items` lists the task's items
with the refused ones marked, and `timeline_error` still reads
`unapproved_recipe` until the digests are passed. `ListAttentionItems` keeps
its fail-closed contract for the sync bootstrap, which must never omit
state.

**A refused row exposes its id, two subject columns, and the error, and
nothing decoded.** The subject columns are the row's own extracted
`subject_task_id` and `subject_run_id`. When reconstruction fails, the check
that binds them to the canonical body may not have run, so they are
unverified. The record uses them only to route a refused row to a task, and
prints only the row's id and error. A forged column can therefore at most
put a refused row's id and error into another task's record, or keep it out
of the record it belongs to; it cannot hide, alter, or mis-attribute an item
that reads, because a readable item's ownership comes from its verified
subject. The second case would make a record look complete while the store
refused one of its rows, which the old whole-list `items_error` did not
allow, so `counts.unattributed_refused_items` counts every refused row the
record did not place in the task. The error text is the store's own, which
the record already printed whole as `items_error` before this change.

**The read names the recipe and the command says what it needs.** The
evidence gate's `unapproved_recipe` error now names the recipe digest
(`domain.ErrUnapprovedRecipe` stays in the chain). A direct read that meets
the kind writes one note to stderr naming `-approved-recipe` and where a
stopped root's digests live. Stdout stays one JSON value, so an agent's
parser is unaffected. A served read gets no note: the running daemon's own
set is the authority there, and no flag adds to it.

## Rejected Options

**Filtering items by task in SQL, over reading every row.** A stall notice
has the system as its subject and ties to an invocation only through its id,
so a subject-column filter would drop the task's stall notices; and the
task's own refused rows still need a row-level answer, so the filter would
not remove the per-row read.

**A JSON field for the recipe hint, over stderr.** The hint is advice to the
operator about how the read was invoked, not a recorded fact about the task,
and a field would put it into the versioned shape that served and direct
reads must print identically.

## Where the Implementation Left the Plan

**The digest's location depends on the root.** The plan pointed operators at
`approved_recipe` in `<root>/daemon/settings.json`. Only a deputy root has
that file; a `scripts/run-real-work.sh` root, which is what the exit run that
found this used, takes the digest from `FREESIDE_REAL_RUN_APPROVED_RECIPE` and
records it nowhere in the root. The stderr note and the README name both.

## Refute-First Findings

A fresh-context reviewer tried to refute seven claims before commit.

- **Confirmed and fixed: a corrupt row could leave its task's record
  silently.** A row of task T whose stored subject columns no longer name T
  is refused and routed away, and before the fix nothing in T's record
  showed it. The record now counts refused rows it did not place
  (`unattributed_refused_items`), and a test forges the columns of one of
  the task's rows.
- **Disproved: the scan split changes behavior.** The cross-check body is
  unchanged; the column scan returns the same unwrapped errors, and the
  history and snapshot paths run the same gate sequences. The store suite's
  forged-row tests, which exercise every gate through both paths, pass
  unchanged. A generated-input comparison of old and new was not run: the
  split moves the `Scan` call and rebinds the same locals, so the forged-row
  suite is the comparison.
- **Disproved: the row read gates differently from the list.** Same SQL,
  order, and reconstruction function; a test holds the approved rows equal
  to `ListAttentionItems`.
- **Allowed: error text can quote stored values.** A few gates quote a claim
  label, a display name, or a repository. The old `items_error` printed the
  same text, for the first failing row of any task; the record now prints
  the text of each refused row placed in the task. The README already says
  a message is the store's own text and a few checks quote the value they
  refuse. The class is unchanged; the volume can grow.
- **Allowed: a database error inside a row's gates is reported as that
  row's.** The store cannot tell it from a refusal; a cancelled read still
  fails the whole record, because `readTaskRecord` checks the context last.
- **Disproved: the error wrap breaks callers.** It wraps with `%w`, and no
  test asserts the old text. With several unapproved recipes the message
  names the first; the stderr note asks for every digest the daemon ran
  with.

## Revisit When

- A second diagnostic read needs per-row attention items: the store could
  then offer a by-task read that keeps the stall-notice join.
- The real-work scripts start recording the approved digest in the root:
  the note and README can then point at one place.
