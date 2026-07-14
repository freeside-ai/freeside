package exec

// Status is the lifecycle state of an external invocation, shared by
// StageDriver.Inspect and ReviewSource.Inspect: both drive the same
// reconciliation loop, so one vocabulary avoids duplicate mapping in every
// caller. Review-specific meaning (findings vs clean pass) lives in
// ReviewResult, not in status. Named string per the domain enum convention:
// the JSON token is the human-readable string and the zero value "" is
// invalid by design.
type Status string

const (
	// StatusPending means the invocation intent is committed but execution
	// has not started.
	StatusPending Status = "pending"
	// StatusRunning means the invocation is executing.
	StatusRunning Status = "running"
	// StatusCompleted means the invocation finished and committed a
	// successful result.
	StatusCompleted Status = "completed"
	// StatusFailed means the invocation finished and committed a failed
	// result.
	StatusFailed Status = "failed"
	// StatusCanceled means the invocation was canceled and committed a
	// canceled result.
	StatusCanceled Status = "canceled"
	// StatusGone means the provider-side session is lost (crash, eviction):
	// Inspect can observe nothing more, but a result committed before the
	// loss remains collectable by invocation id (plan §5.3 reconciliation).
	// Gone is not a terminal outcome; the caller resolves it through
	// Collect/Poll (a committed result, or ErrNoResult).
	StatusGone Status = "gone"
)

// AllStatuses lists every valid Status; it drives table-driven tests and is
// the single place a new status is registered.
var AllStatuses = []Status{
	StatusPending,
	StatusRunning,
	StatusCompleted,
	StatusFailed,
	StatusCanceled,
	StatusGone,
}

func (s Status) valid() bool {
	switch s {
	case StatusPending, StatusRunning, StatusCompleted, StatusFailed,
		StatusCanceled, StatusGone:
		return true
	default:
		return false
	}
}

// Terminal reports whether s is a final outcome a result can carry
// (completed, failed, or canceled). Dispatch switch without default so the
// exhaustive linter forces a new member to be classified.
func (s Status) Terminal() bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusCanceled:
		return true
	case StatusPending, StatusRunning, StatusGone:
		return false
	}
	return false // invalid zero value
}
