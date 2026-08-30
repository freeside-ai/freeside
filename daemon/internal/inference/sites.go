package inference

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/advisory"
	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

const (
	ClassifierSiteID          = "finding_classifier"
	DiagnosticSiteID          = "execution_diagnostic"
	AttentionDiscussionSiteID = "attention_discussion"
)

// Ordinal is the landed materiality and confidence vocabulary (§7).
type Ordinal string

const (
	OrdinalLow    Ordinal = "low"
	OrdinalMedium Ordinal = "medium"
	OrdinalHigh   Ordinal = "high"
)

var AllOrdinals = []Ordinal{OrdinalLow, OrdinalMedium, OrdinalHigh}

func (o Ordinal) valid() bool {
	switch o {
	case OrdinalLow, OrdinalMedium, OrdinalHigh:
		return true
	default:
		return false
	}
}

type classifierOutput struct {
	Materiality Ordinal `json:"materiality"`
	Confidence  Ordinal `json:"confidence"`
	Note        string  `json:"note"`
}

// ClassificationDecision keeps policy effects deterministic and outside the
// model output. ReducesWork is true for the sole ceiling-permitted reduction;
// RequiresAttention is the critical/high, non-high-confidence guard.
type ClassificationDecision struct {
	Classification    domain.Classification
	ReducesWork       bool
	RequiresAttention bool
	Fallback          bool
}

// ConservativeClassifierDecision is the classifier site's deterministic
// fail-safe when the daemon has no inference client. It preserves the site's
// declared high-materiality, low-confidence fallback without letting an
// unavailable model turn a shadow finding into missing evidence.
func ConservativeClassifierDecision(
	finding domain.Finding, version int,
) ClassificationDecision {
	output := classifierOutput{
		Materiality: OrdinalHigh,
		Confidence:  OrdinalLow,
		Note:        "inference unavailable; conservative classification",
	}
	contract := ClassifierSite(Budget{}).Annotation
	severity := contract.normalizedSeverity(finding.Source, string(finding.Severity))
	return ClassificationDecision{
		Classification: domain.Classification{
			FindingID: finding.ID, Version: version,
			Materiality: string(output.Materiality), Confidence: string(output.Confidence),
			Note: "producer=deterministic/fallback; " + output.Note,
		},
		RequiresAttention: contract.requiresSecondAdjudication(severity, string(output.Confidence)),
		Fallback:          true,
	}
}

// ClassifierSite declares the first ceiling-bounded annotation site.
func ClassifierSite(budget Budget) Site {
	fallback := `{"materiality":"high","confidence":"low","note":"inference unavailable; conservative classification"}`
	return Site{
		ID: ClassifierSiteID, Authority: AuthorityAnnotate,
		Fields: []FieldPolicy{
			{Name: "finding_id", Sensitivity: SensitivityOperational},
			{Name: "source", Sensitivity: SensitivityOperational},
			{Name: "severity", Sensitivity: SensitivityOperational},
			{Name: "location", Sensitivity: SensitivityRepository},
			{Name: "message", Sensitivity: SensitivityRepository},
			{Name: "raw_text", Sensitivity: SensitivityRepository},
		},
		FailSafe: fallback, Retention: 30 * 24 * time.Hour, Timeout: 30 * time.Second,
		MaxInputBytes: 128 << 10, MaxOutputBytes: 8 << 10, MaxComputeUnits: 10_000,
		Budget: budget, AuditEvery: 10,
		Annotation: &AnnotationContract{
			Materiality: []string{string(OrdinalLow), string(OrdinalMedium), string(OrdinalHigh)},
			Confidence:  []string{string(OrdinalLow), string(OrdinalMedium), string(OrdinalHigh)},
			ReducesWork: []AnnotationOutput{{Materiality: string(OrdinalLow), Confidence: string(OrdinalHigh)}},
			SeverityMappings: []SeverityMapping{
				{Source: "codex_local", Native: "p1", Normalized: "high"},
				{Source: "codex_local", Native: "p2", Normalized: "medium"},
				{Source: "codex_local", Native: "p3", Normalized: "low"},
				{Source: "claude_local", Native: "p1", Normalized: "high"},
				{Source: "claude_local", Native: "p2", Normalized: "medium"},
				{Source: "claude_local", Native: "p3", Normalized: "low"},
			},
			UnknownSeverityFallback: "high",
			NormalizedSeverityCeilings: []SeverityCeiling{
				{NormalizedSeverity: "critical", MinimumHandling: "credible"},
				{NormalizedSeverity: "high", MinimumHandling: "credible"},
			},
			SecondAdjudicationRules: []SecondAdjudicationRule{
				{NormalizedSeverity: "critical", ConfidenceBelow: string(OrdinalHigh), Fallback: "attention"},
				{NormalizedSeverity: "high", ConfidenceBelow: string(OrdinalHigh), Fallback: "attention"},
			},
		},
		ValidateOutput: func(data []byte) error {
			var output classifierOutput
			if err := decodeStrictObject(data, &output, 8<<10); err != nil {
				return err
			}
			if !output.Materiality.valid() || !output.Confidence.valid() || strings.TrimSpace(output.Note) == "" {
				return errors.New("invalid classifier lattice output")
			}
			return nil
		},
	}
}

