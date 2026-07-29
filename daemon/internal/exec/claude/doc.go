// Package claude is the production Claude stage driver (#237, plan §5.3,
// §5.4): the exec.MaterializedStageDriver that turns one admitted, digest-
// verified input bundle into a ward workspace handoff running the pinned
// Claude CLI in subscription_contained mode, and its export into the durable
// ExecutionExport record the publication chain binds heads against.
//
// The driver holds no store, transport, or runtime handle. Everything with a
// side effect beyond its own state directory arrives as a port (Gate, Seeder,
// ExportRecorder, AdmissionAuthority, AuthStoreVolumes), supplied by the
// daemon composition; production adapters live beside the composition, the
// wardstore precedent. Durable per-invocation state is a JSON intent file in
// the driver's state directory, mirroring exec/fake's persistence: committing
// a new store table for driver-private state would be a shared persistence
// change, which is this unit's stop condition.
//
// Replay discipline: the intent file pins every nondeterministic input
// (RecordedAt, CommitDate) at StartWithInputs, so a crashed pipeline replays
// to byte-identical importer commits and export records, and the write-once
// store rows converge instead of colliding.
package claude
