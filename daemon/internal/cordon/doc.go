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
// A launch's directory, <RunsRoot>/<RunID>, holds three parts:
//
//	run/       RUN_ROOT, the one place the rules reopen
//	  worktree/  the seeded checkout and the agent's working directory
//	  config/    CLAUDE_CONFIG_DIR, Freeside's, never the owner's
//	  tmp/       TMPDIR
//	ctl/       the rule file, the proof's tag and control, the transcript
//	           (the agent's stdout), its stderr, and the end and export
//	           records (record.go)
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
// Export (export.go) turns a launch with a clean, proven end into the
// handoff the importer takes: the trusted exporter's walk, run in the
// daemon's process over the still worktree, and the same credential scan
// as the ward's check 7. AuthenticateReleasedExport checks a released
// export against the record Export wrote. Why that stands in for the
// ward's exporter VM, and where it is weaker:
// devlog/2026-11-22-2046-cordoned-export.md.
//
// This package is the launch and export halves of a cordoned handoff,
// behind its own API: it does not implement the stage driver's Gate port,
// and nothing outside the package calls it yet. The driver port, restart
// recovery (#3468), the credential (#3469), conformance (#3470), and
// admission (#3472) are later units.
package cordon