// ClassifyFinding produces one versioned annotation without mutating the raw
// finding or deriving credibility, trust, transitions, or publication state.
func (c *Client) ClassifyFinding(
	ctx context.Context, project, root string, finding domain.Finding, version int,
) (ClassificationDecision, error) {
	result, err := c.Call(ctx, ClassifierSiteID, project, root, map[string]InputField{
		"finding_id": {Value: string(finding.ID), Sensitivity: SensitivityOperational},
		"source":     {Value: finding.Source, Sensitivity: SensitivityOperational},
		"severity":   {Value: string(finding.Severity), Sensitivity: SensitivityOperational},
		"location":   {Value: findingLocationText(finding.Location), Sensitivity: SensitivityRepository},
		"message":    {Value: finding.Message, Sensitivity: SensitivityRepository},
		"raw_text":   {Value: finding.RawText, Sensitivity: SensitivityRepository},
	})
	if err != nil {
		return ClassificationDecision{}, err
	}
	var output classifierOutput
	if err := json.Unmarshal(result.Output, &output); err != nil {
		return ClassificationDecision{}, fmt.Errorf("decode validated classifier output: %w", err)
	}
	contract := c.sites[ClassifierSiteID].Annotation
	severity := contract.normalizedSeverity(finding.Source, string(finding.Severity))
	requiresAttention := contract.requiresSecondAdjudication(severity, string(output.Confidence))
	classification := domain.Classification{
		FindingID: finding.ID, Version: version,
		Materiality: string(output.Materiality), Confidence: string(output.Confidence),
		Note: fmt.Sprintf("producer=%s; %s", result.Producer, output.Note),
	}
	return ClassificationDecision{
		Classification:    classification,
		ReducesWork:       contract.reducesWork(string(output.Materiality), string(output.Confidence)),
		RequiresAttention: requiresAttention, Fallback: result.Fallback,
	}, nil
}

// EvaluateClassification revalidates a persisted annotation before the
// engine consumes its ceiling. Stored strings never become trusted merely by
// surviving reconstruction.
func (c *Client) EvaluateClassification(
	finding domain.Finding, classification domain.Classification,
) (requiresAttention bool, err error) {
	return EvaluateClassifierClassification(finding, classification)
}

// EvaluateClassifierClassification revalidates a classifier annotation
// without requiring a live inference client. Reconstruction and the
// deterministic unavailable-client fallback therefore consume the same
// lattice and producer-label contract as a live site.
func EvaluateClassifierClassification(
	finding domain.Finding, classification domain.Classification,
) (requiresAttention bool, err error) {
	if !Ordinal(classification.Materiality).valid() || !Ordinal(classification.Confidence).valid() {
		return true, errors.New("persisted classification is outside the site lattice")
	}
	producerNote, labeled := strings.CutPrefix(classification.Note, "producer=")
	producer, note, separated := strings.Cut(producerNote, "; ")
	if !labeled || !separated || !strings.Contains(producer, "/") || strings.TrimSpace(note) == "" {
		return true, errors.New("persisted classification lacks a producer-labeled note")
	}
	contract := ClassifierSite(Budget{}).Annotation
	return contract.requiresSecondAdjudication(
		contract.normalizedSeverity(finding.Source, string(finding.Severity)), classification.Confidence,
	), nil
}

// findingLocationText renders a finding's optional location as the annotation
// input string: empty for a nil (review-level) location, otherwise its
// canonical textual form.
func findingLocationText(loc *domain.FindingLocation) string {
	if loc == nil {
		return ""
	}
	return loc.String()
}

func (c *AnnotationContract) reducesWork(materiality, confidence string) bool {
	for _, output := range c.ReducesWork {
		if output.Materiality == materiality && output.Confidence == confidence {
			return true
		}
	}
	return false
}

