# The Launcher Keeps Its Own Files Out of the Writer's Workspace

Work unit: #1929. The Claude launch command for new runs (`file_v2`) adds
nothing to `/workspace` while the agent runs except two empty, searchable
directories. The transcript streams to the container's own filesystem and
the evidence descriptor is not written until the agent has exited. The
owner set the outcome in the issue; the mechanism below is the agent's
choice, executed from the issue's implementation plan.

This narrows one decision in
[the Claude driver note](2026-07-28-1845-claude-driver.md): "a root-only
control subdirectory protects the outcome marker". The control directory is
now root-only whenever it holds a file, not for the whole run.

## Why the Old Layout Failed

A tool that walks the repository root (Biome under `npm run lint` in the
reported case) met three launcher-owned entries in the writer's workspace
and none in the verifier's:

- A root-owned `0700` `.freeside-evidence/.control`, which the agent user
  cannot open.
- A one-line `evidence.json`, which a formatter flags.
- The streaming `agent-transcript.jsonl`, which grows while it is read.

A check that passes in verification therefore failed for the writer, and
the writer could not fix it: none of the three was its to change.

## What the Agent Chose

- **A new protocol member, not an edited command.** Ward stores a digest of
  the whole handoff spec, command included, and refuses recovery on a
  mismatch. Chose a `file_v2` member of `stage.PromptDelivery` over
  changing the `file_v1` command because a run started before the upgrade
  must rebuild its command byte for byte. `argument` and `file_v1` keep
  their commands and their four goldens. The type's name now undersells
  it: a member selects a whole launch command, not only how the prompt
  travels. Renaming the stored field was out of scope.
- **The control directory is open while empty and closed before use.** It
  is root-owned and `0755` during the run, so a walker can open it and
  finds nothing. After the agent exits the launcher sets it to `0700`
  before writing anything into it. What protects the marker was never the
  read bit: it is root ownership, no write bit for anyone else, and a
  root-owned sticky parent, so the agent can neither write into the
  directory, rename it, nor unlink it. Those hold throughout.
- **The transcript streams outside every mount.** It goes to a root-only
  `/var/lib/freeside-launch` on the container's root filesystem, which the
  launcher clears and recreates before the drop because a project image may
  be derived from an untrusted base. The agent reaches the file only
  through the descriptors it was started with, as before.
- **The launcher takes the agent's write access away before it places its
  files.** A process the agent left behind may outlive it. After the agent
  exits the launcher sets the evidence directory to `0755` and the control
  directory to `0700`, then for each file: completes it inside the control
  directory, removes whatever sits at the final path, and renames it into
  place with `mv -T`. Both paths are on the workspace volume, so the move
  is one rename. It replaces a file or symlink without following it and
  refuses a directory, and `set -e` stops the launcher before the marker.
- **The reserved paths go in the checkout's own exclude file.** The
  launcher appends `/.freeside-evidence/` and `/.freeside-commit-plan.json`
  to `.git/info/exclude` as root, before hydration and before the
  ownership sweep, when the tree holds only the daemon's seed. The export
  walk records `.git` unexplored, so the entries never leave the workspace.

## Rejected Options

- **The exclude entry alone.** It hides the subtree from tools that honor
  git excludes. It does nothing for a tool that does not, and the
  unreadable directory fails those outright.
- **A mode change alone.** It fixes the permission error and leaves a
  descriptor a formatter flags and a transcript that grows under the
  reader.
- **The marker on another volume.** It would let the workspace hold nothing
  of the launcher's, but it moves ward's observer, failure-evidence, and
  marker paths, which the issue rules out, and it changes what every
  provider's launcher must do.
- **Moving the transcript straight from the launch directory.** That is a
  copy across filesystems, not a rename: `mv` would create the destination
  in a directory the agent could write, and could follow what it finds
  there. Copying into the closed control directory first makes the last
  step a rename.
- **Keeping the evidence directory world-writable through the moves.** The
  first version did. A hostile stand-in agent then recreated a directory at
  the final path between the remove and the rename; the launcher failed
  closed every time, which is safe but lets a leftover process deny the
  run its outcome. Closing the directory first removes that window.
- **Symlink guards on the exclude write.** An earlier draft refused to
  append when `.git/info` or the exclude file was a symlink. Ward refuses
  to build a seed from a source containing a symlink, and no agent process
  exists before the drop, so the guarded state cannot occur. The
  independent review found the guards untested and unreachable; they were
  removed, not tested.

## Refute-First Findings

Run against the pinned agent image (Debian trixie, GNU coreutils 9.7) with
the real generated command, real root, and a real `setpriv` drop to uid
1001 with no capabilities.

- **Disproved: a leftover process can make the launcher follow a planted
  entry.** With the evidence directory still writable, a tight racer hit
  the window between remove and rename in all 160 runs. Symlink,
  symlink-to-directory, and file plants were replaced by root-owned `0644`
  regular files holding the launcher's content, with the link targets
  untouched. A planted directory made `mv -T` fail and no marker was
  written. No run ended with agent content at either path.
- **Confirmed and narrowed: the same racer could deny the outcome.** 40
  of those 160 runs failed closed. With the evidence directory closed
  first, 160 of 160 runs completed and the racer never won the window.
- **Allowed: a leftover process can still fail its own run.** It can keep
  filling a directory it left at a final path while the launcher removes
  it, as it already could with `node_modules`. The removal fails, the
  launcher stops before the marker, and the agent gains nothing a nonzero
  exit would not give it.
- **Disproved: the agent can read or forge the marker during the run.** As
  uid 1001, writing into the control directory, renaming or removing it,
  changing its mode, renaming the evidence directory, and reading the
  launch directory by path were all denied. A directory descriptor held
  open across the close did not expose the marker.
- **Disproved: a consumer can read the descriptor or transcript before the
  launcher finishes.** Ward's exporter has one call site, after the
  outcome observer returns status 0; failure-evidence capture requires a
  nonzero marker. The marker is written only after both moves, so a
  launcher that stops early leaves nothing a consumer will read.
- **Checked: the root filesystem can hold the transcript.** Under Apple
  `container` it is a disk-backed ext4 volume of several hundred GiB, not
  memory.
- **Allowed: a killed writer leaves no transcript on the volume.** The
  transcript reaches the workspace only when the launcher runs to the end.
  The issue lists this as a non-goal; a run killed mid-flight already
  produced no marker and so no export.
- **Allowed: an older daemon cannot recover a `file_v2` run.** It does not
  know the member. Downgrading across an in-flight run was not supported
  for `file_v1` either.

## Revisit When

- A supported agent base image ships an `mv` without `-T`. The launcher
  then fails before the marker on every run, which the regression check
  catches on Linux; the fix is a different rename primitive, not dropping
  the flag.
- A launcher needs the transcript to survive a killed writer. That needs a
  second volume or a ward-side capture, not a return to streaming into the
  workspace.
- Another provider gets a launcher. The layout lives in the Claude command
  today; a second copy is the point to move it behind ward.
