package domain

import "errors"

// Sentinel validation errors. Validators wrap these with %w and context, so
// callers match a class with errors.Is without string comparison. Each names
// the invariant it guards.
var (
	// Enum-membership failures.
	ErrUnknownAttentionType      = errors.New("unknown attention type")
	ErrInvalidSubjectType        = errors.New("invalid subject type")
	ErrInvalidProducerClass      = errors.New("invalid producer class")
	ErrInvalidDeliveryStatus     = errors.New("invalid delivery status")
	ErrInvalidDeviceStatus       = errors.New("invalid device status")
	ErrInvalidCredentialKind     = errors.New("invalid device credential kind")
	ErrInvalidInterruptionClass  = errors.New("invalid interruption class")
	ErrInvalidAction             = errors.New("invalid action")
	ErrInvalidPriority           = errors.New("invalid priority")
	ErrInvalidItemStatus         = errors.New("invalid item status")
	ErrInvalidSensitivityClass   = errors.New("invalid sensitivity class")
	ErrInvalidHeadBinding        = errors.New("invalid head binding")
	ErrInvalidAuthor             = errors.New("invalid author")
	ErrInvalidConversationStatus = errors.New("invalid conversation status")
	ErrInvalidProvenanceSource   = errors.New("invalid provenance source")
	ErrInvalidPRExecutionMode    = errors.New("invalid pr execution mode")
	ErrInvalidAutomationChanges  = errors.New("invalid automation change policy")
	ErrInvalidTokenPermissions   = errors.New("invalid token permissions mode")
	ErrInvalidReviewMode         = errors.New("invalid review mode")
	ErrInvalidCommitPlanMode     = errors.New("invalid commit plan mode")
	ErrUnknownMessageRuleset     = errors.New("message ruleset is not in the built-in ruleset registry")
	ErrInvalidCommitPlanNotice   = errors.New("invalid commit plan notice reason")
	ErrInvalidFindingClass       = errors.New("invalid candidate finding class")
	ErrInvalidFindingCategory    = errors.New("invalid control-plane category")
	ErrInvalidFindingDisposition = errors.New("invalid finding disposition")
	ErrInvalidFindingOrigin      = errors.New("invalid candidate finding origin")
	ErrInvalidOutcome            = errors.New("invalid verification outcome")

	// Structural failures.
	ErrEmptyID    = errors.New("required identifier is empty")
	ErrEmptyField = errors.New("required field is empty")
	// ErrNoActions is raised by signet's per-type action policy, not by
	// structural validation: an empty requested_decision is structurally valid
	// (the read-only blocked type offers none, plan §4).
	ErrNoActions                = errors.New("attention item offers no requested decision")
	ErrNonPositiveSeq           = errors.New("message sequence must be positive")
	ErrNonPositive              = errors.New("value must be positive")
	ErrParentKeyMismatch        = errors.New("child record's parent key does not match its enclosing record")
	ErrStatusMissingTimestamp   = errors.New("status lacks its corresponding timestamp")
	ErrStatusTimestampTooStrong = errors.New("record carries a timestamp stronger than its status")
	ErrMissingTimestamp         = errors.New("required timestamp is zero")
	ErrTimestampOutOfOrder      = errors.New("timestamps are out of lifecycle order")
	ErrConsumptionInconsistent  = errors.New("pairing code consumption fields are internally inconsistent")
	ErrSubjectRunIDMismatch     = errors.New("subject type must not carry a run_id")
	ErrForeignDelivery          = errors.New("delivery belongs to a different item")
	ErrDuplicate                = errors.New("duplicate identity in a collection")
	ErrInconsistentTiming       = errors.New("timing summary is internally inconsistent")
	ErrNonContiguous            = errors.New("ordinals must be contiguous and increasing from one")
	ErrEvidenceHeadMismatch     = errors.New("evidence artifact head does not match the item's pr_head_sha")
	ErrArtifactIdentityConflict = errors.New("artifact id maps to conflicting digests or spans evidence and claims")
	ErrProvenanceInconsistent   = errors.New("provenance fields are internally inconsistent")
	ErrNonAgentClaim            = errors.New("agent claim provenance must carry the agent producer class")
	ErrInvalidClaimMediaType    = errors.New("claim text media type is not a registered ClaimMediaType")
	ErrClaimTextNotUTF8         = errors.New("claim text content is not valid UTF-8")
	ErrClaimTextTooLarge        = errors.New("claim text content exceeds the inline size cap")
	ErrClaimTextDigestMismatch  = errors.New("claim digest does not match its text content")
	ErrHighSensitivityClaimText = errors.New("high-sensitivity claim content cannot be carried inline")
	ErrBindingMismatch          = errors.New("artifact_digests does not equal the item's rendered evidence and claim digests")
	ErrDigestsNotCanonical      = errors.New("artifact digests are not in canonical (sorted, deduplicated) order")
	ErrUnboundInvocation        = errors.New("agent invocation binds neither input artifacts nor a conversation prefix")
	ErrInvocationInconsistent   = errors.New("agent invocation conversation-binding fields are internally inconsistent")
	ErrPatternsNotCanonical     = errors.New("protected-path patterns are not in canonical (sorted, deduplicated) order")
	ErrFindingsNotCanonical     = errors.New("candidate findings are not in canonical (encoding-sorted) order")
	ErrTimestampNotUTC          = errors.New("identity-bearing timestamp must be UTC")
	ErrCategoryInconsistent     = errors.New("control-plane category is required exactly for control-plane findings")
	ErrWaiverInconsistent       = errors.New("waiver record is required exactly for waived findings")
	ErrFindingPathConflict      = errors.New("finding carries both path and path_hex")

	// Trust-boundary failures.
	ErrPlaintextCredential         = errors.New("credential material must be a sha256 digest, never plaintext")
	ErrAgentArtifactInEvidence     = errors.New("agent-produced artifact cannot enter evidence snapshot")
	ErrUnapprovedRecipe            = errors.New("artifact was not produced under an approved recipe")
	ErrMissingKeyProvenance        = errors.New("resolved-policy key lacks provenance")
	ErrPublishEligibleInconsistent = errors.New("publish_eligible is inconsistent with provenance")
	ErrPolicyDigestMismatch        = errors.New("resolved-policy digest does not match its content")
	ErrKeysNotCanonical            = errors.New("resolved-policy keys are not in canonical (key-sorted) order")
	ErrProfileDigestMismatch       = errors.New("trust-profile digest does not match its content")
	ErrAuthorizationInconsistent   = errors.New("candidate authorization id or authorizes_publication does not match its content")
	ErrTrustProfileDrift           = errors.New("observed automation authority drifted from the approved trust profile")
	ErrNonWaivableFinding          = errors.New("finding class is non-waivable")
	ErrAgentWaiver                 = errors.New("an agent cannot author a waiver")

	// Transition failures: how a persisted aggregate may change between its
	// stored version and an update (the transition validators). A writer maps
	// these onto its own conflict/stale-write errors at its boundary.
	ErrImmutableTransition = errors.New("an immutable field or recorded history would change")
	ErrStaleTransition     = errors.New("an update does not advance the aggregate's version or lifecycle")
)
