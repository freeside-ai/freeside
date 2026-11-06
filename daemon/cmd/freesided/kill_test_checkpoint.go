package main

// killTestCheckpoint marks a point in the intent lifecycle where
// TestDaemonRecoversAcrossSIGKILL may SIGKILL the daemon. It is a no-op in
// every build; only that test, running its own binary as the daemon,
// replaces it, so production code never reaches the blocking hook.
var killTestCheckpoint = func(string) error { return nil }
