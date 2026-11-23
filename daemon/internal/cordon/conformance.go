package cordon

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ErrConformance is the class sentinel for a denial the cordon could not
// prove. ConformanceFailure unwraps to it, so errors.Is matches the class
// while errors.As reaches the check.
var ErrConformance = errors.New("cordon: denial unproven")

// ErrConformanceRecord marks a pass whose durable record could not be
// written. It is the one conformance failure the daemon stops on: a pass
// that fails only leaves the class undeclared, but a record that can't be
// written leaves the durable state unknown.
var ErrConformanceRecord = errors.New("cordon: conformance record not written")

// ConformanceFailure is the typed failure of one check: a probe check that
// failed, a report that didn't prove its checks, a positive control that
// failed outside the launch, a precondition, or an effect the suite looked
// for after the probe.
type ConformanceFailure struct {
	Check  string
	Reason string
}

func (e *ConformanceFailure) Error() string {
	return fmt.Sprintf("cordon: %s unproven: %s", e.Check, e.Reason)
}

// Unwrap makes errors.Is(err, ErrConformance) match the class.
func (e *ConformanceFailure) Unwrap() error { return ErrConformance }

func failure(check, format string, args ...any) error {
	return &ConformanceFailure{Check: check, Reason: fmt.Sprintf(format, args...)}
}

// ConformanceRecorder persists a pass's BackendConformance record. Its
// method set is the ward's, so the daemon's store-backed recorder serves
// every backend; cordon defines it rather than importing the store.
type ConformanceRecorder interface {
	RecordBackendConformance(ctx context.Context, record domain.BackendConformance) error
}
