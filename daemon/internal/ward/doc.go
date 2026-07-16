// Package ward is the runner layer: runner backends, the workspace-handoff
// gate, conformance, and operating modes (plan §5.7).
//
// The first backend realizes the fresh_vm_read_only_volume_handoff isolation
// class proven on Apple container 1.1.0 by the workspace-handoff spike
// (docs/spikes/workspace-handoff.md). Its handoff gate enforces the spike's
// required backend contract, checks 1-5 and 7 plus teardown: the
// credential-bearing writer VM is proven terminated by observed state (never
// scheduling intent), the workspace is remounted read-only in a fresh
// credential-free exporter VM whose mounts are verified against a generated
// allowlist before execution, and the exported output has its digests
// verified and scanned before anything reaches the out-of-process gauntlet
// worker. Check 6 — running only the trusted export helper — is gauntlet
// logic (daemon/internal/export) that ships inside the pinned exporter image;
// the gate collects its output but never owns its content.
//
// Two owner decisions bind this package (issue #76; the Wave 1 planning
// decision note):
//
//   - The same-VM fallback class (terminate the agent process, detach
//     credentials, export in the same VM) is refuted by execution, not merely
//     weaker: release 1.1.0 exposes no host hot-detach, and a guest unmount
//     leaves the credential block device attached and remountable. It must
//     not be implemented, declared, or scaffolded.
//   - The workspace-copy export cost is accepted: the exporter copies the
//     read-only workspace into its own root filesystem because 1.1.0 has no
//     direct named-volume export. No workaround may reach into Apple
//     container's private block-image state.
//
// Layout, by concept:
//
//   - errors.go        the Check vocabulary and typed ConformanceFailure
//   - runtime.go       the Runtime seam over the container runtime, and its
//     report vocabulary
//   - runtime_cli.go   CLIRuntime, the os/exec-backed Apple container
//     implementation (the package's only os/exec importer)
//   - config.go        Backend configuration and validation
//   - backend.go       the exec.RunnerBackend implementation and its frozen
//     capability declaration
//   - conformance.go   pure verifiers for checks 1, 2, 4, and 5
//   - handoff.go       the gate lifecycle: checks 3-5 sequencing and teardown
//   - export_verify.go check 7: safe archive extraction, manifest and digest
//     verification, and the fail-closed output-scanner hook
//
// The full-lifecycle integration test runs only against the reference
// runtime (Apple container 1.1.0 on macOS) and is opt-in via
// FREESIDE_WARD_LIVE_TEST=1; CI does not run it, a recorded verification
// gap. Everything else, including every check's failure path, runs against
// the scripted fake runtime.
package ward
