# A Cordoned Run Records `host_open`, and Its Start Spec Names the Class

Issue #3583, split from #3472 (tracker #3462), gives a cordoned run a
truthful record before anything admits one. Plan §5.4 says every cordoned
run records the `host_open` egress profile, and the stage driver needs to
know which class a start was admitted on. The owner handed over the
issue's contract and its implementation plan with `Handle #3583`; the
choices below are the plan's, kept unchanged, plus the agent's
implementation choices.

## Chose a Two-Way Profile and Class Rule in Every Mode

`ExecutionAdmission.Validate` refuses a `host_` profile on any class but
`cordoned_host_process`, and refuses a cordoned admission whose profile
isn't a `host_` one. The rule reads no operating mode, so an attended
admission can't record a ward profile for a host process either. A
cordoned writer has the host's network; an admission that said
`provider_only` would claim a containment nothing enforces.

Rejected:

- **Only refusing `host_` profiles off the cordon, leaving "a cordoned
  admission records a `host_` profile" to #3472.** It would keep the
  engine's cordoned dispatch test alive, but until #3472 a cordoned
  admission could still be built with a false profile. The record is the
  point of this unit, so the stricter rule ships now.
- **A new error sentinel.** The refusal wraps `ErrInvalidEgressProfile` and
  names both the profile and the backend; callers already branch on that
  sentinel.

## Chose an Identity for `host_open`

`validateAuthIdentity` treats `host_open` like the provider profiles: the
admission names an auth identity. A cordoned writer runs under an
enrollment's credential (plan §5.4, `subscription_contained`), and #3584
reads that enrollment from the admission. Revisit when a cordoned role
with no credential is planned; its profile would need the
clean-verification stance instead.

## Kept the Ward Refusing Host Profiles

`engine.WithAdmission`, the ward's admission option, refuses `host_open`
as an enforceable profile and so as its default. The ward, the stage
driver, and the Claude spec builder already refuse every profile but
`provider_only` and `provider_registry`, so nothing downstream changed.

## Carried the Class in the Start Spec Only When Cordoned

`exec.StartSpec` gains `RunnerClass` with `omitempty`, and
`StartSpecFromAdmission` sets it only for a cordoned admission. The start
authority compares a journaled spec with the one its admission rebuilds,
field for field, so the class is bound with no new check. Setting it on
every spec would have made every journaled warded spec differ from its
rebuild and refused in-flight recovery after an upgrade; the unchanged
`start_spec.golden` pins that warded bytes don't move.

Rejected: telling the driver the class through its composition instead of
the spec. A composition-wide setting isn't bound to the admission, so a
recovered start could run on a class its record never named.

## Removed the Engine's Cordoned Dispatch Test Until #3472

`TestCordonedDispatchWaitsForACapAndReservesNoMemory` dispatched a
specification stage on a fake backend named `cordoned_host_process`.
After this rule no engine path can build a valid cordoned admission: the
ward option refuses `host_open`, and a specification requires
`provider_only`. Removed as `accepted:` the parts stay covered (the cap by
the store tests in `cordoned_launch_cap_internal_test.go`, the hold by
`TestHoldClassificationGolden`, the launch order by
`TestLaunchOrderWaitsOnTheRefusingResource`, the memory skip by
`TestCordonedExecutionReservesNoMemory`), and nothing in production admits
a cordoned attempt before #3472, which restores the end-to-end test on its
real admission path.
