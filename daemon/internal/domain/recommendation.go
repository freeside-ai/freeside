package domain

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
)

// DaemonPolicyRecommendationProvenance binds a deterministic rule and its
// canonical input.
type DaemonPolicyRecommendationProvenance struct {
	RuleDigest  Digest `json:"rule_digest"`
	InputDigest Digest `json:"input_digest"`
}

// AgentJudgmentRecommendationProvenance binds a declared judgment invocation
// to its immutable output artifact.
type AgentJudgmentRecommendationProvenance struct {
	JudgmentSite   JudgmentSite `json:"judgment_site"`
	InvocationID   InvocationID `json:"invocation_id"`
	ArtifactDigest Digest       `json:"artifact_digest"`
}

// ProjectPolicyRecommendationProvenance binds an applied policy key to the
// run's resolved policy and the daemon-authored application record.
type ProjectPolicyRecommendationProvenance struct {
	PolicyKey            string `json:"policy_key"`
	ResolvedPolicyDigest Digest `json:"resolved_policy_digest"`
	ApplicationDigest    Digest `json:"application_digest"`
}

// RecommendationProvenance is a closed source-specific union. Exactly one
// variant must be present and it must match Recommendation.Source.
type RecommendationProvenance struct {
	DaemonPolicy  *DaemonPolicyRecommendationProvenance  `json:"daemon_policy"`
	AgentJudgment *AgentJudgmentRecommendationProvenance `json:"agent_judgment"`
	ProjectPolicy *ProjectPolicyRecommendationProvenance `json:"project_policy"`
}

// Validate reports whether exactly the source-matching provenance variant is
// present and every identity field is usable.
func (p RecommendationProvenance) Validate(source RecommendationSource) error {
	variants := 0
	if p.DaemonPolicy != nil {
		variants++
	}
	if p.AgentJudgment != nil {
		variants++
	}
	if p.ProjectPolicy != nil {
		variants++
	}
	if variants != 1 {
		return fmt.Errorf("recommendation provenance has %d variants: %w", variants, ErrRecommendationProvenanceInconsistent)
	}
	switch source {
	case RecommendationDaemonPolicy:
		if p.DaemonPolicy == nil {
			return fmt.Errorf("recommendation source %q lacks daemon_policy provenance: %w", source, ErrRecommendationProvenanceInconsistent)
		}
		if !isSHA256Digest(string(p.DaemonPolicy.RuleDigest)) ||
			!isSHA256Digest(string(p.DaemonPolicy.InputDigest)) {
			return fmt.Errorf("daemon policy recommendation provenance: %w", ErrInvalidDigest)
		}
	case RecommendationAgentJudgment:
		if p.AgentJudgment == nil {
			return fmt.Errorf("recommendation source %q lacks agent_judgment provenance: %w", source, ErrRecommendationProvenanceInconsistent)
		}
		if !p.AgentJudgment.JudgmentSite.valid() {
			return fmt.Errorf("judgment site %q: %w", p.AgentJudgment.JudgmentSite, ErrInvalidJudgmentSite)
		}
		if p.AgentJudgment.InvocationID == "" {
			return fmt.Errorf("agent judgment invocation_id: %w", ErrEmptyID)
		}
		if !isSHA256Digest(string(p.AgentJudgment.ArtifactDigest)) {
			return fmt.Errorf("agent judgment artifact_digest %q: %w", p.AgentJudgment.ArtifactDigest, ErrInvalidDigest)
		}
	case RecommendationProjectPolicy:
		if p.ProjectPolicy == nil {
			return fmt.Errorf("recommendation source %q lacks project_policy provenance: %w", source, ErrRecommendationProvenanceInconsistent)
		}
		if p.ProjectPolicy.PolicyKey == "" {
			return fmt.Errorf("project policy recommendation policy_key: %w", ErrEmptyField)
		}
		if !isSHA256Digest(string(p.ProjectPolicy.ResolvedPolicyDigest)) ||
			!isSHA256Digest(string(p.ProjectPolicy.ApplicationDigest)) {
			return fmt.Errorf("project policy recommendation provenance: %w", ErrInvalidDigest)
		}
	default:
		return fmt.Errorf("recommendation source %q: %w", source, ErrInvalidRecommendationSource)
	}
	return nil
}

