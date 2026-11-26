# Serve a Task's Pull Request as a Reference on Its History

Issue #3684. Tier B, kind:contract.

## Decision

Work-unit completion facts and every task timeline event that carries
`pr_number` (`pr_opened`, `pr_merged`, `external_review_reentered`) also
carry `pr_reference`, the existing `PRReference` (`repo`, `number`), so a
client can link a task's pull request without an item that happens to hold
one. The field is optional in the schema, nullable only on `TaskEvent`, and
built at read time from the recorded PR binding. `pr_number` stays.

## Choices

- **Chose the reference on every event that has a number over `pr_opened`
  alone.** One rule ("a number comes with its reference") is simpler to
  state, validate, and read than a per-kind exception, and the daemon
  already reads a binding at each of the three sites: the work-unit binding
  for `pr_opened`, the authenticated effective binding for completion facts
  and `pr_merged`, and the ready item's binding for an external re-entry.
  `domain.TaskEvent.Validate` enforces the pairing once, across kinds.
- **Chose optional over required, and not nullable on completion facts.**
  Required would make an app reject a timeline from a daemon built before
  the field, which `api/README.md` (Response Tolerance) rules out without a
  compatibility-level bump; leaving it out of `required` is an added
  response field, so the level stays 1 and the floor 0. Completion facts
  always have a PR, so null would describe no state a daemon can serve; the
  event field is nullable because the other kinds carry no PR, matching the
  event's other kind-scoped details.
- **Chose reading the repository from the binding over storing it on the
  completion.** `WorkUnitPRBinding.Repo` and `ReadyItemPRBinding.Repo` have
  been stored and required non-empty since those records existed, and
  completion authentication already reads and compares the binding against
  the run's ready resource. A stored copy would repeat a fact the store
  already holds (against the store-once rule for new tables) and would need
  a migration and backfill for rows recorded before it, while the read-time
  reference serves those rows unchanged.

## Revisit When

- A stored binding's `repo` is found that isn't an `owner/name` coordinate.
  `WorkUnitPRBinding.Validate` requires only non-empty, while
  `PRReference.Validate` requires the pair; such a row now fails its task
  timeline read as an integrity error, the Tier B rule for a bad row.
