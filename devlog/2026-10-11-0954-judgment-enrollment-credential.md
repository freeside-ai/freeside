# Read Each Judgment Call's Credential From Its Enrollment

Work unit #1426, plan §5.4. Follows the lineup lookup of #1425
(`devlog/2026-10-10-1602-judgment-lineup-lookup.md`), which left the call
record saying `credential_source: interim_flag`. The issue body carries the
contract and the plan. This note records the two owner decisions, the options
they rejected, where the implementation left its plan, where the token is
while a read runs, and what the refute-first pass found.

## Owner Decisions (2026-10-11)

**The unit is narrowed to what can start today.** It removes
`-judgment-auth-snapshot` and nothing else. Removing every judgment and
review flag in one unit was rejected because each other removal waited for
work that had not merged: `-judgment-model` for #1619, the review and shadow
model and effort flags for a contract unit that lets reviewer agents carry
them, `-judgment-publication-author-prompt` for #1428, and the CLI pin flags
for a home near #1424. #1619 merged while this unit was open and kept
`-judgment-model` for a line on the native-default offer, so that removal is
still its own unit.

Follow-up: #1964 (`-judgment-model`), #1965 (review and shadow model and
effort), #1966 (`-judgment-publication-author-prompt`), #1967 (the CLI pin
and digest flags).

**The daemon reads the token from the enrollment's volume and keeps it in
memory for that enrollment generation.** No operator file and no flag
replaces the snapshot. Rejected:

- **Keeping an operator file.** A file names an account no enrollment
  records, so the daemon could call on a credential that the lineup, the
  integrity probe, and re-enrollment never see.
- **Holding the token for the daemon's whole life, whatever the generation.**
  A re-enrollment would leave the daemon calling with the token it replaced
  until a restart.
- **Putting the token in the proof file.** The proof is parsed, compared, and
  quoted in refusals. The copy sits beside it, in a file the proof parser and
  its errors never open.

## Where the Implementation Left the Plan

- **The reader takes an enrollment id, not an enrollment.** The plan passed
  the resolved `ClientEnrollment`. The client and method that make a store
  readable, and the identity the read hold lands on, would then be the
  caller's copy. `wardstore.CredentialReader` loads the stored enrollment
  itself.
- **The cache holds one token per enrollment, not one per generation.** The
  plan keyed a map by enrollment and ordinal. A newer generation's token
  replaces the older one, so a replaced token does not stay in memory. The
  older token is dropped before the newer one is read, so that holds when
  the read fails too. A resolve that still names the older generation cannot
  put it back, and does not drop the current token: the read refuses a
  generation that is no longer current.
- **Preflight reads without the read hold.** Preflight opens the store
  read-only, where a hold cannot be recorded. It holds the daemon lock, which
  every command that enrolls, adopts, or renews takes first, and
  `ReadUnheld` keeps the generation and digest checks.
- **A trailing line ending is trimmed.** The launch path reads the same file
  with `$(cat)`, which drops it, so a hand-written token file works for both.
  The digest check runs over the file's raw bytes first.
- **`interim_flag` stays a valid credential source.** No call writes it, but
  a ledger written before this unit must still load.
- **One read runs at a time, for every enrollment.** The plan wanted two
  calls that miss at once to start one container. All seven roles ask at
  startup, so a caller waits for the read in flight. A second enrollment's
  first read waits for the first's, and a caller that waits leaves at its
  own deadline with no token.
- **The role's health item gained a second remedy.** It told the operator to
  fix the lineup line and restart, which is wrong for an unreadable
  credential.

## Where the Token Is

The token reaches the driver as an `inference.Secret` and nothing else. A
failed read returns one fixed error, because the inference client copies a
resolver's error into the health item and the preflight manifest; the cause,
which names a volume and a runtime step, goes to the daemon log. Errors from
the read path are fixed sentences that name what the store's records said,
never what the store held. A test passes a recognizable token through the
success path and each failure path and searches the log, every returned
value, the health items, the call ledger, and a preflight manifest for it.

