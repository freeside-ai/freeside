package domain

// InterpretationFailureCause is why the engine could not carry a completed,
// exported invocation's output forward. It is a separate vocabulary from
// FailureCause: a FailureCause rides on an ExecutionOutcome, and the store
// refuses an outcome beside an export, so a failure found while reading an
// export's output has no outcome to carry one. There is no unknown member:
// every writer names its cause at the call site.
type InterpretationFailureCause string

const (
	// InterpretationFailureResearchURLRefused: a requested research URL, or a
	// redirect it led to, failed the fetch policy.
	InterpretationFailureResearchURLRefused InterpretationFailureCause = "research_url_refused"
	// InterpretationFailureResearchTooLarge: a research response exceeded the
	// fetch size bound.
	InterpretationFailureResearchTooLarge InterpretationFailureCause = "research_too_large"
	// InterpretationFailureResearchFetchFailed: a research fetch failed for
	// any other reason.
	InterpretationFailureResearchFetchFailed InterpretationFailureCause = "research_fetch_failed"
	// InterpretationFailureIterationsExhausted: the stage's iteration budget
	// ran out before the output settled.
	InterpretationFailureIterationsExhausted InterpretationFailureCause = "iterations_exhausted"
	// InterpretationFailureInvalidOutput: the exported output was read and
	// rejected.
	InterpretationFailureInvalidOutput InterpretationFailureCause = "invalid_output"
	// InterpretationFailureTranscriptUnreadable: the exported output could
	// not be read at all.
	InterpretationFailureTranscriptUnreadable InterpretationFailureCause = "transcript_unreadable"
	// InterpretationFailureInputUndeliverable: the output was accepted, but
	// the next input it calls for could not be delivered.
	InterpretationFailureInputUndeliverable InterpretationFailureCause = "input_undeliverable"
)

// AllInterpretationFailureCauses lists every valid InterpretationFailureCause;
// it is the single registration point.
var AllInterpretationFailureCauses = []InterpretationFailureCause{
	InterpretationFailureResearchURLRefused,
	InterpretationFailureResearchTooLarge,
	InterpretationFailureResearchFetchFailed,
	InterpretationFailureIterationsExhausted,
	InterpretationFailureInvalidOutput,
	InterpretationFailureTranscriptUnreadable,
	InterpretationFailureInputUndeliverable,
}

func (c InterpretationFailureCause) valid() bool {
	switch c {
	case InterpretationFailureResearchURLRefused, InterpretationFailureResearchTooLarge,
		InterpretationFailureResearchFetchFailed, InterpretationFailureIterationsExhausted,
		InterpretationFailureInvalidOutput, InterpretationFailureTranscriptUnreadable,
		InterpretationFailureInputUndeliverable:
		return true
	default:
		return false
	}
}
