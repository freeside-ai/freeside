# The Janitor Keeps an Expired Intent's Installation

Chose to keep, from metadata alone, the installation that the current
epoch and revision's expired intent matches, over removing it once the
intent expires. Removing it cost the operator a GitHub reinstall and a new
approval every time an intent expired mid-onboarding (#2382's exit run,
friction log F20; #3434). Plan revision 133 replaces the "only until
expiry" and "lets ordinary reconciliation resume" sentences in §10, GitHub
App Agent Identity.

## The Rule

- **Kept:** the installation that matches an expired envelope whose epoch
  and revision equal the snapshot's frontier, by account login and ID and
  by installation ID when the envelope names one, with
  `repository_selection: selected`. The janitor records nothing for it,
  mints no grant-read token, gives it no coverage, and never reports it
  ready. It stays until a replacement intent covers it or the operator
  removes it.
- **Expansion:** a trusted installation that an expired expansion envelope
  names keeps its trusted path. A grant equal to the expired expected set is
  not `repository_grant_drift`; coverage stays the trusted set.
- **Unchanged:** a stale epoch, a superseded revision, another account,
  another installation on the expected account, identity drift, and a
  non-`selected` mode are removed or quarantined as before. An expired
  intent still authorizes no onboarding token and can't be promoted.

The expired envelope lives in its own field (`validatedInstallationAuthority.expired`),
never in `pending`, so the onboarding token source and `PendingReady`, which
read only `pending`, can't treat it as authority.

## Rejected

- **Quarantine instead of keep.** Quarantine suspends and then deletes, and
  a quarantined ID can never be named by a later intent, so it can't keep
  the installation for a replacement intent.
- **A grant-read token for the kept installation,** to check its repository
  set. It would break the plan's pre-token gate: no token for an
  installation without a trusted binding or a live envelope. The keep is
  checked from the installation record alone, so a kept installation may
  hold any selected set; it still covers nothing.
- **A deadline after expiry.** It would bring back the reinstall cost the
  issue reports. An abandoned onboarding leaves an installation on the
  operator's own account that has no token or coverage, and
  `pendingIntentGate` holds the recorded intent until `-replace-intent`.
- **A per-root janitor journal or authority document for exit roots.** Exit
  roots run against production's App on purpose, one authority document per
  App under one writer lock (`devlog/2026-11-16-0950-app-authority-writer-lock.md`).
  A second journal or document for the same App would let one janitor delete
  installations the other trusts. The onboard error names the journal's path
  instead, and `scripts/exit-root.sh` already prints the onboard log tail.

## Bounds That Still Hold

- **A quarantined installation is never kept.** `candidate` checks the
  quarantine set before either envelope, so the zero-ID escape that
  `devlog/2026-07-24-2117-installation-authority-snapshot.md` bounded by
  envelope expiry stays closed.
- **One match per envelope.** A second installation matching the expired
  envelope fails the registration's pass, as it does for a live one.

## Onboard Fails Fast

The exit run's hang was a `-resume` of a replacement intent that still named
the removed installation ID: the poll waited for an installation GitHub no
longer had until the intent expired. Onboard now reads the journal's
removal and quarantine entries (`InstallationAuthorityStore.InstallationRemovals`)
and refuses, naming the installation, the time, the reason, and the journal
path:

- recording an intent (first call or `-replace-intent`) that names a removed
  installation ID;
- a `-resume` whose recorded intent names one, before the janitor starts or
  the poll is armed;
- a wait during which the janitor removes the intent's installation (by ID,
  or for a zero-ID intent, any removal on its account since the wait
  started, except a retried removal of an installation the journal already
  recorded, such as a quarantined one that outlived its deletion).

The journal records removal requests before the delete, so a failed delete
still reads as a removal; the message says "requested removal" for that
reason.

A zero-ID intent whose installation was removed before the `-resume` began,
and a named installation whose quarantine withdrew the intent, still end in
the old wait or "disappeared" error: the envelope records no creation time
to match against. Follow-up: #3509.

The recovery it names records a new zero-ID intent first, then reinstalls
the App, then resumes. Reinstalling first would be wrong: an installation
made while no live intent covers it is removed again.

## Revisit When

- #3437 changes how an expired intent is recorded or promoted.
- An operator reports a kept installation they wanted gone; the operator
  removal path would then need a command rather than a GitHub visit.
