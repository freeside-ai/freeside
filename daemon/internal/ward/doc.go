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
// The credential-bearing writer receives exactly one per-run host-only
// network. Its only route beyond that network is a daemon-side CONNECT proxy
// advertised at the network's host gateway; the proxy admits only configured
// canonical DNS host:port authorities and requires matching TLS SNI before
// forwarding the ClientHello. The gate attests both the runtime's host-only
// mode and the writer's exact named attachment before execution. Proxy
// environment is compared as an exact key/value set independent of runtime
// ordering, and direct external and guest-DNS egress remain structurally
// absent if the writer ignores or clears those variables. Suite.Full also
// requires each configured provider/CDN to reject an alternate encrypted HTTP
// Host; without TLS termination, that application-layer rule is an explicit
// provider assumption rather than a property CONNECT can enforce.
//
// Host-only describes the network shape, not absence of a host neighbor: the
// runtime gateway remains an address through which a wildcard-bound host
// service could be reached. The production freesided API therefore enforces
// loopback or an exact address reported by the local Tailscale client as a
// separate fail-before-bind invariant; any other host service needs its own
// declared binding policy, while the ward CONNECT proxy is the deliberate
// agent-reachable exception. The reference-runtime lifecycle test starts the
// real freesided API on loopback, proves it is live from the host, then
// requires the writer to fail a TCP connection to that same port through its
// gateway while the declared provider path succeeds.
//
// The conformance suite (suite.go) is the invocable form of that contract,
// run at the plan §5.7 cadence points without a real work item (doctor
// scheduling is a downstream operations-unit concern). Suite.Full proves the
// whole contract as one pass on the current runtime: a synthetic handoff with
// a benign writer and a seeded fake credential exercises checks 1-5 and 7
// together (including declared-provider success, undeclared CONNECT refusal,
// and direct external-IP refusal), then two of the spike's three negative probes run — the
// read-write-attach exclusion (a second VM cannot attach the workspace a live
// writer holds read-write) and credential-marker containment (the marker is
// absent from the export yet still readable from the detached credential
// volume, so its absence was mount omission, not deletion). A separate
// networkless-export probe verifies an explicit empty runtime attachment set
// before start, then deliberately attempts DNS and a direct-IP connection;
// only a complete green Full pass adds supports_networkless_export to the
// backend declaration. The third handoff-spike probe
// (same-VM guest unmount is not a detach) needs a CAP_SYS_ADMIN guest
// process, which the gate's ContainerSpec vocabulary deliberately cannot
// express — that minimality is checks 1-2's isolation argument — so it is a
// permanent reference-runtime test driving the CLI directly
// (TestLiveConformanceSameVMRefutation), never a Suite member and never a
// widening of the spec. Every suite result is fail-closed: only nil is
// conformant. A violated check or probe is a *ConformanceFailure naming that
// assertion; an operational error that prevents the suite from reaching a
// verdict remains non-nil and gates unattended operation without pretending a
// specific contract assertion was disproved (the §3.1 non-waivable class,
// which never auto-promotes or offers a bypass).
//
// Suite.PreJob is the lightweight probe run before each unattended job. It
// verifies only cheap preconditions — including the Full-earned networkless
// capability declaration —
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
// Beyond the spike's blank-workspace scope, the gate seeds the workspace at a
// declared exact base (plan §5.9) and attests what it actually holds. The
// mechanism is forced by the reference runtime rather than chosen: Apple
// container 1.1.0 refuses to copy into a container that is not running, and a
// copy whose destination lies inside a mounted volume writes nothing while
// still reporting success. So a pinned, credential-free, network-free seeder
// holds the workspace read-write, receives the daemon-owned checkout into its
// own root filesystem, and its fixed command moves the tree onto the mount; a
// separate sentinel copy signals that the staged tree is whole, because a
// directory copy is not atomic. None of that attempt is believed. After the
// seeder is proven absent, a second container mounts the workspace read-only
// and writes what it observes, bound to the invocation's unpredictable
// ownership token, into its own root filesystem, which the gate exports and
// parses. The attestation runs before the writer because the base is a
// pre-writer fact, and a workspace that does not match what was declared fails
// the gate rather than being reported.
//
// What is attested is content, not just a pointer. HEAD names a commit but
// says nothing about what is checked out, and the intended producer makes that
// gap concrete: publish.Transport.FetchBase moves HEAD to the base and never
// checks anything out, so its directory carries a .git and an empty working
// tree. The observer therefore asks the pinned Git in its image for HEAD's
// tree, then compares each regular file's raw blob identity and executable
// mode and refuses extras. This avoids trusting copied index flags and Git
// attribute conversions. Replacement objects are disabled during resolution
// and rejected when present. Independently, the observer reports a digest
// over every file the host computes over the source snapshot, so the two agree
// only if the tree that landed is the tree that was approved. The two proofs
// together bind the writer's starting tree to the declared commit; a
// legitimately empty commit remains clean and is accepted.
//
// Layout, by concept:
//
//   - errors.go        the Check vocabulary and typed ConformanceFailure
//   - runtime.go       the Runtime seam over the container runtime, and its
//     report vocabulary
//   - seed.go          workspace seeding: host-side source verification, the
//     seeder lifecycle, and the read-only base attestation
//   - runtime_cli.go   CLIRuntime, the os/exec-backed Apple container
//     implementation (the package's only os/exec importer)
//   - egress.go        the host-only network lifecycle and exact CONNECT
//     allowlist proxy
//   - config.go        Backend configuration and validation
//   - backend.go       the exec.RunnerBackend implementation and its frozen
//     capability declaration
//   - conformance.go   pure verifiers for checks 1, 2, 4, and 5, the seeding
//     roles' allowlist, and the seeded-base proof
//   - handoff.go       the gate lifecycle: seeding, checks 3-5 sequencing, and
//     teardown
//   - export_verify.go check 7: safe archive extraction, manifest and digest
//     verification, and the fail-closed output-scanner hook
//   - suite.go         the invocable conformance suite: Full (checks 1-5, 7,
//     agent egress probes, two handoff negative probes, and networkless
//     export) plus PreJob
//
// The full-lifecycle integration test and the conformance suite's
// reference-runtime members (Suite.Full/PreJob end to end, and the
// same-VM-refutation probe) run only against the reference runtime (Apple
// container 1.1.0 on macOS) and are opt-in via FREESIDE_WARD_LIVE_TEST=1; CI
// does not run them, a recorded verification gap. The seeding and
// base-attestation path is in that same class: the fake models the reference
// runtime's copy semantics (running-only, and silently discarded into a mount)
// so the fail-closed paths are covered in CI, but the guest-side behaviour of
// the seeder and observer commands is proven only by the opt-in live run.
// Everything else, including every check's failure path and the suite's
// orchestration and fail-closed results, runs against the scripted fake
// runtime.
package ward
