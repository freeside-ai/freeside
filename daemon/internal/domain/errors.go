package domain

import "errors"

// Sentinel validation errors. Validators wrap these with %w and context, so
// callers match a class with errors.Is without string comparison. Each names
// the invariant it guards.
var (
	// Enum-membership failures.
	ErrUnknownAttentionType             = errors.New("unknown attention type")
	ErrInvalidSubjectType               = errors.New("invalid subject type")
	ErrInvalidProducerClass             = errors.New("invalid producer class")
	ErrInvalidDeliveryStatus            = errors.New("invalid delivery status")
	ErrInvalidDeviceStatus              = errors.New("invalid device status")
	ErrInvalidCredentialKind            = errors.New("invalid device credential kind")
	ErrInvalidInterruptionClass         = errors.New("invalid interruption class")
	ErrInvalidAction                    = errors.New("invalid action")
	ErrInvalidPriority                  = errors.New("invalid priority")
	ErrInvalidItemStatus                = errors.New("invalid item status")
	ErrInvalidSensitivityClass          = errors.New("invalid sensitivity class")
	ErrInvalidHeadBinding               = errors.New("invalid head binding")
	ErrInvalidAuthor                    = errors.New("invalid author")
	ErrInvalidConversationStatus        = errors.New("invalid conversation status")
	ErrInvalidProvenanceSource          = errors.New("invalid provenance source")
	ErrInvalidPRExecutionMode           = errors.New("invalid pr execution mode")
	ErrInvalidAutomationChanges         = errors.New("invalid automation change policy")
	ErrInvalidTokenPermissions          = errors.New("invalid token permissions mode")
	ErrInvalidReviewMode                = errors.New("invalid review mode")
	ErrInvalidCommitPlanMode            = errors.New("invalid commit plan mode")
	ErrUnknownMessageRuleset            = errors.New("message ruleset is not in the built-in ruleset registry")
	ErrInvalidCommitPlanNotice          = errors.New("invalid commit plan notice reason")
	ErrInvalidFindingClass              = errors.New("invalid candidate finding class")
	ErrInvalidFindingCategory           = errors.New("invalid control-plane category")
	ErrInvalidFindingDisposition        = errors.New("invalid finding disposition")
	ErrInvalidFindingOrigin             = errors.New("invalid candidate finding origin")
	ErrInvalidOutcome                   = errors.New("invalid verification outcome")
	ErrInvalidRunnerCapability          = errors.New("invalid runner capability")
	ErrInvalidRunnerBackendClass        = errors.New("invalid runner backend class")
	ErrInvalidConformanceOutcome        = errors.New("invalid conformance outcome")
	ErrInvalidOperatingMode             = errors.New("invalid operating mode")
	ErrInvalidCredentialMode            = errors.New("invalid credential mode")
	ErrInvalidEgressProfile             = errors.New("invalid egress profile")
	ErrInvalidRefreshStrategy           = errors.New("invalid refresh strategy")
	ErrInvalidBackupHealthStatus        = errors.New("invalid backup health status")
	ErrInvalidExecOutcome               = errors.New("invalid execution outcome status")
	ErrInvalidRunMilestoneKind          = errors.New("invalid run milestone kind")
	ErrInvalidObservedStatus            = errors.New("invalid observed invocation status")
	ErrInvalidRunHoldReason             = errors.New("invalid run hold reason")
	ErrInvalidScheduleKind              = errors.New("invalid schedule kind")
	ErrInvalidScheduleStatus            = errors.New("invalid schedule status")
	ErrInvalidScheduleSubjectType       = errors.New("invalid schedule subject type")
	ErrInvalidScheduleResolution        = errors.New("invalid schedule resolution reason")
	ErrInvalidScheduleOccurrenceStatus  = errors.New("invalid schedule occurrence status")
	ErrInvalidScheduleOccurrenceOutcome = errors.New("invalid schedule occurrence outcome")

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
	ErrCapabilitiesNotCanonical = errors.New("capability snapshot is not in canonical (sorted, deduplicated) order")
	ErrImageNotDigestPinned     = errors.New("image reference is not pinned to a sha256 digest")
	ErrAuthIdentityInconsistent = errors.New("auth identity is inconsistent with the stage's egress profile")
	ErrExportBaseMismatch       = errors.New("observed export base does not match the admitted base")
	ErrStageInputsNotCanonical  = errors.New("stage input snapshot must use canonical digest and array forms")
	ErrOutcomeInconsistent      = errors.New("execution outcome fields are internally inconsistent")
	ErrMilestoneDetailMismatch  = errors.New("milestone detail fields do not match the kind's contract")
	ErrObservationInconsistent  = errors.New("invocation observation fields are internally inconsistent")
	ErrScheduleDetailMismatch   = errors.New("schedule detail fields do not match the kind's contract")

	// Trust-boundary failures.
	ErrPlaintextCredential               = errors.New("credential material must be a sha256 digest, never plaintext")
	ErrAgentArtifactInEvidence           = errors.New("agent-produced artifact cannot enter evidence snapshot")
	ErrUnapprovedRecipe                  = errors.New("artifact was not produced under an approved recipe")
	ErrMissingKeyProvenance              = errors.New("resolved-policy key lacks provenance")
	ErrPublishEligibleInconsistent       = errors.New("publish_eligible is inconsistent with provenance")
	ErrPolicyDigestMismatch              = errors.New("resolved-policy digest does not match its content")
	ErrKeysNotCanonical                  = errors.New("resolved-policy keys are not in canonical (key-sorted) order")
	ErrProfileDigestMismatch             = errors.New("trust-profile digest does not match its content")
	ErrWorkflowAuditEvidenceInvalid      = errors.New("workflow-audit evidence is invalid")
	ErrWorkflowAuditEvidenceTooLarge     = errors.New("workflow-audit evidence exceeds the storage limit")
	ErrWorkflowAuditEvidenceMismatch     = errors.New("workflow-audit evidence does not match its audit binding")
	ErrAuthorizationInconsistent         = errors.New("candidate authorization id or authorizes_publication does not match its content")
	ErrTrustProfileDrift                 = errors.New("observed automation authority drifted from the approved trust profile")
	ErrAdmissionInconsistent             = errors.New("execution admission id does not match its content")
	ErrUnknownAdmissionFloor             = errors.New("no capability floor is configured for the admission's operating mode")
	ErrCapabilityBelowFloor              = errors.New("admitted capability class is below the current policy floor")
	ErrCredentialModeNotApproved         = errors.New("unattended admission runs under a credential mode policy has not approved")
	ErrWaiverNotConfigured               = errors.New("admission claims a backup encryption waiver the operator has not configured")
	ErrWaiverRepositoryMismatch          = errors.New("admission targets a repository the backup encryption waiver does not cover")
	ErrWaiverModeMismatch                = errors.New("backup encryption waiver claimed outside unattended running")
	ErrTrustProfileInconsistent          = errors.New("trust profile digest is inconsistent with the admission's operating mode")
	ErrTrustProfileSuperseded            = errors.New("admission names a trust profile revision that is no longer active")
	ErrBackupAuthorizationMissing        = errors.New("unattended admission presents no backup authorization")
	ErrBackupEncryptionWaiverUnsupported = errors.New(
		"backup encryption waiver is unsupported by this encrypted-checkpoint build")
	ErrBackupHealthUnavailable    = errors.New("backup health is unavailable")
	ErrCheckpointNotEncrypted     = errors.New("backup checkpoint is not encrypted")
	ErrCheckpointAuthentication   = errors.New("backup checkpoint authentication failed")
	ErrCheckpointDigestMismatch   = errors.New("backup checkpoint digest does not match its content")
	ErrCheckpointNotCurrent       = errors.New("backup checkpoint is not current")
	ErrArtifactClosureIncomplete  = errors.New("backup artifact closure is incomplete")
	ErrRestoreTestStale           = errors.New("backup restore test is stale")
	ErrRepositoryIdentityMismatch = errors.New("recorded repository identity does not match the repository's trusted profile")
	// ErrPathBoundaryMismatch is a current-configuration verdict, not record
	// corruption: the run's durable declared paths and the containment
	// boundary this runner is configured to enforce disagree, which a
	// reconfigured daemon resolves without touching the recorded attempt.
	ErrPathBoundaryMismatch     = errors.New("resolved run policy's declared paths disagree with the configured containment boundary")
	ErrStageInputDigestMismatch = errors.New("stage input snapshot digest does not match its content")
	ErrNonWaivableFinding       = errors.New("finding class is non-waivable")
	ErrAgentWaiver              = errors.New("an agent cannot author a waiver")

	// Backend-conformance failures (issues #327, #320).
	ErrConformanceOverclaim = errors.New(
		"conformance record claims capabilities beyond the backend class's provable ceiling")
	ErrConformanceCapabilitiesWithoutPass = errors.New(
		"only a passed conformance record can carry a proven capability set")
	ErrConformanceConfigurationUnbound = errors.New(
		"conformance record is not bound to a backend configuration")
	ErrAdmissionExceedsConformance = errors.New(
		"admission capability snapshot exceeds the backend's proven conformance declaration")
	ErrAdmissionConfigurationMismatch = errors.New(
		"admission backend configuration differs from the current conformance proof")

	// Unattended operating-state and §4 blocking failures (issues #319, #321).
	ErrInvalidUnattendedOperationState = errors.New("unknown unattended operation state")
	ErrInvalidSupersessionKind         = errors.New("unknown blocking supersession kind")
	ErrSupersessionOutsideSystemHealth = errors.New("blocking supersession is a system_health semantic")
	ErrUnattendedOperationStopped      = errors.New("unattended operation is stopped by operator decision")
	ErrBlockingSystemHealth            = errors.New("an open system_health item blocks unattended admission")
	ErrTransitionUnbacked              = errors.New("operating transition names no accepted command")
	ErrTransitionCommandMismatch       = errors.New("operating transition disagrees with its accepted command")

	// Transition failures: how a persisted aggregate may change between its
	// stored version and an update (the transition validators). A writer maps
	// these onto its own conflict/stale-write errors at its boundary.
	ErrImmutableTransition = errors.New("an immutable field or recorded history would change")
	ErrStaleTransition     = errors.New("an update does not advance the aggregate's version or lifecycle")
)
