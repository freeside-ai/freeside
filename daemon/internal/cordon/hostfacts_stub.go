//go:build !darwin || !cgo

package cordon

import "errors"

// Off macOS with cgo the approvals can't be read, so every launch refuses,
// as the run-end proof already makes it.
func readHostFacts() (HostFacts, error) {
	return HostFacts{}, errors.New("the launcher's approvals can be read only on macOS with cgo")
}
