//go:build !darwin || !cgo

package cordonhost

import "errors"

// preventIdleSleep refuses: a power assertion needs macOS and cgo, which
// every cordoned launch needs too.
func preventIdleSleep() (func() error, error) {
	return nil, errors.New("a power assertion needs macOS with cgo")
}
