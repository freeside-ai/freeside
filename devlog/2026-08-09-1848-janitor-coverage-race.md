# Janitor Coverage Race

## Decision

Chose to make runtime coverage checks wait for an already-running janitor pass
over retaining the previous pass's coverage or shifting either cadence. Waiting
keeps the withdraw, reconcile, publish lifecycle fail-closed: no caller receives
stale authority while the janitor examines remote grants, and a clean completed
pass no longer appears to be a stopped janitor.

Published diagnostics now partition every completed-pass withdrawal into a
registration fault, removal churn, or incomplete reconciliation caused by the
pass-wide removal budget. The resolver, startup gate, and credential doctor
consume the same three classes. Bare `ErrJanitorInactive` remains the state of a
janitor that has not completed its priming pass.

Persistent active-resource observation failure becomes one advisory
`system_health` item after four consecutive failed passes and resolves on the
next successful observation or when the ready resource is no longer active.
Chose advisory posture after #625 supplied the explicit contract because the
affected mint path already fails closed and should not stop unrelated work.
Remote error text remains in the log only; the durable item carries trusted
local coordinates so an untrusted forge response cannot author attention text.

## Reconstructed Cause

Run 482 was a deterministic phase-locked race, not sticky authority, credential,
or HTTP-client state. The first failed active-resource observation began at
`2026-08-09T15:29:13.217Z`. The janitor occurrence immediately before it was
created at `15:29:13.205Z` and consumed at `15:29:13.755Z`, placing the
observation inside the deliberate coverage-withdrawal window. The 15-minute
active-resource cadence and 30-second durable janitor cadence stayed aligned,
so all 18 observations hit the same window while 538 janitor occurrences in
the outage interval completed as `handled`.

The authority state directory contains no installation-janitor journal, which
rules out recurring removal churn. Restart moved the active-resource phase from
`:13` to `:38` relative to the durable janitor schedule, so the next priming and
observation passed without changing disk or GitHub state. The apparent restart
recovery was cadence dephasing.

## Rejected Alternatives

- Keeping prior coverage during a pass would hide a stalled or newly unsafe
  reconciliation behind stale authority and violate the fail-closed lifecycle.
- Jittering or rescheduling either cadence would reduce collision probability
  without removing the race.
- Treating churn or incomplete reconciliation as a generic inactive janitor
  would preserve the misleading production error and leave operators unable to
  distinguish a never-primed process from a completed fail-closed pass.
- Persisting the observation error in the health item would let untrusted remote
  response text cross into the attention surface.

## Verification Findings

The refute-first coverage matrix enumerates every combination of fault, churn,
and incomplete diagnoses and confirms their union still withdraws the same
registrations while sibling registrations remain covered. A scheduled-pass race
test blocks reconciliation after withdrawal and proves resolution waits, then
succeeds only after the clean pass publishes. A separate drift sequence proves
coverage withdraws with churn attribution and returns on the next clean pass
without restart. Targeted publish and daemon-command packages pass.

Revisit when janitor reconciliation becomes cancelable at the waiting caller's
context boundary; the current pass-completion coordination inherits the pass's
own bounded context, matching the existing onboarding and conformance gates.
