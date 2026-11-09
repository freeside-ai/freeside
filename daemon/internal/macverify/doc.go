// Package macverify is the credential-free macOS verification executor's
// runner backend (plan §5.7, macOS Execution): the host-side room that runs a
// trusted recipe's Apple-only checks against the daemon-materialized exact
// head and authors only the evidence channel. It holds no provider or GitHub
// credential and no placeholder and runs no agent, which is why it is the one
// host-side class unattended admits, under its own capability declaration,
// supports_credential_free_host_verification, and only for verification.
//
// Each job is confined four ways: network access denied by a sandbox profile
// and by packet-filter rules keyed on the slot's uid, writes confined to the
// job directory, dependencies taken only from a trusted offline cache keyed by
// digest, and the slot's home reset between jobs. The executor's full
// conformance pass (#2879) proves all four and earns the declaration.
//
// The package declares the backend (its class, its configuration digest, and
// the re-gate on the declaration) and builds its Room, the executor's
// verify.Room. The conformance suite and pre-job probe are #2879.
//
// The Room runs each recipe command as a slot account leased from a pool, in
// the slot's own launchd context, under the configured sandbox profile. The
// daemon has no root, so it reaches the slot only through slot-run, a
// root-owned helper whose bytes this package embeds, invoked through one
// sudoers rule. The helper decides which accounts it may touch. Absence is
// proved by uid, not process group: each slot runs one job at a time, so once
// the slot's uid owns no live process nothing of the job survives, a
// descendant that left its session included. A slot without that proof is
// quarantined.
//
// Tier A gates. The backend declares the capability only from a passed
// BackendConformance record for its own class and current configuration
// digest, and only on darwin. A fresh backend declares nothing, and a record
// for another class, another configuration, or a non-passing outcome is
// refused rather than published. The Room refuses a job unless the
// installed helper is the embedded one and root-owned, re-checks the job
// directory prepare reports, and releases a slot only once its uid owns no
// process.
package macverify
