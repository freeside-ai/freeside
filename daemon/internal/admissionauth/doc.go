// Package admissionauth is the Tier A admission and import authority. It
// implements stage.AdmissionAuthority for the Claude driver and
// daemoncompose.ScenarioImportAuthority for the scenario lane: before the
// driver starts, recovers, or imports an invocation, Authority binds the
// driver's private replay state back to the durable admission and re-checks
// it against current state, and it builds the importer options that import
// enforces.
//
// The package holds no configuration of its own. Its caller supplies the
// store, the blob store, the daemon's configured path allowlist, and the
// GitHub App identity resolver in an Authority value; preflight and submit
// call ResolvedPathAllowlist to refuse a policy the daemon would refuse at
// start.
package admissionauth
