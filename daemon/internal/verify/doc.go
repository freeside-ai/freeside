// Package verify is the gauntlet's clean verifier (§5.6, §5.15): it
// re-runs the trusted verification recipe against the exact candidate
// head in a fresh daemon-materialized workspace and emits the evidence
// channel.
//
// Trust model. The recipe loads only from approved control-plane config
// or the trusted base commit (§5.8); the candidate head's copy of the
// recipe path is data, compared and risk-flagged on divergence but
// never executed. Verification output binds to the exact recipe digest
// (sha256 over the trusted bytes as loaded) and the exact head SHA; a
// head the checkout does not hold fails closed before any command runs.
// Evidence is only what the verifier itself authors (the report and the
// transcript under capture "none"), stamped with verifier provenance
// through domain.NewArtifact, so publish eligibility originates in
// trusted policy and a candidate-planted file has no path into
// evidence_snapshot. Verification-control file changes (dependency
// pins, build entrypoints, lint config, the recipe path) are
// mechanically identified and risk-flagged from the importer's audited
// change account; gating stays downstream.
//
// Outcome split, matching the importer: integrity violations (invalid
// options, unreadable trusted recipe, head mismatch, room failure) fail
// closed with typed errors and no Result; policy signals accumulate as
// Findings and never block execution.
//
// Named residual (§5.6): candidate test code executes inside the warded
// verifier. Running the recipe's `go test` runs the candidate's test
// functions; that is inherent to verification and containment is the
// room's job. In this unit the room is ProcRoom, a process-level fake
// and an explicitly weaker isolation class than the ward's room (§5.7):
// it scrubs the environment but cannot deny network or filesystem
// access, so it is for tests and bring-up, never a silent substitute
// where ward isolation is required.
//
// Lane: gauntlet. See docs/plan.md §5.6 (workspace handoff, import, and
// clean verification), §5.8 (trusted config), and §5.15 (evidence and
// images); decision note devlog/2026-07-17-0035-clean-verifier.md.
package verify