A read cannot keep the token off the host disk. The observer has no network
and a read-only mount, so its exported root filesystem is its only way out,
and Apple container 1.1.0 materializes every export as a file first. While a
read runs, about three seconds on the development machine, these hold the
token:

- **The stopped observer's root filesystem,** in the runtime's storage, until
  the container is deleted. The read deletes it and proves it absent before
  it returns a token, and reaps it on every failure. A killed daemon leaves
  it. Under a production rig its name is in the rig manifest before it is
  created, which is what the rig's recovery works from; without a rig only
  its `freeside-preflight-credential-` name marks it.
- **The daemon's archive,** mode 0600 in a 0700 directory from
  `os.MkdirTemp`, removed on every return path. A removal that fails is a
  failed read. A killed daemon leaves it.
- **The runtime's own archive,** `<user temp dir>/<UUID>/archive.tar`, mode
  0644 in a 0755 directory inside the user's 0700 temporary directory. The
  CLI removes it when it exits on its own. Measured on the real runtime: a
  CLI killed during the export, by SIGKILL or SIGTERM, leaves the complete
  archive behind, and `TMPDIR` does not move it.

The third copy is why a token-read export is not cancelled. The daemon kills
a runtime call when its context ends, so a deadline or a shutdown during the
export would have left the token on disk on an ordinary path. The export now
runs to its end under its own one-minute bound, and the read honors the
caller's cancellation afterwards by failing.

What still leaves a copy: a daemon killed inside the read, or an export that
outruns its bound. Each copy is readable only by the daemon's user, who can
read the volume itself. Nothing in Freeside reaps the host temporary
directory.

Rejected for the disk copies:

- **Streaming the export.** The CLI streams only after the runtime's service
  has written the whole archive, so no file is avoided.
- **Masking the copy with a one-time pad the daemon passes on the command
  line.** No clear copy would outlive the container. It needs byte arithmetic
  in the observer's shell, and what it removes is a file only the volume's
  own reader can open.
- **Reading the token from the container's log.** The runtime keeps logs on
  disk too, and the runtime seam would need a new method.

## Other Consequences

- **Judgment usage moves to the enrollment's account.** A role is unbound
  while that enrollment cannot be read, and a line moved to an agent on
  another enrollment moves the bill.
- **The judgment configuration digest no longer covers a token.** A digest
  from an earlier preflight does not match, and a retained real-work session
  that had judgments on cannot resume across the upgrade.
- **A failed read starts a container on every call.** Failures are not
  remembered, so a repaired store binds a role at its next call. Judgment
  calls are a handful per task. The startup check asks for each role in
  turn, so a store that cannot be read is tried once for each role before
  the daemon is up.
- **An unreadable drift auditor credential holds unattended work until a
  restart.** Its item blocks unattended admission, as a missing line's does,
  so nothing may call the role again to clear it. Nothing probes a credential
  between calls.

## Refute-First Findings

A fresh-context reviewer attacked the reader and the hold before their
commit.

- **Confirmed by a check on the real runtime, fixed:** the runtime's export
  archive outlives a killed CLI. See above.
- **Confirmed by reading, fixed:** the reader trusted the caller's enrollment
  for the client, the method, and the identity to hold.
- **Confirmed by a test, fixed:** a token held in an unexported struct field
  prints in the clear, because `fmt` cannot call a formatter there. No struct
  held a `SetupToken` that way. The daemon's cache entry held its `Secret` in
  one; the field is exported now, and the leak tests search the decimal byte
  form too.
- **Confirmed by a test, fixed:** the observer selector had no `valid()`, and
  its zero value ran an observation on the fake runtime. It now starts
  nothing.
- **Confirmed by reading, fixed:** a failed removal of the archive directory
  was silent. For a token read it fails the read.
- **Allowed by decision:** on an adopted generation the tree digest and the
  token copy are taken in separate passes, so a writer between them could
  swap the token. Every writer Freeside has holds the mutation lease or the
  daemon lock, which the read excludes. A writer outside both could have
  written the volume before adoption, and the launch path would read the same
  swapped file.
