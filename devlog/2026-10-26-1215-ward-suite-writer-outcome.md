# Ward Suite Writer: Report Its Outcome Like a Production Writer

Work unit: #2345. Mandatory note: it widens what rig cleanup and stale
recovery may act on (a destructive path) and changes what a conformance
failure reason may print (a credential-leak surface).

## Decision: Use the Production Outcome Marker, Not a Suite-Only Channel

Chose to have the suite writer record `<nonce> <status>` at the production
marker path (`<workspace>/.freeside-evidence/.control/writer-outcome`) and
set `AgentSpec.OutcomeMarkerPath`, over teaching the suite to infer the
writer's fate from the export. The Wave 8 exit run's failure (#1613) showed
why inference fails: a writer that stopped before its workspace clear
exported the seeded checkout, and the reason could only say the sentinel
was missing. With the marker, `Backend.Handoff` reads the status before it
exports, and the suite names the step from a code table the script is built
from. A status of 0 still proves nothing: the sentinel digest stays the
proof.

## Decision: Admit the Observer's Name to the Rig Namespace

The marker needs the handoff's writer-outcome observer, a new container,
`freeside-handoff-conf-<16 hex>-writer-check`, for each synthetic handoff.
Full binds every name it may create into the rig manifest before its first
runtime call, and the rig's container pattern refused this one; a daemonlock
test pinned that refusal as an "unused synthetic handoff role". Chose to add
exactly `writer-check` to the conformance handoff roles, matching the
production handoff prefix, which already admits it. The refusal test now
pins `cred-pre`, a production-only role the suite never creates. This is a
departure from the issue's ward-only path list: without it, the production
rig refuses to bind Full's names and the deputy can't start.

## Decision: Summarize, Never Echo, the Export

Every containment reason after a clean writer exit appends the entry count
and, for at most four entries, kind, size, and blob state. Only
`result.txt` and `nested/state.txt` print by name, with whether their
content matches the fixture; every other path, including invalid-path
entries, prints as `<redacted>`. No digest or blob content prints.

## Refute-First Pass

A fresh-context reviewer tried to prove the change wrong.

- **Naming the step hid teardown errors.** CONFIRMED, FIXED. `Handoff`
  joins teardown, journal, and cancellation errors onto the writer
  failure, and replacing the whole error with the containment reason
  dropped them, so a leaked workspace volume went unreported. The suite now
  replaces only a bare writer failure, joins any other error after the
  reason, and leaves a canceled run's error alone. A test injects a
  workspace delete failure.
- **Two step labels could misname the step.** CONFIRMED, FIXED. `find
  -exec rm` exits 0 when `rm` fails, so exit 95 covers only the fixture
  writes, and an incomplete clear fails the export's shape check instead;
  the tool check now covers `nc`, which used to surface as a refused
  CONNECT.
- **No test ran the generated script.** CONFIRMED, FIXED. A test runs it
  under the host shell with stub network tools and checks the exit code,
  the marker, and the workspace for four outcomes. The reviewer also ran
  every code under dash. Busybox ash wasn't checked.
- **Shell semantics.** DISPROVED: `set -e` holds inside the subshell, the
  `{ ... } || exit` group chains as written, and the outer shell writes the
  marker unless it is killed, which fails as a missing marker under
  `writer_termination`.
- **Summary leaks the marker.** DISPROVED: `Kind` is enum-validated before
  the suite reads it, only the two exact fixture paths print, and no digest
  or content prints.
- **A new pass path.** DISPROVED: a forged `nonce 0` still needs the
  sentinel export, and the marker sits under the skipped evidence directory.
- **Regex over-admission.** DISPROVED: the anchored pattern gains exactly
  `freeside-handoff-conf-[0-9a-f]{16}-writer-check`, a superset of the old
  one, so manifests an older daemon bound still validate. Only
  `cmd/freesided` mints `conf-` run IDs.
- **Production writer failures change.** DISPROVED: the error text and
  `errors.Is(err, ErrWriterFailed)` are unchanged, and recovery of a
  conformance record through `recover.go` has no caller today.

## Revisit When

The production marker path or its directory controls change, the
conformance handoff gains another runtime object, or #574 adds a cause
field to `ConformanceFailure`.
