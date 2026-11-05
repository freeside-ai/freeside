# Task Attachments Contract

#2587 lets a `submit_task` command, and `freesided submit --attach`, carry
files the client uploaded first by digest. The daemon records them on the
task. This note records where they live, who decides their type, the
owner's limits and name rules, and the refute-first result. The work is
Tier A by path: it changes `daemon/internal/signet/http.go`, where client
requests enter.

## Decisions

- **Chose a task-side table (`task_attachments`, migration 0124) over
  registering attachments as artifacts.** Attachments are operator input,
  not evidence (plan §5.15). Making them artifacts would put them in every
  artifact listing and evidence path. It would also route the change
  through the Tier A artifact file (`internal/domain/artifact.go`), whose
  trust rules exist for verifier output. The table is written once, in the
  transaction that creates the task. A read re-validates every row and
  requires contiguous positions, so an edited row fails loudly (Tier B).
- **Chose a closed, daemon-sniffed media type over trusting the client's
  claim.** The daemon maps `net/http.DetectContentType` onto
  {png, jpeg, gif, webp, octet-stream}, reading the leading 512 bytes. A
  format with no reliable signature, SVG included, is octet-stream. The
  sniffed type picks the size limit. The client's claim is kept as
  `claimed_media_type`, a display hint that decides nothing. It must be a
  lowercase `type/subtype` token with no parameters, so the hint can't
  carry arbitrary text. A change in Go's signature table can only move a
  file between an image type and octet-stream; the sniff test pins the
  mapping.
- **Limits and name rules are the owner's decisions of 2026-11-04.** An
  image is at most 10 MiB; any other file is at most 32 MiB. A task holds
  at most 100 MiB and 1,000 attachments. A name is a relative,
  "/"-separated path: valid UTF-8, no leading "/", no empty, ".", or ".."
  segment, no control character, each segment at most 255 bytes, and
  unique within the task. The agent added three rules within that intent:
  - **A name may not be a directory of another** ("docs" beside
    "docs/a.md"), since both could not be written out.
  - **A whole name is at most 1,024 bytes.** Without this, nine names of
    short segments could fill most of a 1 MiB command body. Such a name can
    never become a path, and validating a list of them took about a second,
    on every snapshot read. Review found this (below).
  - **Control means Go's `unicode.IsControl`**, so C1 controls are refused
    along with C0 and DEL.
- **Chose to allow a zero-byte attachment.** Folders routinely hold empty
  files, and refusing one would break a later folder upload for no gain.
- **Refusals name the rule and the position, never the refused text.** A
  `submit_task` refusal reaches the client and the logs, and the text may
  be something pasted into the wrong field. This matches the task-line
  refusals. The one exception is a well-formed digest whose blob is not
  stored: the refusal names it, so the client knows which upload to repeat.
- **The CLI writes blobs straight into the daemon's blob directory**
  (`<db>.blobs`) before it writes the recovery journal. The journal keeps
  digest references only, so a manual retry resubmits the same list and
  finds every blob. `BlobStore.Put` is atomic and converges per digest, so
  a daemon on the same directory sees a whole blob or none. This assumes
  `freesided submit` runs as the daemon's user with the daemon's database
  path; the flag's help says so, and a wrong user fails loudly on the
  0o750 directory.
- **Pinned digests do not move.** The attachment list joins the
  `submit_task` request digest and the CLI fingerprint with `omitempty`.
  A submission without attachments keeps its digest and journal bytes, so
  one recorded before this change still replays.

## Refute-First Pass

The agent's own checks and a fresh-context reviewer each tried to break
the name rules, the digest gate, replay binding, and all-or-nothing
writes.

**Confirmed and fixed:**

- **No cap on a whole name.** Fixed with the 1,024-byte rule above, in the
  domain, the mock, and the schema.
- **The Swift mock split names by grapheme cluster.** A combining mark
  after "/" hid the separator, so the mock accepted `/́x` and `../́`, which
  the daemon refuses. String equality also treated an NFC name and its NFD
  form as one. The mock now checks names, and compares replayed names, by
  UTF-8 bytes, as the daemon does.
- **`--attach` on a FIFO blocked in `open`.** The CLI now checks the file
  kind before opening it.

**Disproved by checks:**

- **Encoded separators:** `%2F`, a backslash, and U+2215 are ordinary
  characters.
- **Name shape:** an overlong segment and an inner `..` segment are
  refused.
- **Malformed input:** claim parameters and uppercase digest hex are
  refused before the blob store is touched.
- **Replay:** a reordered, added, dropped, renamed, or re-claimed list
  conflicts on replay. An empty but present list is refused. A legacy
  submission row with no request digest conflicts when the replay carries
  attachments.
- **All-or-nothing:** every check runs before the write transaction, and a
  replay returns before any row is written.
- **Size and sniff:** both come from one open file. Blobs are never
  deleted, so nothing can race them.
- **Image limit:** exactly 10 MiB is accepted, and one byte more is
  refused.

**Allowed by decision:**

- **Case- and normalization-only differences pass.** `A.png` beside
  `a.png`, or an NFC name beside its NFD form, would collide on a
  case-insensitive or normalizing filesystem, and are distinct names on a
  case-sensitive one. Which filesystem receives them is delivery's
  concern, so delivery (#715) decides how to write them where they would
  collide.
- **Unicode format characters (Cf) pass.** Refusing them would refuse
  ordinary emoji names, which use the zero-width joiner. The name is the
  operator's own input, shown back to the operator.

## Revisit When

- **A delete path:** `BlobStore` gains one. The size read before the
  transaction then needs a re-check inside it.
- **New limits:** the owner changes the limits or the media-type set.
- **Delivery:** it targets a case-insensitive filesystem.