func (c *AnnotationContract) requiresSecondAdjudication(severity, confidence string) bool {
	ordinal := map[string]int{string(OrdinalLow): 0, string(OrdinalMedium): 1, string(OrdinalHigh): 2}
	for _, rule := range c.SecondAdjudicationRules {
		if rule.NormalizedSeverity == severity && ordinal[confidence] < ordinal[rule.ConfidenceBelow] {
			return true
		}
	}
	return false
}

// ReserveAttention attributes one stable-identity attention sink before it is
// created. Replays of the same item id do not consume the bound again.
func (c *Client) ReserveAttention(siteID, project, root, itemID string) error {
	site, ok := c.sites[siteID]
	if !ok || project == "" || root == "" || itemID == "" {
		return errors.New("invalid inference attention attribution")
	}
	return c.ledger.recordAttention(site, project, root, itemID)
}

func (c *AnnotationContract) normalizedSeverity(source, raw string) string {
	severity := strings.ToLower(strings.TrimSpace(raw))
	for _, mapping := range c.SeverityMappings {
		if mapping.Source == source && mapping.Native == severity {
			return mapping.Normalized
		}
	}
	switch severity {
	case "critical", "high", "medium", "low":
		return severity
	default:
		// A missing or unrecognized scale member is malformed returned data,
		// never permission to bypass the critical/high ceiling.
		return c.UnknownSeverityFallback
	}
}

type diagnosticOutput struct {
	ProbableCause string `json:"probable_cause"`
	Explanation   string `json:"explanation"`
}

// DiagnosticSite declares the first advisory-only site.
func DiagnosticSite(budget Budget) Site {
	return Site{
		ID: DiagnosticSiteID, Authority: AuthorityExplain,
		Fields: []FieldPolicy{
			{Name: "run_id", Sensitivity: SensitivityOperational},
			{Name: "failure_class", Sensitivity: SensitivityOperational},
			{Name: "failing_step", Sensitivity: SensitivityOperational},
			{Name: "reason", Sensitivity: SensitivityRepository},
		},
		FailSafe:  `{"probable_cause":"","explanation":""}`,
		Retention: 14 * 24 * time.Hour, Timeout: 30 * time.Second,
		MaxInputBytes: 64 << 10, MaxOutputBytes: 8 << 10, MaxComputeUnits: 10_000,
		Budget: budget, AuditEvery: 10,
		ValidateOutput: func(data []byte) error {
			var output diagnosticOutput
			if err := decodeStrictObject(data, &output, 8<<10); err != nil {
				return err
			}
			if strings.TrimSpace(output.ProbableCause) == "" || strings.TrimSpace(output.Explanation) == "" {
				return errors.New("empty diagnostic claim")
			}
			return nil
		},
	}
}

// DiagnosticInput contains daemon facts only; the returned claim is stored in
// the advisory lane and is never returned to the policy caller.
type DiagnosticInput struct {
	Project      string
	RootLineage  string
	RunID        string
	FailureClass string
	FailingStep  string
	Reason       string
}

// DiagnoseExecutionFailure writes a labeled advisory claim. Inference-down or
// invalid output skips the claim and returns nil so the control plane remains
// operable.
func (c *Client) DiagnoseExecutionFailure(ctx context.Context, input DiagnosticInput) error {
	result, err := c.Call(ctx, DiagnosticSiteID, input.Project, input.RootLineage, map[string]InputField{
		"run_id":        {Value: input.RunID, Sensitivity: SensitivityOperational},
		"failure_class": {Value: input.FailureClass, Sensitivity: SensitivityOperational},
		"failing_step":  {Value: input.FailingStep, Sensitivity: SensitivityOperational},
		"reason":        {Value: input.Reason, Sensitivity: SensitivityRepository},
	})
	if err != nil || result.Fallback {
		return err
	}
	var output diagnosticOutput
	if err := json.Unmarshal(result.Output, &output); err != nil {
		return err
	}
	created := c.now().UTC()
	body := "Probable cause: " + output.ProbableCause + "\n\n" + output.Explanation
	id := contentaddr.Sum([]byte("diagnostic_claim\x00" + DiagnosticSiteID + "\x00" + result.InputDigest + "\x00" + created.Format(time.RFC3339Nano)))
	return c.advisory.Append(ctx, advisory.Entry{
		ID: id, RootLineage: input.RootLineage, Site: DiagnosticSiteID,
		Producer: result.Producer, Kind: "diagnostic_claim", InputDigest: result.InputDigest,
		Body: body, CreatedAt: created, RetainUntil: created.Add(c.sites[DiagnosticSiteID].Retention),
	})
}

