package procbound

import (
	"errors"
	"os"
	"syscall"
)

// KillGroup sends SIGKILL to process group pgid and reports a group that
// has already finished as os.ErrProcessDone. It is the shared body of the
// Cancel callbacks that Bind and the verify room install.
//
// The mapping matters because os/exec treats only os.ErrProcessDone as a
// benign Cancel result and wraps anything else as "exec: canceling Cmd:
// ...", which Wait returns when the process itself exited 0. A raw kill
// error from a group that finished just before cancellation reached it
// would turn a command that succeeded, or a verify step that passed, into
// a failure. A command that exited nonzero or by signal is unaffected:
// Wait prefers its *exec.ExitError over the Cancel result.
//
// Two kill results mean the group is done:
//
//   - ESRCH: the group no longer exists, because the leader was reaped
//     and nothing else remained in it.
//   - EPERM, on darwin only, when a member lookup made after the kill
//     finds every member a zombie, or finds none. Darwin refuses a group
//     kill whose only members are zombies with EPERM rather than ESRCH,
//     so the window between a command's exit and its reap needs this
//     check. Off darwin the lookup never reports done and EPERM comes
//     back unchanged.
//
// This is the gate that re-checks a canceled command's group before its
// exit status may stand: EPERM alone also means a live member the daemon
// cannot signal, which on exit 0 would read as passed, so EPERM maps to
// done only when the lookup proves no member is alive. That is safe
// because EPERM means the kill delivered no signal, so the exit status
// Wait reports is the one the command produced, and a zombie cannot fork,
// so a zombie-only group gains no live member before the reap. The lookup
// must stay after the kill: a member that turns zombie between the two
// reads as dead, which it is by then.
//
// A failed lookup keeps the group not done and is joined to the kill's
// EPERM, so errors.Is(err, syscall.EPERM) still holds. Every other kill
// error, and nil, comes back unchanged.
func KillGroup(pgid int) error {
	err := syscall.Kill(-pgid, syscall.SIGKILL)
	switch {
	case errors.Is(err, syscall.ESRCH):
		return os.ErrProcessDone
	case errors.Is(err, syscall.EPERM):
		done, lookupErr := groupOnlyZombies(pgid)
		if lookupErr != nil {
			return errors.Join(err, lookupErr)
		}
		if done {
			return os.ErrProcessDone
		}
		return err
	default:
		return err
	}
}
