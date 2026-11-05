# Stage-Input Attachments

#2588 gives the stage-input snapshot an ordered list of named attachments
(digest, name, daemon media type, size) and materializes them, verified,
for #715 to deliver. It is a `kind:contract` change to a stored,
content-addressed record, Tier B: neither file is a Tier A path, and the
list grants no authority. No snapshot carries attachments until #715 fills
them at admission.

## Decisions

- **Chose `omitempty` on the existing encodings over a new
  `freeside.stage.inputs/v4` version.** Snapshots are content-addressed, so
  any byte change to the canonical form changes every stored ID. Omitting
  an empty list keeps `v1`, `v2`, and `v3` byte-identical for every
  snapshot without attachments. A non-empty list can only appear in a
  snapshot admitted after this change, so it needs no version of its own to
  be told apart. The constructor normalizes an empty list to nil, and a
  stored `[]` validates under the same ID, so absence has one identity
  however it is spelled. The goldens for existing snapshots and admissions
  did not change.
- **Chose a per-attachment byte budget over counting attachments against
  the materializer's limits.** `MaxInputBytes` (4 MiB) and `MaxTotalBytes`
  (32 MiB) were sized for prompt-role inputs, and a task may legitimately
  carry a 10 MiB image or 100 MiB of files that submit already accepted.
  Each attachment is read up to its admitted size and must be exactly that
  size; the snapshot's own validation (`MaxTaskAttachmentsTotalBytes`)
  bounds the list before any blob is opened. Rejected: raising the shared
  limits, which would loosen the prompt-role bound for every start.
- **Chose a snapshot-specific entry type without `claimed_media_type`.**
  The client's claim is a display hint on the task. Leaving it off the
  snapshot keeps it out of what an agent is shown and out of the
  snapshot's identity, so two tasks with the same files and different
  hints admit the same inputs.
- **Shared the whole list rule with `ValidateTaskAttachments` rather than
  copying it.** A list submit accepted must be a list delivery can write;
  one function over both entry types makes that hold by construction. It
  runs the checks in the task validator's original order, so a task list
  keeps its results and messages even when it has several defects.

## Known Gap: Backup Closure

The local backup's artifact closure (`daemon/internal/store/local_backup.go`)
covers neither stage-input attachment digests nor #2587's
`task_attachments` rows. This unit leaves it: that file is a Tier A path
outside its scope, and no snapshot carries attachments yet. The owner
decides which unit closes both gaps; #715 should list it as a dependency
or a declared path, since #715 is the first unit whose snapshots carry
attachments.

Follow-up: #2635.

## Revisit When

- **Streaming delivery:** #715 needs attachments delivered without holding
  up to 100 MiB per start in memory. A reader seam on
  `MaterializedAttachment` can add that without changing the snapshot.
- **Restart survival:** #715 decides attachments must survive a driver
  restart, which `durableInputs` does not cover today.
