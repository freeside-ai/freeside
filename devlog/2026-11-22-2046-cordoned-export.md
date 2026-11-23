# A Cordoned Export Runs in the Daemon's Process and Refuses Hard Links

`cordon.Backend.Export` turns a cordoned launch with a clean, proven end
into the handoff the importer takes, and `AuthenticateReleasedExport`
checks a released export against what `Export` recorded. Nothing calls
either yet. Issue #3467, tracker #3462. The choices below are the agent's
implementation defaults under the plan comment on #3467 unless a line
says otherwise.

## Chose the Export Package's Walk in the Daemon's Process

The ward runs `freeside-export` in a networkless VM over a read-only
remount of the workspace. The host has neither. `Export` calls
`export.Export` directly, in the daemon's process, over an `os.Root` on
the worktree. It stands in for the VM in three ways:

- **Nothing of the run is alive.** `Export` refuses without the end
  record (`ctl/ended.json`) that `Launch` writes only after the run-end
  proof passed and the output closed.
- **Nothing else writes the tree.** Every launch's rules deny writes under
  `RunsRoot` outside its own `RUN_ROOT`, and the output lands in
  `<RunsRoot>/<RunID>/export`, beside `run/` and `ctl/`.
- **Nothing from the worktree runs.** The walk starts no git and no other
  program; a test plants a hook, a filter driver, and `core.fsmonitor`
  and shows none runs.

Rejected:

- **A host build of `freeside-export` started under a rule file.**
  Seatbelt rules match paths, so they don't stop a read through a hard
  link (below), and the daemon would have to locate and trust a second
  binary.
- **Re-executing `freesided` as the exporter.** It adds a process
  boundary without a privilege boundary, since it runs as the same
  account, and the daemon's commands are outside this unit's scope.

Where it is weaker than the ward, stated so a later unit can close it:

- **No second VM.** A defect in the walk, the `.gitignore` hint, or the
  evidence descriptor parser runs with the daemon's privileges.
- **No read-only mount.** The tree holds still because no process of the
  run is alive and other launches are denied, not because the kernel
  forbids writes. A process outside every launch, running as the owner,
  could still change it.
- **The launch's own writes can reach host files.** The rules let a
  launch write RUN_ROOT itself, so it can leave `run/` or `run/worktree` a
  symlink to any directory the account reads, and `os.OpenRoot` follows a
  symlink in the path it opens. `Export` opens the worktree through the
  run's own directory, which the launch can't write, refuses either
  component as a symlink, and checks the opened handle is the directory
  it inspected. Hard links are the other way in; the next section closes
  them.

## Chose to Refuse Every Hard-Linked File

On the host, a hard link in the worktree can name any file the account
can read, and the exporter would copy it with the daemon's access. The
cordoned export sets `export.Options.RefuseLinkedFiles`, which fails on
any regular file with more than one link wherever the export reads: the
repo walk, the evidence descriptor, each evidence source, and the commit
plan. The last three are read outside the walk, so a check only in the
walk would have let a linked `.freeside-commit-plan.json` or evidence
source through. The check uses the lstat each path already makes, before
the file is opened. A platform that can't report the count refuses.

The ward leaves the option off: its exporter VM sees only the workspace
volume, where a link can't reach a host file.

Rejected: **refusing only links to files outside the worktree.** A link's
other names aren't discoverable from the file, so the export can't tell an
inside link from an outside one. Git checkouts write fresh files, so a
seeded tree has none; the darwin test shows a real launch's worktree
exports.

Whether a launch can hard-link a file its rules deny reading is
untested. If it can, the agent can read the secret itself, which is a
hole in the cordon and belongs in the conformance probes (#3470), not
only here.

## Chose Records in ctl Over a Store Journal

The end and export records are JSON files in the launch's `ctl/`
directory, written once with `atomicfile.WriteFileNoReplace`. `ctl/` sits
under a protected root that every launch is denied, so the records say
only what the daemon wrote. The export record binds the digests of
`manifest.json`, `evidence.json`, and the commit plan, with null for an
absent member; the importer checks every blob against the manifest, so
those three bind the whole export.

Rejected: **a store journal, as the ward's handoff journal is.** A new
table is a migration and a contract unit, and the stage driver port that
would read it isn't written yet. The port unit can move the records into
the store if restart recovery (#3468) needs them there.

## Chose to Build the Export Before the Driver Port

#3466's planning had this unit wait for a `kind:contract` driver unit.
Planning for #3467 reversed the order: the driver holds every handoff to
ward-only facts (`handoffSpec` in `internal/exec/stage/driver.go`), so
the port is written against a launch and export API that must exist
first. This unit builds the export behind the package's API, with
`ExportResult` mirroring the fields the driver's `exportOutcome` takes
from a warded handoff, and `AuthenticateReleasedExport` matching
`stage.Gate`'s signature without implementing the port.

Revisit when the driver port unit is written: it may want the export
inside `Handoff`, a different result shape, or the records in the store.

## Refute-First Findings

One fresh-context reviewer tried to prove the change wrong (pre-push
review, which also served as the refute-first pass).

- **Confirmed, fixed: a replaced worktree.** The agent could swap
  `run/worktree` for a symlink to an owner directory, and the export would
  read it, and create and remove files in it, with the daemon's access.
  `openWorktree` now refuses that; `TestExportRefusesAReplacedWorktree`
  covers both components.
- **Confirmed, fixed: two concurrent exports of one run.** Both could pass
  the existing-export check and write blobs into one directory. Nothing
  calls `Export` yet, and authentication would have refused the mix, but
  an exclusive `Mkdir` of the export directory now makes the claim.
- **Confirmed, fixed in passing: a flaky darwin survivor test.** The fake
  agent created `pids` before writing it, and the canceled case could
  cancel in between. It predates this unit and could turn the macOS
  required check red, so the fake agent now renames the file into place.
- **Second round, no blockers. Deferred to the driver port unit, which
  the planner files (#3467's planning notes):** `AuthenticateReleasedExport` has `stage.Gate`'s
  signature only, since the pipeline's path check accepts only ward
  handoff directories and its retry classification would retry an
  unauthenticated export; and an export error can carry a path the agent
  chose, so the caller screens it as it screens a warded handoff's
  failure. Both are now stated where they apply.
- **Declined:** a file swapped between the walk's lstat and its read
  needs a writer alive after the run-end proof, which the export already
  assumes away (a sandbox escape is #3470's); and a nested evidence
  source behind a symlinked directory fails the export rather than being
  left out, but no source path is nested.
- **Disproved:** moving a denied path's parent into the worktree, because
  the rules already deny renaming any directory above a denied path
  (#3466). And by the reviewer's own checks: the ward's export path is
  unchanged (same caps, the link check off), the hard-link check runs at
  all four read sites before a file is opened, a non-directory
  `.freeside-evidence` is removed rather than followed, and every record
  sits in `ctl/`, outside RUN_ROOT.
