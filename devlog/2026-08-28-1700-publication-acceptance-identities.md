# Bind Production Acceptance to Both Authenticated Invocation Owners

Work unit: #996. Mandatory note: returned-object trust boundary and revision of
the 2026-08-17 production-supervision decision.

## Decision

Production supervision carries two authenticated invocation identities from
the validated publication task. The producing invocation owns the completed
terminal record. The dedicated publication invocation owns
`publication_ready`. Acceptance requires each record under its own owner after
the publication task is dispatched, and re-anchors `publication_ready` to the
store-owned ready item and publication binding before treating it as authority.

This revises one assumption in
`2026-08-17-1537-production-supervision-snapshot.md`: the two final records do
not both name one invocation. That pairing was unsatisfiable in the production
workflow, which records `publication_ready` under `task.PublicationID` and the
terminal under `task.ProducingInvocationID`.

Re-keying `publication_ready` to the producing invocation was rejected. Signet
sync and the durable ready binding authenticate the milestone as belonging to
the dedicated publication invocation. Moving the write would invalidate that
persisted convention and make future ready records fail authentication.

## Refute-First Verification

- Confirmed: `decodeProductionPublicationTask` runs `task.validate()` before
  either identity can cross the completion boundary. A forged
  `PublicationID` fails the deterministic publish-invocation check.
- Confirmed: incomplete and error returns carry zero identities. Only a
  dispatched task with its authenticated completed terminal returns both.
- Confirmed: `publication_ready` under the producing invocation, or a terminal
  under any invocation other than the producer, leaves supervision at
  `publication_ready`.
- Confirmed: milestones without an invocation cannot panic the acceptance
  scan and do not satisfy either side.
- Confirmed: a structurally valid `publication_ready` milestone is not
  authority by itself. A missing or divergent ready-item binding now fails the
  snapshot read before supervision can report `published`; reconstruction of
  that binding rechecks the ready item, producing admission and export, and
  publication intent and outcome.
- Confirmed: `publication_blocked` is unaffected. The acceptance gate runs
  only for the final published outcome.

## Revisit When

Milestone ownership changes, the production publication task stops
authenticating both invocation identities, or supervision gains authority to
resume or perform publication effects.
