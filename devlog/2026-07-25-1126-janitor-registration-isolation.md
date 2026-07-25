# Deny One Registration, Not The Whole Janitor Loop

Work unit: #281. Changes the blast radius of a runtime capability gate
on a destructive path, so a note and a refute-first pass are both
mandatory. Prior notes: `devlog/2026-07-24-2117-installation-authority-snapshot.md`
(#276, which filed this), `devlog/2026-07-25-0946-keystore-entry-policy.md`
(#284, whose skip-and-report shape this follows).

## Problem

`InstallationJanitor.Run` withdrew coverage before every pass and
returned on the first error `runCycle` produced, and `runCycle`
returned at the first failing registration. One unusable registration
therefore shut the gate for all of them: `ActiveFor` went false
everywhere, every mint failed with `ErrJanitorInactive`, and only a
daemon restart cleared it. `runCycle` also ended the pass silently,
with no error at all, whenever a registration completed removals.

#276 made this reachable in ordinary operation. Its authority store
must refuse a registration its operator-authored snapshot does not
name, because an empty authority is an instruction to delete every
installation that registration has, and onboarding writes the keystore
record before the operator writes the snapshot entry. #276 mitigated it
by contract; this closes it in code.

## Decisions

- **A failure is either attributable to one registration or fatal to
  the pass, and the split is explicit.** Attributable failures (the
  authority source, authority validation, the forge's answers about one
  registration's installations) become faults: that registration is
  omitted from coverage, which shuts its own gate, and the pass
  continues. Three classes stay fatal: keystore enumeration, a canceled
  context, and `errJanitorUnsafe`.
- **`errJanitorUnsafe` is the daemon's own safety machinery, not one
  registration's state.** It covers the shared audit journal, and a
  grant-read token the janitor minted that it cannot account for. The
  refute pass demonstrated the first two members: a full disk faulted
  whichever registration's drift reached the journal first while every
  other registration kept minting on a withdrawal barrier the daemon
  had just proven it could not write; and a permanently failing revoke
  turned one leaked live token into one per pass forever (measured 35
  leaked tokens in 2s at a 50 ms interval, against 1 before the
  change). Continuing past either would be acting destructively without
  a barrier, or issuing credentials the daemon cannot take back.
- **The mint's own outcome is part of that class, because minting is
  not idempotent.** Review found the sibling the refute pass missed:
  a lost response, a 5xx, or a 201 whose token value never reached the
  daemon all leave a credential that may be live for an hour and that
  nothing can revoke, and retrying every pass accumulates them. Only a
  4xx refusal proves GitHub created nothing, so that alone stays this
  registration's own failure (a wrong key, a withdrawn installation).
  The rule is stated on the outcome, not on the error text: once the
  request is on the wire, the daemon must be able to account for what
  it may have created.
- **The removal bound is spent on destructive attempts, not
  completions.** `cycle.Removed` counted successful deletes, which
  bounded the pass only because a failed removal ended it. With the
  pass continuing, every registration got one destructive request
  beyond the operator's bound; reproduced as two account-visible
  suspends under a bound of one. An unexported `attempted` counter
  carries the bound; `Removed` keeps meaning "completed".
- **Coverage is withdrawn from any registration ID that also faulted in
  the same pass.** Enumeration is keyed by owner directory, so two
  keystore records can carry one registration ID; that ambiguity is why
  `ErrAmbiguousAppRegistration` exists. Without the withdrawal the
  record that reconciled opened the gate for an ID whose sibling record
  failed validation, and the doctor could never report it, since it
  consults faults only when the gate is shut.
- **Faults outlive the gate they explain.** Coverage is withdrawn
  before every pass; faults are replaced only when a pass finishes.
  Clearing both would leave a registration failing for hours reporting
  as merely unvisited for as long as each pass takes; measured at 33%
  fault visibility before the change.
- **Surfacing is an accessor plus a doctor code, not a logger.** There
  is no logger anywhere in the daemon and no production composer for
  `Run`, so its error return is a dead channel. `RegistrationFaults()`
  holds the reason; `CredentialDoctor` reports
  `janitor_registration_failed` where it previously could only say
  `janitor_inactive`. This is #284's `UnexpectedEntries` →
  `unexpected_keystore_entry` shape.
- **The doctor discovers the fault port on the `JanitorStatus` it
  already holds**, by unexported interface assertion, rather than
  taking a second dependency. The fault explaining a shut gate then
  always comes from the janitor that shut it. `JanitorStatus` is
  unchanged, so the resolver and every fake keep their two methods.
- **`RunCycle` still returns per-registration failures as an error.**
  The one-off diagnostic form has no later pass to report through.

### Owner Decisions Recorded Here

- **The silent `!complete` case was pulled into scope.** The issue's
  acceptance names only the error paths, but a registration that
  completed removals ended the pass for every sibling with no error and
  no diagnosis: the same blast radius, and removals are routine. The
  owner chose to include it.
- **Surfacing goes all the way to a doctor finding**, rather than
  stopping at an accessor with no consumer. `janitor_inactive` cannot
  distinguish "has not run yet" from "failing every pass", and after
  this change the second state persists indefinitely instead of killing
  the daemon.

## Rejected Alternatives

- **Narrowing the resolver's gate loop** so an inactive registration
  does not deny an unrelated owner's resolve. Rejected on the merits:
  the unscoped `Resolve` considers every candidate registration
  precisely to detect ambiguity, and skipping one narrows the set it
  picks among, which can resolve to the wrong App (`keystore.go:476-480`,
  the #284 note). Consequence recorded honestly below and filed.
- **A logger or an injected observer.** Would establish a logging
  pattern the daemon does not have, for one call site, ahead of the
  composer that would configure it.
- **Widening `JanitorStatus` with the fault accessor.** Forces every
  fake and the resolver to carry a method they do not want, for a
  diagnostic only the doctor reads.
- **Keeping `Removed` as the bound and counting it before the effect.**
  Would silently redefine a reported counter rather than adding the one
  the bound actually needs.

## Refute-First Verification (Required for This Risk Class)

Three independent fresh-context lenses, each prompted to disprove the
change: the destructive path, the capability gate and trust boundary,
and concurrency plus test strength.

**Confirmed and fixed**

1. The removal bound stopped bounding destructive attempts once the
   pass continued (two suspends under a bound of one; reproduced).
2. A shared audit-journal failure was attributed to one registration
   while the rest kept minting (reproduced). Now `errJanitorUnsafe`.
3. An unrevoked grant-read token was re-minted every pass, turning one
   leaked live token into roughly `3600/interval` of them (measured).
   Now `errJanitorUnsafe`.
4. One registration ID could be faulted and covered in the same pass
   through two owner records (reproduced). Now withdrawn.
5. Faults were cleared for the whole duration of every pass, so the new
   doctor code was visible in 33% of samples. Faults now outlive the
   pass boundary.
6. `TestInstallationJanitorRemovalDoesNotDenyASibling` passed against
   the old code: its clean registration sorted *before* the destructive
   one and was already in `covered` when the old code ended the pass.
   The fixture now makes the destructive registration the one
   enumeration reaches first.
7. The migrated token-failure table had been weakened from an exact
   revoke count to a boolean, which is what would have caught finding 3.
   It runs one bounded pass again and asserts the exact count.
8. Three behaviours the new code introduced survived every mutation
   (the cancellation branch, the shutdown guard, fault ordering). The
   cancellation branch was dropped as unearned; the other two are now
   pinned. A canceled pass reports the cancellation with the cause
   wrapped inside it, rather than either one alone.

Each of findings 1-6 was re-checked by mutation after the fix: reverting
it makes the corresponding test fail.

**Found by review, after the refute pass** (P1 on #292)

9. The refute pass and this note both stated the credential rule as
   "revocation failed", which is a symptom rather than the class. An
   ambiguous *mint* leaves the same unrevocable live token, and the
   loop retried it every pass. Closed by classifying the mint's whole
   outcome space (`TestInstallationJanitorStopsOnAnUnaccountableMint`)
   rather than by adding the cited transport case: the same
   enumerate-the-input-space discipline the keystore units arrived at.
   The three "must stop the loop" tests were also rerouted through a
   bounded helper, since a regression in this class otherwise hangs the
   package instead of failing.

**Rejected by verification** (probed, no failure constructed; not to be
re-raised)

- No snapshot invariant spans registrations: every rule in
  `validateInstallationAuthority` and the authority document is scoped
  to one registration, so partial coverage breaks none of them. A
  malformed document fails every registration and denies everything.
- No effect can land without its barrier: every path in the removal
  loop returns before `suspendInstallation`/`deleteInstallation`, and a
  dead-recorder run performed zero deletes.
- `covered` is appended on exactly one path, and a pending envelope
  never widens the allow-set.
- No data race and no state escape: `publishPass` clones faults,
  `RegistrationFaults` clones on read, coverage maps are per-pass and
  never written after return (`-race -count=5` clean).
- The fault's error reaches no durable record; the finding carries IDs
  only, and the enumeration token is asserted absent from the fault
  text.
- `janitorCode` cannot panic on a nil or typed-nil janitor, and
  `janitor_registration_failed` breaks no consumer: the finding
  vocabulary has no reader outside `internal/publish`.
- No hot spin: a faulting pass still waits out the interval.
- Quarantine ordering is unaffected; `applyQuarantine` only removes.

**Accepted by decision** (not fixed here)

- **A permanently failing *delete* still retries every pass**, writing
  one durable audit entry each time, with no backoff. Measured 249.3
  bytes per entry against the real store, so the 8 MiB write cap is
  reached after roughly 33,600 entries: about 23 days at a one-minute
  tick. It fails closed (the cap is checked before the write, so the
  daemon cannot produce a journal it cannot read) and the affected
  registration's gate is shut throughout, but the terminal state needs
  an operator to trim the journal. Retry policy and journal retention
  are their own unit (#289). The two failure classes that leak
  credentials or bypass the barrier are handled above.
- **A failing removal abandons the registration's remaining actions**,
  so an unremovable installation can starve a removable one behind it
  forever. Pre-existing ordering behaviour; the change makes it
  permanent and quiet rather than fatal and loud (#290).
- **A registration that can never reach a clean pass reports as
  `janitor_inactive`**, not as a fault, because a removal is not a
  failure. Accurate but uninformative (#290).
- **The unscoped `Resolve` still denies when any registration is
  inactive**, so this change does not restore minting for an unrelated
  owner; what it restores is the loop, `ResolveRegistration`, the
  doctor's per-registration diagnosis, and self-healing without a
  restart. Fail-closed and deliberate; the question of whether a
  non-matching registration belongs in that gate's scope is #291.
- **`RunCycle` now performs destructive work for registrations after
  one has faulted**, within the same bound. No non-test caller exists.
- **A `JanitorStatus` wrapper that does not forward
  `RegistrationFaults` silently degrades** to `janitor_inactive`. The
  cost of discovery over a second port; no wrapper exists.

## Revisit When

The daemon gains a logger or a status surface: the fault's error text
is currently readable only in process, and `RegistrationFaults` is the
natural thing to hang it off. Also when a composer sets the loop
interval, since every rate in the accepted-by-decision list is stated
per tick and the retry-policy unit will want that number.
