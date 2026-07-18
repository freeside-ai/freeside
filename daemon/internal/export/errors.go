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

	// Evidence-manifest failures (the second workspace-exit channel).
	ErrUnknownEvidenceVersion  = errors.New("unknown evidence manifest version")
	ErrEvidenceNotCanonical    = errors.New("evidence entries are not in canonical (label-sorted, deduplicated) order")
	ErrInvalidLabel            = errors.New("label is not a non-empty NUL-free UTF-8 string")
	ErrInvalidMediaType        = errors.New("media_type is empty")
	ErrInvalidEvidenceProducer = errors.New("producer_class is not the agent class this channel admits")
	ErrEmptyInvocationID       = errors.New("producer_invocation_id is empty")
	ErrInvalidHeadBinding      = errors.New("head_binding is not an explicit binding mode")
	ErrProvenanceInconsistent  = errors.New("source_head_sha contradicts the head_binding mode")
	ErrInvalidSensitivityClass = errors.New("sensitivity_class is not a valid confidentiality tier")
	ErrTrailingContent         = errors.New("manifest carries trailing content")
	ErrInvalidUTF8             = errors.New("manifest bytes are not valid UTF-8")
	ErrNotCanonicalEncoding    = errors.New("manifest bytes are not the canonical encoded form")
	ErrNullEntries             = errors.New("entries must be a non-null array")

	// Export failures.
	ErrWorkspaceChanged = errors.New("workspace content changed during export")
	ErrTooManyEntries   = errors.New("workspace exceeds the manifest entry cap")
	ErrOutputNotEmpty   = errors.New("output directory is not empty")
)
