package domain

import "errors"

// Sentinel validation errors. Validators wrap these with %w and context, so
// callers match a class with errors.Is without string comparison. Each names
// the invariant it guards.
var (
	// Enum-membership failures.
	ErrUnknownAttentionType     = errors.New("unknown attention type")
	ErrInvalidSubjectType       = errors.New("invalid subject type")
	ErrInvalidProducerClass     = errors.New("invalid producer class")
	ErrInvalidDeliveryStatus    = errors.New("invalid delivery status")
	ErrInvalidInterruptionClass = errors.New("invalid interruption class")
	ErrInvalidAction            = errors.New("invalid action")
	ErrInvalidPriority          = errors.New("invalid priority")
	ErrInvalidItemStatus        = errors.New("invalid item status")
	ErrInvalidSensitivityClass  = errors.New("invalid sensitivity class")
	ErrInvalidAuthor            = errors.New("invalid author")
	ErrInvalidProvenanceSource  = errors.New("invalid provenance source")

	// Structural failures.
	ErrEmptyID        = errors.New("required identifier is empty")
	ErrEmptyField     = errors.New("required field is empty")
	ErrNoActions      = errors.New("attention item offers no requested decision")
	ErrNonPositiveSeq = errors.New("message sequence must be positive")

	// Trust-boundary failures.
	ErrAgentArtifactInEvidence = errors.New("agent-produced artifact cannot enter evidence snapshot")
	ErrUnapprovedRecipe        = errors.New("artifact was not produced under an approved recipe")
	ErrMissingKeyProvenance    = errors.New("resolved-policy key lacks provenance")
)
