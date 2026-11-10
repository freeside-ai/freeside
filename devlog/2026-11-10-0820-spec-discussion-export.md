# A Specification Discussion's Unattended Export Routes by Its Marker

#2991 adds a second export-only arm to the unattended branch of
`RecordExecutionExport` (`daemon/internal/engine/production_export_record.go`).
A discussion on a specification card (`specification-discussion-<command>`)
runs under the specification run and stage. Before this arm, the unattended
branch sent its export to `loadProductionRequest`, which looked for an
`inv-implement-<run>` marker that never exists for a specification run, so
the export-record step retried forever. The reply never posted and the task
could not be stopped. This is the #768 shape for a second invocation family.

## Decision

- **Chose the invocation's own outbox marker over an ID-prefix check.** The
  #768 arm uses `IsSpecificationInvocationIdentity`, which works because
  `inv-specify-<run>-<iter>` names its run, so the check binds the ID to the
  admitted run and stage. A discussion ID names a command, not a run, so a
  prefix check could tell only that the ID looks like a discussion, not that
  it belongs to this run. The arm reads the marker under the invocation ID.
  When its kind is `KindSpecificationDiscussionRequested`, it re-checks the
  marker with `AuthenticateSpecificationDiscussionTransition` against the
  admitted run and stage. That is the same check admission runs in
  `storeAdmissionAuthority.authenticateSpecificationInvocation`.
- **Chose to fail on a marker that fails that check over falling through.**
  If the authentication fails, the arm returns the error and records nothing.
  Falling through would hand a known discussion to the production path, which
  would refuse it anyway, but with an error that names the wrong cause. An
  absent marker or another kind falls through unchanged.
- **Kept the gate in the Tier A file.** The plan put the helper in
  `specification_discussion.go`. The arm decides whether an export may mint
  a publication task, so the helper sits in `production_export_record.go`,
  already on the Tier A list, and its doc comment names what it re-checks.
  `specification_discussion.go` only provides the authenticator that
  admission already calls, and it joins no list.
- **The change narrows publication.** The new arm only routes more exports
  to the export-only write, which mints no publication task. No export that
  used to mint a task now skips one: an `inv-implement-*` invocation's marker
  has the production kind, so the arm returns false for it.

## Refute-First Pass

A fresh-context reviewer tried to prove the arm wrong. It found no blocking
defect.

- **Disproved: a production invocation can be read as a discussion.** The
  outbox holds one row per key, so a production invocation's row has the
  production kind. A forged discussion row under a production ID would also
  have to decode with its key equal to its request's invocation ID and pass
  the run, stage, and binding checks.
- **Disproved: the authenticator fails a legitimate discussion later in its
  life.** `verifySpecificationDiscussionBinding` and
  `verifySpecificationTerminal` check only append-only facts: the chain, the
  terminal, the approval claims, the inputs, the conversation prefix digest,
  the discussion artifact, and the operator's discuss marker (only for
  quarantine, which only a migration sets). They never check item status or
  version or whether a marker was dispatched. An approval, stop, or request
  for changes during the discussion, a dispatched marker, and a crash replay
  all still authenticate. Admission and import ran the same check, so a
  state that fails here already failed there.
- **Disproved: the arms overlap.** The operator-feedback arm keys on its own
  marker kind, and the #768 arm's `inv-specify-` IDs can't collide with
  `specification-discussion-` IDs.
- **Disproved: a later step on the unattended path still fails.** Terminal
  acceptance, which posts the reply, doesn't depend on the operating mode.
- **Confirmed, fixed: the tests' "no publication task" check was weak.** It
  looked up one run's task key, so it would miss a task minted under another
  key. Both export-only tests now require that no publication task is
  pending at all.
- **Allowed: a refused discussion surfaces as an authority conflict.** The
  stage driver maps `ErrParentKeyMismatch` from the export record to
  `errExportAuthorityConflict`, which fails loud and is not retried. That is
  the right outcome, although that error's comment describes a conflicting
  stored export. The operator-feedback arm behaves the same way.

Revisit when another invocation family runs under a specification or
implementation run with an ID that doesn't name its run.
