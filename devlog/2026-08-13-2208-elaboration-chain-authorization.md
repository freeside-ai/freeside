# Elaboration Chain Authorization

Chose one engine-level, per-provenance transition-chain verifier over a
structural store seal because the existing durable records already contain the
authority needed to derive every elaboration edge. The verifier reconstructs a
policy-bounded history in one read snapshot and admits a current request only
when its complete ordered input vector follows from the preceding terminal or
the exact stored `request_changes` command.

## Durable Authorization

The iteration-1 implementation claim remains the root. Each later request must
retain that root's run, project, policy, implementation reservation,
publication, work-unit declaration, and optional issue subject. Its invocation
ID, request payload, and `AgentInvocation` must agree exactly.

Research is authorized only by the preceding terminal's ordered research IDs,
with daemon provenance naming that invocation. A prior specification is
authorized only by the immediately preceding specification terminal, with
agent provenance naming that invocation. Revision feedback is authorized only
by the deterministic approval item and its one effective same-item
`request_changes` command; its ID, content digest, and daemon/head-independent
provenance must bind that command and the superseded specification invocation.
The derived input order is source, accumulated research, current prior
specification, then chronological feedback.

Every state-creating consumer reuses this authority: pending dispatch,
dispatched driver start, attempt acceptance, gate reconciliation, revision
enqueue, implementation start, and the production-submission reservation
gate. Backup closure and damaged-reservation discovery remain parsing-only:
they can retain or refuse work, but cannot grant execution or create a
transition.

## Rejected Structural Seal

Rejected a new schema-level integrity seal for this unit. It would widen an
engine-local fix into a persisted contract and still require semantic
authorization of heterogeneous terminal- and command-derived inputs. A seal
may become worthwhile only when multiple independent workflows need the same
cross-record integrity primitive.

## Refute-First Findings

- **Confirmed and fixed:** research fetched after a revision was appended
  after the prior specification and feedback. Reconstruction exposed that the
  production transition did not follow the selected canonical role order, so
  production now rebuilds the vector from roles before persisting it. The
  verifier additionally accepts the one exact pre-change persisted ordering,
  derived by appending that terminal's authorized research to the preceding
  request, so upgrading does not quarantine a valid in-flight revision.
- **Confirmed and fixed:** the final driver-start callback authenticated only
  the current marker's run and stage, and its first chain-verifying revision
  misclassified a nested missing chain record as “not elaboration,” allowing a
  production-authentication fallback. It now reconstructs the complete chain
  inside the same store snapshot and treats a deterministic elaboration
  identity or an already classified marker as fail-closed even when any
  required record is missing.
- **Confirmed and fixed:** the production-submission reservation gate checked
  only the root claim. It now requires the verified terminal output and, when
  configured, the exact resolved approval command.
- **Confirmed and fixed:** the damaged-reservation fallback recognized only a
  surviving iteration-1 marker. Any canonical later marker naming the
  implementation now also proves that direct production submission must be
  refused, including an audit-preserved marker already quarantined out of the
  dispatch lane. This conservative probe can deny work but never authorize it.
- **Rejected by verification:** backup payload closure and the fallback
  damaged-reservation scan do not create work or grant execution. Their
  standalone decoding is deliberately retained and documented as
  non-authoritative.

Revisit when a second workflow needs cross-record semantic integrity that
cannot be derived from its existing durable records, or when elaboration gains
another input provenance class.
