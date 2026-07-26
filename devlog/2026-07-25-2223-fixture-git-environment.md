# Fixture Git Environment Isolation

Work unit: #286. Scope: `daemon/internal/importer`,
`daemon/internal/integration`, and `daemon/internal/ward` test helpers.
Destructive-retarget surface; mandatory note.

## Decisions

**Fixture Git helpers remove every ambient `GIT_*` entry while preserving the
rest of the process environment.** Chose the publish fixture's scrub-and-append
pattern over the verify fixture's full environment replacement because macOS
Git needs ordinary Darwin environment entries to resolve its temporary
directory without contaminating command output. Each helper appends only its
deliberate Git config, identity, and timestamp values after the scrub, so
rebase, hook, and parent-tool Git state cannot retarget the fixture.

**The regression performs a harmless repository-local config write and reads
both configs directly.** Chose a test-owned decoy repository over pointing
`GIT_DIR` at the project checkout, so a failed regression proves the retarget
class without risking user work. Direct file reads prove the marker reached
only the intended fixture and cannot repeat the helper bug while checking the
result.

**The small scrubbers remain package-local.** Rejected a shared test utility
because the three helpers live in separate packages and a new shared package
would widen this test-only fix beyond its declared paths.

## Refute-First Verification

- A full replacement was rejected by verification: under the constrained
  macOS test environment, Git emitted a Darwin temporary-directory warning
  into stdout, corrupting fixture SHA results. Preserving non-Git environment
  entries removed that failure while retaining the `GIT_*` boundary.
- Each affected helper was exercised with `GIT_DIR` aimed at a distinct
  temporary decoy. The intended repository alone received the config marker.
- A mechanical scan confirmed the three issue-listed helpers no longer append
  directly to `os.Environ()`.
- An independent fresh-context refute pass added simultaneous hostile
  `GIT_DIR`, `GIT_WORK_TREE`, `GIT_OBJECT_DIRECTORY`, and `GIT_CONFIG_*` state,
  found no surviving retarget path or false-positive regression, and passed
  all three affected packages.

## Revisit When

A fourth fixture helper needs the same boundary, at which point a narrowly
scoped shared test utility may be warranted.
