package inference

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const AdjudicatorSiteID = "finding_adjudicator"

// ErrAdjudicationNotAvailable is the typed fail-safe result for a missing,
// malformed, below-budget, or otherwise unavailable adjudicator response.
// The engine treats it like every other not-accepted batch and parks.
var ErrAdjudicationNotAvailable = errors.New("finding adjudication unavailable")

// AdjudicationFinding is the complete per-finding fact set the engine permits
// the finding-adjudicator site to observe.
type AdjudicationFinding struct {
	Finding            domain.Finding               `json:"finding"`
	Classification     *domain.Classification       `json:"classification"`
	RemediationSurface string                       `json:"remediation_surface"`
	Compatibility      domain.WorkUnitCompatibility `json:"compatibility"`
}

// AdjudicationDissent is the typed structured re-entry signal. Conversational
// text is not representable here and therefore grants no routing authority.
type AdjudicationDissent struct {
	Kind       string             `json:"kind"`
	FindingIDs []domain.FindingID `json:"finding_ids"`
	Evidence   string             `json:"evidence"`
}

// AdjudicationFeedback is the exact immutable Discuss prefix that requests a
// successor proposal. ConversationPrefix is the canonical domain encoding, not
// live conversation state, and PrefixDigest lets the durable successor re-bind
// the same bytes at persistence.
type AdjudicationFeedback struct {
	InvocationID       domain.InvocationID      `json:"invocation_id"`
	ConversationID     domain.ConversationID    `json:"conversation_id"`
	ThroughSequence    int                      `json:"through_sequence"`
	PrefixDigest       domain.Digest            `json:"prefix_digest"`
	ConversationPrefix json.RawMessage          `json:"conversation_prefix"`
	Attachments        []AdjudicationAttachment `json:"attachments"`
}

// AdjudicationAttachment is a bounded, digest-verified text attachment from
// the immutable Discuss prefix. Opaque binary attachments fail closed because
// the direct inference driver has no artifact tools or multimodal channel.
type AdjudicationAttachment struct {
	Digest  domain.Digest `json:"digest"`
	Content string        `json:"content"`
}

// FindingAdjudicationInput is the allowlisted input to one batch proposal.
// It intentionally carries no implementer reasoning history.
type FindingAdjudicationInput struct {
	RunID                     domain.RunID
	Round                     int
	ApprovedSpecDigest        domain.Digest
	ApprovedSpecification     string
	InstructionSnapshotDigest domain.Digest
	InstructionSnapshot       string
	ResolvedPolicyDigest      domain.Digest
	DeclaredPaths             []string
	Findings                  []AdjudicationFinding
	PriorDispositions         []domain.ReviewDispositionRecord
	PriorEntries              []domain.FindingAdjudicationEntry
	Dissent                   *AdjudicationDissent
	Feedback                  *AdjudicationFeedback
}

type adjudicatorOutput struct {
	Entries *[]adjudicatorEntry `json:"entries"`
}

type adjudicatorEntry struct {
	FindingID        domain.FindingID              `json:"finding_id"`
	GoalRelationship domain.GoalRelationship       `json:"goal_relationship"`
	Compatibility    nullableProposedCompatibility `json:"compatibility"`
	Route            nullableAdjudicationRoute     `json:"route"`
	Confidence       domain.AdjudicationConfidence `json:"confidence"`
	Rationale        string                        `json:"rationale"`
	Evidence         *[]string                     `json:"evidence"`
	CitedRules       *[]string                     `json:"cited_rules"`
	Assumptions      *[]string                     `json:"assumptions"`
	Alternatives     *[]string                     `json:"alternatives"`
	OpenQuestions    *[]string                     `json:"open_questions"`
}

type nullableProposedCompatibility struct {
	Present bool
	Value   *domain.ProposedCompatibility
}

func (v *nullableProposedCompatibility) UnmarshalJSON(data []byte) error {
	var value *domain.ProposedCompatibility
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	v.Present = true
	v.Value = value
	return nil
}

