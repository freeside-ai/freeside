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
	// ErrHeadMismatch rejects a verification whose requested candidate
	// head the checkout does not hold exactly; verification output binds
	// to one head, so a mismatch fails closed before any command runs.
	ErrHeadMismatch = errors.New("checkout does not hold the requested candidate head")
	// ErrBaseMismatch rejects an enforced base the checkout does not
	// hold as exactly that commit: the report claims base_sha as the
	// trusted base, and the base-commit recipe source reads from it, so
	// a tree-ish that is not the named commit fails closed.
	ErrBaseMismatch = errors.New("checkout does not hold the enforced base commit")
	// ErrWorkspaceMismatch rejects a materialized workspace whose bytes
	// are not exactly the head tree's: a conversion or stray file means
	// the recipe would verify content other than the bound head.
	ErrWorkspaceMismatch = errors.New("materialized workspace does not match the head tree")
	// ErrMalformedTree rejects a head tree that cannot be safely
	// materialized: an entry nested under another entry (a blob under a
	// symlink) would escape the workspace on write.
	ErrMalformedTree = errors.New("head tree is malformed")

	// Verify invocation failures.
	ErrInvalidOptions = errors.New("verify options are invalid")
	// ErrSymlinkEntrypoint rejects a trusted recipe whose command
	// entrypoint is a symlink in the candidate head: exec would follow
	// it to a target the recorded path does not name, so the recipe must
	// name the target file directly.
	ErrSymlinkEntrypoint = errors.New("recipe command entrypoint is a symlink")
)
