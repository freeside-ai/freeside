# A First Codex Enrollment Leaves No Hold

Revises `devlog/2026-08-11-1623-codex-auth-enrollment.md`, which chose that
"initial enrollment creates the same blocked marker as re-enrollment". Issue
#3438 records what that cost: a fresh Codex-reviewed root needed a
placeholder store for a setup start, an `enroll-codex` run with a guessed
project, and then a paired client's **Resolve re-enrollment** for a
credential the operator had just signed in with. An earlier session spent a
real sign-in on a raw login placed at the store path.

## The Order

Chose one documented order, enroll the reviewer and then start the deputy,
over a placeholder store or a deputy that starts without one. The apparent
circle was not real: `enroll-codex` creates the database on a fresh root, a
fresh database holds no recipe-gated evidence so it needs no
`-approved-recipe`, and `-project` is the settings' `repository.project`,
known before any start. The deputy still refuses a missing store, now naming
the path and the enrollment step, and `scripts/exit-root.sh` stops with
`approval=codex_enrollment:` before its first start. A raw login at the store
path stays wrong: only enrollment creates the store, because a refresh would
spend that login without enrolling the identity.

## The Hold

Chose to resolve a first enrollment's marker by the daemon over two
alternatives:

- **Skip the marker for a first enrollment.** Rejected: the journal, its
  lease fence, and the verified-before-projection retry all bind to a
  marker, and a second path without one would duplicate the recovery state
  machine the 2026-08-11 note chose to reuse.
- **Keep the hold and document the paired-client step.** Rejected: the hold
  protects against a revoked chain, and a first enrollment has none. Its
  evidence is the same verified rotation a resolved re-enrollment rests on,
  so a person inspecting it adds nothing the verification didn't check.

The journal body carries an `initial` flag. Enrollment sets it only when it
opens the identity's first marker (occurrence 1), and the store refuses an
initial journal on any other marker. Projection still attaches the binding
and the resolve action, which domain validation requires of any bound item,
then concludes the item by daemon in the same transaction. The admission
predicate treats a daemon-resolved item under an initial journal as no hold;
the ward boundary accepts that carrier only when the port reports it
initial. Rows written before this change have no flag and read as not
initial, so a hold an older build left open stays open.

A revocation after a first enrollment opens occurrence 2 or later, which can
never be initial, so a revoked chain keeps its paired-client step as plan
§4's `system_health` rules require. Enrolling an already enrolled identity
again, once a refresh has rotated its store, is treated the same way.

A revocation recorded while the first enrollment is still unverified finds
marker 1 open and unbound and changes nothing, so the retry stays initial
and resolves it. That revocation could only concern the store from before
enrollment, which the verified enrollment replaces, so folding it in loses
no protection.

## The Retry

Chose to let a retry inherit `initial` from the latest journal when it reuses
the same open, unbound first marker, over deciding by occurrence alone. A
failed first enrollment (a rejected token, a verification failure) leaves
marker 1 open with no binding; the operator's retry with a new login is
still the identity's first successful enrollment. Deciding afresh would read
the reused marker as a re-enrollment and bring back the hold this change
removes. A crash between verification and projection is the other retry: it
replays the verified journal, which already carries the flag.

## Refute-First

One fresh-context reviewer tried to prove the change wrong.

- **Confirmed and fixed: a rerun after a finished first enrollment was no
  longer idempotent.** Resolving marker 1 at projection hid it from
  `RecoverableVerified`, which looked only for an open marker. A rerun after
  a crash between the projection's commit and the printed result, or after
  a failed lease release, began a new operation on marker 2: it wrote the
  already spent login over the live store and left an open hold. Recovery
  and projection now also accept the latest marker when an initial
  journal's projection resolved it with that journal's binding, so the
  rerun re-verifies the unchanged live store and reports the same result.
  Once a refresh has moved the store, its digest no longer matches and a
  rerun is a replacement, as before.
- **Confirmed and fixed in the docs: a store shared across roots.** The
  machine file's store serves every root on the machine, each with its own
  database. Enrolling it again from a second root would be that database's
  first enrollment and replace the store the others read, so the
  walkthrough says to enroll once per machine.
- **Disproved by trace:** a revocation in the crash window between
  verification and projection supersedes marker 1, so projection fails on
  the marker mismatch and the hold stays; journals written before this
  change read as not initial; an `auth add` rerun stops before it opens a
  marker.
- **Allowed:** the deputy's error names the database as
  `<root>/daemon/freeside.db`, since the settings don't carry the root.

Revisit when a first enrollment can be shown to carry a risk a revoked
chain's replacement doesn't, or when `auth add` replaces `enroll-codex`
(#1673) and the two entry points collapse into one.
