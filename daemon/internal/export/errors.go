package export

import "errors"

// Sentinel validation errors. Validators wrap these with %w and context, so
// callers match a class with errors.Is without string comparison. Each names
// the invariant it guards.
var (
	// Manifest-shape failures.
	ErrUnknownManifestVersion = errors.New("unknown manifest version")
	ErrInvalidEntryKind       = errors.New("invalid entry kind")
	ErrEntriesNotCanonical    = errors.New("entries are not in canonical (path-sorted, deduplicated) order")

	// Per-entry failures.
	ErrInvalidPath       = errors.New("path is not a canonical relative slash-separated path")
	ErrInvalidPathHex    = errors.New("path_hex is not lowercase hex of a non-representable name")
	ErrKindFieldMismatch = errors.New("entry carries a field its kind forbids or lacks one it requires")
	ErrInvalidMode       = errors.New("mode is not valid for the entry kind")
	ErrNegativeSize      = errors.New("size must be non-negative")
	ErrInvalidDigest     = errors.New("digest is not a sha256 content address")

	// Export failures.
	ErrWorkspaceChanged = errors.New("workspace content changed during export")
	ErrTooManyEntries   = errors.New("workspace exceeds the manifest entry cap")
	ErrOutputNotEmpty   = errors.New("output directory is not empty")
)
