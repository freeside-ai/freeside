# Cut Over to Agent Selection

Work unit #867: `freesided auth adopt`, lineup admission, and the removal of
the four identity flags. This note records the owner's decisions of
2026-10-01, the choices the cutover forced, and what the refute-first passes
changed.

## Owner Decisions

- **One PR for parts A to E.** The tree loader, the baseline adapters, `auth
  adopt`, lineup admission, and the flag removal land together, because the
  flags may go only after admission is proven in the same change.
- **The tree is read from an exact commit of an operator-named checkout.**
  `-agent-tree` and `-agent-tree-commit` name them, and the files are read
  through git. Rejected: reading `policy/` from the working tree, where an
  uncommitted edit would select an agent nobody reviewed.
- **Adapter conformance is a declared set.** `-run-conformance` records each
  baseline adapter with the capability set its code declares. It is a
  declaration, not a proof against the real harness. Follow-up: #1672.
- **Review launch coverage waits for #898.** A review role's line must
  resolve, be selected, and hold a current credential at startup and before
  each review. The proved step (launch coverage) arrives with the review
  record. The contract's "pass admission checks" is met for steps 1, 2, and 4
  for the review roles, and for all five for the writer roles.

## Selection at Startup

Chose to keep the daemon running with admission held when a writer role fails
its check, and to stop startup when a review role fails.

- **A writer failure holds the gate.** The engine consults one gate before
  every lineup admission, and a refusal holds the invocation in either
  operating mode. The daemon stays up so paired clients see the
  `system_health` item naming the role. The role check is fixed for the run:
  the tree is read once, so only a restart on a fixed tree clears it.
- **A review failure stops startup.** A review source is composed with one
  identity and cost owner, and both enter the approved configuration digest.
  With no identity there is nothing to compose. The item is recorded before
  the daemon exits and the next start converges it.
- **Each review rechecks its line.** The tree cannot change under a running
  daemon, but the store can: an identity is disabled, a credential expires.
  The review source refuses the request before it records anything, and
  again before it launches a request recorded earlier. An
  enrollment the source refreshes under its lease is not held to an expiry
  here, because the review refreshes it.
- **A review role needs its offer on sale and its attended mark.** Neither
  needs the review record: the offer's `not_after` is in the tree, and the
  review launch already has a digest the adopted tree marks. Review sources
  run only unattended, so an unmarked reviewer stops startup. A review has no
  attempt budget until #898, so the offer and the credential are held to the
  instant of the check, not to a deadline past it.
- **Rejected: refuse to start on any role failure.** A stopped daemon answers
  no client, so the operator would learn the cause only from a log.

## Retirement

Chose retirement as an operator act in `auth adopt`, and a daemon that only
holds and reports. No table was added.

- **A retired identity is disabled and holds no enrollment.** No agent can
  resolve to it. The daemon reads that state; it never creates it.
- **`auth adopt -retire-unadoptable <identity>` retires.** It disables the
  named identity and records a Stop for each open task it owns. No `disable`
  command exists until #1639. The flag names one identity and is refused for
  an identity the run adopts: adoption reports "unadoptable" for a missing
  `-claude-account` too, and an omission must not cost an identity its work.
- **The daemon stops nothing.** Rejected: the daemon cancels a retired
  identity's tasks at startup, which this unit first built. The retired state
  is also what a flag-era identity nobody adopted yet looks like when it was
  stored disabled (see below), so an upgrade before `auth adopt` would have
  stopped live work. The daemon holds lineup admission and raises one item
  naming the identity and the tasks.
- **Ownership comes from the newest admission.** A task belongs to an
  identity when its newest admission is a legacy one (an identity, no agent
  binding) naming it. A task whose later attempt was admitted through the
  lineup has no flag-era owner. Review records carry no identity until #898.
- **Open means no task-level decision.** A task with no completed, stopped,
  or abandoned decision is owned, including one whose last run failed.
- **Cancellation uses the task-stop path.** The plan named
  `RequestCancellation` on the ward journal. `StopTask` is the path whose
  teardown `ReconcileTaskCancellations` already proves, so retirement records
  a Stop under a device name of its own and a command id derived from the
  identity, task, and episode. A rerun records no second Stop. `auth adopt`
  requires the daemon stopped, so the Stop needs no ordering against a
  launch; the daemon tears the work down after its next start.
- **The item clears at a start, not on confirmation.** The gate reads the
  retired work on each admission and opens once it is closed. The blocking
  item is converged only at startup, so unattended admission resumes after
  one more restart. Revisit when an operator finds the second restart costly.

Revisit when #1639 lands `auth disable`: the flag then duplicates it. Revisit
the ownership rule when #898 puts an identity on the review record.

## A Disabled Flag-Era Identity

Finding: the real-run harness rewrote its two identities on every launch
without the `enabled` field, so an instance it launched since #894 holds them
disabled. The flag path never read the bit.

- **The harness no longer rewrites a recorded identity.** It records one the
  store lacks, enabled, in the flag-era shape `auth adopt` enrolls, and
  otherwise checks the fixed bindings. A rewrite would also strip the account
  binding and cost owner adoption sets, and the store refuses that.
