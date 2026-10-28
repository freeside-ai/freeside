# One Admission Can Skip Workspace Hydration

Mandatory note: contract change (#2405). `domain.ExecutionAdmission` and
`exec.StartSpec` gain an optional `WorkspaceHydration` fact that the stage
driver honors, and `domain.ImageUpkeepRefusal` exports its canonical
preimage. #2336's repair task needs both: it runs at a refused tip whose
manifests disagree with the image's baked copy, so the hydration helper
would fail the attempt before the agent starts. The plan is the
implementation-plan comment on #2405; this note records why the contract
has this shape.

## Decisions

- **Chose a per-admission fact over a daemon-wide toggle or a field on
  `StartSpec` alone.** Hydration was composition config, so no single start
  could opt out. The admission is the record recovery re-authenticates a
  start against (`claude_driver.go` compares the journaled spec with
  `StartSpecFromAdmission` by deep equality), so the fact has to live there
  and be content-addressed. A spec-only field would let a journaled spec
  disagree with its admission, and a daemon toggle would skip hydration for
  every task that runs while it is set.
- **Chose a named enum over a `bool`.** `WorkspaceHydration` has one member,
  `none`, and the zero value means "as composition says". A second mode
  (for example, hydrating from a different manifest) can register a member
  without a new field or encoding. The stage driver dispatches on it with a
  default-less switch, so the `exhaustive` linter makes a new member decide
  there.
- **Chose a new `v6` encoding selected last, over folding the field into an
  existing version.** The field is omitted from the canonical encoding when
  unset, so every stored admission keeps its ID. A set value selects v6
  after every other version check, so a skipping admission always carries
  the v6 label whatever else it binds. The new golden is agent-bound to pin
  that ordering.
- **Chose to record the skip as an empty `Preparation` in the intent, over
  teaching recovery to read the spec.** Recovery already rebuilds the launch
  command from the intent's recorded argv, and the decode gate already
  accepts an empty value. Recording no argv makes a skipping start recover
  to the same command under any later composition, with no new recovery
  path.
- **Chose to export the refusal's preimage over re-deriving its bytes in the
  engine.** #2336 stores the bytes as the maintenance task's source
  artifact, and the blob store accepts them only because they hash to the
  refusal ID. `ComputeID` now hashes `Preimage()`, so the stored bytes and
  the ID cannot drift apart.

## Not Decided Here

Which tasks skip hydration, and the engine path that sets the fact, belong
to #2336. Nothing in this unit sets it in production. Decoding re-checks
only that the value is a member and that the ID still addresses the row; no
policy here limits which admissions may carry the skip. The skip only
removes the helper from the launch, so it cannot widen what a start runs,
but #2336 owns the rule for which tasks may set it.

## Review Findings

A fresh-context refute-first review found no contract defect. Two
low-severity findings were declined:

- **A decoded intent can carry the skip and the helper together.** Allowed:
  only a hand-edited intent file reaches that state, and the decode gate
  still limits `Preparation` to the fixed helper, so no argv is added.
- **The production authority's deep-equality check has no test with the
  skip set.** Allowed: it compares the output of the one conversion this
  unit covers, and `daemon/cmd/freesided` is outside this unit's scope.
  #2336 exercises it end to end.

Revisit when a second hydration mode is needed, or when a task other than
#2336's repair task wants to skip hydration.
