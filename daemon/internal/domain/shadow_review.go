package domain

import (
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// ShadowReviewSource names a review source admitted only for shadow evidence.
// The zero value "" is invalid by design. This vocabulary is deliberately
// separate from ReviewMode: a shadow pass is recorded, but never routed, never
// satisfies the review requirement, and never advances review rounds (plan
// §5.3, §7).
type ShadowReviewSource string

// ShadowReviewClaudeLocal is the fresh-context local Claude shadow reviewer.
const ShadowReviewClaudeLocal ShadowReviewSource = "claude_local"

// AllShadowReviewSources is the single registration point for shadow sources.
var AllShadowReviewSources = []ShadowReviewSource{ShadowReviewClaudeLocal}

func (s ShadowReviewSource) valid() bool {
	switch s {
	case ShadowReviewClaudeLocal:
		return true
	default:
		return false
	}
}

// ValidateShadowReviewFinding re-applies the registered source's normalized
// output schema at persistence boundaries. Finding.Validate stays permissive
// enough for native review observations; a shadow result must preserve the
// stricter review-source contract that produced its classifier evidence.
func ValidateShadowReviewFinding(source ShadowReviewSource, finding Finding) error {
	if !source.valid() {
		return fmt.Errorf("shadow review finding source %q: %w", source, ErrInvalidShadowReviewSource)
	}
	if err := finding.Validate(); err != nil {
		return err
	}
	if finding.Source != string(source) {
		return fmt.Errorf("shadow review finding source %q: %w", finding.Source, ErrParentKeyMismatch)
	}
	switch source {
	case ShadowReviewClaudeLocal:
		switch {
		case finding.Severity == "":
			return fmt.Errorf("shadow review finding severity: %w", ErrInvalidFindingSeverity)
		case finding.Location == nil:
			return fmt.Errorf("shadow review finding location: %w", ErrEmptyField)
		case finding.Location.StartLine < 1:
			return fmt.Errorf("shadow review finding location: %w", ErrNonPositive)
		case finding.Message == "" || finding.RawText == "":
			return fmt.Errorf("shadow review finding explanation: %w", ErrEmptyField)
		}
		return nil
	}
	return fmt.Errorf("shadow review finding source %q: %w", source, ErrInvalidShadowReviewSource)
}

// ShadowReviewRecord is the immutable result of one review pass that shadows a
// routed review round. Its distinct type and persistence surface make it
// structurally ineligible for routed-review readiness, round derivation,
// adjudication, and remediation.
type ShadowReviewRecord struct {
	InvocationID        InvocationID       `json:"invocation_id"`
	RunID               RunID              `json:"run_id"`
	ShadowedRound       int                `json:"shadowed_round"`
	Source              ShadowReviewSource `json:"source"`
	Provider            string             `json:"provider"`
	ModelConfiguration  string             `json:"model_configuration"`
	ConfigurationDigest Digest             `json:"configuration_digest"`
	InstructionDigest   Digest             `json:"instruction_digest"`
	CostOwner           string             `json:"cost_owner"`
	BaseSHA             string             `json:"base_sha"`
	HeadSHA             string             `json:"head_sha"`
	CompletedAt         time.Time          `json:"completed_at"`
	CompletionEvidence  Digest             `json:"completion_evidence"`
	Outcome             ReviewOutcome      `json:"outcome"`
	FindingIDs          []FindingID        `json:"finding_ids"`
}

// Validate reports whether a shadow review record is source-bound,
// attributable, exact-base/head bound, completed, and internally consistent.
func (r ShadowReviewRecord) Validate() error {
	switch {
	case r.InvocationID == "":
		return fmt.Errorf("shadow review record invocation_id: %w", ErrEmptyID)
	case r.RunID == "":
		return fmt.Errorf("shadow review record run_id: %w", ErrEmptyID)
	case r.ShadowedRound < 1:
		return fmt.Errorf("shadow review record shadowed_round %d: %w", r.ShadowedRound, ErrNonPositive)
	case !r.Source.valid():
		return fmt.Errorf("shadow review record source %q: %w", r.Source, ErrInvalidShadowReviewSource)
	case r.Provider == "":
		return fmt.Errorf("shadow review record provider: %w", ErrEmptyField)
	case r.ModelConfiguration == "":
		return fmt.Errorf("shadow review record model_configuration: %w", ErrEmptyField)
	case !contentaddr.Valid(string(r.ConfigurationDigest)):
		return fmt.Errorf("shadow review record configuration_digest %q: %w",
			r.ConfigurationDigest, ErrInvalidReviewCompletionEvidence)
	case !contentaddr.Valid(string(r.InstructionDigest)):
		return fmt.Errorf("shadow review record instruction_digest %q: %w",
			r.InstructionDigest, ErrInvalidReviewCompletionEvidence)
	case r.CostOwner == "":
		return fmt.Errorf("shadow review record cost_owner: %w", ErrEmptyField)
	case r.BaseSHA == "":
		return fmt.Errorf("shadow review record base_sha: %w", ErrEmptyField)
	case r.HeadSHA == "":
		return fmt.Errorf("shadow review record head_sha: %w", ErrEmptyField)
	case r.CompletedAt.IsZero():
		return fmt.Errorf("shadow review record completion time: %w", ErrMissingTimestamp)
	case r.CompletedAt.Location() != time.UTC:
		return fmt.Errorf("shadow review record completion time: %w", ErrTimestampNotUTC)
	case !contentaddr.Valid(string(r.CompletionEvidence)):
		return fmt.Errorf("shadow review record completion_evidence %q: %w",
			r.CompletionEvidence, ErrInvalidReviewCompletionEvidence)
	case !r.Outcome.valid():
		return fmt.Errorf("shadow review record outcome %q: %w", r.Outcome, ErrInvalidReviewOutcome)
	}
	if r.FindingIDs != nil && len(r.FindingIDs) == 0 {
		return fmt.Errorf("shadow review record finding_ids: empty list must be nil: %w", ErrFindingsNotCanonical)
	}
	for i, id := range r.FindingIDs {
		if id == "" {
			return fmt.Errorf("shadow review record finding_ids[%d]: %w", i, ErrEmptyID)
		}
		if i > 0 && id <= r.FindingIDs[i-1] {
			return fmt.Errorf("shadow review record finding_ids: %w", ErrFindingsNotCanonical)
		}
	}
	if (r.Outcome == ReviewClean) != (len(r.FindingIDs) == 0) {
		return fmt.Errorf("shadow review record outcome disagrees with findings: %w", ErrInvalidReviewOutcome)
	}
	return nil
}

// NewShadowReviewRecord constructs a detached record with canonical finding
// IDs and a UTC completion time.
func NewShadowReviewRecord(record ShadowReviewRecord) (ShadowReviewRecord, error) {
	record.CompletedAt = record.CompletedAt.UTC()
	record.FindingIDs = slices.Clone(record.FindingIDs)
	slices.Sort(record.FindingIDs)
	record.FindingIDs = slices.Compact(record.FindingIDs)
	if len(record.FindingIDs) == 0 {
		record.FindingIDs = nil
	}
	if err := record.Validate(); err != nil {
		return ShadowReviewRecord{}, err
	}
	return record, nil
}

// ClassifierAccuracyAssessment records the adjudicated result of one sampled
// classifier annotation. The zero value "" is invalid by design.
type ClassifierAccuracyAssessment string

const (
	ClassifierAssessmentAccurate      ClassifierAccuracyAssessment = "accurate"
	ClassifierAssessmentInaccurate    ClassifierAccuracyAssessment = "inaccurate"
	ClassifierAssessmentIndeterminate ClassifierAccuracyAssessment = "indeterminate"
)

// AllClassifierAccuracyAssessments is the single registration point for
// classifier-accuracy assessment outcomes.
var AllClassifierAccuracyAssessments = []ClassifierAccuracyAssessment{
	ClassifierAssessmentAccurate,
	ClassifierAssessmentInaccurate,
	ClassifierAssessmentIndeterminate,
}

func (a ClassifierAccuracyAssessment) valid() bool {
	switch a {
	case ClassifierAssessmentAccurate,
		ClassifierAssessmentInaccurate,
		ClassifierAssessmentIndeterminate:
		return true
	default:
		return false
	}
}

// ClassifierAccuracySample is one immutable adjudicated sample of a versioned
// classification over a finding from a named shadow result. The four identity
// fields are stable relational join keys; the store re-runs every join on
// reconstruction rather than trusting the decoded row.
type ClassifierAccuracySample struct {
	RunID                 RunID                        `json:"run_id"`
	FindingID             FindingID                    `json:"finding_id"`
	ClassificationVersion int                          `json:"classification_version"`
	ShadowInvocationID    InvocationID                 `json:"shadow_invocation_id"`
	Assessment            ClassifierAccuracyAssessment `json:"assessment"`
	RecordedAt            time.Time                    `json:"recorded_at"`
}

// Validate reports whether the sample carries complete stable join keys, a
// declared assessment, and a stable UTC timestamp.
func (s ClassifierAccuracySample) Validate() error {
	switch {
	case s.RunID == "" || s.FindingID == "" || s.ShadowInvocationID == "":
		return fmt.Errorf("classifier accuracy sample identity: %w", ErrEmptyID)
	case s.ClassificationVersion < 1:
		return fmt.Errorf("classifier accuracy sample classification_version %d: %w",
			s.ClassificationVersion, ErrNonPositive)
	case !s.Assessment.valid():
		return fmt.Errorf("classifier accuracy sample assessment %q: %w",
			s.Assessment, ErrInvalidClassifierAssessment)
	case s.RecordedAt.IsZero():
		return fmt.Errorf("classifier accuracy sample recorded_at: %w", ErrMissingTimestamp)
	case s.RecordedAt.Location() != time.UTC:
		return fmt.Errorf("classifier accuracy sample recorded_at: %w", ErrTimestampNotUTC)
	}
	return nil
}
