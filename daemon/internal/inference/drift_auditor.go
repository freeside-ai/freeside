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

const DriftAuditorSiteID = "drift_auditor"

// ErrDriftAuditNotAvailable is the typed fail-safe result for a missing,
// malformed, over-budget, or otherwise unavailable drift-auditor response. No
// artifact exists for a failed audit, so the failure can never count as a
// run's first over_hardened verdict (plan §7 Review Drift, Fail-safe default).
var ErrDriftAuditNotAvailable = errors.New("drift audit unavailable")

// driftAuditFailSafe carries no verdict, so it fails the site's own output
// schema: the fallback bytes can never be parsed into an artifact.
const driftAuditFailSafe = `{"verdict":null}`

// DriftAuditorInput is the allowlisted input to one drift audit. It
// intentionally carries no implementer reasoning history. The caller loads
// every field from daemon records; none is model output.
type DriftAuditorInput struct {
	// RunID and Round identify the audited review round. Both are sent and
	// both bind the artifact.
	RunID domain.RunID
	Round int
	// BaseSHA and HeadSHA are the round's bound base and candidate head from
	// its review record. They bind the artifact and are not sent as fields of
	// their own; DiffMetrics already names both commits.
	BaseSHA string
	HeadSHA string
	// ApprovedSpecDigest and ApprovedSpecification are the run's approved
	// specification and its content address.
	ApprovedSpecDigest    domain.Digest
	ApprovedSpecification string
	// InstructionSnapshotDigest and InstructionSnapshot are the repository
	// instructions the run was admitted under.
	InstructionSnapshotDigest domain.Digest
	InstructionSnapshot       string
	// ResolvedPolicyDigest is the run's resolved policy. It is sent and binds
	// the artifact.
	ResolvedPolicyDigest domain.Digest
	// DeclaredPaths is the work unit's declared scope.
	DeclaredPaths []string
	// RoundOneDiff is the bound base against the round-1 candidate head, and
	// CurrentDiff the bound base against this round's. Both are engine-computed
	// patches, sent whole: an input over the site's limit fails safe instead of
	// being shortened.
	RoundOneDiff string
	CurrentDiff  string
	// Dispositions and AdjudicationEntries are the run's review history so far.
	// A reversal may cite only a finding one of them names.
	Dispositions        []domain.ReviewDispositionRecord
	AdjudicationEntries []domain.FindingAdjudicationEntry
	// DiffMetrics is the round's diff shape, nil when nothing was recorded for
	// the round. It is sent as diff_metrics (null for a gap).
	DiffMetrics *domain.ReviewRoundDiffMetrics
}

// driftAuditorOutput is the whole model response. Every field is a pointer so
// an omitted field is distinguishable from an empty one.
type driftAuditorOutput struct {
	Verdict     *domain.DriftVerdict           `json:"verdict"`
	Confidence  *domain.AdjudicationConfidence `json:"confidence"`
	Reversals   *[]domain.DriftReversal        `json:"reversals"`
	Explanation *string                        `json:"explanation"`
}

// decodeDriftAuditorOutput is the site's whole output schema. The strict
// decoder alone is not enough: encoding/json matches object keys to struct
// fields case-insensitively, so "Verdict" beside "verdict" is neither an
// unknown field nor a byte-exact duplicate, and the later one would win. The
// exact-key check requires the response and each reversal to carry their keys
// byte for byte and no others.
func decodeDriftAuditorOutput(data []byte) (driftAuditorOutput, error) {
	var output driftAuditorOutput
	if err := decodeStrictObject(data, &output, domain.MaxDriftAuditBytes); err != nil {
		return driftAuditorOutput{}, err
	}
	var object struct {
		Reversals []map[string]json.RawMessage `json:"reversals"`
	}
	var keys map[string]json.RawMessage
	if err := errors.Join(json.Unmarshal(data, &keys), json.Unmarshal(data, &object)); err != nil {
		return driftAuditorOutput{}, err
	}
	if !hasExactKeys(keys, "verdict", "confidence", "reversals", "explanation") {
		return driftAuditorOutput{}, errors.New("drift auditor output is not exactly its four fields")
	}
	for _, reversal := range object.Reversals {
		if !hasExactKeys(reversal, "finding_id", "undo", "rationale") {
			return driftAuditorOutput{}, errors.New("drift auditor reversal is not exactly its three fields")
		}
	}
	if output.Verdict == nil || output.Confidence == nil || output.Reversals == nil || output.Explanation == nil {
		return driftAuditorOutput{}, errors.New("drift auditor output omits a field")
	}
	if !slices.Contains(domain.AllDriftVerdicts, *output.Verdict) {
		return driftAuditorOutput{}, errors.New("drift auditor verdict is outside the lattice")
	}
	if !slices.Contains(domain.AllAdjudicationConfidences, *output.Confidence) {
		return driftAuditorOutput{}, errors.New("drift auditor confidence is outside the lattice")
	}
	return output, nil
}