type nullableAdjudicationRoute struct {
	Present bool
	Value   *domain.AdjudicationRoute
}

func (v *nullableAdjudicationRoute) UnmarshalJSON(data []byte) error {
	var value *domain.AdjudicationRoute
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	v.Present = true
	v.Value = value
	return nil
}

func (o adjudicatorOutput) validateEntries() error {
	if o.Entries == nil {
		return errors.New("adjudicator output omits entries")
	}
	for _, proposed := range *o.Entries {
		if proposed.Evidence == nil || proposed.CitedRules == nil || proposed.Assumptions == nil ||
			proposed.Alternatives == nil || proposed.OpenQuestions == nil {
			return errors.New("adjudicator proposal omits a labeled explanation field")
		}
		if !proposed.Compatibility.Present || !proposed.Route.Present {
			return errors.New("adjudicator proposal omits an authority-lattice field")
		}
		if proposed.GoalRelationship == domain.GoalRequired && proposed.Compatibility.Value == nil &&
			proposed.Route.Value == nil {
			if _, err := domain.NewEngineModelAdjudicationEntry(
				proposed.FindingID, proposed.GoalRelationship, proposed.Confidence,
				proposed.Rationale, *proposed.Evidence, *proposed.CitedRules,
				*proposed.Assumptions, *proposed.Alternatives, *proposed.OpenQuestions,
			); err != nil {
				return err
			}
			continue
		}
		if proposed.Route.Value == nil {
			return errors.New("adjudicator proposal omits route")
		}
		if _, err := domain.NewModelAdjudicationEntry(
			proposed.FindingID, proposed.GoalRelationship, proposed.Compatibility.Value,
			*proposed.Route.Value, proposed.Confidence, proposed.Rationale,
			*proposed.Evidence, *proposed.CitedRules, *proposed.Assumptions,
			*proposed.Alternatives, *proposed.OpenQuestions,
		); err != nil {
			return err
		}
	}
	return nil
}

