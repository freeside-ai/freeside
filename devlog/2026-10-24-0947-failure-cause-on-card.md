# Failure Cause on the Execution-Failure Card

Issue #2210. #899 recorded a cause on every failed execution outcome but left
the attention item without it, so an operator on a phone could not see why a
run failed. This unit copies the outcome's cause onto the item's facts, the
API, both clients, and `inspect task`, and gives the daemon's own launch
refusal a positive cause.

## Decisions

- **Chose `cause,omitempty` plus stored-row canonicalization over a pointer
  or a backfill.** The facts mirror `ExecutionOutcome`: a cause is present
  exactly on a failed outcome, `unknown` included, and absent otherwise, so a
  canceled or lost item's stored body keeps its bytes. A failed item stored
  before this change decodes with no cause; `AttentionItem.CanonicalizeStoredRow`
  gives it `unknown`, the cause its outcome reconstructs with. That is what
  lets the store's put-idempotence compare and the card-fact immutability
  check converge on the old row instead of failing as an immutable
  transition. No migration rewrites item bodies.
- **Chose to authenticate the card's cause against the outcome over trusting
  the copied value (the agent's call, beyond the plan).** The store's
  execution-failure gate already re-reads the immutable outcome and refuses
  an item whose outcome status disagrees. The cause is the same kind of
  copied coordinate, and under #869 it will drive retry policy, so the gate
  now refuses a cause that disagrees too. This is safe for stored rows only
  because no production writer recorded a positive cause before this unit:
  every legacy failed item and its outcome both canonicalize to `unknown`.
- **Chose a narrow classifier over reading the writer's transcript.** Only a
  handoff spec the stage driver refuses to build (`ErrUnsupportedStart`)
  records `invalid_configuration`; that refusal is the daemon's own
  judgment, so the cause is trusted. A writer that exits nonzero keeps
  `unknown`. Its transcript is untrusted provider text, and a cause read out
  of it would become a policy input under #869, so that classifier is a trust
  decision for its own unit. Most configuration refusals are caught earlier,
  when `Start` authenticates the spec, so in practice most failed items still
  read `unknown`, with the retained transcript claim beside them.

## Known Limits

- A writer's nonzero exit is never classified; the item says `unknown`.
- `invalid_configuration` is wider than operator configuration. Every
  `ErrUnsupportedStart` from the handoff-spec build classifies, including
  the driver's checks of a provider-built spec against the durable input,
  whose failure is a daemon fault. No policy reads the cause yet
  (`DecideFailureDisposition` treats it like `unknown`); #869 must narrow
  it before acting on it.
- The `ward launch` log line still carries no cause: it is written where the
  cause isn't known and exists for container sizing.

Revisit when a transcript classifier or #869's retry card lands, since either
changes which causes reach the card and what they authorize.