- **Disproved by tests:** token bytes in an error from a truncated archive, a
  hostile entry name, or a failed parse; a tree-digest preimage stored as the
  token file; a symlink, directory, empty, or oversized token file; a copy
  written into the proof or the volume; an observer or archive left behind
  after cancellation; a read that outlives its hold.

A second fresh-context reviewer attacked the daemon's credential source and
its wiring before their commit, and found no path that leaks the token or
binds the wrong one.

- **Confirmed by a test, fixed:** a rendered credential source printed its
  held tokens, because the map that holds them is unexported. The exported
  field covered only an entry rendered by itself. The source now formats as
  one fixed word, and the leak test renders it through `fmt` and both `slog`
  handlers.
- **Confirmed by a mutation, fixed:** no test failed when a failed read of a
  newer generation fell back to the older token. One does now.
- **Confirmed by a test, fixed:** a caller waiting behind another
  enrollment's read ignored its own deadline. Callers now wait on a channel
  and leave when their context ends; a test holds a read open to show both
  that and that callers who miss at once share one read.
- **Confirmed by reading, fixed:** the health item and the README said an
  unreadable credential needs no restart. That is untrue for the drift
  auditor. See Other Consequences.
- **Disproved by a check:** a daemon with judgments off reading the
  credential anyway. A Claude daemon with no judgment flags stops earlier,
  where `ward.NewTaskJudgments` refuses a nil driver, so the role check never
  runs without a driver. #1945 already tracks that start and the roles being
  off in it.
- **Allowed by decision:** a store that cannot be read is tried once for each
  role at startup, and preflight's read has no bound of its own beyond its
  caller's context, like the integrity probe beside it.
- **Declined:** unit tests for the two production constructors. They join
  parts that are tested; the live suite reads a real volume through the same
  reader.

A third fresh-context reviewer read the whole diff before the push.

- **Confirmed by reading, fixed:** preflight read a role's stored credential
  while an unapproved shadow review configuration held every other
  credential probe at `not_run`. The role check now waits for the same
  approval, and the test of that ordering covers it.
- **Confirmed by reading, fixed:** the daemon's leak test searched for the
  token in the clear only. A token type printed where `fmt` cannot call its
  formatter shows as bytes, so the test now searches the decimal and hex
  forms too.
- **Allowed by decision:** the token is read before the client admits the
  role, so a line that resolves and then fails admission still costs one
  read, and a failed read is the reason shown ahead of the admission
  failure. The resolver returns a call's credential and the client admits
  it; reading after admission needs a lazy credential on the inference
  contract. The token read is the one any admitted role on that enrollment
  causes.
- **Declined:** preflight logging a read's cause to the process's standard
  error and not the command's injected writer. The command is only ever
  given the process's own.

The automated reviewer read the pushed diff.

- **Confirmed by a test, fixed:** a failed read of a newer generation left
  the older token held until a later read succeeded. The cache now drops it
  before that read. No command appends a Claude generation under a running
  daemon yet, so this path waits on #1639.
- **Declined:** exporting the observer selector's registry as
  `AllCredObservers`. The type is unexported, and an unexported enum keeps
  an unexported registry, as `allInvocationStates` and
  `allJanitorActions` do.

## Revisit When

- **#1945 lets a Claude daemon start with judgments off.** A role that is
  off must be answered before its credential is read. Otherwise the daemon
  reads a token no call uses, and an unreadable one raises the drift
  auditor's blocking item.

- **#1639 lands.** Re-enrollment appends a generation, which the cache
  already keys on; check that the first call after it reads once.
- **A call driver that is not Claude arrives.** The reader refuses every
  enrollment but a Claude Code setup-token one, so such a role would be
  unbound.
- **Apple container can stream a stopped root filesystem or place its export
  archive.** The uncancelled export and its bound can then go.
- **A real run shows reads failing in a loop while the runtime is down.** A
  wait between attempts changes when a repaired store binds a role, which is
  the owner's call.
