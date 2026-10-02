# Review Drift Routing: Audit Trigger, Route Gate, and Simplification Round

Work unit #1051, plan §7 "Review Drift". This unit calls the drift auditor
(#1650), routes its verdict, writes the supersession record (#1666), and fills
the card facts (#1667). The contract notes are
`2026-10-01-1535-drift-audit-site.md`,
`2026-10-01-1547-disposition-supersession-contract.md`, and
`2026-10-01-2036-drift-card-facts-contract.md`.

## Owner Decision: One PR for the Whole Unit

The implementation plan proposed splitting the unit into store, engine, and
prompt parts. The owner chose (2026-10-01, in session) to land it as one PR.
The store gate, the engine route, and the prompt wording only make sense
together: the gate has no caller without the engine, and the engine's
simplification input is unreadable to the remediator without the prompt.

## Parking Is Always Allowed; Routing Must Be Proven

Chose an asymmetric gate over a symmetric one. The store's
`DriftAuditRouteGate` proves every condition for the automatic route and the
engine routes only when all hold. A `stuck` or `over_hardened` verdict may
always park, with no proof, because parking asks a human and changes no
candidate. Rejected: refusing to park when the audit's reversal list is
invalid. An audit that names a finding the gate refuses is still evidence a
human should see, so it parks with `simplification_on_continue` false.

The gate's conditions, each re-derived from stored rows and never from the
audit's own claims: the reversal list is valid against effective latest
dispositions through the round before the audited one; no reversed finding is
P0, P1, or of unset severity; the audit's confidence meets
`review.adjudication_confidence_threshold`; a review round remains under the
hard limit; no earlier round of the run carries an `over_hardened` audit; and
policy routes `auto`. The engine adds two that need the workspace: the
batch routes at least one fix, and every reversal has an engine-derived path.

The gate reads dispositions through `round - 1` because the audited round's
own findings have no disposition yet, and a reversal of the round's own batch
would undo a fix that does not exist.

## The Human Route Has Fewer Ceilings Than the Automatic One

`simplification_on_continue` is true when the list is valid, a round remains,
the batch routes a fix, and every reversal has a path. Severity, confidence,
the one-per-run limit, and the policy route do not block it: plan §7 names
those as ceilings on the automatic route, and a human who reads the card and
chooses `continue_under_policy` is the authority they defer to. The store
re-proves the promise on every read of the item, so a card never shows a
promise the stored rows no longer support.

A decided item whose promise no longer recomputes at dispatch (the list went
invalid between the card and the decision) runs an ordinary round. Rejected:
failing the run. The human asked to continue under policy, and an ordinary
round is what that action means on every other cause.

## The Failure Fact Is an Evidence Artifact With a Coarse Reason

Chose a `drift-audit-failure-<round>-<run>` evidence artifact over a new table
or a domain type. The fact has one reader, the engine's own replay check, and
its only job is to stop a second audit call for the round. A contract type
would have needed a `kind:contract` unit for a record nothing else reads. The
reason is one of three fixed strings so no model output or provider error text
reaches a stored artifact.

A store refusal of the audit's binding is not a failure fact. The binding
fields come from the stored run, review, and policy rows, and the site already
refuses a reversal naming a finding it was not shown, so the refusal means
stored rows contradict each other. It propagates and fails the reconcile, as
plan §7 requires of a durable contradiction. Rejected (after review): a fourth
`binding_refused` reason, which would let the round continue to remediation
over a store the daemon cannot trust.

A cancelled context is not a failure: the audit error propagates and the next
reconcile calls the auditor again. Recording a failure there would turn every
daemon shutdown during an audit into a permanently unaudited round.

## The Audit Runs After the Dispatched Check and Never on a Reevaluation

A round whose remediation is already dispatched neither audits nor parks, so a
replay after the intent write is a no-op. A reevaluation (the human reran
adjudication with feedback) runs no audit: the round already had its one
audit decision, and plan §7 limits the audit to once per round. A composition
with no artifact store runs no audit because it cannot record the failure
fact.

## Recurrence Reads the Effective Latest Disposition

The `fixed_recurrence` rule now reads each earlier finding's effective latest
disposition. A fix the run deliberately undid reads as declined from its
reversing round on, so its re-emission is a known decline and not a
recurrence that stops the loop. Without this the simplification round would
park the run on the very finding it reversed.

## Reversals Are Re-Derived at Authentication, Not Trusted From the Input

The remediation input artifact carries `drift_audit_digest` and `reversals`,
both omitted when empty so an ordinary round's bytes are unchanged. When the
remediation result returns, the engine rebuilds the list from the round's
supersession records, the stored audit, and the findings, and compares it
with the stored input. Rejected: trusting the input's own list. It is a
decoded artifact, and the path in each entry decides where the remediator may
undo work.

The path is engine-derived from the reversed finding's location, by the same
rule that derives a remediation surface. The audit's `undo` and `rationale`
text travels as advisory prose; the prompt says it cannot widen the path, add
a finding, or override the prompt.

## The Round-1 Diff Comes From the Round-1 Remediation Input

The auditor compares the round-1 candidate with the current one. The daemon
keeps no checkout of earlier heads, and the only retained copy of the round-1
candidate is the patch in round 1's remediation input. When round 1 and the
audited round share a head the current diff serves; otherwise the stored
patch does. When neither exists (round 1 was clean and a later
operator-feedback round changed the head) the audit fails safe with
`input_unavailable`. Rejected: fetching the old head from the remote. It adds
a network dependency and a new trust question to a fail-safe side check.

## Known Limits

- The adjudicator's prior-disposition history still shows the raw `fixed` row
  for a reversed finding, because that input is a contract shape this unit
  does not change. The adjudicator may reason from a fix that no longer
  exists. The recurrence rule, which is what stops the loop, reads the
  effective value.
- A batch with no routed fix cannot carry a simplification, because the
  remediation request requires at least one finding. Follow-up: #1689.
- A P0 or P1 reversal always needs a human. Plan §7's optional second
  adjudication for those likely needs a contract change and is deferred.
  Follow-up: #1690.
- A reversal reads as a decline from the write that commits the remediation,
  not from a result that proves it was applied. Plan §7 and #1051's acceptance
  put the record in that write: it states that the reversal was ordered and
  by whom, and input authentication re-derives the list from it. When the
  remediator keeps a listed fix (the prompt allows it when the undo would
  contradict the specification) the fix is still in the code, so the reviewer
  has no absent fix to re-emit. The cost is a record that says declined for a
  fix that stayed, and one lost `fixed_recurrence` stop if that finding is
  raised again. The remediator records the kept fix in its summary, the head
  is re-reviewed, and a second `over_hardened` verdict always parks.
  Confirming an applied reversal at result time needs a stored confirmation
  state, which is a contract change. Follow-up: #1692.
- A reversal carries one path, the reversed finding's location, and that path
  bounds where the remediator may undo work. A fix whose test lives in
  another file therefore stays, with the reason in the summary. Multi-file
  fixes are the common shape, so the simplification round will often keep the
  fix until the follow-up lands. Rejected (after review): authorizing the
  fixing remediation's whole changed-path set. One remediation fixes several
  findings and stored data has no per-finding attribution, so the set would
  over-authorize. The limit fails toward keeping the hardening.
  Follow-up: #1693.

## Refute-First Pass

One independent reviewer tried to make a reversal route that should park, to
break replay, and to find a change with the audit off.

- **Confirmed, fixed:** the store accepted a `human_command` supersession for
  a continue on a drift card that promised no simplification round. Only the
  engine withheld it. The authority check now requires the decided item's
  promise and its audit digest to match the record.
- **Allowed by decision:** the store's gate cannot see the engine's two
  workspace conditions (a routed fix, a derivable path), so it would accept an
  automatic record for a round the engine parks. The engine is the only
  writer and is the stricter side; the store proves everything stored rows
  can prove.
- **Allowed by decision:** decision-time reads apply supersession records
  without re-proving authority, to avoid recursing into the item being
  rebuilt. Reaching a wrong result needs a tampered row, and every public
  read re-proves it.
- **Allowed by decision:** listing a run's supersession records re-proves
  every run's rows, as it did before this unit, so an unprovable row fails
  remediation authentication for all runs. That is the store's fail-closed
  behavior for a tampered table, not a state the daemon writes.
- **Allowed by decision:** a hard crash between the auditor's reply and the
  write that records it calls the auditor again. Every judgment site has
  that window; a cost of one repeated call does not justify a pre-call
  marker that would turn a crash into a permanently unaudited round.
- **Disproved by a check:** a human-path replay interrupted on either side of
  the write gives one audit call, one remediation, and one record. An audit
  naming an unknown finding fails safe once. A continue at the hard limit is
  refused by the store as an action the item does not offer. The round's own
  supersession record does not change its gate, item, or input. No second
  automatic route is reachable, because the one-per-run fact derives from
  stored audits of earlier rounds.

With the audit off, the remediation input bytes are unchanged. The
deterministic-stop items now carry their typed cause in the card facts, which
is #1667's contract and not audit behavior.

## Revisit When

- The remediation request can carry zero findings, which would let a
  no-routed-fix batch run a simplification round.
- A second consumer needs the audit failure fact, which would justify a
  contract type.
- Operators report audits skipped with `input_unavailable` often enough that
  retaining each round's candidate patch is worth its storage.