// Recommendation is the optional daemon-derived lead for one item. It never
// widens or reorders RequestedDecision.
type Recommendation struct {
	Action     Action                   `json:"action"`
	Reason     string                   `json:"reason"`
	Source     RecommendationSource     `json:"source"`
	Provenance RecommendationProvenance `json:"provenance"`
	Confidence *AdjudicationConfidence  `json:"confidence"`
}

// Validate reports whether the source, provenance, and projected payload are
// structurally sound. Item-specific action and artifact containment are checked
// by AttentionItem.Validate.
func (r Recommendation) Validate() error {
	if !r.Action.valid() {
		return fmt.Errorf("recommendation action %q: %w", r.Action, ErrInvalidAction)
	}
	if r.Reason == "" {
		return fmt.Errorf("recommendation reason: %w", ErrEmptyField)
	}
	if !r.Source.valid() {
		return fmt.Errorf("recommendation source %q: %w", r.Source, ErrInvalidRecommendationSource)
	}
	if err := r.Provenance.Validate(r.Source); err != nil {
		return err
	}
	if r.Confidence != nil && !r.Confidence.valid() {
		return fmt.Errorf("recommendation confidence %q: %w", *r.Confidence, ErrInvalidAdjudicationConfidence)
	}
	return nil
}

// RecommendationSourceRecord is the immutable daemon-written source record
// from which an item's recommendation is derived. Digest content-addresses
// every other field.
type RecommendationSourceRecord struct {
	ItemID                ItemID                   `json:"item_id"`
	Source                RecommendationSource     `json:"source"`
	Provenance            RecommendationProvenance `json:"provenance"`
	Action                Action                   `json:"action"`
	Reason                string                   `json:"reason"`
	Confidence            *AdjudicationConfidence  `json:"confidence"`
	DecisionSurfaceDigest Digest                   `json:"decision_surface_digest"`
	Digest                Digest                   `json:"digest"`
}

type recommendationSourceRecordPreimage struct {
	ItemID                ItemID                   `json:"item_id"`
	Source                RecommendationSource     `json:"source"`
	Provenance            RecommendationProvenance `json:"provenance"`
	Action                Action                   `json:"action"`
	Reason                string                   `json:"reason"`
	Confidence            *AdjudicationConfidence  `json:"confidence"`
	DecisionSurfaceDigest Digest                   `json:"decision_surface_digest"`
}

// NewRecommendationSourceRecord detaches the record, computes its content
// address, and validates the completed immutable value.
func NewRecommendationSourceRecord(record RecommendationSourceRecord) (RecommendationSourceRecord, error) {
	record.Provenance = cloneRecommendationProvenance(record.Provenance)
	record.Confidence = clonePtr(record.Confidence)
	record.Digest = ""
	digest, err := record.ComputeDigest()
	if err != nil {
		return RecommendationSourceRecord{}, err
	}
	record.Digest = digest
	if err := record.Validate(); err != nil {
		return RecommendationSourceRecord{}, err
	}
	return record, nil
}

