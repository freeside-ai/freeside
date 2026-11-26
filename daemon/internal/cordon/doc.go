// Package cordon runs one cordoned writer launch on the daemon's host
// (docs/plan.md §5.7, Cordoned Runs; #3466). A launch is the agent CLI
// started as the daemon's own account under a Seatbelt rule file, with no
// VM, no second account, and no sudo. The rule file (rules.sb, rendered by
// RenderRules) allows by default and denies a short list: the owner's
// secrets and keychain, launchd jobs and Launch Services, the Simulator and
// container services, and every protected root. The whole CLI starts under
// it, so the agent's file tools are held as its shell commands are, and
// the CLI's own confinement mode stays off because Seatbelt can't be
// applied twice.
//
// The agent gets the enrolled identity's setup token on its own pipe, fd 3,
// named by CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR; the prompt stays alone
// on stdin, and no argv, environment, or file under the launch's directory
// carries the token. Rationale: devlog/2026-11-22-2039-cordoned-writer-token.md.
//
// A launch's directory, <RunsRoot>/<RunID>, holds three parts:
//
//	run/       RUN_ROOT, the one place the rules reopen
//	  worktree/  the seeded checkout and the agent's working directory
//	  config/    CLAUDE_CONFIG_DIR, Freeside's, never the owner's
//	  tmp/       TMPDIR
//	  mise-state/ MISE_STATE_DIR, so mise's trust in the worktree's config
//	             ends with the run (#3601)
//	ctl/       the rule file, the proof's tag and control, the transcript
//	           (the agent's stdout), its stderr, the prompt, the
//	           supervisor's stderr, and the records (record.go): the
//	           supervisor's request, the claim, the cancellation marker,
//	           and the end and export records
//	export/    the export's manifests and blobs, once Export runs
//
// ctl and export sit under a protected root and outside RUN_ROOT, so the
// launch can neither read nor write them. The worktree is untrusted: the daemon runs no
// hook, config, or script from it, and touches it only to seed it.
//
// The run-end proof (runend.go) finds a launch's processes by their
// sandbox, not their process group: each launch's rules deny the metadata
// of its own tag path, and sandbox_check tells which processes that rule
// applies to, however they detached. It needs cgo on macOS; any other build
// refuses every launch before starting it. Rationale and the alternatives
// rejected: devlog/2026-11-22-1236-cordoned-host-launch.md.
//
// The daemon runs no launch itself. It writes the launch's request and
// starts a supervisor (supervisor.go, as `freesided cordon supervise`, from
// the daemon's own executable, in a session of its own) that claims the run
// by creating ctl/supervisor.json with its pid and start time, then starts
// the canary and the agent as their parent, copies their output, runs the
// proof, and writes the end record. A launch therefore outlives a daemon
// restart, and a restarted daemon finds everything it needs under the
// run's directory (recover.go): Recover waits for a live supervisor,
// finishes an interrupted export, and ends lost, after the proof, a run
// whose supervisor died with no end record or never claimed it. Whoever
// creates the claim first owns the run, so a supervisor that starts after
// recovery claimed it starts nothing. RequestCancellation writes a durable
// marker that outranks a clean exit, so a canceled run never exports, and
// ReleaseFinished removes only a run whose end is recorded and whose
// supervisor is gone. No record names a path, and a process is signaled
// only while its pid has the start time its claim records. Rationale:
// devlog/2026-11-23-0752-cordon-restart-recovery.md.
//
// Export (export.go) turns a launch with a clean, proven end into the
// handoff the importer takes: the trusted exporter's walk, run in the
// daemon's process over the still worktree, and the same credential scan
// as the ward's check 7. AuthenticateReleasedExport checks a released
// export against the record Export wrote. Why that stands in for the
// ward's exporter VM, and where it is weaker:
// devlog/2026-11-22-2046-cordoned-export.md.
//
// The backend declares the class's four capabilities only after a
// Suite's Full pass is recorded for its ConfigurationDigest (suite.go).
// Full runs this package's probe (probe.go, as `freesided cordon probe`,
// from a copy of the daemon's executable) inside real cordoned launches,
// against stand-in targets the suite creates, each with a positive control
// outside the launch, so every refusal is the sandbox's own. A launch with
// RequireProof first runs the lighter pre-job probe, whose failure
// withdraws the declaration. Rationale:
// devlog/2026-11-22-2144-cordon-conformance-proof.md.
//
// This package is the launch, recovery, and export halves of a cordoned
// handoff, behind its own API. The stage driver reaches it through its
// CordonGate port (internal/exec/stage/cordon.go), which a daemoncompose
// adapter maps onto this API and composes with the daemon's token read
// (internal/daemoncompose/cordon_gate.go). Nothing admits a cordoned
// attempt until admission chooses the class (#3472).
package cordon
