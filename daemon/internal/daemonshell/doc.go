// Package daemonshell runs the freesided process that package main
// composes. A Shell serves the API on its listener and the loopback twin,
// serves and closes pairing control, tracks every long-running goroutine
// (Go), supervises their exits (ComponentExited), publishes the readiness
// record, and tears the process down (Close). The listener functions here
// decide which addresses the daemon may bind: loopback, or an address
// Tailscale reports as its own.
//
// Exit supervision classifies each ComponentKind once. An HTTP or pairing
// control failure ends the process for a restart; a workflow-side failure
// records a durable stop that keeps the API up and unattended admission
// closed until an operator resumes it after a fresh start
// (EnableDurableStopRecovery); a panic stays a panic.
//
// Pairing control sits behind the PairingControl interface, Serve and
// Close only, because its configuration and control routes live in package
// main's Tier A pairing files and stay there. For the same reason the
// order of cleanup on a failed start stays in run(): it stops signal
// handling first and then calls AbortStart, and it alone closes the store
// and lock on a start that fails before the shell exists.
package daemonshell