func hasExactKeys(object map[string]json.RawMessage, names ...string) bool {
	if len(object) != len(names) {
		return false
	}
	for _, name := range names {
		if _, ok := object[name]; !ok {
			return false
		}
	}
	return true
}

// DriftAuditorSite declares the third ceiling-bounded annotation site.
func DriftAuditorSite(budget Budget) Site {
	classifier := ClassifierSite(Budget{}).Annotation
	contract := &DriftAuditContract{
		Verdicts:                   driftAuditVerdicts(),
		Confidence:                 driftAuditConfidences(),
		ReducesWork:                []string{string(domain.DriftVerdictOverHardened)},
		SeverityMappings:           slices.Clone(classifier.SeverityMappings),
		UnknownSeverityFallback:    classifier.UnknownSeverityFallback,
		NormalizedSeverityCeilings: slices.Clone(classifier.NormalizedSeverityCeilings),
		SecondAdjudicationRules:    slices.Clone(classifier.SecondAdjudicationRules),
	}
	return Site{
		ID: DriftAuditorSiteID, Authority: AuthorityAnnotate,
		Fields: []FieldPolicy{
			{Name: "run_id", Sensitivity: SensitivityOperational},
			{Name: "round", Sensitivity: SensitivityOperational},
			{Name: "approved_spec_digest", Sensitivity: SensitivityOperational},
			{Name: "approved_spec", Sensitivity: SensitivityRepository},
			{Name: "instruction_snapshot_digest", Sensitivity: SensitivityOperational},
			{Name: "instruction_snapshot", Sensitivity: SensitivityRepository},
			{Name: "resolved_policy_digest", Sensitivity: SensitivityOperational},
			{Name: "declared_paths", Sensitivity: SensitivityRepository},
			{Name: "round_one_diff", Sensitivity: SensitivityRepository},
			{Name: "current_diff", Sensitivity: SensitivityRepository},
			{Name: "disposition_history", Sensitivity: SensitivityRepository},
			{Name: "adjudication_entries", Sensitivity: SensitivityRepository},
			// Counts and commit ids only; no repository text.
			{Name: "diff_metrics", Sensitivity: SensitivityOperational},
		},
		// The limits are the adjudicator's, so the composition's shared per-root
		// allowance (sized to the largest site bound) still holds.
		FailSafe: driftAuditFailSafe, Retention: 30 * 24 * time.Hour, Timeout: 120 * time.Second,
		MaxInputBytes: 2 << 20, MaxOutputBytes: domain.MaxDriftAuditBytes,
		MaxComputeUnits: 10_000, Budget: budget, AuditEvery: 10,
		DriftAudit: contract,
		ValidateOutput: func(data []byte) error {
			_, err := decodeDriftAuditorOutput(data)
			return err
		},
	}
}

