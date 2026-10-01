// Package agentbaseline holds what the code knows about the two agents the
// daemon ran before agent selection (plan §5.4, "The baseline is honest"):
// the launch each ward stage hands a harness, the adapter fragments for the
// pinned Claude Code and Codex builds, and the baseline tree `freesided auth
// adopt` proposes.
//
// The adapters' capability sets are declarations. No stage contract suite
// drives the real harness yet, so each set names only what the launch the
// daemon builds today does, and the adapter conformance record written from
// it attests exactly that. A capability no launch control backs
// (auxiliary-inference control, exact resume) is deliberately absent, so a
// launch that needs one fails admission closed.
package agentbaseline
