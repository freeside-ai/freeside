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
)
