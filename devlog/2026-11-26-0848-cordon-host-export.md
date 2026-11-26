# Return a Cordoned Run's Export From Its Host as Untrusted Input

Issue #3624, tracker #3462. Tier A (import and handoff, runner
containment). It completes the export half of the host launcher
(`devlog/2026-11-26-0409-cordon-host-launcher.md`). A clean run on a host
now returns its export to the daemon's client, and the client admits it
the way the ward's daemon side admits an exporter VM's archive.

## Decisions

- **The export crosses on the operation's own connection.** The launcher
  runs the backend's `Export` or `Recover`, writes the reply, and streams
  the export directory after it, using the same chunked tar stream as the
  seed. A second fetch operation, or a per-blob pull, was rejected. Either
  one adds a step at which the launcher could answer with a different
  tree than the one the reply reported, and a protocol state for a
  half-fetched export.
- **The client admits the export itself.** Every byte from the launcher
  is untrusted. The client extracts through an `os.Root` into a
  `.receiving` directory it owns, within the ward's receipt bounds: 4 GiB
  streamed, 2 GiB extracted, 10,000 entries, and 64 MiB per manifest. It
  accepts only directories and regular files (0600), validates the
  manifest as the ward's `verifyManifest` does, reruns the export
  credential scan, syncs the tree, renames it to `export/`, and writes
  its receipt record last. Trusting the launcher's scan or digests was
  rejected, because the host is the less trusted side.
- **The bounds match the ward's by comment, not by a pinning test.** The
  ward's defaults live in `ward.Config.withDefaults`, which is unexported,
  and `internal/ward` is outside this unit's paths. A change there needs
  the same change in `cordonhost/seed.go`. Revisit when the ward exports
  its defaults.
- **Authentication is local.** `AuthenticateReleasedExport` checks the
  client's receipt record (version, run, base commit, three member
  digests the client computed) and never asks the launcher. The
  provisional wire kind `authenticate_released_export` is gone. A
  launcher's word about an export it could have changed is worth nothing,
  and a launcher that has gone away must not block a publication whose
  export the daemon already holds.
- **The launcher's word is taken for the base commit only.** The reply
  names the base it read from its end record. The client checks its shape,
  and the stage driver compares it with the run's admission, as it does
  for a local export.
- **`ExportRoot` must be the stage driver's `CordonRunsRoot`.** The driver
  accepts a cordoned run's released export only at
  `<CordonRunsRoot>/<runID>/export`. The receipt directory therefore
  shares a name space with a local backend's run directories. The client
  deletes only its three fixed names (`.receiving`, `export`,
  `receipt.json`). It refuses to clear a directory that holds anything
  else, so a local run's directory is never removed even if the roots were
  misconfigured to overlap.
- **A repeat answers from what already exists.** The client returns a
  receipt that has a record without dialing, so a restarted daemon finds
  it. With no record, it clears the partial receipt and asks again. The
  launcher answers a repeated `Export` (`ErrAlreadyExported`) through
  `Recover`, which authenticates its own export record or redoes an
  interrupted export.
- **A receipt refusal ends the run; a cut retries.** A refusal for what
  the export holds (a bound, a name, an entry type, the manifest, the
  base) is `cordonhost.ErrExportRefused`, which `cordonPortError` maps to
  a `CordonExportRefusal`, because asking again brings the same bytes. A
  stream cut short stays retryable. This is the ward's own split between
  a verification failure and host I/O.
- **Host refusals cross as the backend's errors.** New wire codes carry
  `ErrWorktreeReplaced`, `export.ErrLinkedFile`, `ErrNotExportable`,
  `ErrNoEndRecord`, and a scan refusal, so `cordonPortError` classes them
  as it classes local ones. A scan refusal's host text names an item of
  the export and is dropped. The client substitutes a fixed reason.

## Discovered Risks, Fixed Here

- **A sparse tar entry bypassed the byte bound.** A PAX GNU sparse entry
  keeps `TypeReg`, and Go's reader reports its logical size, so its holes
  extract as bytes the stream never carried. Both trees refuse any
  `GNU.sparse.*` record, and the reader bounds the extracted bytes apart
  from the stream bytes.
- **A launcher could export and recover one run at once.** Once a client
  drops its connection, a second connection can run `Recover` while the
  first `Export` is still writing. `Recover` removes an export directory
  that has no record, which is exactly what an export still being written
  looks like. The server now holds a per-run lock across both operations.

## Refute-First Record

A fresh-context reviewer read the change twice to prove it wrong. The
first round's findings, each confirmed by tracing the call path:

- **A crash mid-record-write wedged the run.** `atomicfile` stages
  `.receipt.json-*.tmp` in the receipt directory, which cleanup refused
  as a foreign name, so every retry and the release failed. Confirmed and
  fixed: the staged name counts as the receipt's own
  (`TestACrashedRecordWriteIsCleared`).
- **A deterministic refusal retried forever.** Over-bound or invalid
  content fell through `cordonPortError` as retryable, so recovery asked
  for the same bytes again without end. Confirmed and fixed with
  `ErrExportRefused` (above).
- **Tar overhead counted against the 2 GiB file bound.** Confirmed and
  fixed: the stream has its own 4 GiB bound, the ward's
  `MaxArchiveBytes`.
- **Cleanup or traversal reaching outside the receipt, and
  authentication asking the launcher:** the reviewer found neither.

The second round found no blocker. Its findings:

- **Extraction failures for what the stream holds stayed retryable:** a
  duplicate entry, a missing parent, or a file the tar ends inside.
  Confirmed and fixed: any extraction failure but a cut or host I/O
  (`ENOSPC`, `EDQUOT`, `EIO`) is `ErrExportRefused`.
- **The receipt directory's own entry was never synced,** so a power
  loss after the driver recorded the export could lose the record and
  end the run unauthenticated. Confirmed and fixed: the export root is
  synced once the run's directory exists.
- **The launcher's run lock ends before the stream:** allowed. Once the
  export record exists, another operation only reads the export, and a
  release mid-stream reads as a cut, which is asked for again.

Checks that ran against claims raised while building the change:

- **Escaping names, links, special files, oversized trees, cut streams,
  a credential, a malformed manifest, a malformed base:** each refused at
  receipt with nothing kept at or beside the export root
  (`TestAReceiptRefusesWhatNoExportHolds`; each refusal's reason was read
  to confirm the intended check fired).
- **A launcher answering anything or nothing on authentication:**
  disproved by `TestAReceiptIsKeptAndAuthenticatedOnTheClient`, which
  authenticates through a fresh client whose socket doesn't exist, and is
  refused after an edited manifest or a removed record.
- **Cleanup reaching a directory it didn't create:**
  `TestAReceiptDirectoryHoldingAnythingElseIsNotCleared`.
- **A clean exit importing differently from a local export:** disproved
  by the contract suite's `clean exit exports` case, which compares the
  imported trees on both seams with real launches.

## Revisit When

- A launcher is reached over a network (#3625): the receipt's bounds
  stand, but the transport needs its own authentication.
- The ward's receipt bounds change, or `internal/ward` exports them.
