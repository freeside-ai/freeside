# Shared Read Holds for Claude Writer Runs

Contract change for #1585. It changes the auth-store window a read-only
identity mount runs under, and the store rows the ward trusts for it.

## Chose a Shared Read Hold Beside the Exclusive Lease

Since #383, a Claude writer mounts its setup-token volume read-only but
still takes the identity's exclusive mutation lease. Nothing in Freeside
mutates a Claude setup token, so that lease only serialized writers against
each other: with `max_parallel_executions` above 1, the second writer on an
identity ended as a failed stage instead of running. The design was chosen
in planning on 2026-09-29, subject to owner veto.

Chose a second window kind, `AuthStoreReadHold`, keyed by identity and
holder. Read holds share an identity with each other, and the exclusive
lease and read holds refuse each other in both directions. Each refusal is
checked and written inside one immediate SQLite write transaction, so no
interleaving can leave both live. The ward keeps every check the lease
drives: the identity-to-volume binding, the pre- and post-writer credential
digests, the atomic journal open, the pre-start re-verify, and recovery's
re-gate. Only the window changes, and since a read hold has no fence, the
ward binds it by identity, holder, and exact bounds instead.

The mount decides the kind, not a new `HandoffSpec` field: a read-only
mount takes a read hold, and a writable mount takes the exclusive lease. A
"shared lease on a writable mount" therefore can't be expressed, Codex's
writable path is unchanged, and spec digests of in-flight journal records
survive the upgrade.

Rejected:

- **No lease for read-only handoffs, plus a fence.** The ward ties the
  volume check, the credential digests, and the journal open to the lease
  claim, so this still needs a new claim concept. The pre- and post-writer
  digests can only detect a mid-run token replacement after the fact; a
  window that excludes mutation prevents it.
- **A per-run token snapshot,** as the Claude review does. It adds a volume
  and a seeder container to every writer run, plus their recovery, to guard
  against a mutation no Freeside path performs.

## Recovery Accepts a Pre-Upgrade Exclusive Record

A Claude run journalled before this change holds the exclusive lease for a
read-only mount. Recovery accepts that pairing and finishes the run on the
lease path; rejecting it would leave the run unable ever to recover. The
reverse pairing, a read hold on a writable mount, can't be produced by any
version and is rejected as a damaged record.

## A Same-Holder Read Hold Never Converges

A read hold for a holder that already has a live window is refused rather
than returned. Holders are invocation IDs, unique per run, so a second
acquire under a live holder is a stale or duplicated actor, and handing it
the existing window would let two runs share one release. An ended window
for the same holder is replaced. The in-process slot follows the same rule,
keyed by identity and holder.

## Verified on the Reference Runtime

The design depends on Apple `container` allowing two running VMs to attach
one volume read-only at once (`docs/spikes/workspace-handoff.md`). A live
ward test on `container` 1.1.0 runs two read-held writers on one
setup-token volume with overlapping intervals; both hand off and release.

## Refute-First Findings

An independent reviewer tried to break the change before commit.

- **Confirmed, fixed:** recovery's exact-window rule for read holds had no
  test. Tests now pin that a later window for the same holder is never
  released and that an incoherent re-gate row fails closed.
- **Allowed by decision:** the lease's read-hold exclusion selects
  candidates by the `released_at` and `expires_at_unix_nano` columns, so a
  row whose columns were edited to disagree with a live body is skipped
  rather than refused. Only the store writes those columns, beside the body,
  in one statement; the divergence needs direct database tampering, and the
  lease row's own columns are trusted the same way.
- **Allowed by decision:** a delayed lease acquire whose instant falls
  inside an already released read window succeeds, where the lease refuses
  an instant before its own last release. The reader is gone, and any later
  read window is caught by the instant-before-acquisition check.
- **Disproved by checks:** a check-then-write race (both acquires run in an
  immediate write transaction), a lease path that skips the read-hold check
  (every path funnels through the bound acquire), a read-only claim with no
  window or a writable mount with a read hold (every branch fails closed,
  and recovery rejects the pairing), and migration loss (0082 keeps the
  table's full column set, CHECKs, and foreign key).

## Revisit When

- A Freeside path starts mutating Claude auth stores. A read-hold refusal
  caused by a live mutation lease then becomes reachable, and should become
  an admission hold rather than a failed run.
- #1596 moves execution limits to provider pools, which may change where
  per-identity concurrency is admitted.
