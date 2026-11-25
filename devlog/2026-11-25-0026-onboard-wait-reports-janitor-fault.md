# Onboarding's Installation Wait Reports the Janitor's Fault

#2382's routing rerun waited out `freesided onboard`'s whole hour on a
trusted installation and failed with a bare `context deadline exceeded`.
The janitor knew the cause all along: a sibling installation's mint hold
refused its grant read, so the registration got no coverage. #3581 makes
the wait say so. The coverage rule itself is unchanged: one held
installation still leaves its whole registration uncovered, because the
held token may be live.

## Chose a "Hold Ends at or After the Deadline" Rule With No Margin

The wait fails at once when a mint hold on the registration ends at or
after the wait's deadline (its start plus `-install-wait`). A hold ending
earlier is printed and the wait goes on.

A hold that ends a few seconds before the deadline still can't succeed in
practice: after it ends, a pass must mint, read grants, and publish
coverage. We kept the exact rule anyway, as the issue's contract states
it. A margin would be a guess about pass timing that could fail a wait
that would have succeeded, and the cost of the exact rule is at most a
short wait that ends in a timeout naming the same hold.

## Chose a Typed Refusal Over a New Janitor Accessor

`checkMintHold` now returns `*publish.JanitorMintHeldError`, carrying the
installation ID and `held_until`. Its message is the old text, and its
`Is` matches `ErrJanitorMintHeld`, so `supervision.go`,
`project_readiness.go`, and the hold tests behave as before.

Rejected:

- **A janitor accessor listing current holds.** It would add a second
  published view beside `RegistrationFaults()` with its own lifetime
  rules, and the fault already carries the hold on the path that denies
  coverage.
- **Parsing the time out of the fault's text.** It would tie the wait to
  the message format.

The fields are diagnostic only. Nothing that grants coverage, a mint, or
readiness reads them; `AllowsRepository` stays the wait's only gate.

## Read Faults Before Stopping the Janitor

`RegistrationFaults()` is empty once `Run` returns, so the timeout path
reads the faults before it cancels the janitor, as
`withDurableInstallationPoll` already does on intent expiry.

## Refute-First Findings

One fresh-context reviewer tried to prove the change wrong.

- **Disproved by trace:** the typed error changes no caller. Every
  existing caller matches with `errors.Is`, none compares with `==` or
  matches the text, and `reconcileApp` wraps with `%w`, so `errors.As`
  reaches the hold. `janitor_hold_test.go` now asserts both.
- **Disproved by trace:** onboarding's own janitor can't fail the wait
  early on a hold it took itself. While it holds the token,
  `checkMintHold` retries the revoke instead of refusing.
- **Allowed, immaterial:** when the janitor stops on its own (an unsafe
  pass), its faults clear before `Run` returns. So the wait may print a
  "cleared" line just before it returns the janitor's error. The error
  still names the real cause. Ruling the line out would need janitor-side
  stopped state, which the line doesn't justify.
- **Allowed by the contract:** the early failure counts a held sibling
  even if the operator removes that installation before the hold ends.

Revisit when: the janitor covers a registration's other installations
while one is held, or the hold's lifetime changes so that early failure
would turn away waits that could succeed.
