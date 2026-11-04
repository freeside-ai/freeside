# The Project-Image Provenance Gate Is Tier A

#562 moves the OCI archive decoding and verification out of
`daemon/internal/projectimage/apple.go` into `oci_evidence.go`. Neither
file was on the AGENTS.md Tier A list, though the code in both decides
whether ward may run an image built from untrusted input. This note
records the classification (the owner accepted it on 2026-11-04), what
stayed in `apple.go`, and the refute-first result for the move.

## Decision

- **Chose Tier A for `apple.go` and `oci_evidence.go`** over leaving them
  as ordinary Tier B code. A project image is built by running the
  repository's own dependency install, so its exported archive is
  untrusted input. `appleBackend.CheckProvenance` reads that archive
  through `readOCIEvidence` and refuses an image that isn't derived from
  the approved base, sets a non-root user, replaces the base's BusyBox,
  or lacks the pinned toolchain bytes. Plan §5.7 (Golden Agent
  and Project Images) makes this the gate a derived project image passes
  before ward runs it: "a compliant base does not make the extension
  trusted". That is runner containment with untrusted input reaching it,
  a Tier A category, so under the AGENTS.md rule "an unlisted file joins
  the list" both files join the runner-containment bullet. Only
  `projectimage/rebuild_gate.go` was listed before.
- **Kept the provenance expectations and `decodeStrictJSON` in
  `apple.go`.** `rootImageUser`, `toolchainEvidence`, `pinnedArchive`,
  `expectedToolchainEvidence`, `projectEvidenceTargets`, and the archive
  size limits are no more Apple-specific than the decoder, but they are
  provenance policy (what an image must contain), not OCI decoding (how
  to read an archive). Moving them is a separate decision for whoever
  adds a second runtime. `decodeStrictJSON` stays because the Apple
  inspect code and `identity.go` call it too.

## Refute-First Pass

The change is a pure move inside one package, so the old-versus-new check
is declaration identity rather than generated inputs. A scratch program
parsed package `projectimage` at the base (`0267e409`) and at the move
commit with `go/parser`, printed every top-level declaration with
`go/printer` including its comments, keyed by name, with each constant
keyed individually as name, type, and value so the split `const` block
compares entry by entry. All 505 declarations, tests included, printed
identically. A negative control (one constant's value changed, one
comment added) showed both differences, so the comparison would have
caught a logic edit hidden in the move.

A fresh-context reviewer, using its own declaration dumper, tried to
refute the move:

- **Disproved by checks:** no declaration dropped, duplicated, or changed
  (505 at both revisions; the split constants keep their values); the
  three moved test bodies match the base and 41 tests remain; the
  file comment in `oci_evidence.go` is not package documentation.
- **Allowed by decision: the Apple doc comment says "the only
  Apple-specific step of evidence reading".** `CheckProvenance` also
  calls the Apple inspect code, but that reads image metadata, not the
  archive; the sentence is the contract's wording and scopes to
  evidence reading.
- **Allowed by decision: the blame entry names the move commit's SHA.**
  A rebase that rewrites the move commit makes the entry stale, so it is
  rechecked after every rebase, as for the earlier move entries.

Revisit when a second container runtime needs the provenance
expectations, or when the provenance check moves out of `projectimage`.
