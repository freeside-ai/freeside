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
// canonical DNS host:port authorities and, under provider_registry, the run's
// declared registry hosts on port 443, and requires matching TLS SNI before
// forwarding the ClientHello. A registry host comes from run policy, not
// daemon configuration, so the proxy resolves it itself and refuses the
// tunnel unless every address it resolves to is public unicast. After the
// proxy starts, the gate reads back the allowlist it enforces and fails
// closed when that differs from the handoff's request. The gate attests both
// the runtime's host-only mode and the writer's exact named attachment before
// execution. Proxy environment is compared as an exact key/value set
// independent of runtime ordering, and direct external and guest-DNS egress
// remain structurally absent if the writer ignores or clears those variables.
// Suite.Full also requires each configured provider/CDN to reject an
// alternate encrypted HTTP Host; without TLS termination, that
// application-layer rule is an explicit provider assumption rather than a
// property CONNECT can enforce.
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
// and direct external-IP refusal), the same handoff then runs again as a second
// run under provider_registry with one declared registry witness (reachable
// through the proxy, with an undeclared authority, a direct connection, and
// DNS still refused), then two of the spike's three negative probes run — the
// read-write-attach exclusion (a second VM cannot attach the workspace a live
// writer holds read-write) and credential-marker containment (the marker is
// absent from the export yet still readable from the detached credential
// volume, so its absence was mount omission, not deletion). A separate
// networkless-export probe verifies an explicit empty runtime attachment set
// before start, then deliberately attempts DNS and a direct-IP connection;
// only a complete green Full pass adds supports_networkless_export and
// supports_enforced_provider_egress to the backend declaration, and a
// completed, generation-current pass also records its outcome durably
// through the suite's ConformanceRecorder (issues #327/#320): a passed
// record carries the explicit proven set, a failed record proves nothing,
// and the capabilities are published only after the pass's own row is
// durable (an unpersisted proof is never declared: a record failure fails
// the pass, and a pass run with no recorder proves but declares nothing),
// since the store gates unattended admission on the durable record, not on
// process state. The third handoff-spike probe
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
// verifies only cheap preconditions — including the Full-earned capability
// declarations —
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
// The §5.4 leased auth-store mutation path relaxes exactly one mount, under
// evidence at every step. A spec may mark one credential mount Writable only
// together with an AuthStoreLeaseClaim; the gate first requires its source to
// match the claimed identity's immutable trusted volume binding, then acquires
// the per-identity mutation lease itself (only it knows the handoff deadline
// and when the writer is provably absent), sizes the window past its whole
// budget, re-verifies holder and fence at the last instant before the writer starts,
// and releases in teardown only once the writer's absence is proven — an
// unproven writer keeps the window held, with expiry as the backstop. The
// mutation itself is attested, not assumed: two observer VMs digest the
// leased volume through read-only mounts before the writer and after its
// absence proof, under the base observer's exact proof discipline, and the
// result records that the store changed, never what changed (the §5.4 export
// scan remains the content control). Every other credential mount stays
// read-only under the unchanged checks.
//
// Restart safety is the journal plus Recover. The journal (an injected seam,
// like the lease and conformance recorders; the store adapter is the
// daemon's) records identity and unreconstructible proofs only — the run's
// unpredictable ownership token, the spec digest, the held lease, the two
// pre-writer facts, and writer-complete — never per-stage progress: after a
// restart the runtime world classified by the persisted token is the only
// trustworthy account of progress, and a persisted progress bit would be a
// decoded trust bit recovery would believe instead of re-observing. Begin is
// durable before the first runtime object exists; for a leased production run
// it commits with lease acquisition in one store transaction, so neither an
// object nor a mutation window can outlive the daemon unrecorded. Recover
// re-gates the record, the identity-volume binding, and the caller's
// re-supplied spec (digest-bound), then adopts only a writer-complete run
// whose workspace is provably its own — everything earlier lost its egress
// proof with the process and is torn down — re-earning every release check
// with a fresh exporter. Loss is prove-then-persist: teardown's absence
// proofs, then a full-listing ownership-token audit, then the durable close;
// a nil-error loss is the caller's rerun-safe signal, an error commits
// nothing and is retryable, and no path releases an export that did not just
// pass verification. Serializing Recover against a live Handoff for the same
// run is the caller's contract.
//
// Layout, by concept:
//
//   - errors.go        the Check vocabulary and typed ConformanceFailure
//   - runtime.go       the Runtime seam over the container runtime, and its
//     report vocabulary
//   - seed.go          workspace seeding: host-side source verification, the
//     seeder lifecycle, and the read-only base attestation
//   - runtime_cli.go   CLIRuntime, the os/exec-backed Apple container
//     implementation (one of the package's two os/exec importers, with
//     verification_room.go)
//   - runtime_ops.go   runtimeOps, the runtime and configuration primitives
//     the handoff gate and the review lifecycle share
//   - helper_role.go   runHelperRole, the one create-to-absence sequence a
//     short-lived seeder or observer runs
//   - launch_record.go the one structured record per launch that sizes are
//     tuned from, how the launch ended, memory-limit kill detection, and the
//     stopped-helper check
//   - rootwalk.go      walkRoot, raw relative-name traversal under an os.Root
//   - egress.go        the host-only network lifecycle and exact CONNECT
//     allowlist proxy
//   - config.go        Backend configuration and validation
//   - spec.go          HandoffSpec and its validation: pinned image refs,
//     credential mounts and manifest policy, launch-state policy, and the
//     observer tool registry
//   - sizes.go         launch classes and the CPU and memory each declares
//   - backend.go       the exec.RunnerBackend implementation and its frozen
//     capability declaration
//   - conformance.go   pure verifiers for checks 1, 2, 4, and 5, the seeding
//     roles' allowlist, and the seeded-base proof
//   - handoff.go       the gate lifecycle: seeding, checks 3-5 sequencing,
//     the lease lifecycle and credential-store observation, and teardown
//   - handoff_roles.go handoffRoles, the one ordered table of handoff roles
//     that names, binds, tears down, and recovers each run object
//   - state.go         per-launch state volumes (config root, continuity,
//     scratch): creation, seeding, and the observed proof
//   - prompt.go        PromptFile, the bounded user prompt delivered as a file
//   - input.go         operator input files: the manifest rules, the staged
//     archive with byte-for-byte digest checks, and the observed listing proof
//   - staged_volume.go the seeder/observer handshake that fills a read-only
//     writer input volume
//   - instruction.go   the Claude instruction bundle composed from the
//     exact-base snapshot, its seeding, and its observation from another VM
//   - stall.go         stallWatch, the running writer's stall heartbeat (§5.12)
//   - writer_outcome.go the writer's exit-status marker proof and ErrWriterFailed
//   - failure_evidence.go bounded diagnostic output from a failed launch
//   - read_hold.go     shared auth-store read holds for a read-only handoff
//   - journal.go       the durable handoff record and HandoffJournal seam
//   - cancellation_coverage.go CancellationCoverage, the tasks that predate
//     runtime ownership journaling
//   - task_judgments.go TaskJudgments, the task-owned journal under advisory
//     inference calls
//   - recover.go       restart-safe recovery: adopt-or-loss, the orphan
//     audit, and the committed outcome
//   - runtime_release.go releasing a closed run's rig names once the runtime
//     lists no object under them
//   - export_verify.go check 7: safe archive extraction, manifest and digest
//     verification, and the fail-closed output-scanner hook
//   - suite.go         the invocable conformance suite: Full (checks 1-5, 7,
//     agent egress probes, two handoff negative probes, and networkless
//     export) plus PreJob
//   - lineup_probe.go  one lineup probe call through an image, and the
//     credentials a probe container reads
//   - claude_model_discovery.go the initialize probe's model report, decoded
//     into an enrollment's discovered model catalog, and the report checks
//     both harnesses share
//   - codex_model_discovery.go the Codex model/list probe's pages, decoded
//     into the same report
//   - auth_enrollment.go EnrollmentBootstrap, a harness client's first
//     enrollment (§5.4)
//   - auth_adoption.go adopting a flag-era identity's existing store as an
//     enrollment
//   - claude_auth_enrollment.go, claude_auth_probe.go
//     the redacting Claude SetupToken and the enrollment probe's verdict
//   - codex_auth_enrollment.go, codex_auth_refresh.go, codex_auth_renewal.go
//     Codex login enrollment, the host-only refresh transaction and
//     re-enrollment marker, and operator renewal under the same lifetime gates
//   - credential_manifest.go, credential_integrity.go, credential_read.go
//     networkless, read-only observers over credential stores: the volume
//     manifest proof, the integrity observation, and the setup-token read
//   - review_credential.go ReviewCredential, a redacting holder of the
//     credential one review launch delivers
//   - review_provider.go the reviewProvider and credentialStrategy seams, and
//     the Codex provider
//   - claude_review.go the Claude review provider and its setup-token
//     credential strategy
//   - codex_review.go, codex_review_lifecycle.go, codex_review_lifecycle_owner.go
//     the review topology request and its refusals, the durable launch
//     journal and intent states, and CodexReviewLifecycle, the runtime owner
//   - codex_review_workspace.go the candidate checkout snapshot into a
//     ward-owned volume, its HEAD and tree proof, and its cleanup
//   - codex_review_inputs.go the task's attachments seeded into a ward-owned
//     input volume for the reviewer, and its listing proof
//   - codex_review_access.go the sandboxed probe that the review can read the
//     candidate
//   - codex_review_volume_lease.go serialized review-volume attachment leases
//   - codex_review_collection.go inspect, collect, clean up, and abort a
//     stopped review
//   - codex_review_source.go the review source's configuration, instruction
//     artifacts, and the persisted outcome and its re-gates
//   - codex_review_usage.go usage measurements from a Codex transcript
//   - codex_review_rig_recovery.go RecoverCodexReviewResources, journal
//     reconciliation scoped to one runtime namespace
//   - review_evidence.go decoding a retained review journal at the read-only
//     client boundary
//   - usage_collector.go, claude_usage_collector.go
//     the UsageCollector port (§10) and the Claude collector that answers
//     from writer-turn captures
//   - verification_room.go ProjectImageRoom, networkless recipe runs in the
//     admitted project image (the second os/exec importer)
//   - verification_ownership.go task-bound verification launch ownership,
//     StopRun's owned cleanup, and ProveInvocationAbsent's per-invocation
//     absence proof
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
