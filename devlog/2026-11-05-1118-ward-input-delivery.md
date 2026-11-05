# Ward Input Delivery

#715 delivers operator input files, a task's attachments and the
attachments on user-authored discussion messages, to the specifier,
implementer, and remediator through Ward. It is Tier A: it changes what
untrusted bytes reach a writer and how Ward proves them, and it adds a
field to the durable handoff.

## Boundary

- **Only operator input is delivered.** Admission lists the task's
  attachments, then the attachments of `AuthorUser` messages in the bound
  conversation prefix. Agent- and daemon-authored attachments are never
  delivered. The old path that passed every message attachment as an
  opaque prior artifact outside specification is gone: no stage read it as
  a file, and keeping it would let an agent's attachment reach a later
  agent.
- **Only Ward writers receive files.** The same `wardRole` predicate that
  picks the writer's credential decides delivery, so a stage that runs no
  writer gets none. Review and shadow review are #2589.
- **The files sit outside the workspace, read-only.** Ward copies them
  into an owned volume mounted read-only at `/var/lib/freeside/inputs`.
  Everything is root-owned, directories 0755 and files 0444, so the
  agent's user reads but cannot write them. They are never exported or
  read back: they are input, not evidence.

## Decisions

- **Chose a host-built tar and a listing digest over per-file proofs.**
  The host streams each blob into one GNU tar while hashing it, and
  refuses a blob whose bytes or size differ from the manifest before any
  seeder runs. The seeder extracts it and sets owner and modes. The
  read-only observer prints one SHA-256 over a sorted listing of
  `d <path>` and `f <sha256> <path>` lines, plus an `x` line for any
  other entry type, owner, or mode, so a link, special file, extra file, or
  changed byte changes the digest. One digest fits the 4 KiB proof limit
  that 1,000 per-file lines would not. Names hold no control character, so
  one name cannot forge another's line.
- **Chose names only inside the archive.** No input name becomes a host
  path, so the host's case-insensitive filesystem cannot merge `A.png` and
  `a.png`; both arrive on the volume's case-sensitive filesystem. This
  answers the collision question `2026-11-04-2120-task-attachments-contract.md`
  left to delivery. A BusyBox run in the exporter image as root proved the
  listing format for backslashes, quotes, `$`, backticks, `%s`, a leading
  dash, 255-byte segments, a ~1,000-byte path, an empty file, and the
  case pair (`TestExporterInputScriptsLive`).
- **Chose `task/` and `discussion/<sequence>/<index>.<ext>` names.** The
  prefixes keep task and discussion files from colliding. A discussion
  file has no operator-chosen name, so its extension comes from the type
  sniffed from the stored bytes. The prefix spends five bytes of the
  1,024-byte name limit, so a task name within five bytes of it fails
  admission rather than being renamed.
- **Chose no new prompt delivery member.** A live probe ran the pinned
  CLI under the unchanged `file_v6` commands for both the specifier and
  the other writers, and the Read tool opened an image under the input
  directory outside the working directory with no new flag
  (`TestPinnedClaudeInputReadLive`).
- **Chose the admitted spec as the prompt's source for the file list.**
  The prompt lists each file's path, type, and size from
  `Spec.StageInputs.Attachments`, not from materialized bytes, so a stored
  intent re-renders the same prompt at regate. `stage.InputFilesFor` is
  the one source of the Ward manifest, and the driver refuses a provider
  spec whose inputs differ from it in name, digest, size, or order.
- **Chose to fail admission for a run that names no task.** The store
  binds every run to a task, so an empty task ID is a caller bug, and
  skipping would silently drop the task's files. The one binding not read
  from the store, the prospective implementation run that specification
  acceptance checks delivery for, now carries its specification run's
  task, which is the task the store binds it to.

- **Chose to classify a list refusal as undeliverable input.** A
  discussion image over its limit, a combined list over the count or byte
  budget, or a name the `task/` prefix makes too long can never be
  delivered. Admission tags it as the stage's undeliverable input, so the
  dispatcher records a terminal outcome, as it does for a materializer
  shape refusal; left plain, the intent stayed pending forever and a
  specification discussion stayed awaiting the agent. A missing blob stays
  an operational fault and is retried.

## Refute-First Result

A fresh-context reviewer tried to break the change.

- **Confirmed and fixed:** the untagged list refusal above.
- **Disproved by check or code reading:** listing forgery (fixed-width
  hash and `d`/`f`/`x` prefixes keep lines injective, `LC_ALL=C` sort, a
  host and observer sort mismatch only fails closed); byte, size, extra
  file, symlink, and mode tampering; traversal and case collisions; a
  writable or non-read-only mount; resource names missing from teardown,
  recovery, or the rig; recovery accepting a legacy digest with inputs or
  a changed proof; a provider changing the manifest; agent-authored
  attachments reaching admission; and a behavior change in the shared
  staging handshake.
- **Allowed by decision:** no hardlink, FIFO, or wrong-owner case in the
  script tests, since only the daemon's own seeder writes the volume and
  the observer's `x` line already covers any other type or owner; no
  recovery test that swaps the input volume, since the generic prepared
  launch state check covers its fingerprint.
- **Unverified:** whether the seeder's root filesystem holds a 100 MiB
  archive and staging fits the handoff timeout at the full budget. It
  fails closed if not; #2654 proves it live.

## Scope Beyond the Plan

The durable handoff journal (`internal/store/handoff_journal.go`), the
`wardstore` adapters, and the rig's resource namespaces
(`internal/daemonlock`) had to learn the input volume, seeder, and
observer names so recovery and rig cleanup own them. The prompt volume's
staging handshake was extracted into a shared helper first, with no
behavior change, so the input volume reuses it.

Revisit when an agent's output needs to reach a later agent as a file, or
when attachment blobs move out of the shared content-addressed store.
