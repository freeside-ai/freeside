# Observe Claude Adapter Conformance

For #1672, chose the production Claude writer launcher over a separate probe
command. A nonce read from the fixture and a nonce delivered only through
instructions make read, instruction, and mutation observations independent.
The launcher-authored summary label proves the structured evidence channel;
the normalized evidence manifest retains labels and provenance, not source
paths. Session and route observations come from the pinned harness transcript
and ward's attested topology and credential-store observations.

The local fixture also carries ward's required daemon-authored repository
marks (canonical repository name and numeric ID) and a detached HEAD. The plan
assumed checking the local HEAD would suffice; ward independently requires
these identity marks and the detached checkout before it observes the base.
The configured identity is preserved while the
fixture's commit and file tree remain isolated from repository instructions.

Chose an invocation-specific enrollment binding over the legacy identity
volume. Conformance has no task admission, so the ordinary volume resolver
would otherwise read the identity's interim binding. The daemon binds only
its unpredictable conformance invocation to the implementer's exact enrolled
generation and retains the store-backed shared read hold. Role resolution
checks its integrity mark before launch. Recovery can still clean up an old
generation after it is marked. Ordinary invocations keep their existing
admission-bound resolver.

A failed observation appends a failed adapter generation even after provider
timeout or cancellation. Persistence gets a separate ten-second context so an
expired provider context cannot leave an earlier declaration-only pass active.
Observation failures hold writer admission without invalidating a passed ward
suite. Persistence failures remain errors.

The plan's fixture inventory was stale at implementation: the engine admission
fixture also called the removed declaration recorder. Its proof now names an
explicit capability set, like the other admission fixtures. No production
engine behavior changes.

## Refute-First Findings

- Confirmed and fixed: the provider builder leaves resource sizing to the
  shared stage driver. The conformance entry point now supplies ward's
  conformance class and fixed size; a regression assertion pins both.
- Confirmed and fixed by independent review: a direct journaled handoff had
  no durable recovery owner after a cleanup failure or daemon crash. The
  daemon now writes the spec and enrollment binding before handoff, keeps
  the seed until ward proves runtime absence, and recovers retained runs
  before startup or another full pass. Recovered output never becomes proof.
- Disproved by tests: a modified credential store or an extra telemetry
  authority could retain route/store coverage. Each removes that capability.
- Disproved by tests: timeout cancellation could prevent a failed proof
  from superseding a prior pass. Failure persistence uses a detached context.
- Confirmed and fixed in reconstruction: the volume resolver re-reads the
  enrollment's identity binding from the store instead of trusting the
  enrollment fields decoded from a recovery intent. A cross-identity fixture
  is refused.

Revisit the judge when the pinned harness's init or result envelope changes,
or when a new adapter build changes the launch contract.

## Live Discovery

The pinned Claude Code 2.1.282 launch completed the challenge and observed five
capabilities, but its init record reports one plugin. A repeat diagnostic run
confirmed that the requested session matches and the MCP-server array is
empty, while the plugin array has one entry. This invalidates the plan's
assumption that this launch would report no plugins. It does not by itself
prove that user configuration entered the launch.

The judge keeps `context_severance` absent rather than interpreting an
unexplained plugin as safe. Per #1672's explicit stop condition, the change
must stay a draft until the plugin's origin and the appropriate severance
evidence are resolved. The issue carries the next decision.
