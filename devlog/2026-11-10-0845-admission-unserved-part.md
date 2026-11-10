# An Admission Hold Names the Unserved Part, Never the Typed Text

#2992 lets an `agent_resolution` refusal say which part of a task line's
selection the daemon doesn't serve: the enrollment, the model, or the
effort. The hold records the part and, for a model or effort, the ID of
the enrollment row the store holds. The held-work notice, the Mac, and the
iPhone name the part from those fields. This note records why the hold
carries those two fields and nothing more, why only three legs get a part,
and what the refute-first pass found.

## Decision

- **Chose a typed part and the held enrollment's ID over the refused
  value.** An operator types a task line's names, and a credential can be
  pasted into them, so admission already hides a line's refusal text
  (`hideCause` in `daemon/internal/engine/agent_admission.go`). The `change_task_line`
  endpoint shows the safe shape: it names the field, never the value. The
  part is a code. The enrollment ID comes from the row
  `GetClientEnrollment` returned, so it's a name the daemon holds, not
  whatever the operator typed. When the typed enrollment matches no row,
  the hold names part `enrollment` and no ID, because the only candidate
  would be the typed text.
- **Chose a marker on the enrollment read over classifying
  `store.ErrNotFound` at the refusal site.** The read transaction that
  resolves a line also reads the line, its card, its generation, and its
  catalog, and any of those can return `store.ErrNotFound`. Only the read
  of the names' own enrollment wraps its not-found in `enrollmentNotHeld`,
  so a missing generation or line doesn't read as an unheld enrollment.
- **Chose to keep the served-tuple cause in `ResolveAgentSelection`'s
  chain over parsing its text.** The refusal's text quotes the names, so it
  can't be read. `servedRefusal` keeps the text as it was and also unwraps
  to `ErrDiscoveredModelUnserved` or `ErrDiscoveredEffortUnserved`. The
  part is read before `refuse` flattens the error.
- **Chose to name a part only for the three served-tuple legs.** A disabled
  identity, a route missing from the tree, a route model list, a value a
  launch can't pass as one option, and no adapter for the launch each have
  their own fix, and none of them is "this enrollment doesn't serve that".
  They record no part, as before.
- **Chose no part for an alternate-agent card's refusal.** The card governs
  the held attempt ahead of the task line, so the Mac's instruction to
  change the task's agent would not reach it. A card whose enrollment is
  gone, or whose catalog stopped serving its model, reads as it did before.

## Consequence for the Effort Leg

An effort the adapter can't send is refused before the served-tuple rule
runs: `AdapterForLaunch` picks the adapter by effort and fails with
`ErrNoAdapterForLaunch`, which names no part. So the effort part appears
only for a native effort the enrollment's catalog doesn't list for the
model. A typed effort such as `ultra` still reads as a resolution refusal
with no part. The issue's non-goals keep that leg out; the engine test
`TestOtherResolutionRefusalsNameNoPart` pins it.

## Refute-First

A fresh-context reviewer tried to refute the change before push.

- **Disproved: typed text reaches the hold or the notice.** The held ID
  comes only from the row `GetClientEnrollment` returned, the enrollment
  part carries no ID, and both wrappers keep the refusal's text unchanged.
- **Disproved: an admission decision or error class changes.** No hold
  class or caller branches on the newly wrapped `ErrDiscovered*`
  sentinels, and `ErrAgentJoinInvalid` still matches.
- **Disproved: a not-found from another read names the enrollment.** A
  missing generation or line returns no marker, so it names no part.
- **Confirmed and fixed: the pacer ignored the part.** A hold whose part
  changed kept its pace state, so the old part stayed on the Mac until the
  next refresh. The part and its enrollment now join the pace state.
- **Confirmed and fixed: a card's refusal took the line's guidance.** See
  Decision above.
- **Allowed: the full card and lineup admission paths have no engine test
  of their own.** A unit test pins the card rule in `unservedPart`; an
  end-to-end card admission runs only in the integration suite.

Revisit when admission resolves a selection's effort before it picks an
adapter, or when the adapter-selection refusal gets its own part.
