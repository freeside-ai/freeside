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
)

const (
	ClassifierSiteID = "finding_classifier"
	DiagnosticSiteID = "execution_diagnostic"
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
	if !Ordinal(classification.Materiality).valid() || !Ordinal(classification.Confidence).valid() {
		return true, errors.New("persisted classification is outside the site lattice")
	}
	producerNote, labeled := strings.CutPrefix(classification.Note, "producer=")
	producer, note, separated := strings.Cut(producerNote, "; ")
	if !labeled || !separated || !strings.Contains(producer, "/") || strings.TrimSpace(note) == "" {
		return true, errors.New("persisted classification lacks a producer-labeled note")
	}
	contract := c.sites[ClassifierSiteID].Annotation
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
