# Durable Backend Conformance and the Enforced-Egress Admission Floor

Issues #327 and #320, landed as one spine-owned `kind:contract` PR; the
§5.7 amendment (plan revision 21) is the PR's direct subject. #302's
egress proof was ward-internal process state whose result was
discarded; nothing persisted what a backend proved, so a confused
writer could over-claim a capability class and persist it as admission
provenance (the hole `AdmittedUnder` documented against #320).

## Decisions

**The capability is `supports_enforced_provider_egress`, attesting the
mechanism, not the request.** `domain.EgressProfile` (`provider_only`)
states what policy asked for; the capability states what the suite
proved the runtime enforces (host-only network, exact-authority CONNECT
proxy, live in-writer allow/deny/DNS/direct-IP probes). Widening
`EgressProfile` was rejected as a non-goal: request and enforcement are
different trust classes.

**Durable conformance is an append-only log whose row id is the proof
generation.** The newest row per backend is its current declaration:
a newer append (any outcome) supersedes, a failed append invalidates,
the exact discipline of ward's in-memory generation guard, made
durable on the `unattended_operation_transitions` precedent. Rejected:
ward-supplied generations (the process counter resets on restart, so
restart would forge supersession order) and `proved_at` ordering
(RFC3339Nano trims trailing zeros and misorders sub-second instants,
the recorded #301 lesson). The store refuses a caller-supplied
generation outright.

**Over-claims are refused by a domain-registered per-class provable
ceiling.** The store cannot re-run probes, so `ProvableCapabilities`
registers, per backend class, the set its suite could ever prove; for
`fresh_vm_read_only_volume_handoff` that excludes the two
runtime-refuted capabilities (`credential_volume_detach`,
`workspace_snapshot`). The ceiling lives inside
`BackendConformance.Validate` rather than a transaction-scoped gate
because it is compiled-in vocabulary, not live policy: accept and
decode boundaries enforce it identically, and a ward drift test binds
ward's explicit proven set to the ceiling in both directions.

**The admission gate is write-time only, and unattended only (owner
decisions, 2026-07-27).** `RequireBackendConformant` sits beside
`RequireUnattendedAdmissible` in the admitting transaction: no current
passed record, or a snapshot exceeding it, refuses the write; a replay
after a lapse refuses rather than converging. Reconstruction and
dispatch deliberately do not re-run it, preserving the #301
frozen-admission decision (a lapse stops what happens next, never the
readability of recorded history; re-checking a frozen admission at
dispatch is exactly the re-gating #320 names a non-goal).
#320's unqualified "an admission ... fails closed" is read as "an
unattended admission", owner-ratified: §5.7 explicitly admits a
weaker, unproven runner class for `attended_dev`, and the engine's dev
loop runs fakes no conformance suite ever sees.

**Revised (owner decision, 2026-07-28): a beginning recheck durably
supersedes the previous declaration.** The original design held that
"suite-begin writes nothing durable" because the cleared in-memory
declaration already refuses new spawn-time freezes. Codex's round-3
finding changed that assumption: a snapshot frozen just before the
recheck begins is written to the store afterwards, and during the
whole recheck the previous passed row was still durable-latest, so the
write gate admitted against a declaration the backend had already
withdrawn. `Full` now appends a `superseded` marker (a third
`ConformanceOutcome`, nil capabilities) through the recorder inside
`beginConformanceProof`'s critical section, before any probe runs; the
recheck's completed outcome supersedes the marker in turn, and a
recheck whose supersession cannot be made durable does not run.
Codex's alternative (binding admissions to an observed generation)
stays rejected: it would not close this window (no append had happened
yet, so the generation was unchanged) and it pushes proof state into
the admission schema against the #301 frozen-admission decision. A
recorderless suite cannot write the marker, but it also never declares
(round 2), so no snapshot can freeze against its passes; the residual
mixed case (a recorded pass followed by a recorderless recheck over
the same store) is accepted and noted.

**Restart does not invalidate the durable record (owner decision,
2026-07-27).** The record is a ceiling, never a grant: between boot
and the startup `Full` pass the in-memory declaration is empty, so the
live capability floor already refuses admission; the surviving record
is honest history of the last completed pass. Rejected: a boot-time
synthetic "failed" append that no probe observed, which could prevent
nothing the floor does not already prevent.

**Ward publishes and records on one currency judgment.** One shared
proof generation guards both suite-earned capabilities (both come from
the same all-or-nothing `Full` pass; separate counters would be
duplicate machinery). `finishConformanceProof` reports whether the
pass was still current; only a current pass records durably, so
durable-latest always describes the same pass as the in-memory
declaration. A recorder failure withdraws the declaration and fails
the pass: an unpersisted proof is not a proof. The recorder is an
injected ward interface (`ConformanceRecorder`) because ward cannot
import the store; an integration adapter test proves the seam composes
with `RecordBackendConformance`, and the real wiring lands with
#303/#237. Rejected: `Full` returning the record for the caller to
persist, which makes "run suite ⇒ durable outcome" forgettable at
every call site.

## Refute-First Findings

- **Rejected by verification: an over-ceiling conformance row admits
  nothing.** A raw-SQL row claiming beyond the class ceiling (and
  disordered, duplicated, unregistered-name, and failed-with-caps
  variants) fails reconstruction loudly, and never as `ErrNotFound`
  absence (`TestTamperedConformanceRowFailsClosed`); the admission
  gate consumes that same path, so the tampered row refuses admission
  rather than widening it.
- **Rejected by verification: a replayed admission cannot converge
  past a failed pass.** A byte-identical replay of an already-stored
  unattended admission is refused once a failed append supersedes the
  passed record (`TestUnattendedAdmissionRequiresBackendConformance`),
  the same fail-closed ordering the stop gate uses.
- **Rejected by verification: a conformance lapse does not poison
  history.** After the failed append, the stored admission still
  reconstructs; the gate is absent from the scan path by design.
- **Rejected by verification: a superseded pass cannot resurrect or
  record.** The older overlapping `Full` that finishes last neither
  publishes in-memory nor records durably
  (`TestSuiteFullOlderSuccessCannotOverrideNewerFailure`).
- **Confirmed and fixed by fresh-context refutation review: publish
  and durable append were not atomic.** The first implementation
  released the generation mutex between the currency check and the
  recorder call, so an older pass concluding concurrently with a newer
  pass's fast failure could append its stale success after the newer
  failure, inverting the durable log's supersession order (audit
  corruption only: the in-memory flags belonged to the newer pass, so
  the floor still refused admission). The record step now runs inside
  `concludeConformanceProof`'s critical section, a new pass's begin
  blocks for the append, and
  `TestConcludeHoldsTheGuardAcrossTheRecordStep` pins the ordering.
- **Confirmed and fixed by automated review (Codex P1): publication
  preceded persistence inside that critical section.** Publishing the
  flags before the append opened a window where a concurrent admission
  was enabled by the fresh declaration yet gated against the previous
  pass's stale passed row; if the append then failed, the admission
  stood on evidence the pass never persisted. The order is now
  persist-then-publish: the capabilities become observable only after
  the pass's own row is durable-latest, a record failure leaves them
  undeclared, and `TestConcludePersistsBeforePublishing` pins the
  window shut. Codex's alternative (binding each admission to an exact
  durable generation) was rejected as heavier than needed: with
  publication following the append, the stale-evidence window no
  longer exists, and the frozen-admission decision keeps generations
  out of the admission schema.
- **Confirmed and fixed by automated review (Codex P1, round 2): the
  nil-recorder allowance re-opened the same window.** "Nil recorder is
  safe because absence fails closed" held only while no durable record
  existed at all; once an earlier recorded pass left a passed row, a
  later recorderless green pass would publish fresh flags that enable
  admission against that stale row. A recorderless pass now supersedes
  but never declares the suite-earned capabilities; declaration always
  requires the pass's own durable record.
- **Confirmed and fixed by design review: the recorder must not run
  under the suite's own cancelled budget.** `Full`'s publish defer
  runs after the suite-timeout cancel (LIFO), so the record call takes
  the caller's context, captured before the timeout wrap.

**Revisit when** a second backend class registers a ceiling (the
ceiling switch forces the decision) or when #303/#237 wire the real
suite runner: the recorder adapter and the startup-`Full` cadence are
theirs to place.
