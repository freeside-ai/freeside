package verify

import "errors"

// Sentinel fail-closed errors. Validators wrap these with %w and
// context, so callers match a class with errors.Is without string
// comparison. Each names an integrity invariant; policy outcomes are
// Findings on the Result, never errors.
var (
	// ErrRecipeInvalid rejects trusted recipe bytes that do not parse as
	// a well-formed recipe. The recipe is trusted input (approved config
	// or the trusted base commit), so a malformed one is a configuration
	// bug that must fail loud, never a policy finding.
	ErrRecipeInvalid = errors.New("recipe failed validation")
	// ErrRecipeUnreadable rejects a trusted recipe source that cannot be
	// resolved to readable bytes; there is no fallback to candidate
	// content.
	ErrRecipeUnreadable = errors.New("trusted recipe cannot be read")

	// Checkout failures.
	ErrGitPlumbing     = errors.New("git plumbing failed")
	ErrUnsupportedRepo = errors.New("checkout repository is not supported")

	// Verify invocation failures.
	ErrInvalidOptions = errors.New("verify options are invalid")
)
