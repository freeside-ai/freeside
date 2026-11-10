# Admission Authority Package Boundary

Decision record for #2552: the second slice of moving library code out of
`daemon/cmd/freesided`, after #560's first. The issue contract fixed the
package name and exported surface; the agent decided the test layout and
fixture while executing the plan. The note is mandatory because the
authority is a Tier A gate.

## The Authority Moves to `internal/admissionauth`

Chose a new package over folding the authority into `internal/exec/stage`,
`internal/engine`, or `internal/daemoncompose`. `stage` declares
`stage.AdmissionAuthority` but can't import `engine`, which imports
`stage`. `engine` is what the authority calls into, and the gate would
disappear among its lifecycle code. `daemoncompose` could import
everything the authority needs, but it is composition, and a Tier A gate
listed by its own path is easier to audit than one file inside a
composition package. The name avoids colliding with the many locals named
`admission` at its callers. It moved now because #2863 moves
the Claude composition, which constructs it, out of package main.

## The Exported Surface Keeps Today's Construction Shapes

- **`Authority` exports its fields** (`Store`, `Blobs`, `AllowedPaths`,
  `CommitAuthors`, `StartAuthors`) over a constructor with options. The
  scenario lane builds it with only a store and blobs, the Claude driver
  with all five, and the tests change single fields between assertions; a
  constructor would have added a shape for each without adding a check.
  A nil `StartAuthors` still means authenticate on every start, and a nil
  `CommitAuthors` still refuses a production author claim; the field
  comments say so.
- **`CommitAuthorResolver`, `CommitAuthorCache`, `NewCommitAuthorCache`,
  and `ResolvedPathAllowlist`** are exported because a caller outside the
  package needs each. Everything else stays unexported, including the
  `admissionReadKind` constants. `AllAdmissionReadKinds` was already
  exported in package main and nothing reads it; it moved unchanged, and
  lint accepts it.

## The Tests Seed Through the Engine

The authority tests reached their state through `runSubmitCommand`,
which is package main. Chose an engine-seeded fixture over keeping those
tests behind in `cmd/freesided`, because a gate's tests belong beside it
and #2863 would have to move them anyway. The fixture does what submit
does: the specification and policy bytes go to the blob store, both
artifacts register with `engine.RegisterSubmissionArtifact`, and the
specification run converges with `engine.SubmitSpecificationRunTx`;
production runs come from `engine.SubmitProductionRun`. Its policy keys
are written in canonical order so their JSON hashes to the resolved
policy's digest, which the specification run checks against the policy
artifact. That rebuild is its own commit, still in package main, so the
move commit stays mechanical.

Tests that use only the exported surface (the path allowlist and released
import) run from `admissionauth_test`; the rest call unexported methods
and stay internal, as #560 allowed. `export_test.go` aliases the shared
fixture and counting resolver for the external tests, chosen over a second
copy of each.

## The Move Changes No Behavior

The refute-first pass tried to show the move changes behavior, two ways:

- **Identity.** With the renames reversed, the moved `authority.go`
  (before its doc comments changed) is byte-identical to lines 388 to 1127
  of `claude_driver.go` at `1c06bf36`; only the package clause and imports
  differ. Each moved test, with the same renames, is identical to its
  pre-move version apart from package qualification and one inline
  replacement for `storeConformanceRecorder`, which stays in main. The
  move commit also adds the little test code the moved tests need outside
  package main: `export_test.go` and a copy of the
  `testRunEvidenceMetadata` helper. Chose
  this over running old and new code on generated inputs, because the
  compiled logic is the same text over the same dependencies, and a
  differential run would compare the code with itself.
- **Gate mutations.** Eleven mutations were applied one at a time, before
  and after the move, with the same results. Ten each fail at least one
  moved test: dropping the App identity comparison, skipping the
  specification marker read, treating a missing marker as production,
  accepting a configured path list the policy disagrees with, skipping
  backend conformance, never consulting the start-author cache, ignoring a
  changed cache binding, ignoring the bound issue in the fallback message,
  resolving the latest trust profile for an unbound unattended admission,
  and skipping the remediation transition check.
- **Confirmed gap, not caused here.** Disabling the start-spec comparison
  fails no test in `./cmd/freesided` or `./internal/admissionauth`, before
  or after the move. Follow-up: #3007.

Revisit when #2863 moves the Claude composition: `daemoncompose` can then
build the scenario's bare `Authority` itself instead of receiving it in
`ScenarioParts.Imports`.
