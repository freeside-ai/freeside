package inference

// AdjudicationRefusal identifies where the adjudicator refused a batch.
type AdjudicationRefusal string

const (
	AdjudicationInputRefused     AdjudicationRefusal = "input_refused"
	AdjudicationJudgmentFallback AdjudicationRefusal = "judgment_fallback"
	AdjudicationOutputRefused    AdjudicationRefusal = "output_refused"
)

var AllAdjudicationRefusals = []AdjudicationRefusal{
	AdjudicationInputRefused, AdjudicationJudgmentFallback, AdjudicationOutputRefused,
}

func (r AdjudicationRefusal) valid() bool {
	switch r {
	case AdjudicationInputRefused, AdjudicationJudgmentFallback, AdjudicationOutputRefused:
		return true
	default:
		return false
	}
}

// AdjudicationUnavailable preserves the fail-safe sentinel and fixed cause.
// The underlying validation error remains available to errors.Is/As, but is
// never included in Error because it can contain untrusted output.
type AdjudicationUnavailable struct {
	Cause    AdjudicationRefusal
	Fallback FallbackReason
	Err      error
}

func (e *AdjudicationUnavailable) Error() string { return ErrAdjudicationNotAvailable.Error() }
func (e *AdjudicationUnavailable) Unwrap() []error {
	if e.Err == nil {
		return []error{ErrAdjudicationNotAvailable}
	}
	return []error{ErrAdjudicationNotAvailable, e.Err}
}
