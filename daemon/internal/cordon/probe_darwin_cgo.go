//go:build darwin && cgo

package cordon

/*
#include <errno.h>
#include <stdlib.h>
#include <unistd.h>
#include <mach/mach.h>
#include <servers/bootstrap.h>

extern int sandbox_check(pid_t pid, const char *operation, int type, ...);

enum {
	cordon_probe_filter_none = 0,
	cordon_probe_filter_global_name = 2,
	cordon_probe_no_report = 0x40000000,
};

// Each asks about this process: whether its rules deny the operation, with
// the filter its shape takes. They log no violation.
static int cordon_probe_denies(const char *op) {
	return sandbox_check(getpid(), op, cordon_probe_filter_none | cordon_probe_no_report);
}

static int cordon_probe_denies_service(const char *name) {
	return sandbox_check(getpid(), "mach-lookup", cordon_probe_filter_global_name | cordon_probe_no_report, name);
}

// cordon_probe_look_up returns the bootstrap server's answer and drops any
// port it gave.
static int cordon_probe_look_up(const char *name) {
	mach_port_t port = MACH_PORT_NULL;
	kern_return_t kr = bootstrap_look_up(bootstrap_port, name, &port);
	if (kr == KERN_SUCCESS && port != MACH_PORT_NULL) {
		mach_port_deallocate(mach_task_self(), port);
	}
	return kr;
}
*/
import "C"

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The bootstrap server's refusals, from <servers/bootstrap.h>: a lookup the
// sandbox denies, and a name nothing registered.
const (
	bootstrapNotPrivileged  = 1100
	bootstrapUnknownService = 1102
)

func sandboxAnswer(r C.int, err error) (bool, error) {
	if r == -1 {
		return false, fmt.Errorf("sandbox_check: %w", err)
	}
	return r == 1, nil
}

func sandboxDeniesOperation(op string) (bool, error) {
	cop := C.CString(op)
	defer C.free(unsafe.Pointer(cop))
	r, err := C.cordon_probe_denies(cop)
	return sandboxAnswer(r, err)
}

func sandboxDeniesService(name string) (bool, error) {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	r, err := C.cordon_probe_denies_service(cname)
	return sandboxAnswer(r, err)
}

func lookUpService(name string) int {
	cname := C.CString(name)
	defer C.free(unsafe.Pointer(cname))
	return int(C.cordon_probe_look_up(cname))
}

// processStart is pid's start time in microseconds, the run-end proof's
// identity for a process.
func processStart(pid int) (int64, error) {
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, fmt.Errorf("read process %d: %w", pid, err)
	}
	if int(k.Proc.P_pid) != pid {
		return 0, fmt.Errorf("process %d is gone", pid)
	}
	return startMicros(&k.Proc), nil
}
