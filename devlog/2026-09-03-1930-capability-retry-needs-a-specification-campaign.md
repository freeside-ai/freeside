# Capability Retry Is Only Reachable on a Specification-Backed Campaign

Chose to drive the real specification-approval path in the #1102 admission
proof over submitting a bare production run, because a capability retry is
structurally unreachable without a campaign, and only specification approval
creates one.

## What Changed the Approach

The issue and its implementation plan assumed `engine.SubmitProductionRun`
would produce a run a capability retry could extend. It does not.
`SubmitProductionRun` writes no `ProductionAttempt` unless it is handed a
campaign that already exists, and every path a retry needs reads one:

- `reconcileCapabilityRetry` resolves the parent through
  `GetProductionAttemptByRun`, so a run with no attempt row fails the pass.
- `loadReattemptInputs` additionally requires the parent's approved spec
  digest to match its run, the parent's publication marker, and the
  specification request marker at `specificationInvocationID(specRun, 1)`.

Hand-seeding that set would have forged most of the lineage the proof is
supposed to exercise. Driving `SubmitSpecificationRun` plus an approval
command produces all of it from production code, and the failure card, the
failed admission, and the stage attempt then come from the engine as the
acceptance requires.

## Rejected Options

- **Seed the attempt-1 lineage row directly.** Smaller fixture, but the
  retry's own input loader reads four more parent records, so the seed grows
  until the test is asserting about state it wrote itself.
- **Keep the fixture's default composition.** The failure producer offers a
  manifest only when the composition declares its profile enforceable, and
  the admission gate re-gates against that same set, so the unattended
  fixture had to gain a way to widen it.

## Revisit When

A campaign becomes creatable outside specification approval, or the retry
reconciler stops resolving its parent through the production attempt. Either
would make a lighter fixture correct for this proof.
