# Auth Add Adopts A Codex Store A First Enrollment Finished

Follows `devlog/2026-11-19-1348-first-codex-enrollment-no-hold.md` (#3438).
After it, a finished first `enroll-codex` leaves its marker resolved by the
daemon, and `RecoverableVerified` reports that as recoverable so a rerun
recovers the result instead of spending the login again. `auth add -client
codex_cli` refused whenever recovery was found, so for an identity
`enroll-codex` first enrolled it refused for good, and its advice (finish
with `enroll-codex`, resolve the item) led nowhere. Issue #3492.

## The Fix

Chose to record the client enrollment over the live store, as `auth adopt`
does, over two alternatives:

- **Lift the refusal.** Rejected: `auth add` would begin a new operation,
  write the input login over the live store, and open a hold. The README
  tells the operator to pass the `enroll-codex` input flags, so that login is
  usually the one `enroll-codex` already spent, and writing it over the store
  breaks the reviewer. That is the failure #3438's refute-first pass fixed
  for `enroll-codex` reruns.
- **Keep refusing with better words.** Rejected: outside the deputy's
  `auth adopt`, no step records the enrollment, so any refusal is a dead end.

The journal port now says whether a found result is pending (its marker is
still open) or a finished first enrollment. Pending still refuses, with
steps that work. Finished returns `ward.ErrCodexAuthAlreadyEnrolled` before
the input is read, and the command calls `ward.AdoptCodexAuth`, which reads
the store, binds the account its tokens name, and appends generation 1, or
reuses the enrollment an earlier run recorded. `enroll-codex` is unchanged.

An identity whose first enrollment was an `auth add` takes the same path on
a rerun, so that rerun now reuses the enrollment instead of refusing.

## Findings

Refute-first pass, recorded so the claims don't return:

- **Wrong account.** Allowed by decision: adoption never reads the input
  login, so it binds the store's account, the one the first enrollment
  verified, even if the operator passes another login. Comparing the two
  would mean reading the input on this path. The stderr line says the login
  was not used, and `auth list` shows the masked label.
- **Route on reuse.** Confirmed and fixed: `reuseEnrollment` doesn't compare
  routes, so a rerun naming another `-route` reported success while recording
  nothing. `auth add` now refuses that; `auth adopt` keeps its behavior.
- **Disabled identity.** Allowed by decision: adoption enables the identity,
  while `auth add`'s own enrollment keeps the stored bit. The only disabled
  identity this path can reach is one the cutover retired that `enroll-codex`
  then enrolled again, and the operator's `auth add` asks to put it to use.
- **Crashed refresh intent.** Allowed by decision: `AdoptCodexAuth` records
  the store as it stands without recovering a refresh a crash left
  mid-transaction. `auth add` refuses a running daemon, so no refresh races
  it, and the next daemon refresh recovers the intent under the lease. This
  is the exposure `auth adopt` already has; changing `AdoptCodexAuth` is a
  non-goal of #3492.
- **Store not re-checked against the journal.** Allowed by decision:
  adoption checks the store's shape, account, and expiry, not the verified
  binding, so a store whose chain was revoked with no marker opened yet is
  adopted as it stands. `auth adopt` does the same, and the first run that
  meets the revocation opens the hold.
- **Pending mistaken for finished.** Disproved by the predicate: finished
  requires no open marker and pending requires exactly one, so the two
  can't both hold, and a revoked chain's replacement reports pending
  (`TestFirstCodexEnrollmentLeavesNoHold`).

Revisit when #1673 folds `enroll-codex` into `auth add`.
