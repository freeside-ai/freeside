# Let Auth Adopt Enable the Identity It Adopts

Work unit #1679. `freesided auth adopt` now enables an identity that is stored
disabled, in the store transaction that records its first enrollment. This
note records the owner decision that allows it, the decision it overturns, and
the rule that keeps it safe once `auth disable` (#1639) exists.

## Owner Decision

The owner decided on 2026-10-01 that adoption may enable.

- **Overturned, for the `auth adopt` path only:** "enrollment is not the place
  to re-enable an identity". That rule was stated in the
  `recordEnrollingIdentity` comment and applied to adoption in
  `2026-10-01-1439-agent-selection-cutover.md` ("A Disabled Flag-Era
  Identity"). `auth add` still keeps the stored bit.
- **Why it was wrong for adoption.** The old real-run harness seed step
  rewrote its identities without the `enabled` field, so an instance it
  launched since #894 holds them disabled. #867 removed the identity flags,
  which never read the bit. That instance could enroll but not select, and had
  no command to enable an identity.
- **Changed assumption.** No deliberate disable can exist before #1639 ships
  `auth disable`. Until then a disabled flag-era identity is always the
  seed-step artifact.

## The Ordering Rule

Chose to enable only in the transaction that records the identity's first
enrollment, over enabling whenever adoption sees a disabled identity, because
the changed assumption expires when #1639 lands.

- **An enrolled identity keeps its stored bit.** Adoption reports it `reused`,
  and `disabled` when it is. After #1639 a disable on an enrolled identity can
  be deliberate, and adoption can't tell it from the seed-step artifact. With
  this rule the unit is correct whichever of it and #1639 merges first.
- **The enable shares the first-enrollment write.** The bootstrap carries the
  request (`ward.EnrollmentBootstrap.EnableIdentity`), and the store sets the
  bit in the record that binds the account and cost owner. Only adoption sets
  the field.
- **The store re-checks the request.** In that transaction it refuses a
  bootstrap that asks to enable an identity already holding an enrollment
  with a generation, so the rule holds whatever a caller passes. No current
  caller reaches the refusal.
- **Rejected: a second write from `auth_adopt.go` after the enrollment.** A
  failure between the two writes leaves the identity enrolled and disabled. A
  rerun reports it `reused` and, by the rule above, leaves it disabled: the
  stuck state this unit removes.
- **Rejected: enabling a `reused` identity.** It would undo a deliberate
  disable after #1639.

## Accepted Consequences

- **A disabled identity with no completed enrollment is enabled when
  adopted.** After #1639 an operator could disable a flag-era identity and
  then adopt it. Adopting an identity names it for use, so this is the
  decision working as stated. The same holds for an adoption whose first run
  recorded the enrollment and failed before its generation: the rerun
  completes the first enrollment and enables.
- **An instance that already adopted under #867 is not fixed.** Its identity
  is enrolled and disabled, so adoption reports it `reused` and `disabled`.
  It needs `auth enable` (#1639).
- **The report reads the bit before adoption.** `enabled` and `disabled` come
  from the identity read before the write. That is accurate only while `auth
  adopt` holds the daemon lock.

## Refute-First Outcomes

A fresh-context reviewer tried to refute the uncommitted change.

- Disproved by a check: adoption's `Begin` reaching an identity that holds an
  enrollment with a generation. `Enrolled` matches on the harness client, not
  the enrollment id, and runs before `adoptStore`. Provider is a fixed
  binding, `auth add` builds only `claude` with `claude_code` or `openai` with
  `codex_cli`, and each adoption path gates on provider, so no identity holds
  an enrollment for a second client.
- Disproved by a check: another path setting the field. `adoptStore` is its
  only writer; both `auth add` bootstraps leave it unset.
- Disproved by a check: a refused first-enrollment transaction changing the
  identity. The identity write, the enrollment write, and the lease share one
  transaction that commits only on success.
- Allowed: an enrollment left without a generation (a failed append) is not
  "enrolled" for this rule, so a disable recorded in that window is undone by
  the next adoption. See Accepted Consequences.
- Allowed: when `Begin` commits and the generation append fails, the run
  aborts with no report line, and the rerun reads the identity as enabled, so
  no run reports `enabled` for it. Neither report is false.
- Confirmed and fixed: the store-backed failed-append test did not pin the
  identity's state between the failed run and the rerun.

Revisit when #1639 lands: the `reused`, `disabled` report line and the README
can then name `auth enable` as a command that exists.