// AuditDrift obtains one drift audit for a review round and returns it as a
// digest-addressed artifact bound to the input's run, round, commits, and
// digests. The caller passes the run id as root, which charges the call to
// the run's lineage. Every failure returns ErrDriftAuditNotAvailable and the
// zero artifact, so callers cannot confuse fail-safe bytes with a verdict; a
// low-confidence verdict is a valid artifact, and routing it is the caller's
// decision. The site makes one call and never retries.
func (c *Client) AuditDrift(
	ctx context.Context, project, root string, input DriftAuditorInput,
) (domain.DriftAudit, error) {
	declaredPaths, err := driftAuditJSON(input.DeclaredPaths)
	if err != nil {
		return domain.DriftAudit{}, err
	}
	dispositions, err := driftAuditJSON(input.Dispositions)
	if err != nil {
		return domain.DriftAudit{}, err
	}
	entries, err := driftAuditJSON(input.AdjudicationEntries)
	if err != nil {
		return domain.DriftAudit{}, err
	}
	diffMetrics, err := driftAuditJSON(input.DiffMetrics)
	if err != nil {
		return domain.DriftAudit{}, err
	}
	result, err := c.Call(ctx, DriftAuditorSiteID, project, root, map[string]InputField{
		"run_id":                      {Value: string(input.RunID), Sensitivity: SensitivityOperational},
		"round":                       {Value: fmt.Sprint(input.Round), Sensitivity: SensitivityOperational},
		"approved_spec_digest":        {Value: string(input.ApprovedSpecDigest), Sensitivity: SensitivityOperational},
		"approved_spec":               {Value: input.ApprovedSpecification, Sensitivity: SensitivityRepository},
		"instruction_snapshot_digest": {Value: string(input.InstructionSnapshotDigest), Sensitivity: SensitivityOperational},
		"instruction_snapshot":        {Value: input.InstructionSnapshot, Sensitivity: SensitivityRepository},
		"resolved_policy_digest":      {Value: string(input.ResolvedPolicyDigest), Sensitivity: SensitivityOperational},
		"declared_paths":              {Value: declaredPaths, Sensitivity: SensitivityRepository},
		"round_one_diff":              {Value: input.RoundOneDiff, Sensitivity: SensitivityRepository},
		"current_diff":                {Value: input.CurrentDiff, Sensitivity: SensitivityRepository},
		"disposition_history":         {Value: dispositions, Sensitivity: SensitivityRepository},
		"adjudication_entries":        {Value: entries, Sensitivity: SensitivityRepository},
		"diff_metrics":                {Value: diffMetrics, Sensitivity: SensitivityOperational},
	})
	if err != nil {
		return domain.DriftAudit{}, errors.Join(ErrDriftAuditNotAvailable, err)
	}
	if result.Fallback {
		return domain.DriftAudit{}, fmt.Errorf("%w: %s", ErrDriftAuditNotAvailable, result.Reason)
	}
	audit, err := driftAuditFromOutput(result.Output, input, c.now().UTC())
	if err != nil {
		return domain.DriftAudit{}, errors.Join(ErrDriftAuditNotAvailable, err)
	}
	return audit, nil
}

// driftAuditFromOutput turns a schema-validated response into the artifact.
// The response supplies only the verdict, confidence, reversals, and
// explanation; every binding field comes from the input. It re-runs the
// schema check instead of trusting that the caller did.
func driftAuditFromOutput(
	body []byte, input DriftAuditorInput, createdAt time.Time,
) (domain.DriftAudit, error) {
	output, err := decodeDriftAuditorOutput(body)
	if err != nil {
		return domain.DriftAudit{}, err
	}
	// A reversal may cite only a finding the site was shown. The store
	// re-checks each id against the run's review records when the artifact is
	// stored; this check keeps an invented identity from becoming a verdict.
	shown := make(map[domain.FindingID]bool, len(input.Dispositions)+len(input.AdjudicationEntries))
	for _, disposition := range input.Dispositions {
		shown[disposition.FindingID] = true
	}
	for _, entry := range input.AdjudicationEntries {
		shown[entry.FindingID] = true
	}
	for _, reversal := range *output.Reversals {
		if !shown[reversal.FindingID] {
			return domain.DriftAudit{}, errors.New("drift auditor reversal cites a finding it was not shown")
		}
	}
	// NewDriftAudit enforces the rest: the reversal count each verdict
	// requires, distinct finding ids, and nonempty undo, rationale, and
	// explanation text.
	audit, err := domain.NewDriftAudit(domain.DriftAuditInput{
		RunID: input.RunID, Round: input.Round, BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA,
		ApprovedSpecDigest: input.ApprovedSpecDigest, ResolvedPolicyDigest: input.ResolvedPolicyDigest,
		Verdict: *output.Verdict, Confidence: *output.Confidence,
		Reversals: *output.Reversals, Explanation: *output.Explanation,
		CreatedAt: createdAt,
	})
	if err != nil {
		return domain.DriftAudit{}, err
	}
	// The binding fields make the artifact larger than the response, and the
	// store refuses an encoding over the bound.
	encoded, err := audit.Encode()
	if err != nil {
		return domain.DriftAudit{}, err
	}
	if len(encoded) > domain.MaxDriftAuditBytes {
		return domain.DriftAudit{}, errors.New("drift audit artifact exceeds its size bound")
	}
	return audit, nil
}

func driftAuditVerdicts() []string {
	verdicts := make([]string, 0, len(domain.AllDriftVerdicts))
	for _, verdict := range domain.AllDriftVerdicts {
		verdicts = append(verdicts, string(verdict))
	}
	return verdicts
}

func driftAuditConfidences() []string {
	confidences := make([]string, 0, len(domain.AllAdjudicationConfidences))
	for _, confidence := range domain.AllAdjudicationConfidences {
		confidences = append(confidences, string(confidence))
	}
	return confidences
}

func driftAuditJSON(value any) (string, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return "", errors.Join(ErrDriftAuditNotAvailable, fmt.Errorf("encode drift-auditor input: %w", err))
	}
	return string(body), nil
}
