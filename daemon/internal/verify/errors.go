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
	// ErrCommitFileUnreadable rejects a path a commit's tree holds in a
	// shape ReadFileAtCommit refuses: not a regular blob, or one beyond the
	// caller's size limit. It is distinct from absence, which is a result.
	ErrCommitFileUnreadable = errors.New("file at commit is not a regular blob within the size limit")

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
	// ErrEvaluatedMismatch rejects an evaluated commit that is not the
	// merge of the candidate head into the enforced base: the report
	// claims its tree's result for that head on that base, so any other
	// commit fails closed before any command runs.
	ErrEvaluatedMismatch = errors.New("evaluated commit is not the merge of the head into the base")
	// ErrWorkspaceMismatch rejects a materialized workspace whose bytes
	// are not exactly the head tree's: a conversion or stray file means
	// the recipe would verify content other than the bound head.
	ErrWorkspaceMismatch = errors.New("materialized workspace does not match the head tree")
	// ErrMalformedTree rejects a tree that cannot be safely materialized
	// or read: an entry nested under another entry (a blob under a
	// symlink) would escape the workspace on write, and a blob or symlink
	// entry that names an object other than a blob (a tag, a tree), or a
	// blob other than the size its listing gives, is refused before its
	// bytes are used.
	ErrMalformedTree = errors.New("head tree is malformed")

	// Verify invocation failures.
	ErrInvalidOptions = errors.New("verify options are invalid")
	// ErrSymlinkEntrypoint rejects a trusted recipe whose command
	// entrypoint is a symlink in the candidate head: exec would follow
	// it to a target the recorded path does not name, so the recipe must
	// name the target file directly.
	ErrSymlinkEntrypoint = errors.New("recipe command entrypoint is a symlink")
	// ErrRoomUnavailable rejects a trusted recipe that names an execution
	// class the caller supplied no room for. The caller chose the rooms,
	// so this is an integrity failure, never a finding: the verification
	// does not start, and no command runs in another class's room.
	ErrRoomUnavailable = errors.New("recipe names an execution class with no room")
	// ErrHistoryUnavailable rejects a verification whose recipe asks for
	// history the verifier can't give exactly: the head and the enforced
	// base share no single merge base, or the verified tree holds a path
	// component the written .git would collide with. Nothing runs. The
	// path refusal also wraps ErrMalformedTree, since no retry gets past it.
	ErrHistoryUnavailable = errors.New("candidate history cannot be given to a recipe command")
)
