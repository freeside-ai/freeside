# Verify Completed History at the Caller

For #1852, chose the existing authenticated completed-checkpoint verifier and
remote merge check over accepting supervision success or weakening ordinary
publication verification. Supervision can first observe an implementation
after its PR merged. That selects retained verification, but establishes no
success by itself. The caller requires a successful verifier exit, its exact
completed marker, a fresh completed checkpoint bound to the selected run,
and the existing check of the remote publication and recorded merge.

Keep the selected admission invocation even when a publication successor
produced the accepted head. The verifier authenticates that successor itself.
Client-target identity comes from the selected target after supervision binds
it, rather than from the ignored seed.

Completed history ends the exercise through its existing EXIT trap. It does
not enter the ready-publication walkthrough or grant new execution authority.
This preserves the distinction recorded in
[completed-history restoration](2026-09-09-1918-completed-history-restoration.md)
and closes the caller gap identified in
[completed supervision](2026-10-08-0815-completed-supervision-state.md).

## Refute-First Findings

- **Supervision alone could establish success.** Disproved by extracted
  caller fixtures that refuse verifier failure or skip, missing positive
  evidence, malformed checkpoints, and remote refusal. The grounding caller
  rejected the direct completed fixture through normal publication verification.
- **A foreign result or partial run token could authenticate completion.**
  Disproved by wrong snapshot run, missing snapshot run, malformed snapshot,
  wrong checkpoint run, missing binding, wrong marker run, and marker-prefix
  fixtures. Ready and retained evidence cannot satisfy completed verification.
- **Stale output or inherited verifier inputs could change the result.**
  Disproved by seeded old positive logs and checkpoints, skip fixtures, and
  explicit mode/path assertions in the verifier stand-in. The log is cleared
  before verification and a completed checkpoint gets a new private directory.
  Failure to clear the log stops the caller.
- **Mode assignments could precede an existing environment option.** Confirmed
  by the full caller suite: the specification environment begins with an
  unset option outside client-target mode. Keep that array before the new mode
  assignments. The focused fixtures extract the shipped setup and reject an
  inherited specification identity when none was selected.
- **The accepted successor could replace the admission identity.** Disproved
  by fixtures whose snapshot names a different invocation while the verifier
  receives the selected admission. Existing daemon fixtures authenticate
  original and successor completion, reject forged completion and foreign
  publication facts, and preserve normal-mode and continuation refusals.
- **Completion could enter the ready walkthrough or mask cleanup failure.**
  Disproved by executing the shipped EXIT trap with restoration spies.
  Completed success calls remote verification and cleanup without a walkthrough;
  restoration failure changes the overall exit to failure. Published success
  keeps its walkthrough and never falls back to completed verification.

## Revisit When

Revisit if supervision success states, checkpoint authentication or output,
selected admission binding, remote merge checks, or cleanup semantics change.
