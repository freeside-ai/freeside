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
// This package declares the backend only: its class, its configuration
// digest, and the re-gate on the declaration. It runs nothing yet; the room
// is #2877 and the conformance suite and pre-job probe are #2879.
//
// Tier A re-gate: the backend declares the capability only from a passed
// BackendConformance record for its own class and current configuration
// digest, and only on darwin. A fresh backend declares nothing, and a record
// for another class, another configuration, or a non-passing outcome is
// refused rather than published.
package macverify
