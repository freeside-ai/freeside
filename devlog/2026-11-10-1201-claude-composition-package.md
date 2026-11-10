# Claude Composition Package Boundary

Decision record for #2863: the slice of moving library code out of
`daemon/cmd/freesided` that takes the Claude composition, after #2861's
process shell, #2862's scenario and fake compositions, and #2552's
admission authority. The note is mandatory because the move carries Tier
A files: the Claude driver's GitHub wiring, agent selection, judgment
credentials, the host verification executor, and review credentials.

## One Unit Over a Loops and Driver Split

Planning proposed two units: the background loops (Tier B) first, then
the driver composition (Tier A). The owner chose one deliberately larger
unit on 2026-11-10; the issue records the choice, not a reason. What the
execution showed: the loops and the driver share `startupBindings`, the
session closer, and the configuration input, which one unit exports
once. The cost is a large review: about 8,400 production and 9,900 test
lines move and 766 test lines are copied, behind 345 authored
lines. The rename and the move are separate mechanical commits so the
authored part can be read on its own.

## Main's Fixture Tests Stay in Main

Chose to keep tests that need a fixture only main owns in package main,
reaching the package through exported names, over rebuilding those
fixtures inside `internal/`. The fixtures are the `auth adopt` command,
preflight, `run()`, main's `config`, and the seeded template store; a
rebuild would have been authored test code inside a move whose point is
that nothing authored hides in it. The cost:

- **Test-only exports.** 42 package-level names and 18 fields exist only
  for main's tests. `doc.go` lists them. The plan estimated about 130
  exports in all; the compiler settled 156 (96 for production, 60 for
  tests).
- **Copied helpers.** 57 test declarations (766 lines) that tests on
  both sides use are copied, identical apart from package qualifiers.
  `doc.go` names them, because a copy changed on one side lets the tests
  drift.
- **One fixture changed instead of copied.** The intake recipe test
  computed its work item's digest through `submissionBytes`, which would
  have pinned about 270 lines of main's intake fixture as copies.
  Intake's reconcile and that test now compute the digest with
  `contentaddr.Sum`, the same SHA-256 over the same bytes, in an authored
  commit before the rename.

Revisit when `auth adopt` or a seeding fixture can be reached from
`internal/`: then the main-fixture tests can move and the test-only
exports and copies go.

## Two Files Join the Tier A List

- **`internal/daemoncompose/agent_cutover.go`.** Its `reviewCredential`
  reads the setup token a review admission runs with, so the "unlisted
  file joins the list" rule applies. It was not listed under
  `cmd/freesided` either; planning found the gap.
- **`internal/daemoncompose/auth_adopt.go`.** It holds only
  `ValidReviewLaunchModel`, which was Tier A under `cmd/freesided`'s
  `auth*.go` entry. The contract listed the declaration as Tier A but not
  its new file; the agent added the file so the move does not demote it.

`cmd/freesided/macverify_executor.go` keeps its entry for the flag parser
that stays there, beside the new `internal/daemoncompose/` entry for the
executor it builds.

## The Move Changes No Behavior

The refute-first pass tried to show it does, two ways:

- **Identity.** A `go/ast` script compared declarations, doc comments
  included. In the move commit, 732 declarations move and 57 are copied,
  each byte-identical to its rename-commit text apart from
  `daemoncompose.` qualifiers, except one test method whose one-line body
  lost gofmt's column alignment with neighbours that stayed behind. Every
  staying main declaration changes only by qualifiers, and every one
  before the move is accounted for. A token-level script checked the
  rename commit: 85 files, every changed token a mapped rename. The 152
  declarations in the Tier A files match `origin/main` with the renames
  reversed.
- **Gate mutations.** Five mutations were applied one at a time at
  `origin/main` and after the move, and each failed the same tests both
  times: dropping the unattended admission floor
  (`TestClaudeStoreOptionsCarryTheSelectedModePolicy`, which stays in
  main), letting a Codex review source read another identity's
  credential (`TestReviewCredentialReadsTheAdmittedGeneration`), recording
  capabilities the adapter cannot launch
  (`TestRecordObservedAdapterConformance`), accepting an empty judgment
  token (`TestJudgmentCredentialsFailClosed`), and serving a held judgment
  token for any generation (`TestJudgmentCredentialsFailClosed` and
  `TestJudgmentCallsCarryTheirEnrollmentsToken`). After the move, all but
  the floor's test fail in `internal/daemoncompose`.

The scenario's main-supplied inputs (`ScenarioParts`) stay as they were.
#2552's note expected this unit to let `daemoncompose` build the bare
import authority itself; #2863 made that a non-goal, so the inputs are
unchanged.
