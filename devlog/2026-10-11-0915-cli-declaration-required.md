# Require the Work-Unit Declaration on a CLI-Submitted Task

Work unit #1931. A task submitted with `freesided submit` and no work-unit
declaration published a pull request, the owner merged it, and the task
stayed "Ready": the daemon records completion against the declaration, and
there was none. A client-composed task and issue intake always declare one,
so only the CLI could create such a task. It now refuses to.

## Chose to Refuse Over Defaulting

Chose to refuse a new submission that has no declaration, in `freesided
submit`, on the daemon's side of the control socket, and in `freesided
preflight`. The operator writes the file, and the refusal names the smallest
one, `{"completion_criterion":"bound_pr_merged"}`. The owner settled this at
planning (2026-10-11). A refusal adds no field and changes no digest, so
every saved submission recomputes the identity it had.

Rejected: recording a default declaration for an undeclared CLI submission,
the way the daemon does for a client task. The declaration's digest joins the
run identity, and there are two ways to default it. Each breaks something:

- **A default with a digest** changes the identity. The recovery journal
  keeps the operator's raw files and submit recomputes everything from them,
  so an undeclared submission saved before the change would recompute a
  different run on retry.
- **A default with no digest** is refused twice over. Request validation
  requires a declaration to match its digest, and the engine will not
  declare a run it stored without one, so replaying an accepted undeclared
  submission would fail. Both checks would need exceptions.

The plan already says an operator-authored declaration stays a CLI input
(`docs/plan.md`, client task submission), and the capture decision
(`devlog/2026-08-01-2308-capture-hooks.md`) keeps an undeclared submission's
derivation byte-for-byte. Refusing keeps both.

## Left Accepted Undeclared Tasks As They Are

Chose not to backfill a declaration onto a run the database already holds. A
task accepted before the rule still replays, stays "Ready" after its merge,
and keeps its work-in-progress slot until `freesided abandon` releases it.
Finishing it would need the backfill, and explaining it in a client would
need an `api/` field; both are out of this unit.

The exemption has the shape the source rule (#1928) and the egress-policy
check use. The CLI lets an undeclared submission through only when its saved
journal exists, and the daemon accepts it only when the stored submission
row equals this one. A journal with no stored row creates no work. A legacy
run-id lookup is exempt because it never creates a task. Preflight reads the
journal alone, and requires the same task, an equivalent publication record,
and a journal that saved no declaration; submit stays the authority.

## Findings That Shaped the Change

- **The refusal text repeats no submitted bytes.** Preflight prints it in
  the composition manifest, so it is one fixed message.
- **Client-target mode needs the declaration too.** The harness submits its
  visibility seed through the CLI, so the fourth argument is required in
  both modes until #1332 removes the seed.
- **The harness warning about a source issue with no declaration is gone.**
  It described a submission the daemon accepted; the daemon now refuses it,
  so the case the warning explained cannot be reached.
- **The merge half is not joined to a real submission in one test.** A test
  drives a declared CLI submission through specification approval and
  asserts the implementation run carries the `bound_pr_merged` declaration.
  The completion tests start from a run holding that same declaration. One
  test spanning both would rebuild the engine composition around fixtures
  written for a hand-seeded run, which cost more than the seam it covers.

## Refute-First Findings

An independent pass tried to prove the refusal wrong at the control-socket
boundary, by code trace, scratch tests, and twelve mutations of the three
production files.

- **Disproved: an undeclared request can create work.** With no declaration
  the write never commits: a new submission is refused unless the stored row
  equals it, and then it only replays. A run-id lookup never creates. A
  digest with no declaration fails request validation.
- **Disproved: a submission the database did not accept can pass as
  accepted.** The stored row must equal the request's, and its request
  digest is recomputed on the daemon's side from the validated digests,
  including the declaration's and the task lines'.
- **Disproved: a refused submission leaves something behind.** The CLI
  refuses before the journal is saved and before any store opens; the daemon
  refuses before any bytes or rows are written.
- **Disproved: the preflight exemption can be borrowed.** It fails for an
  empty database path, another identity, a journal copied under another
  identity, and a journal that saved a declaration. Preflight takes the
  database path only from the authenticated rig.
- **Allowed: an accepted undeclared submission whose journal was deleted by
  hand no longer replays by repeating its identity.** The CLI refuses before
  it can ask the database, which is the shape the source rule already has.
  Nothing in the repository deletes a journal, and the run-id lookup still
  returns the run. Asking the database first would need a read before the
  journal is saved, for a state only manual deletion produces.
- **Allowed: one error reads differently.** An undeclared request that
  conflicts with a stored row now fails as "inspect saved submission", still
  an immutable conflict, because the stored row is read before the write.
- **Confirmed and fixed: no test held the journal to its own identity.** A
  mutation that dropped the check survived, so a test now files a copied
  journal under another identity and expects the refusal.

## Revisit When

Revisit defaulting when the run identity stops depending on the
declaration's digest, or when a submission format arrives whose journal does
not recompute identity from the operator's raw files: a default would then
break no saved submission.
