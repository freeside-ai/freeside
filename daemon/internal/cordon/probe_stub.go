//go:build !darwin || !cgo

package cordon

import "errors"

// The probe's sandbox and bootstrap queries exist only on macOS with cgo.
// Off it every check fails, and no launch runs there to probe anyway.
var errProbeUnsupported = errors.New("the cordon probe needs macOS and cgo")

const (
	bootstrapNotPrivileged  = 1100
	bootstrapUnknownService = 1102
)

func sandboxDeniesOperation(string) (bool, error) { return false, errProbeUnsupported }

func sandboxDeniesService(string) (bool, error) { return false, errProbeUnsupported }

func lookUpService(string) int { return -1 }

func processStart(int) (int64, error) { return 0, errProbeUnsupported }
