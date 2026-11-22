//go:build !darwin || !cgo

package cordon

import "fmt"

// newHostProcesses has no way to find a launch's processes off macOS or
// without cgo, since sandbox_check has no Go binding. Launch refuses before
// it starts anything, so no launch runs that can't be proven ended.
func newHostProcesses() (hostProcesses, error) {
	return nil, fmt.Errorf("%w: the run-end proof needs macOS and cgo", ErrRunEndUnproven)
}
