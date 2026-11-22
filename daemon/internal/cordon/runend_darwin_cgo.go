//go:build darwin && cgo

package cordon

/*
#include <errno.h>
#include <stdlib.h>
#include <sys/types.h>

// sandbox_check is exported by libsystem_sandbox but declared in no public
// header, so it is declared here. It is variadic, which cgo can't call, so
// each shape the proof needs gets a small wrapper. It returns 0 when the
// operation is allowed, 1 when it is denied, and -1 with errno set on error.
extern int sandbox_check(pid_t pid, const char *operation, int type, ...);

enum {
	cordon_filter_none = 0,
	cordon_filter_path = 1,
	// SANDBOX_CHECK_NO_REPORT: the query logs no violation.
	cordon_check_no_report = 0x40000000,
};

static int cordon_sandboxed(pid_t pid) {
	return sandbox_check(pid, NULL, cordon_filter_none);
}

static int cordon_metadata_denied(pid_t pid, const char *path) {
	return sandbox_check(pid, "file-read-metadata", cordon_filter_path | cordon_check_no_report, path);
}
*/
import "C"

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// From darwin's <sys/proc.h>, which golang.org/x/sys doesn't define:
// SIDL, a process fork is still creating; SZOMB (internal/procbound uses
// the same value); and P_WEXIT, a process working on its exit.
const (
	stateCreating = 1
	stateZombie   = 5
	flagExiting   = 0x00002000
)

// darwinHost answers the proof's queries from the kernel.
type darwinHost struct{}

func newHostProcesses() (hostProcesses, error) { return darwinHost{}, nil }

// live lists every process on the host, not only the account's: a member
// that ran a setuid program changed its uid but not its rules.
func (darwinHost) live() ([]hostProcess, error) {
	procs, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, fmt.Errorf("list processes: %w", err)
	}
	listed := make([]hostProcess, 0, len(procs))
	for i := range procs {
		if p := &procs[i].Proc; p.P_stat != stateZombie {
			listed = append(listed, hostProcess{pid: int(p.P_pid), start: startMicros(p), settled: settled(p)})
		}
	}
	return listed, nil
}

// same reads pid's entry again. The kernel answers a pid it doesn't know
// with an empty entry or EIO, depending on the release.
func (darwinHost) same(p hostProcess) (bool, error) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", p.pid)
	if errors.Is(err, syscall.EIO) || errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read process %d: %w", p.pid, err)
	}
	return int(k.Proc.P_pid) == p.pid && settled(&k.Proc) && startMicros(&k.Proc) == p.start, nil
}

// settled reports a process neither being created nor exiting nor a
// zombie. sandbox_check's answer for a process in transition can't be
// trusted: a fork chain's links read as unsandboxed while they hand over.
func settled(p *unix.ExternProc) bool {
	return p.P_stat != stateCreating && p.P_stat != stateZombie && p.P_flag&flagExiting == 0
}

func startMicros(p *unix.ExternProc) int64 {
	return p.P_starttime.Sec*1_000_000 + int64(p.P_starttime.Usec)
}

// member reports whether pid is sandboxed, denied the launch's tag, and
// allowed its control. On this host a zombie reads as denied both, and a
// missing path reads as denied for every sandboxed process, which is what
// the control check catches.
func (darwinHost) member(pid int, target proofTarget) (bool, error) {
	sandboxed, err := C.cordon_sandboxed(C.pid_t(pid))
	if gone, err := checkResult(sandboxed, err); gone || err != nil {
		return false, err
	}
	if sandboxed != 1 {
		return false, nil
	}
	tagDenied, err := metadataDenied(pid, target.tag)
	if err != nil || !tagDenied {
		return false, err
	}
	controlDenied, err := metadataDenied(pid, target.control)
	if err != nil {
		return false, err
	}
	return !controlDenied, nil
}

func metadataDenied(pid int, path string) (bool, error) {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	r, err := C.cordon_metadata_denied(C.pid_t(pid), cpath)
	if gone, err := checkResult(r, err); gone || err != nil {
		return false, err
	}
	return r == 1, nil
}

// checkResult reads a sandbox_check result: -1 with ESRCH means the process
// is gone, any other -1 is an error.
func checkResult(r C.int, err error) (gone bool, _ error) {
	if r != -1 {
		return false, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return true, nil
	}
	return false, fmt.Errorf("sandbox_check: %w", err)
}

func (darwinHost) kill(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}
