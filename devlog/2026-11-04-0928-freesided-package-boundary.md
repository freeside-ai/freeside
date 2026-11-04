# Freesided Package Boundary, First Slice

Decision record for #560: the first slice of moving library code out of
`daemon/cmd/freesided` and behind `internal/` package boundaries. The owner
accepted the slice boundary and the exported names on 2026-11-04, in the
session that assigned the unit; the agent decided the remaining points
while executing the issue's plan.

## Two Controls Move First; the Admission Authority Waits

Chose to move only the §5.4 export credential scan and the project-image
preparation re-gate, over moving `storeAdmissionAuthority` in the same unit,
because the two controls are small, self-contained, and depend only on
`ward` and `domain`, while the admission authority is about 735 lines,
leans on `internal/engine` and `internal/importer`, and needs an exported
API designed for seven construction sites in six files. Moving it here
would have doubled the unit and mixed a mechanical move with API design.
The parent session files the admission move as its own follow-up issue.

## Where Each Control Landed

- **Credential scan: new package `internal/exportscan`.** Chose a package
  of its own over folding it into `ward` or `internal/export`. `ward`
  takes the scan as an injected `OutputScanner` hook, and keeping the
  hook's implementation outside `ward` preserves that seam.
  `internal/export` is the trusted helper that ships inside the exporter
  image and runs before the ward verifies the export, the wrong side of
  the gate for a check on verified output. It is listed under Import and
  handoff in AGENTS.md's Tier A paths, because it refuses a handoff.
- **Preparation re-gate: existing `internal/projectimage`, as
  `ResolvePreparation` and `ErrComposition`.** That package owns
  `PreparationPath` and the builder and onboarding policy the gate re-runs,
  so the re-check now sits beside the value it pins. It is listed beside
  `rebuild_gate.go` in the Tier A paths.
- **`ResolvePreparation` takes the image, repository, and repository ID,
  not `claudeDriverConfig`.** Chose explicit arguments over exporting or
  mirroring the driver config, because the gate reads only those three
  fields and `projectimage` must not depend on `package main`. Preflight
  no longer builds a throwaway driver config to call it.

## The Moves Change No Behavior

The refute-first pass for this behavior-preserving refactor compared the
code at `0267e409` with the moved code on generated inputs, using temporary
tests that were not committed:

- **Scanner:** 400 generated export trees, with nested directories,
  symlinks, near-miss and real credential shapes, files straddling the
  1 MiB chunk boundary, and missing roots (112 refusals, 274 clean, 14
  operational errors). Every error string and every `errors.Is` result
  against `ward.ErrOutputScanRefused` and `os.ErrNotExist` matched, as did
  a cancelled context. The pattern list, its names, and the chunk constants
  are identical.
- **Re-gate:** 20,000 generated record and configuration combinations (167
  admitted, 19,833 refused). Outputs, error strings, and sentinel matching
  were identical, and an admitted command is always a copy, never an alias
  of the record's slice.

Revisit when the admission-authority follow-up lands: it should follow the
same rule (state the gate's re-checks in its doc comment, list its new path
in AGENTS.md Tier A Paths in the same PR).
