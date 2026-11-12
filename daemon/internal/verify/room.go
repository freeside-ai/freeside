package verify

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

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

// EnvRoom is a Room that can also run a command with extra environment
// variables. The verifier uses it for a command that asks for history
// (Command.History), which needs the range's SHAs and git's ownership
// override; a command that doesn't ask always goes through Run. A
// history command whose room isn't an EnvRoom fails closed with
// ErrRoomUnavailable before any workspace is built.
type EnvRoom interface {
	Room
	// RunEnv is Run with env's KEY=VALUE entries added on top of the
	// room's own fixed environment. An entry CheckRoomEnv refuses, or
	// one that would replace a variable the room fixes, is an error and
	// nothing runs.
	RunEnv(ctx context.Context, workdir string, argv, env []string) (StepResult, error)
}

// CheckRoomEnv refuses extra environment entries a room can't pass
// safely: an entry without "=", a key outside [A-Z_][A-Z0-9_]*, a value
// carrying NUL, CR, or LF, a key given twice, or a key in fixed (the
// variables the room sets itself, which an extra entry must never
// replace). The verifier writes every entry, so a refusal is a verifier
// bug, never a candidate outcome.
func CheckRoomEnv(env []string, fixed ...string) error {
	seen := make(map[string]bool, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || !envKeyPattern.MatchString(key) {
			return fmt.Errorf("room environment entry %q is not KEY=VALUE: %w", entry, ErrInvalidOptions)
		}
		if strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("room environment value for %s carries NUL, CR, or LF: %w", key, ErrInvalidOptions)
		}
		if slices.Contains(fixed, key) {
			return fmt.Errorf("room environment entry would replace the room's own %s: %w", key, ErrInvalidOptions)
		}
		if seen[key] {
			return fmt.Errorf("room environment names %s twice: %w", key, ErrInvalidOptions)
		}
		seen[key] = true
	}
	return nil
}

var envKeyPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