- **Adoption does not enable.** The enrollment writer keeps the stored
  `enabled` value by an earlier decision ("enrollment is not the place to
  re-enable an identity"). `auth adopt` reports the identity as `disabled`,
  and its lines refuse to resolve until it is enabled.
- **Open for the owner:** an instance in that state cannot cut over until
  `auth enable` (#1639) exists, or adoption is allowed to enable the identity
  it adopts. No deliberate disable can exist before #1639, so enabling on
  adoption would be safe today.

## An API-Key Reviewer Cannot Cut Over

Finding from the PR's automated review: `-review-auth-mode api_key` is a
supported daemon flag, and an API-key store has no account and no expiry.
Adoption reports it unadoptable, the tree carries no reviewer line, and an
unattended daemon then stops at startup.

- **Not fixed here.** An enrollment has two auth methods, setup token and
  OAuth, and requires an account binding. Admitting an API key changes
  `daemon/internal/domain`, which this unit may not touch. Follow-up: #1677,
  a `kind:contract` unit.
- **Open for the owner:** merge the cutover with API-key review unable to
  cut over, or hold it for #1677. The baseline the contract names is the
  subscription login, which adopts.

## What Stays From the Flag Era

- **`auth adopt` keeps the flag names as arguments.** It cannot read the
  daemon's flags, and the names say which interim selection is being adopted.
- **`AuthIdentity.Interim` stays.** Codex refresh and renewal read it, and a
  legacy run's volume lookup reads it. Selection no longer does.
- **The engine keeps identity-only admission.** `AdmissionEnvironment` still
  accepts one identity for engine tests and the legacy fixtures. The daemon
  never sets it.
- **Preflight probes one writer volume.** The three writer roles must share a
  credential volume, which is true of every baseline tree. Revisit when a
  lineup gives a writer role another enrollment.
- **The preflight manifest names the lineup revision** in place of the two
  identity ids. A composition retained before the cutover carries no
  revision, so resuming it refuses.

## Refute-First Outcomes

Each pass ran a fresh-context reviewer against the uncommitted part.

Part C (`auth adopt`):

- Confirmed and fixed: the account binding leaked into the report through a
  wrapped store error; a Codex failure after the Claude adoption dropped the
  report; the report shared stdout with the patch; no store-backed test of
  lease release or a failed generation append.
- Allowed: the Claude generation's manifest digest is the volume's tree
  digest under adoption and the token digest under `auth add`. Nothing
  compares either to an observation today. Revisit when a verifier does.
- Declined: an identity bound to a different account is an ordinary refusal,
  not unadoptable; a `-patch` path whose last component is a symlink into a
  checkout (the guard is against accident); a crash after `Begin` leaves a
  lease that expires, as `auth add` does.

Part D (lineup admission):

- Confirmed and fixed: the volume lookup's holder argument was unasserted at
  its call sites; preflight inspected the flag identity while the engine
  admitted the lineup's (resolved by part E).
- Allowed: the attempt deadline is the admission instant plus the handoff
  timeout, which undercounts dispatch delay and recovery. It fails closed at
  use. Revisit when a writer credential has an observable expiry.
- Allowed: an admission stays pinned to the generation it read if another is
  appended inside one dispatch pass. Pinned by ordinal is the snapshot
  contract.

Part E (cutover):

- Confirmed and fixed: the daemon stopped the tasks of any disabled,
  unenrolled identity, which includes an identity not yet adopted (see
  Retirement). `-retire-unadoptable` was a bare switch that retired on an
  operator omission. Ownership counted every historical admission.
- Confirmed and fixed: a gate or role refusal was not a dispatch hold, so in
  attended mode it returned from dispatch as an error and stopped the
  workflow loop. It now holds the invocation as an admission-policy refusal.
- Confirmed and fixed: a recorded review request whose launch never started
  relaunched from `Inspect` without rechecking its line.
- Allowed: preflight resolves the writer roles but does not run their full
  admission check. It takes no prompt package, operating mode, or attempt
  budget. The daemon's startup check is the authority and fails closed.
- Allowed: the retirement item needs a restart to clear (see Retirement).
- Declined: a transient store error at the startup role check holds
  admission until a restart. It fails closed.
- Disproved: the review digest golden being generated by the changed code.
  The pre-existing shadow composition golden and the preflight golden's
  review digest are unchanged.

Automated review of the PR:

- Confirmed and fixed: a review role was not held to its offer's `not_after`
  or to the attended mark, so an expired offer or an unmarked agent reviewed
  unattended. Launch coverage (step 3) still waits for #898.
- Allowed: the review checks use the check instant as the deadline (see
  Selection at Startup). Revisit when #898 gives a review an attempt budget.

## Follow-Ups

- Follow-up: #1672, the adapter contract suite against the real
  harness.
- Follow-up: #1673, retire `enroll-codex` in favour of `auth add`.
- Follow-up: #1677, admit an API-key review credential as an enrollment.