// ComputeDigest returns the content address of every record field except Digest.
func (r RecommendationSourceRecord) ComputeDigest() (Digest, error) {
	body, err := json.Marshal(recommendationSourceRecordPreimage{
		ItemID: r.ItemID, Source: r.Source, Provenance: r.Provenance,
		Action: r.Action, Reason: r.Reason, Confidence: r.Confidence,
		DecisionSurfaceDigest: r.DecisionSurfaceDigest,
	})
	if err != nil {
		return "", fmt.Errorf("recommendation source canonical encoding: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Validate reports whether the source record is structurally sound and its
// digest matches its canonical content.
func (r RecommendationSourceRecord) Validate() error {
	if r.ItemID == "" {
		return fmt.Errorf("recommendation source item_id: %w", ErrEmptyID)
	}
	projection := Recommendation{
		Action: r.Action, Reason: r.Reason, Source: r.Source,
		Provenance: r.Provenance, Confidence: r.Confidence,
	}
	if err := projection.Validate(); err != nil {
		return err
	}
	if !isSHA256Digest(string(r.DecisionSurfaceDigest)) {
		return fmt.Errorf("recommendation source decision_surface_digest %q: %w", r.DecisionSurfaceDigest, ErrInvalidDigest)
	}
	if !isSHA256Digest(string(r.Digest)) {
		return fmt.Errorf("recommendation source digest %q: %w", r.Digest, ErrInvalidDigest)
	}
	want, err := r.ComputeDigest()
	if err != nil {
		return err
	}
	if r.Digest != want {
		return fmt.Errorf("recommendation source digest %q, recomputed %q: %w", r.Digest, want, ErrRecommendationSourceDigestMismatch)
	}
	return nil
}

func cloneRecommendationProvenance(p RecommendationProvenance) RecommendationProvenance {
	return RecommendationProvenance{
		DaemonPolicy: clonePtr(p.DaemonPolicy), AgentJudgment: clonePtr(p.AgentJudgment),
		ProjectPolicy: clonePtr(p.ProjectPolicy),
	}
}

// RecommendationProjection is the canonical payload rederived from an
// authenticated source-and-item pair.
type RecommendationProjection struct {
	Action     Action
	Reason     string
	Confidence *AdjudicationConfidence
}

// AgentJudgmentRecommendation is the authenticated source result returned by
// RecommendationAuthority. RunID and Round bind it back to the item's typed
// adjudication projection. DecisionSurfaceDigest is the artifact's commitment
// copied verbatim by the authority.
type AgentJudgmentRecommendation struct {
	RunID                 RunID
	Round                 int
	DecisionSurfaceDigest Digest
	Projection            RecommendationProjection
}

// FindingAdjudicatorRecommendationReason is the item-level explanation for
// accepting the adjudicator's per-finding routes. The detailed rationales and
// confidences remain in the bound adjudication artifact.
const FindingAdjudicatorRecommendationReason = "Accept the adjudicator's recommended route for each finding."

// DaemonPolicyRule re-evaluates one registered deterministic rule against the
// current item and surface. The returned input digest authenticates exactly
// what the source record committed to.
type DaemonPolicyRule interface {
	EvaluateRecommendation(AttentionItem, DecisionSurface) (RecommendationProjection, Digest, bool, error)
}

// RecommendationAuthority resolves current authoritative state without
// letting the item or source record choose it. ResolveAgentJudgment returns
// the artifact's decision-surface commitment with the authenticated result.
// Production has no daemon-policy rule registry entries in this unit; tests
// and future producers supply them through this seam.
type RecommendationAuthority interface {
	ResolveAgentJudgment(JudgmentSite, InvocationID, Digest) (AgentJudgmentRecommendation, error)
	DaemonPolicyRule(Digest) (DaemonPolicyRule, bool)
	CurrentResolvedPolicyDigest(RunID) (Digest, error)
}

// DeriveRecommendation implements the unique-or-none rule. An authenticated
// agent judgment must commit to the current decision surface. Invalid, stale,
// or payload-substituted records produce absence; infrastructure errors from
// an authority resolver are returned to the persistence boundary.
func DeriveRecommendation(
	item AttentionItem,
	surface DecisionSurface,
	records []RecommendationSourceRecord,
	authority RecommendationAuthority,
) (*Recommendation, error) {
	if !surface.Matches(item) {
		return nil, nil
	}
	type candidate struct {
		record RecommendationSourceRecord
		rule   DaemonPolicyRule
	}
	eligible := make([]candidate, 0, len(records))
	for _, record := range records {
		if err := record.Validate(); err != nil || record.ItemID != item.ID {
			continue
		}
		if VerifyDecisionSurfaceCommitment(surface, record.DecisionSurfaceDigest) != nil {
			continue
		}
		var rule DaemonPolicyRule
		switch record.Source {
		case RecommendationAgentJudgment:
			p := record.Provenance.AgentJudgment
			if item.FindingAdjudication == nil ||
				p.JudgmentSite != JudgmentSiteFindingAdjudicator ||
				item.FindingAdjudication.AdjudicationDigest != p.ArtifactDigest ||
				!slices.Contains(item.ArtifactDigests, p.ArtifactDigest) {
				continue
			}
		case RecommendationDaemonPolicy:
			var ok bool
			rule, ok = authority.DaemonPolicyRule(record.Provenance.DaemonPolicy.RuleDigest)
			if !ok {
				continue
			}
		case RecommendationProjectPolicy:
			if item.Subject.RunID == nil {
				continue
			}
			current, err := authority.CurrentResolvedPolicyDigest(*item.Subject.RunID)
			if err != nil {
				continue
			}
			if current != record.Provenance.ProjectPolicy.ResolvedPolicyDigest {
				continue
			}
		}
		eligible = append(eligible, candidate{record: record, rule: rule})
	}
	if len(eligible) != 1 {
		return nil, nil
	}
	c := eligible[0]
	projection := RecommendationProjection{}
	switch c.record.Source {
	case RecommendationAgentJudgment:
		p := c.record.Provenance.AgentJudgment
		resolved, err := authority.ResolveAgentJudgment(p.JudgmentSite, p.InvocationID, p.ArtifactDigest)
		if err != nil {
			return nil, nil
		}
		binding := item.FindingAdjudication
		if binding == nil || resolved.RunID != binding.RunID || resolved.Round != binding.Round {
			return nil, nil
		}
		if resolved.DecisionSurfaceDigest != surface.Digest {
			return nil, nil
		}
		projection = resolved.Projection
	case RecommendationDaemonPolicy:
		p := c.record.Provenance.DaemonPolicy
		resolved, inputDigest, applicable, err := c.rule.EvaluateRecommendation(item, surface)
		if err != nil {
			return nil, nil
		}
		if !applicable || inputDigest != p.InputDigest {
			return nil, nil
		}
		projection = resolved
	case RecommendationProjectPolicy:
		p := c.record.Provenance.ProjectPolicy
		want, err := ComputeProjectPolicyRecommendationApplicationDigest(
			p.PolicyKey, p.ResolvedPolicyDigest, c.record.DecisionSurfaceDigest,
			c.record.Action, c.record.Reason,
		)
		if err != nil || want != p.ApplicationDigest {
			return nil, nil
		}
		projection = RecommendationProjection{
			Action: c.record.Action, Reason: c.record.Reason,
			Confidence: clonePtr(c.record.Confidence),
		}
	}
	if !item.Offers(projection.Action) || projection.Reason == "" ||
		projection.Action != c.record.Action || projection.Reason != c.record.Reason ||
		!confidenceEqual(projection.Confidence, c.record.Confidence) {
		return nil, nil
	}
	return &Recommendation{
		Action: projection.Action, Reason: projection.Reason, Source: c.record.Source,
		Provenance: cloneRecommendationProvenance(c.record.Provenance),
		Confidence: clonePtr(projection.Confidence),
	}, nil
}

type projectPolicyApplicationPreimage struct {
	PolicyKey             string `json:"policy_key"`
	ResolvedPolicyDigest  Digest `json:"resolved_policy_digest"`
	DecisionSurfaceDigest Digest `json:"decision_surface_digest"`
	Action                Action `json:"action"`
	Reason                string `json:"reason"`
}

// ComputeProjectPolicyRecommendationApplicationDigest authenticates the
// daemon-authored application record specified by plan §4.
func ComputeProjectPolicyRecommendationApplicationDigest(
	policyKey string, resolvedPolicyDigest, decisionSurfaceDigest Digest, action Action, reason string,
) (Digest, error) {
	body, err := json.Marshal(projectPolicyApplicationPreimage{
		PolicyKey: policyKey, ResolvedPolicyDigest: resolvedPolicyDigest,
		DecisionSurfaceDigest: decisionSurfaceDigest, Action: action, Reason: reason,
	})
	if err != nil {
		return "", fmt.Errorf("project policy recommendation application encoding: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

func confidenceEqual(a, b *AdjudicationConfidence) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