func (o adjudicatorOutput) domainEntries(
	findings []AdjudicationFinding,
) ([]domain.FindingAdjudicationEntry, error) {
	if err := o.validateEntries(); err != nil {
		return nil, err
	}
	inputs := make(map[domain.FindingID]AdjudicationFinding, len(findings))
	for _, finding := range findings {
		if finding.Finding.ID == "" {
			return nil, errors.New("adjudicator input omits finding id")
		}
		if _, duplicate := inputs[finding.Finding.ID]; duplicate {
			return nil, errors.New("adjudicator input repeats finding id")
		}
		inputs[finding.Finding.ID] = finding
	}
	entries := make([]domain.FindingAdjudicationEntry, 0, len(*o.Entries))
	for _, proposed := range *o.Entries {
		var (
			entry domain.FindingAdjudicationEntry
			err   error
		)
		if proposed.GoalRelationship == domain.GoalRequired && proposed.Compatibility.Value == nil &&
			proposed.Route.Value == nil {
			input, ok := inputs[proposed.FindingID]
			if !ok || input.Compatibility != domain.CompatibilityAllowed {
				return nil, errors.New("engine-authorized adjudication does not match an allowed input")
			}
			entry, err = domain.NewEngineModelAdjudicationEntry(
				proposed.FindingID, proposed.GoalRelationship, proposed.Confidence,
				proposed.Rationale, *proposed.Evidence, *proposed.CitedRules,
				*proposed.Assumptions, *proposed.Alternatives, *proposed.OpenQuestions,
			)
		} else {
			entry, err = domain.NewModelAdjudicationEntry(
				proposed.FindingID, proposed.GoalRelationship, proposed.Compatibility.Value,
				*proposed.Route.Value, proposed.Confidence, proposed.Rationale,
				*proposed.Evidence, *proposed.CitedRules, *proposed.Assumptions,
				*proposed.Alternatives, *proposed.OpenQuestions,
			)
		}
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// AdjudicatorSite declares the second ceiling-bounded annotation site.
func AdjudicatorSite(budget Budget) Site {
	classifier := ClassifierSite(Budget{}).Annotation
	return Site{
		ID: AdjudicatorSiteID, Authority: AuthorityAnnotate,
		Fields: []FieldPolicy{
			{Name: "run_id", Sensitivity: SensitivityOperational},
			{Name: "round", Sensitivity: SensitivityOperational},
			{Name: "approved_spec_digest", Sensitivity: SensitivityOperational},
			{Name: "approved_spec", Sensitivity: SensitivityRepository},
			{Name: "instruction_snapshot_digest", Sensitivity: SensitivityOperational},
			{Name: "instruction_snapshot", Sensitivity: SensitivityRepository},
			{Name: "resolved_policy_digest", Sensitivity: SensitivityOperational},
			{Name: "declared_paths", Sensitivity: SensitivityRepository},
			{Name: "findings", Sensitivity: SensitivityRepository},
			{Name: "prior_disposition_history", Sensitivity: SensitivityRepository},
			{Name: "prior_adjudication", Sensitivity: SensitivityRepository},
			{Name: "dissent", Sensitivity: SensitivityRepository},
			{Name: "conversation_feedback", Sensitivity: SensitivityRepository},
		},
		FailSafe: `{"entries":[]}`, Retention: 30 * 24 * time.Hour, Timeout: 30 * time.Second,
		MaxInputBytes: 2 << 20, MaxOutputBytes: domain.MaxFindingAdjudicationBytes,
		MaxComputeUnits: 10_000, Budget: budget, AuditEvery: 10,
		Adjudication: &AdjudicationContract{
			GoalRelationships: []string{
				string(domain.GoalRequired), string(domain.GoalAdjacent),
				string(domain.GoalContradictory), string(domain.GoalUnclear),
			},
			ProposedCompatibilities: []string{
				string(domain.ProposedWorkUnitRevision), string(domain.ProposedSeparateWork),
				string(domain.ProposedHumanDecision), string(domain.ProposedUnknown),
			},
			Routes: []string{
				string(domain.RouteParkRevision), string(domain.RouteParkSeparateWork),
				string(domain.RouteAttentionHumanDecision), string(domain.RouteParkUnknown),
				string(domain.RouteDefer), string(domain.RouteDecline), string(domain.RouteDispute),
				string(domain.RouteAttentionUnclear),
			},
			Rows: adjudicatorRows(),
			Confidence: []string{
				string(domain.ConfidenceLow), string(domain.ConfidenceMedium), string(domain.ConfidenceHigh),
			},
			ReducesWork:                []string{string(domain.RouteDefer), string(domain.RouteDecline)},
			SeverityMappings:           slices.Clone(classifier.SeverityMappings),
			UnknownSeverityFallback:    classifier.UnknownSeverityFallback,
			NormalizedSeverityCeilings: slices.Clone(classifier.NormalizedSeverityCeilings),
			SecondAdjudicationRules:    slices.Clone(classifier.SecondAdjudicationRules),
		},
		ValidateOutput: func(data []byte) error {
			var output adjudicatorOutput
			if err := decodeStrictObject(data, &output, domain.MaxFindingAdjudicationBytes); err != nil {
				return err
			}
			return output.validateEntries()
		},
	}
}

func adjudicatorRows() []AdjudicationRow {
	compatibility := func(value domain.ProposedCompatibility) *string {
		text := string(value)
		return &text
	}
	return []AdjudicationRow{
		{
			GoalRelationship: string(domain.GoalRequired), UsesEngineCompatibility: true,
		},
		{
			GoalRelationship:      string(domain.GoalRequired),
			ProposedCompatibility: compatibility(domain.ProposedWorkUnitRevision),
			Route:                 string(domain.RouteParkRevision),
		},
		{
			GoalRelationship:      string(domain.GoalRequired),
			ProposedCompatibility: compatibility(domain.ProposedSeparateWork),
			Route:                 string(domain.RouteParkSeparateWork),
		},
		{
			GoalRelationship:      string(domain.GoalRequired),
			ProposedCompatibility: compatibility(domain.ProposedHumanDecision),
			Route:                 string(domain.RouteAttentionHumanDecision),
		},
		{
			GoalRelationship:      string(domain.GoalRequired),
			ProposedCompatibility: compatibility(domain.ProposedUnknown),
			Route:                 string(domain.RouteParkUnknown),
		},
		{GoalRelationship: string(domain.GoalAdjacent), Route: string(domain.RouteDefer)},
		{GoalRelationship: string(domain.GoalContradictory), Route: string(domain.RouteDecline)},
		{GoalRelationship: string(domain.GoalContradictory), Route: string(domain.RouteDispute)},
		{GoalRelationship: string(domain.GoalUnclear), Route: string(domain.RouteAttentionUnclear)},
	}
}

// AdjudicateFindings obtains one producer-labeled proposal per residue
// finding. A fallback is exposed as a typed not-available error rather than a
// valid empty batch, so callers cannot confuse fail-safe bytes with judgment.
func (c *Client) AdjudicateFindings(
	ctx context.Context, project, root string, input FindingAdjudicationInput,
) ([]domain.FindingAdjudicationEntry, error) {
	declaredPaths, err := adjudicationJSON(input.DeclaredPaths)
	if err != nil {
		return nil, err
	}
	findings, err := adjudicationJSON(input.Findings)
	if err != nil {
		return nil, err
	}
	dispositions, err := adjudicationJSON(input.PriorDispositions)
	if err != nil {
		return nil, err
	}
	priorEntries, err := adjudicationJSON(input.PriorEntries)
	if err != nil {
		return nil, err
	}
	dissent, err := adjudicationJSON(input.Dissent)
	if err != nil {
		return nil, err
	}
	feedback, err := adjudicationJSON(input.Feedback)
	if err != nil {
		return nil, err
	}
	result, err := c.Call(ctx, AdjudicatorSiteID, project, root, map[string]InputField{
		"run_id":                      {Value: string(input.RunID), Sensitivity: SensitivityOperational},
		"round":                       {Value: fmt.Sprint(input.Round), Sensitivity: SensitivityOperational},
		"approved_spec_digest":        {Value: string(input.ApprovedSpecDigest), Sensitivity: SensitivityOperational},
		"approved_spec":               {Value: input.ApprovedSpecification, Sensitivity: SensitivityRepository},
		"instruction_snapshot_digest": {Value: string(input.InstructionSnapshotDigest), Sensitivity: SensitivityOperational},
		"instruction_snapshot":        {Value: input.InstructionSnapshot, Sensitivity: SensitivityRepository},
		"resolved_policy_digest":      {Value: string(input.ResolvedPolicyDigest), Sensitivity: SensitivityOperational},
		"declared_paths":              {Value: declaredPaths, Sensitivity: SensitivityRepository},
		"findings":                    {Value: findings, Sensitivity: SensitivityRepository},
		"prior_disposition_history":   {Value: dispositions, Sensitivity: SensitivityRepository},
		"prior_adjudication":          {Value: priorEntries, Sensitivity: SensitivityRepository},
		"dissent":                     {Value: dissent, Sensitivity: SensitivityRepository},
		"conversation_feedback":       {Value: feedback, Sensitivity: SensitivityRepository},
	})
	if err != nil {
		return nil, err
	}
	if result.Fallback {
		return nil, fmt.Errorf("%w: %s", ErrAdjudicationNotAvailable, result.Reason)
	}
	var output adjudicatorOutput
	if err := json.Unmarshal(result.Output, &output); err != nil {
		return nil, fmt.Errorf("decode validated adjudicator output: %w", err)
	}
	entries, err := output.domainEntries(input.Findings)
	if err != nil {
		return nil, errors.Join(ErrAdjudicationNotAvailable, err)
	}
	return entries, nil
}

func adjudicationJSON(value any) (string, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode finding-adjudicator input: %w", err)
	}
	return string(body), nil
}
