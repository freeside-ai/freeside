# Secret-Scan Repository Files at Any Stored Size

The owner chose, during the #2382 exit run, to fix this in-run (#3396).

## Decision

Chose to secret-scan every stored repository blob, whatever its size, over
raising the 1 MiB scan cap or streaming in chunks. Intake already fails the
import on a stored blob over `MaxBlobBytes` (100 MiB) or a total over
`MaxTotalBytes` (1 GiB), so those caps bound one scan's memory and the whole
scan's work. The importer no longer emits `secret_scan_skipped`. The kind
stays registered because export rejections recorded before this change name
it. `SecretMaxScanBytes` now governs only the evidence text that the importer
reads whole and may inline: the review guide and markdown evidence.

## Why

The cap made a skip fatal for every implementation import. The generated
Swift client is already 1.28 MB, and since #3122 every contract unit
regenerates it in the writer's ward. So Freeside couldn't export any change
to the API. The #2968 implementation run was refused this way after 35
minutes. The skip finding was right to refuse silent gaps, but a gap on an
in-scope generated file is one that scanning can close.

## Rejected

- **Raising the shared default:** the evidence path reads text up to the same
  cap and inlines it into attention state, so a larger default would inline
  larger evidence.
- **Raising a repository-only cap to a few MiB:** the generated client grows
  with the API, and any cap below the blob cap keeps the same failure for the
  next large file.
- **Chunked streaming as in `internal/exportscan`:** it bounds memory to a
  chunk but matches across line boundaries, while the importer matches line
  by line and reports the line. Keeping per-line matching keeps every rule's
  meaning unchanged. Memory stays bounded by the blob cap.
- **Tolerating `secret_scan_skipped` for implementation:** it would publish
  unscanned bytes.

## Cost

Measured on the reference Mac: `Types.swift` scans in about 65 ms. A 100 MiB
blob takes 3 to 6 seconds, depending on its line shape, so a full 1 GiB
change set takes about a minute. `splitLines` now yields lines lazily,
because a slice of every line would cost gigabytes for a 100 MiB blob that is
all newlines.

## Tests

`TestImportSecretScanCoversLargeFiles` supersedes
`TestImportSecretScanOverCapSurfaced`. With the evidence cap set below both
files, a clean 1.1 MiB file imports with no finding, and a token on line 1101
of a second file is found on that line.

## Refute-First Findings

From a fresh-context refute pass before commit.

- Confirmed, and intended: scanning more makes two outcomes stricter. A
  secret match in a base-ignored file over 1 MiB used to drop with its
  `secret_scan_skipped`, and now it stays and blocks, as a secret on a
  smaller ignored file already did. Under the specification profile, a
  match in debris over 1 MiB that used to be tolerated as a skip is now a
  fatal `secret`. Both close a gap rather than open one.
- Confirmed and fixed: `io.ReadAll` grows its buffer by doubling, so a scan
  could hold up to twice a blob at the cap. The read now uses one buffer of
  exactly the verified size.
- Confirmed and fixed: two comments still justified behavior by skipped
  blobs: the specification fatality table and the stage pipeline's reason
  for keeping specification blobs out of the CAS.
- Disproved: an unbounded scan. Intake refuses a stored blob over the blob
  cap, or a digest claimed at two sizes, before any snapshot exists. The
  defaults make a zero cap 100 MiB, and validation rejects a negative one.
  Omitted and base-object changes aren't scanned.
- Disproved: a new time or findings cost class. Adversarial single-line
  inputs scan linearly, since Go's `regexp` is RE2. One 100 MiB file costs
  about what 100 files of 1 MiB already did, and 1 GiB of 1 MiB files was
  already reachable.
- Disproved: a change in line numbering. Old and new `splitLines` agreed on
  20,000 random strings over `a`, `\r`, and `\n`.
- Accepted: `SecretMaxScanBytes` keeps its name though it now bounds only
  evidence. Stored replay options carry the field, and no stored replay
  depended on its old meaning for repository content.
- Accepted: the replacement test uses 1.1 MiB files, so a cap reintroduced
  at 2 MiB or more would pass it. The doc comment on `scanSecrets` states
  the rule.

Revisit when a repository legitimately commits text blobs near the blob cap
often enough for scan time to matter, or when the evidence path's inline
bound changes.
