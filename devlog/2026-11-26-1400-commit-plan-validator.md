# Writers Check a Commit Plan With the Importer's Own Code

#988 gives a writer `freeside-commit-plan validate`, which reports whether
the importer would honor the workspace's `.freeside-commit-plan.json`.
Cordoned launches find it on `PATH`; the Claude agent image carries it.
Tier A by path: `internal/importer/` and `internal/cordon/`. The command and
its package grant nothing: the importer still checks every plan as
untrusted data at handoff.

## The Validator Runs the Real Export and the Importer's Intake

Chose to export the workspace into a temporary directory with the
exporter's default options and run the importer's own intake and plan
check (`importer.CheckCommitPlan`) over a second, cheaper derivation of
the change set from git. A second derivation could disagree with the
importer about which paths changed, and a writer would then fix a plan
against the wrong set. Sharing the code makes that disagreement
impossible, at the cost of one more export per run. Measured on the
Freeside repository (about 5,500 tracked files): 41 seconds per run, under
4 of them CPU. A sample put nearly all the wait in the exporter's
per-blob sync (`atomicfile.Create`), which a real handoff needs and a
throwaway export doesn't.

`Import` was split into a read-only intake and a plan decision so both
callers run the same code. The split keeps `Import`'s order, notices, and
hooks; only the base check differs, since the validator runs after the
writer's own commits and needs the base to be a commit, not `HEAD`.

## Cordoned Launches Get a Shim to the Daemon's Own Executable

Chose a per-launch `run/tools/freeside-commit-plan` script that execs the
daemon's executable with the new `commit-plan` verb, first on the launch's
`PATH`, over installing a binary somewhere on `cordon.LaunchPath`. The shim
needs no install step on any host, always matches the daemon's importer,
and leaves `Config.Path` and the configuration digest unchanged (the digest
already names the daemon executable). The planner proposed this; the owner
may veto it.

The shim works only where the daemon's executable is readable inside the
cordon, which holds for the documented layout (`/opt/freeside/bin`). If a
layout breaks that, the command fails and the writer hands off anyway; the
importer's single-commit fallback still applies.

## What "Same Decision" Means

The issue defines "accept" as `Import` under `plan_preferred` returning a
commit with no commit-plan notice. The fixture table also requires every
finding to be advisory. Chose that over the issue's narrower wording because
a secret in a changed file makes `Import` build the commit with no notice,
yet the finding is fatal and the stage pipeline refuses the handoff. A
validator that passed that case would tell the writer to hand off work that
is about to be refused. The validator reports every finding that is fatal
under the default profile.

A workspace with no changes and no plan is honored, matching `Import`,
which builds the commit with no notice. With no changes and a plan present,
the validator reports `no_changes`, matching `Import`'s
`present_but_not_honored` notice.

## The Validator Checks the Default Policy

Chose the importer's default policy with plans honored over reading a
project's settings, which the command can't reach from inside a workspace.
A project's stricter path policy can still refuse a handoff the validator
passed; the README's Limits say so.

## Refute-First Record

- **The refactor changes no import decision: disproved by a check.** 400
  generated handoffs (random base and workspace trees, random plans with
  dropped, duplicated, and extra paths, bad messages, wrong versions,
  truncated JSON, reserved and git-metadata paths, plan secrets, misplaced
  remainders, file and directory swaps, both plan modes) ran through the
  old and new `Import`. Errors, findings, notices, and every commit's tree
  and message matched byte for byte.
- **The validator and `Import` can disagree: confirmed once, fixed.** The
  same generator compared `CheckCommitPlan` with `Import` on 343
  `plan_preferred` cases. One disagreed: no changes and no plan. It is
  fixed and the fixture table now has that row.
- **The seeded checkout may lose `refs/freeside/import-base`: disproved by a
  trace.** The ward stages every regular file except OS metadata
  (`internal/ward/seed.go`, `stageSeedSource`), the host launcher's seed tar
  carries the whole tree including `.git` (`internal/cordonhost/seed.go`,
  `writeSeed`), and nothing in `internal/ward`, `internal/cordonhost`,
  `internal/cordon`, or `internal/exec/stage` deletes a `refs/freeside/` ref.
- **The check may write to the workspace's repository: disproved.** The
  intake runs only read-only git under a scratch `HOME` and index; object
  and ref writes stay in commit construction. A test snapshots the worktree
  and `.git` before and after a run.

## Revisit When

- Writers skip the validator because a run takes too long. Drop the
  throwaway export's syncs first; a second derivation reopens the
  disagreement this design rules out.
- A project policy commonly refuses plans the default policy passes.
