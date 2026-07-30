package exec

import "errors"

// Sentinel errors. Drivers and validators wrap these with %w and context, so
// callers match a class with errors.Is without string comparison (the domain
// package's convention). Each names the invariant it guards.
var (
	// ErrUnknownInvocation: the invocation id was never started here.
	ErrUnknownInvocation = errors.New("no invocation with this id")
	// ErrDuplicateStart: the invocation id already carries a committed
	// intent; starting is once per id (plan §5.3, one committed intent).
	ErrDuplicateStart = errors.New("invocation id already started")
	// ErrResultNotReady: the invocation has not committed a terminal result
	// yet; poll again.
	ErrResultNotReady = errors.New("invocation has no committed result yet")
	// ErrNoResult: the invocation ended (session lost) without ever
	// committing a result; there is nothing to recover.
	ErrNoResult = errors.New("invocation ended without a committed result")
	// ErrStaleHead: the committed review result ran against a different head
	// than the caller expects (freshness, §5.3 verify).
	ErrStaleHead = errors.New("result head does not match expected head")
	// ErrInvalidStatus: a status token is not a member of the vocabulary.
	ErrInvalidStatus = errors.New("invalid invocation status")
	// ErrNonTerminalResult: a committed result must carry a terminal status.
	ErrNonTerminalResult = errors.New("result status is not terminal")
	// ErrInputSourceMissing: no content-addressed store was supplied.
	ErrInputSourceMissing = errors.New("stage input source is missing")
	// ErrInputBodyMissing: a source reported success without returning a body.
	ErrInputBodyMissing = errors.New("stage input source returned no body")
	// ErrInputLimitInvalid: a materialization byte limit is not positive.
	ErrInputLimitInvalid = errors.New("stage input byte limit is invalid")
	// ErrStageInputsMissing: a legacy admission has no materializable snapshot.
	ErrStageInputsMissing = errors.New("stage input snapshot is missing")
	// ErrInputTooLarge: one input or the aggregate exceeds its configured cap.
	ErrInputTooLarge = errors.New("stage input exceeds materialization limit")
	// ErrInputDigestMismatch: resolved bytes do not match the admitted digest.
	ErrInputDigestMismatch = errors.New("stage input content does not match its admitted digest")
	// ErrInputDigestInvalid: an admitted input is not a canonical sha256
	// content address and therefore cannot be passed to a source.
	ErrInputDigestInvalid = errors.New("stage input digest is not canonical sha256")
	// ErrInputUnavailable: input materialization hit operational I/O or
	// cancellation before a process started. The durable outbox intent remains
	// pending and may be retried; missing or corrupt admitted bytes use their
	// permanent integrity classes instead.
	ErrInputUnavailable = errors.New("stage input is temporarily unavailable")
	// ErrMaterializerMissing: the production driver adapter has no input
	// materializer.
	ErrMaterializerMissing = errors.New("stage input materializer is missing")
	// ErrMaterializedDriverMissing: the adapter has no process-facing driver.
	ErrMaterializedDriverMissing = errors.New("materialized stage driver is missing")
	// ErrPreJobRefused: the backend's lightweight pre-job gate failed before
	// process start. The outbox intent remains pending for a later healthy
	// reconcile pass; this operating-state refusal must not stop the daemon.
	ErrPreJobRefused = errors.New("backend pre-job gate refused dispatch")
)