type discussionOutput struct {
	Reply string `json:"reply"`
}

// DiscussionSite declares the advisory-only producer for attention-item
// conversation replies. Its inputs are daemon facts and the immutable
// conversation prefix; its output cannot feed policy or mutate a decision set.
func DiscussionSite(budget Budget) Site {
	return Site{
		ID: AttentionDiscussionSiteID, Authority: AuthorityExplain,
		Fields: []FieldPolicy{
			{Name: "item_type", Sensitivity: SensitivityOperational},
			{Name: "reason", Sensitivity: SensitivityRepository},
			{Name: "card_facts", Sensitivity: SensitivityRepository},
			{Name: "conversation", Sensitivity: SensitivityRepository},
		},
		FailSafe:  `{"reply":""}`,
		Retention: 14 * 24 * time.Hour, Timeout: 30 * time.Second,
		MaxInputBytes: 64 << 10, MaxOutputBytes: 8 << 10, MaxComputeUnits: 10_000,
		Budget: budget, AuditEvery: 10,
		ValidateOutput: func(data []byte) error {
			var output discussionOutput
			if err := decodeStrictObject(data, &output, 8<<10); err != nil {
				return err
			}
			if strings.TrimSpace(output.Reply) == "" || len(output.Reply) > 8<<10 {
				return errors.New("empty or oversized discussion reply")
			}
			if importer.ContainsSecret([]byte(output.Reply)) {
				return errors.New("credential-shaped discussion reply")
			}
			return nil
		},
	}
}

// DiscussionInput contains daemon facts and one immutable conversation
// prefix. The returned reply is written only to the conversation and the
// advisory store by their respective callers.
type DiscussionInput struct {
	Project      string
	RootLineage  string
	ItemType     string
	Reason       string
	CardFacts    string
	Conversation string
}

// DiscussAttentionItem produces and records one producer-labeled advisory
// reply. Fallback remains explicit so the engine can use its fixed fail-safe
// human-facing text while preserving the site's unavailable result.
func (c *Client) DiscussAttentionItem(
	ctx context.Context, input DiscussionInput,
) (reply string, fallback bool, err error) {
	fields := map[string]InputField{
		"item_type":    {Value: input.ItemType, Sensitivity: SensitivityOperational},
		"reason":       {Value: input.Reason, Sensitivity: SensitivityRepository},
		"card_facts":   {Value: input.CardFacts, Sensitivity: SensitivityRepository},
		"conversation": {Value: input.Conversation, Sensitivity: SensitivityRepository},
	}
	result, callErr := c.Call(ctx, AttentionDiscussionSiteID, input.Project, input.RootLineage, fields)
	if callErr != nil && !result.Fallback {
		return "", false, callErr
	}
	var output discussionOutput
	if err := json.Unmarshal(result.Output, &output); err != nil {
		return "", false, fmt.Errorf("decode validated discussion output: %w", err)
	}
	// The fixed fail-safe is engine-owned and must remain deliverable when the
	// advisory store is the unavailable dependency that caused the fallback.
	if result.Fallback {
		return output.Reply, true, nil
	}
	inputDigest := result.InputDigest
	if inputDigest == "" {
		canonical, marshalErr := json.Marshal(map[string]string{
			"item_type": input.ItemType, "reason": input.Reason,
			"card_facts": input.CardFacts, "conversation": input.Conversation,
		})
		if marshalErr != nil {
			return "", false, marshalErr
		}
		inputDigest = contentaddr.Sum(canonical)
	}
	created := c.now().UTC()
	id := contentaddr.Sum([]byte("discussion_reply\x00" + AttentionDiscussionSiteID + "\x00" + inputDigest + "\x00" + created.Format(time.RFC3339Nano)))
	if err := c.advisory.Append(ctx, advisory.Entry{
		ID: id, RootLineage: input.RootLineage, Site: AttentionDiscussionSiteID,
		Producer: result.Producer, Kind: "discussion_reply", InputDigest: inputDigest,
		Body: output.Reply, CreatedAt: created,
		RetainUntil: created.Add(c.sites[AttentionDiscussionSiteID].Retention),
	}); err != nil {
		return "", false, err
	}
	return output.Reply, result.Fallback, nil
}
