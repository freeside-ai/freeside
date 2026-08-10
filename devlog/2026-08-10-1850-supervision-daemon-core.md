# Implement the Supervision Daemon Core

**Decision.** Implement durable daemon stops with an open blocking
`system_health` AttentionItem from the failing loop, rather than widening the
operator-authored unattended transition log. The item is already part of the
Section 4 admission predicate, survives restart, and can be concluded through
the existing operator decision flow. The failing component returns without
touching the fatal channel while HTTP and unaffected read paths remain live.
This preserves the transition log's command-backed authority boundary and
keeps the unit out of shared domain and migration contracts.

Externally caused scheduled doctor or janitor failures remain observations
for two consecutive passes and become a durable stop on the third. The
counter is in memory per schedule kind: persisting it would widen the durable
scheduler schema, while a stop that reaches the threshold is itself durable
and an unresolved external fault re-establishes persistence after restart.
Each observation is logged, and the blocking item's reason records the whole
judged run with timestamps and causes. Errors that cannot be
positively classified as transient external failures stop immediately; an
unknown health failure is not evidence that unattended operation is safe.

Readiness is atomically replaced at `<state-dir>/readiness.json` with mode
`0600` on every configured start. The existing stdout object remains, and
foreground fake-driver starts that do not configure a state directory do not
gain a filesystem requirement. Builds may stamp `main.version` with
`-ldflags -X`; ordinary Go builds use their embedded module version or the
non-empty `devel` fallback so `/health` always satisfies the API contract.

**Rejected alternatives.** A system-authored unattended transition would
weaken the command-only provenance invariant and require its own contract
unit. Persisting pre-threshold counters would add migration and reconstruction
surface without changing the eventual safety result. Treating unclassified
errors as retryable would silently convert local corruption into ambient
network failure.

**Recovery safety.** A durable-stop item offers only acknowledgement and
diagnosis in the process that filed it: its failed loop has returned, so an
in-process `resume_unattended` would reopen admission before that loop exists.
On a fresh daemon start, every loop is reconstructed while the durable item
continues to block admission; startup then offers the existing explicit
`resume_unattended` decision. A same-boot recurrence removes that offer and
keeps the gate closed again. The offer proves a fresh daemon start, not that
the underlying fault is repaired; the operator must use diagnosis and judgment
before resuming. A generation/barrier protocol or a system-authored transition
was rejected because each would widen the recovery and command-authority
contracts beyond this daemon-only unit.

Revisit when: the scheduler schema changes for another reason and persisted
pre-threshold history would materially improve operations, or a new external
error carrier is added that needs an explicit transient or definitive
classification, or a future recovery contract needs to prove fault repair in
addition to a fresh daemon start.
