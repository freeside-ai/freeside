# The Review Guide Comes From the Handoff and Rides the Ready Item

#2750 lets the producing agent write `.freeside-evidence/review-guide.json`,
imports it as a claim, resolves its excerpts against the verified commits,
and attaches the result to the ready-for-final-review item. The guide is
display text built from untrusted pointers; it grants nothing. This note
records the choices that reject a reasonable alternative and what the
refute-first pass found.

## Decision

- **Chose to fail a malformed guide closed over dropping it.** A guide that
  breaks the decoder, a domain cap, or the launcher's declaration fails the
  import with `ErrEvidenceInvalid`, as a malformed `scope-conflict.json`
  does. Rejected: importing the run without its guide. Dropping it silently
  teaches the agent nothing and hides a launcher-shape forgery behind a
  missing card section. The guide is optional, so an agent that is unsure
  leaves it out, and both prompts say so.
- **Chose to bind the claim to the imported commit at import over a
  head-independent claim.** The launcher declares the source
  head-independent, because it writes the descriptor before the daemon
  authors any commit. The importer then binds the claim to the commit it
  built (`bindReviewGuideClaim`) and recomputes the artifact id, so the
  ready item's rule (a head-bound claim at `pr_head_sha`) holds by
  construction. Rejected: binding at the ready item. That would leave a
  claim whose provenance disagrees with the head its excerpts were read
  from. An entry that already declares a head is refused, so the rebind
  never overwrites one.
- **Chose to resolve a missing path or range as unresolved over failing the
  import.** Each excerpt is read from git objects at the verified head or
  base: a literal `ls-tree` lookup that accepts only a regular file, a
  1 MiB file cap read before any bytes, and the domain's 8 KiB text cap.
  A path that is gone, a range past the end, a binary file, or text over a
  cap gets `path_missing`, `range_out_of_file`, `binary`, or `too_large`.
  Rejected: refusing the import. A stale pointer is the agent's mistake
  about the code, not a forgery, and the rest of the guide is still worth
  showing. Only a git failure fails the step.
- **Chose a new prompt delivery, `file_v7`, over changing what `file_v6`
  builds.** The issue expected every handoff golden to change. Recovery
  rebuilds a running launch's command from its stored delivery, and ward
  refuses a spec-digest mismatch, so changing `file_v6` would strand every
  launch in flight. `internal/exec/stage/ports.go` already states the rule
  (a provider adds a member, never changes an existing one). `file_v7`
  differs from `file_v6` only by declaring the review-guide source, which a
  test pins. It also keeps the v1 to v6 commands under the Linux argument
  headroom, which adding the source to all of them broke.
- **Kept `internal/engine/review_guide.go` off the Tier A list.** It reads
  repository content at commits the import and verification already
  accepted, and returns display text. It grants no authority, reaches no
  credential, and changes nothing published, imported, or verified. The
  Tier A checks sit where they already were: the importer admits the claim
  (`internal/importer/`), and `loadCheckpoint` in
  `production_verification.go` re-checks a stored guide against the import
  it rides on, beside the ready item's own validation.

## Refute-First Findings

- **Forged head binding: disproved by a check.** An entry that declares
  `head_bound`, a source head, high sensitivity, or another media type
  fails the import (`TestImportReviewGuideTamperFailsClosed`).
- **Credential in the guide: disproved by a check.** The raw body is
  scanned, then each decoded string, so a token spelled with JSON escapes
  is still a finding (`TestImportReviewGuideSecretIsAFinding`).
- **Excerpt path escaping the tree: disproved by a check.** Excerpts are
  read only from git objects, never the filesystem. Domain validation
  rejects absolute, dot-dot, and non-canonical paths; a symlink,
  directory, or pathspec-shaped name resolves literally or as
  `path_missing` (`TestResolveReviewGuideReadsEachExcerptFromItsSide`).
- **Swapped guide in a stored checkpoint: disproved by a check.** A
  checkpoint guide for another head, or one naming a claim the import does
  not carry, fails reconcile with `ErrParentKeyMismatch` and creates no
  item (`TestProductionCheckpointReviewGuideTamperFailsClosed`). With the
  checkpoint check disabled, the item's own validation still refuses it,
  with a different error.
- **A guide sharing its digest with another claim: confirmed and fixed.**
  The fresh-context review found it. An agent that writes the same bytes
  to the guide and to `summary.md` gets two claims with one digest. The
  ready item names its guide claim by digest and requires exactly one, so
  the pull request published with no ready item and the task stuck on a
  contradiction. The importer now refuses that import
  (`TestImportReviewGuideSharingADigestFailsClosed`).
- **Excerpts show repository text that was never secret-scanned: allowed
  by decision, for the owner to confirm.** The import scans the files the
  agent changed. A base-side excerpt, or one at an unchanged head file,
  can show a credential already committed to the repository, and the card
  carries it inline at `sensitive`, which clients may cache on disk. The
  same text is already in the repository the reviewer reads, so this
  shows it again rather than exposing anything new. Scanning resolved
  excerpts would close the gap if the owner wants it.
- **Lone UTF-16 surrogate escapes: allowed by decision.** They decode to
  U+FFFD, as everywhere else `encoding/json` decodes, and display as such.
  They reach no authority.

Revisit when: the guide gains a field that grants authority or feeds a
gate, or a second provider launcher declares the source.
