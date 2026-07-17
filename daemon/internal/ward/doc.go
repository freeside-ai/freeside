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
// The conformance suite (suite.go) is the invocable form of that contract,
// run at the plan §5.7 cadence points without a real work item (doctor
// scheduling is a downstream operations-unit concern). Suite.Full proves the
// whole contract as one pass on the current runtime: a synthetic handoff with
// a benign writer and a seeded fake credential exercises checks 1-5 and 7
// together, then two of the spike's three negative probes run — the
// read-write-attach exclusion (a second VM cannot attach the workspace a live
// writer holds read-write) and credential-marker containment (the marker is
// absent from the export yet still readable from the detached credential
// volume, so its absence was mount omission, not deletion). The third probe
// (same-VM guest unmount is not a detach) needs a CAP_SYS_ADMIN guest
// process, which the gate's ContainerSpec vocabulary deliberately cannot
// express — that minimality is checks 1-2's isolation argument — so it is a
// permanent reference-runtime test driving the CLI directly
// (TestLiveConformanceSameVMRefutation), never a Suite member and never a
// widening of the spec. Every suite result is fail-closed: nil (conformant)
// or a *ConformanceFailure naming the failed check or probe (the §3.1
// non-waivable class, which never auto-promotes or offers a bypass).
//
// Suite.PreJob is the lightweight probe run before each unattended job. It
// verifies only cheap preconditions — the capability declaration is intact,
// the images are digest-pinned, the runtime is reachable, and a
// create→inspect→delete liveness round-trips — and boots no VM, copies no
// workspace, and exports nothing. It deliberately does NOT re-verify the
// realized isolation Full proves: credential separation holding in a started
// writer, the read-only remount, export digest/scan containment, or the
// negative probes. A green PreJob means the backend is plausibly still
// operable; only Full proves it conformant. The plan §5.7 cadence is
// therefore Full at startup, after configuration changes, and on the doctor
// schedule; PreJob before each job.
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
// Runtime-object cleanup is identity-safe under replacement (#138). The
// runtime exposes no immutable object identity (a container's id is its
// caller-chosen name; volumes have only a name) and no conditional delete,
// so a successful create never holds standing authority over its
// deterministic name: every destructive decision requires fresh evidence
// that the observed object is the one this run created — the invocation's
// unpredictable ownership label, with the creation instant captured right
// after each create as a veto on replacements — and an object failing that
// evidence is a foreign replacement, left untouched and counted as absent.
// The window between the last verification and the name-addressed
// stop/delete call is irreducible on this runtime and accepted: the
// freeside-handoff- name prefix is a daemon-owned namespace, and a host
// actor mutating it holds the same user's full runtime authority, outside
// the threat model. Revisit when Apple container exposes immutable runtime
// IDs or conditional deletion.
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
//   - suite.go         the invocable conformance suite: Full (checks 1-5, 7
//     plus two negative probes) and the lightweight PreJob probe
//
// The full-lifecycle integration test and the conformance suite's
// reference-runtime members (Suite.Full/PreJob end to end, and the
// same-VM-refutation probe) run only against the reference runtime (Apple
// container 1.1.0 on macOS) and are opt-in via FREESIDE_WARD_LIVE_TEST=1; CI
// does not run them, a recorded verification gap. Everything else, including
// every check's failure path and the suite's orchestration and fail-closed
// results, runs against the scripted fake runtime.
package ward
