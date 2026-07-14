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
	ErrEmptyID                  = errors.New("required identifier is empty")
	ErrEmptyField               = errors.New("required field is empty")
	ErrNoActions                = errors.New("attention item offers no requested decision")
	ErrNonPositiveSeq           = errors.New("message sequence must be positive")
	ErrNonPositive              = errors.New("value must be positive")
	ErrParentKeyMismatch        = errors.New("child record's parent key does not match its enclosing record")
	ErrStatusMissingTimestamp   = errors.New("delivery status lacks its receipt timestamp")
	ErrStatusTimestampTooStrong = errors.New("delivery carries a receipt timestamp stronger than its status")
	ErrMissingTimestamp         = errors.New("required timestamp is zero")
	ErrTimestampOutOfOrder      = errors.New("delivery receipt timestamps are out of lifecycle order")
	ErrSubjectRunIDMismatch     = errors.New("subject type must not carry a run_id")
	ErrForeignDelivery          = errors.New("delivery belongs to a different item")
	ErrDuplicate                = errors.New("duplicate identity in a collection")
	ErrInconsistentTiming       = errors.New("timing summary is internally inconsistent")
	ErrNonContiguous            = errors.New("ordinals must be contiguous and increasing from one")
	ErrEvidenceHeadMismatch     = errors.New("evidence artifact head does not match the item's pr_head_sha")
	ErrArtifactIdentityConflict = errors.New("artifact id maps to conflicting digests or spans evidence and claims")
	ErrProvenanceInconsistent   = errors.New("provenance fields are internally inconsistent")

	// Trust-boundary failures.
	ErrAgentArtifactInEvidence     = errors.New("agent-produced artifact cannot enter evidence snapshot")
	ErrUnapprovedRecipe            = errors.New("artifact was not produced under an approved recipe")
	ErrMissingKeyProvenance        = errors.New("resolved-policy key lacks provenance")
	ErrPublishEligibleInconsistent = errors.New("publish_eligible is inconsistent with provenance")

	// Transition failures: how a persisted aggregate may change between its
	// stored version and an update (the transition validators). A writer maps
	// these onto its own conflict/stale-write errors at its boundary.
	ErrImmutableTransition = errors.New("an immutable field or recorded history would change")
	ErrStaleTransition     = errors.New("an update does not advance the aggregate's version or lifecycle")
)
