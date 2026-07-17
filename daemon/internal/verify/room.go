package verify

import "context"

// StepResult is one recipe command's outcome as observed by the room.
type StepResult struct {
	// ExitCode is the command's exit status; -1 records a command that
	// died to a signal (a per-command timeout kill lands here), which is
	// a verification failure, never a masked success.
	ExitCode int
	// Output is the combined stdout and stderr, bounded by the room.
	Output []byte
	// Truncated reports that Output was cut at the room's byte cap, so a
	// bounded transcript is honest about being partial.
	Truncated bool
}

// Room runs one recipe command in the materialized verification
// workspace. The room owns process isolation; the verifier owns every
// trust decision (recipe resolution, head binding, evidence stamping),
// so a stronger ward-provided room later replaces the execution backend
// without moving any trust logic.
type Room interface {
	Run(ctx context.Context, workdir string, argv []string) (StepResult, error)
}
