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
// digest, and the slot's home reset between jobs. The conformance pass,
// Suite.Full, proves the network, write, and home confinements on every slot
// as that slot, each beside a positive control run as the daemon user, and
// earns the declaration (suite.go). The offline cache is enforced per job
// instead: the room refuses a workspace whose cache entry is missing.
//
// The package declares the backend (its class, its configuration digest, and
// the re-gate on the declaration), builds its Room, the executor's
// verify.Room, and runs its conformance pass. Before every recipe command the
// room runs the pre-job probe on the leased slot: the sandbox network and
// write checks again, under the configured profile. A failed check
// quarantines the slot and withdraws the declaration until the daemon
// restarts and restores its durable record.
// The probe is the daemon's own binary (RunProbe, behind the hidden
// freesided macverify probe verb), copied into a workspace and checked
// against the digest the configuration digest names (probe.go).
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
// Install, Inspect, and Remove create, check, and delete the host state the
// room needs (the slot accounts and group, the executor root and helper, the
// sudoers rule, and the packet-filter anchor with its boot LaunchDaemon),
// behind freesided macverify. They act through a Host, and every decision
// about what they may adopt or delete is theirs (install.go).
//
// Tier A gates. The backend declares the capability only from a passed
// BackendConformance record for its own class and current configuration
// digest, and only on darwin: one Full recorded and published under the
// generation guard (backend.go), or one RestoreConformance accepts. A fresh
// backend declares nothing, a record for another class, another
// configuration, or a non-passing outcome is refused rather than published,
// a pass with no recorder never declares, and a superseded pass publishes
// nothing. A probe report proves its checks only when it is exact
// (checkProbeReport). The Room refuses a job unless the installed helper is
// the embedded one and root-owned, the host's Xcode and Swift match the
// configuration's pin, a workspace holding a Package.resolved has a complete
// offline Swift dependency cache entry for its key (swiftcache.go), a
// workspace holding a go.mod or go.sum has the pinned Go toolchain and a
// complete offline Go module cache entry for its key (gocache.go), and the
// leased slot passes the pre-job probe; it re-checks the job directory and
// cache entries prepare reports, and releases a slot only once its uid owns
// no process. Swift populate (swiftcache_populate.go) admits only https
// dependency URLs; Go populate (gocache_populate.go) installs the toolchain
// only from an archive with the pinned SHA-256 and refuses a go.mod that
// asks for a newer Go. Each writes an entry's record last, after proving the
// entry resolves offline.
package macverify
