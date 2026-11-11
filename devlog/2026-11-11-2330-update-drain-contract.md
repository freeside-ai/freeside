# Update Drain and Restart Readiness Contract

Work unit: #3103. Mandatory note: a contract change, and Tier A by path
because the new route is registered in `daemon/internal/signet/http.go`.
It adds the drain record and the restart-readiness read; #3104 makes the
daemon request, hold, and lift a drain.

## Decision: The Drain Is Its Own Log, Not a Third Stop State

Chose a new append-only `update_drain_transitions` log over adding a
`draining` state to `UnattendedOperationState` and its log, which the
filed contract assumed. Three facts at `6e2728d71` ruled that out:

- **The stop log is latest-row-wins.** A `draining` row appended after an
  operator's `stopped` row would make the stop read as lifted.
- **Every stop-log row names an accepted operator command.** The read
  re-derives the state from that command, which is the stop's trust
  binding. A drain is requested over the control socket and lifted by a
  new build, with no command either time.
- **Widening the stop log's CHECK list takes a table rebuild,** which is a
  Tier A migration. The new table's `state` column has no CHECK list
  (`devlog/2026-11-07-1113-enum-columns-go-validation.md`).

A separate log also settles how a stop and a drain combine: they never
share a row, so lifting a drain can't lift an operator stop. A store test
pins it. The drain row grants nothing, so it is Tier B: plain validation,
no command binding, no tamper tests.

The row's `build` is the digest of the daemon's own executable. The
installer stamps no version, so `buildVersion()` can't tell two local
builds apart, and the lift rule in #3104 needs to know the build changed.

## Decision: `ready` Needs a Drain in Force

Chose to report `ready: true` only when a drain is in force and nothing
blocks, over reporting it whenever nothing blocks. Without a drain, a
launch can start the moment after the read, so an empty blocker list
proves nothing about the restart that follows it.

This makes an incomplete read safe. The store records only three of the
seven blocker kinds (held execution slots, held review slots, and live
auth-store mutation leases). Verification jobs, image builds, probes, and
publications exist only in the running process. No drain can be requested
until #3104, which also adds those four kinds, so `ready` can't be true in
production while the read is missing them.

A lease whose holder is a launch already counted isn't counted again. Only
the handoff lease names its invocation as holder
(`daemon/internal/ward/handoff.go`); a Codex auth refresh names its own
holder (`daemon/internal/ward/codex_auth_refresh.go`), so it counts as a
second blocker beside its launch while it runs. Counting too much only
delays readiness, so the read accepts it.

## Decision: The Drain Shows on Sync as a Stop Kind

Chose `update_drain` as a new `UnattendedStopKind` over a sibling field on
`UnattendedOperationSnapshot`. That snapshot's `stops` is empty exactly
when admission is open, and a drain closes admission like any other stop,
so a client that reads only `admission` and `stops` stays correct. A drain
stop names no item and no command; `since` is when the drain began.

The daemon doesn't send it yet. #3104 adds it to the sync projection in
the same change that makes the daemon hold launches, so a client is never
told admission is stopped while launches still start. The mock lists it
after the operator stop and blocking items; #3104 should match that
order or change the mock with it.

## What #3104 Inherits

- **Writing a drain inside a client-visible transaction.** The append is
  on `InternalTx`, so it can run inside a `Write`. Once the drain is on the
  sync surface, the write that requests or lifts it must move the
  revision, or a client won't see the change until the next heartbeat.
- **The sync projection of the drain,** the four in-process blocker kinds,
  and the readiness read over the control socket for the installer, which
  has no paired-device token. The store method is shaped so that route can
  call it.
- **Review admissions that haven't launched.** `review_launch` reuses the
  pool's review-slot rule, which also holds a slot for an admission that
  can still launch. If #3104's drain stops that launch, the admission
  never ends and `ready` stays false until a forced drain. #3104 either
  exempts a drained, unlaunched admission or lets it settle.

The app's wording for a drain has no owner yet. This unit maps the stop
kind to the indicator's generic "stopped" sentence.

## Refute-First Findings

One fresh-context reviewer tried to refute the change before commit.

- **Confirmed, fixed:** the holder comment claimed every execution lease
  names its invocation. Only the handoff lease does; the comment and this
  note now say so.
- **Confirmed, deferred to #3104:** the unlaunched review admission above.
  No drain can be requested in this unit, so it can't block a restart yet.
- **Confirmed, fixed:** no test read readiness with a lifted drain. Added.
- **Disproved by a check:** `ready` true with a counted kind running; an
  error or a missing row reported as ready (any store error is a 500);
  a live lease missed (every lease row's identity declares a lease and
  every identity is read); a lease acquired mid-read going uncounted (the
  read holds the write lock from BEGIN); the route reachable without the
  bearer check; Go and OpenAPI disagreeing on fields, nullability, or
  enums.
- **Allowed:** no HTTP test of the 500 path. The store test covers a
  corrupt drain row, and the handler maps every error to 500.

Revisit when #3105, which re-runs work a restart or a forced drain cut
short, needs more drain history than the latest row.
