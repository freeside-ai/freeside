# The Cordoned Host Process Is a Registered Runner Class

The domain now registers a third `RunnerBackendClass`,
`cordoned_host_process`, with four capabilities of its own:
`supports_cordoned_secret_denial`, `supports_cordoned_launch_denial`,
`supports_cordoned_daemon_protection`, and `supports_cordoned_run_end_proof`.
The class can prove exactly those four (its ceiling), and under `unattended`
it must clear exactly those four whatever the configured floor says (its
minimum). The ward and the macOS verification executor keep their ceilings, so
neither can claim a cordon capability and the cordon can claim none of theirs.
Contract: #3465, tracker #3462.

The names are the plan's (`docs/plan.md` §5.7, revision 134;
`devlog/2026-11-20-2237-cordoned-runs.md`, Names). Renaming one is another plan
revision, not an implementation choice.

This unit admits nothing. No backend carries the class name, nothing writes a
conformance record for it, and an unattended admission still needs a passed
record for its class and configuration at the store's write gate. The
backend (#3466), its proof (#3470), and admission (#3472) come later; #3472
keeps the plan's order that admission waits for #3550.

## Chose No `attended_dev` Minimum Over the Four in Both Modes

Under `attended_dev` the cordoned class has no minimum, as the verification
executor has none. §5.7 lets that mode use a weaker class, and the admission
records what the backend declared, so an attended cordoned run can be
exercised before its proof is complete. Requiring the four in both modes was
the alternative; it would make the dev loop wait on the full conformance
suite. This is the agent's planning default, not an owner decision.

Revisit when the owner wants attended cordoned runs held to the proof, or
when the cordoned backend's proof is cheap enough that the dev loop loses
nothing by requiring it.

## Chose Keeping the Stage Driver's Gate Port Out

Planning found that a cordoned backend can't satisfy `stage.Gate` as written.
`Gate.Handoff` and `Gate.Recover` take a `ward.HandoffSpec`, and the driver
holds each spec to facts only a ward has: a workspace equal to
`ward.WorkspaceRef`, the admitted image, exactly one credential mount, and a
container size. A cordoned launch has none of these. Changing the port changes
the driver's Tier A recovery and authentication paths, and ward refuses to
recover a launch whose rebuilt spec digest differs, so reshaping that type
risks stranding handoffs running across a daemon update. That is a contract
unit of its own, which #3466 starts after; folding it in here would have
coupled a vocabulary registration to a driver change.

## Chose One Snapshot for the Ceiling and the Minimum

`CordonedCapabilities` returns the four for both `ProvableCapabilities` and
`RequiredCapabilities`, so the class's ceiling and its unattended minimum are
equal by construction. They differ for the ward, whose minimum is the
configured floor plus two proofs; for the cordon the plan names one set, and
two copies could drift when #3477 adds the network rule's capability. That
unit decides whether its capability joins the ceiling only or the minimum too.

## Refute-First Findings

One fresh-context reviewer tried to show the registration widens admission.

- **Disproved by trace: an unattended cordoned admission can be written.**
  The store's write gate (`requireAdmissionGates` in
  `internal/store/execution.go`) needs a passed conformance row for the
  admission's class and configuration, and only the ward's and the
  executor's suites write one, each with its own class hardcoded.
- **Disproved by trace: another consumer reads the class or the capability
  list.** Outside tests, nothing iterates either registry, and the only
  switches on the class are the two this unit updates. Engine admission
  checks the configured floor, and doctor's class is hardcoded to the ward.
- **Allowed by decision: `attended_dev` drops the floor for the name.** A
  backend named `cordoned_host_process` was a test backend that kept the
  configured floor; now it has none. No backend carries the name.
