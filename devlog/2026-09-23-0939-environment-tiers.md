# Prod, Dev, and Ephemeral Environment Tiers

Work unit #1499 (plan revision 69). Records why Freeside splits its instances
into three environment tiers, and the choices the plan section makes that a
reasonable reader could have made differently. Heads the environment
isolation tracker, #1507; the follow-on units implement what this note
decides.

## Chose Three Tiers Over a Two-Tier Prod/Dev Shape

Chose `prod`, `dev`, and `ephemeral` over the two-tier `prod`/`dev` shape
because the two tiers serve different callers. `dev` is the operator's own
supervised development install: one instance, launchd-managed, a fixed port,
paired from the operator's app. Agents need many unsupervised instances at
once, one per worktree, each on port `0` and gone when the run ends. A
two-tier shape either hands agents a supervised root, which the operator's
`dev` install already occupies, or makes every agent share that one install.
`ephemeral` also gives the daemon a safe default: a missing `-environment`
flag means `ephemeral`, so a bare run never assumes a supervised identity.

The state root is the parent of both `daemon/` and `credentials/`
(`~/Library/Application Support/Freeside/` for `prod`), not the daemon state
directory, because `freesided onboard` defaults its credentials directory to
the `credentials` sibling of its `-state-dir`, the GitHub App authority state
directory (`daemon/cmd/freesided/onboard.go`). That is the daemon's
`-publication-state-dir`, not its `-state-dir`, so the plan table pins it to
`<root>/daemon/` as well. A root one level lower would split the database
from its tier but leave the credentials shared.

## Deferred Exclusive Locking for Supervised Databases Until IPC

Owner decision of 2026-09-23, made in the revision's review. The issue
recommended opening the `prod` and `dev` databases with
`locking_mode=EXCLUSIVE`, and the first draft adopted it. Every other guard
in the tracker runs in the process it constrains, so an older binary, a test
that opens the store package directly, or a `sqlite3` shell bypasses them;
the database lock would be the one guard production holds for itself, and
in-process WAL checkpoints and the local checkpoint writer
(`daemon/internal/store/local_backup.go`) are unaffected by it.

Review found that the lock also refuses Freeside's own direct-store
clients, not just `sqlite3`. `freesided follow` is a live reader by design
(`daemon/README.md`, through `daemon/internal/observe/observedb`) and shares
`submit`'s direct-store transport; `preflight`, `approve-shadow-review`,
`renew-codex`, `comprehension`, and `rig` open the store with
`store.OpenExisting` or `OpenReadOnly`. Against a running supervised daemon
each would fail with `database is locked`, and a snapshot command cannot
serve live following or mutating commands.

Chose to keep exclusive locking as the end state but gate it on moving those
clients to a daemon IPC transport, with the private Unix socket
`freesided pairing-code` already uses as the natural base, over taking the
lock now with IPC as a hard prerequisite of the locking unit (the same
ordering, stated as a schedule this revision does not set) and over dropping
the lock (it is still the only guard that does not trust the errant process).
Until then every tier keeps the default locking mode, and production relies
on the per-database lock against a second daemon plus the ephemeral guard.
The locking unit (#1502) needs a `starts-after` on a new IPC unit.

## Chose to Ship Dev With the First Installer Change

Chose to ship `dev` with the first installer change (#1500) over waiting until
it falls out of the installer later. Making the installer environment-aware
turns its identity constants (bundle ID, state root, listen address, plist
label) into parameters of the environment anyway, so a second case costs a
second bundled plist and an `SMAppService` registration, not a second
installer. Shipping it at once also gives the operator a supervised install
to develop against the day `prod` stops being the only one, which removes the
main reason to point development at production.

This adopts the issue's recommendation, subject to the owner's review.

## Chose to Refuse Only the Supervised Ports in Ephemeral

Owner decision of 2026-09-23, made in the revision's review. The first draft
had the ephemeral guard refuse every nonzero port. That left the required
1A.2 harness, `scripts/run-real-work.sh`, with no valid tier: it is a
foreground run with no `-environment` flag, so `ephemeral`, but it pins a
fixed nonzero listener so a paired client can reach the
specification-approval gate, and `prod` is launchd-supervised by definition.
Chose to keep the harness `ephemeral` and narrow the guard to refuse the
supervised tiers' ports (`7331`, `7332`) and paths, over a named harness
exception (a carve-out that outlives its reason) or a fourth exercise tier
(a second identity table for one script). Port `0` stays the default. The
guard exists to keep an errant run off production's port, which refusing
the supervised ports still does. #1501's acceptance bullet that refuses any
port other than `0` needs a matching follow-up edit.

## Chose Attended Real-Work Runs as the One Prod-App Exception

Owner decision of 2026-09-23, made in the revision's review. The draft's
credential rule checked only where credentials live: enrolling the `prod`
GitHub App into a `dev` or `ephemeral` root passes every path check. Chose
to state the rule by use instead. `dev` and `ephemeral` instances start with
no publication credentials, and test and agent instances never hold the
`prod` App's credentials. An attended real-work run (today the real-run
harness, `scripts/run-real-work.sh`, and phase-exit runs) may deliberately
enroll the `prod` App's credentials into its `ephemeral` instance, because
it is real work and should publish as the real App. The rule is operator
discipline; no path check can tell whose credentials a directory holds.

Considered and rejected a distinct GitHub App registration for non-prod
publication, which an earlier draft of this revision adopted. `dev` need not
publish, and a real-work run is real work that should publish as the real
App, so a second App would cost a registration and installation for no
instance that needs one. The one argument for it is a possible conflict when
a real-work run and the `prod` daemon act on the same repository as the same
App, and whether GitHub reconciliation keys on App identity there is
unverified.

## Declined a GitHub-Side Lease Between Daemons

Declined a lease on GitHub that would stop two daemons from acting through
the same GitHub App at once. Under the rule above, two daemons share the
`prod` App only in an attended real-work run that the operator starts and
watches, so there is no unattended contention for a lease to arbitrate. The
first draft rested the decline on directory separation alone, which review
showed does not hold. Two machines sharing one App is not a case in use. The
lease would add a GitHub round trip and a recovery path for a stale lease to
guard a state the operator already watches.

## Chose Proposed Dev Identifiers

The `dev` values (`Freeside Dev` root and display name, label
`ai.freeside.daemon.dev`, bundle ID `ai.freeside.app.macos.dev`, port `7332`)
follow the `prod` values with a suffix. They were proposed during planning,
not taken from an existing install; the implementing installer unit reads
them from the plan table.

## Revisit When

- The direct-store clients (`follow`, `submit`, and the operational
  commands) reach the daemon over IPC; exclusive locking for `prod` and `dev`
  becomes schedulable, with a snapshot command for offline `sqlite3` use.
- The operator needs `sqlite3` against the live supervised database once
  exclusive locking lands and the snapshot command does not serve.
- A second machine shares the GitHub App, which reopens the GitHub-side lease.
- A real-work run and the `prod` daemon conflict on the same repository as
  the same App (whether GitHub reconciliation keys on App identity is
  unverified), a publishing run goes unattended outside `prod`, or someone
  other than the operator runs these instances; each reopens a distinct App
  and the GitHub-side lease.
- The hardened dedicated-user mode (plan Section 5.2) is scheduled; it
  replaces the Phase 1 ephemeral guard as the guarantee, and the tier rules
  should be re-read against it.
- A `DEBUG` app build needs to mean `dev` rather than `ephemeral`; only the
  precedence bullet changes.
