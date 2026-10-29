// Package agentbaseline holds what the code knows about the two agents the
// daemon ran before agent selection (plan §5.4, "The baseline is honest"):
// the launch each ward stage hands a harness, the adapter fragments for the
// pinned Claude Code and Codex builds, and the baseline tree `freesided auth
// adopt` proposes.
//
// The adapters' capability sets are declarations. The Claude stage contract
// suite in exec/claude observes the real harness before the daemon records
// its proved set. Codex review proof belongs to its admission cutover (#1980).
// Capabilities no launch control backs (auxiliary-inference control, exact
// resume) remain absent, so a launch needing one fails admission closed.
package agentbaseline
