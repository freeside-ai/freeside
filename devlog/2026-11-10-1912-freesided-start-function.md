# Freesided's Start Function Boundary

Decision record for #2864: `run()` in `daemon/cmd/freesided` shrinks to
config translation and one call to `daemoncompose.Start`, after #2861,
#2862, #2552, and #2863 moved the compositions it calls. The note is
mandatory because two Tier A files move: the credential-integrity probe
and the startup model discovery, which both read stored credentials.

## Main Keeps `config`; Start Takes `StartOptions`

Chose a new `StartOptions` in `daemoncompose` over moving main's
`config`. `config` is the flag set: its fields, defaults, and
`validateRunConfig` belong to the command line, and main's tests build
it directly. `run()` validates and defaults it, reads the seed fixture
and the manual-submission file, and builds `StartOptions`; `Start`
acquires and starts everything. The six test seams moved into a
`StartSeams` struct that `StartOptions` embeds, so the moved code reads
them as before and main's `config` holds one `seams` field.

Main still supplies what only it has: the readiness environment name,
the build version, the auth identity label, the fake driver's
auto-scripting wrapper (beside the kill-recovery checkpoints), the
store options, and `OpenControl`.

## The Control Socket Is an Interface Main Implements

The control routes and the pairing files stay in main; `pairing_*.go` is
Tier A client authority and the contract forbids changing it. `Start`
reaches them through `ControlSocket`: `daemonshell.PairingControl` plus
`Configure`, `RegisterRoutes`, and `Publish`, called at the points
`run()` called them, in the same order. Main's `pairingControlSocket`
adapter carries the values the routes read from main's configuration.
The rejected option, moving the routes, would have moved Tier A pairing
code in a unit scoped as a mechanical move. Revisit when the control
routes move out of main.

## Validation Runs Before the Lock

The interval checks and `storeOptions()` now run in `run()`, before
`Start` takes the database lock, instead of after it. Both acquire
nothing. Two effects follow, both allowed by this decision: a
misconfigured start reports the config error rather than the lock error
while another daemon holds the lock, and a refused config no longer
creates the state directory, which opening the pairing control used to
do first. `TestRunValidatesConfiguration` checks only for an error, and
no test pins which one wins. The start time `/health` reports is now
read inside `Start`, after validation, microseconds later than before.

## Test-Only Exports and Copies

Tests that need `run()`, the doctor command, `auth add`, or the
manual-submission fixture stay in main, as in #2863. That costs three
test-only names (`NewCredentialIntegrityProbe`,
`ProductionCredentialIntegrityObservers`, `RequireManualProjectsFit`)
and eleven fields (`CredentialIntegrityObservers.SetupToken`, `Daemon`'s
`Store`, `Attention`, `Workflow`, `Driver`, and `Usage`, and the five
seams other than `Now`). Ten test declarations are copied into both
packages. Main's copies of `workUnitCompletedMilestones`,
`assertCompletionMilestone`, and `capturedRun` lost their last users and
are deleted. `doc.go` lists the exports and copies.

## Two Files Join the Tier A List

- **`internal/daemoncompose/credential_integrity.go`.** The live probe
  reads setup-token volumes through the rig runtime authorizer and Codex
  stores under the review root.
- **`internal/daemoncompose/model_discovery.go`.** It reads each
  enrollment's setup token or Codex probe credential under the
  identity's read hold and hands it to the discovery probe.

The probe was Tier A under main's `credential_*.go` entry; model
discovery was unlisted in main and joins under the "unlisted file joins
the list" rule. The `cmd/freesided/credential_*.go` entry stays, because
`credentialIdentityLabel` remains in main's `credential_integrity.go`.

## Departures From the Contract

- **`pairing_control_test.go` changes.** The contract says no
  `pairing_*.go` file changes, and also that tests change only in how
  they reach a moved name or a seam. `TestPairingCodeRenewsRunningDaemon`
  sets the clock seam and reads the daemon's store, workflow, and
  attention service, so the seam struct and the exported `Daemon` fields
  the contract proposes reach it: `now:` becomes `seams:
  daemoncompose.StartSeams{Now: ...}` and `h.workflow` becomes
  `h.Workflow`. No assertion changes, and no production pairing file
  changes. Chose that over unexported alias fields on main's `daemon`
  and a `now` field on `config`, which would be authored code whose only
  job is to keep one test file's text still.
- **`daemon/.golangci.yml` changes.** The forbidigo exclusion for the
  completion reconcile's `ObserveRun` read named
  `cmd/freesided/completion_reconcile.go`; it now names the file's new
  path. The contract's scope omitted the lint config.

## The Move Changes No Behavior

The refute-first pass tried to show it does:

- **Identity.** A `go/ast` script compared declarations, doc comments
  included, between the rename commit and the move commit. 73
  declarations move and 10 are copied, each byte-identical apart from
  `daemoncompose.` qualifiers; 3 dead duplicates are deleted, identical
  to the copies that remain; the 1,779 main and 826 `daemoncompose`
  declarations that stay change only by qualifiers. Top-level tests
  number 726 before and after.
- **Rename.** A token-level script checked the rename commit: 40 files
  and 326 changed tokens, every one a mapped rename.
- **Gate mutations.** Six mutations to the two Tier A files were
  applied one at a time at `origin/main` and after the move, running the
  `cmd/freesided` and `daemoncompose` tests. Each gave the same result
  both times. Four fail:
  - Model discovery reading a non-current generation's token, and
    discovering a disabled identity's enrollments, both fail
    `TestModelCatalogRefreshDiscoversEachEnabledClaudeEnrollment`, which
    moved to `daemoncompose`.
  - The probe reading Codex stores outside the review root, and dropping
    a store's "not checked" result, both fail
    `TestDoctorCommandProbesStoredCredentialsWithoutLeakingThem`, which
    stays in main.

  Two survive both before and after the move, so the gap predates this
  unit: the probe observing setup-token volumes with no rig runtime
  authorizer, and Codex discovery taking a Codex enrollment of any auth
  method. #3052 tracks the missing tests.
- **Fresh-context review.** A reviewer prompted to refute found no
  blocking defect. It confirmed acquisition, defer, and cleanup order,
  that every flag reaches `StartOptions`, that the control-socket
  adapter calls back at the same points with the same inputs, and that
  the Tier A files change only by names and qualifiers. Its two
  non-blocking findings are the later start time and the state
  directory a refused config no longer creates; both are recorded above
  and allowed.
