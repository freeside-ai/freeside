package engine

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const (
	shadowReviewRatePolicyKey = "telemetry.shadow_review_rate"
	shadowReviewWait          = 15 * time.Minute
)

var (
	errShadowReviewBlocksReady = errors.New("credible shadow review finding blocks ready")
	errShadowReviewStopped     = errors.New("operator stopped after credible shadow review finding")
)

// ProductionShadowReviewInvocationID gives the shadow arm a namespace that
// can never collide with routed review evidence for the same run and round.
func ProductionShadowReviewInvocationID(runID domain.RunID, round int) domain.InvocationID {
	sum := sha256.Sum256([]byte(fmt.Sprintf("shadow\x00%s\x00%d", runID, round)))
	return domain.InvocationID(fmt.Sprintf("shadow-review-%x", sum[:12]))
}

func shadowReviewAttentionItemID(id domain.InvocationID) domain.ItemID {
	sum := sha256.Sum256([]byte("shadow-attention\x00" + string(id)))
	return domain.ItemID(fmt.Sprintf("item-shadow-review-%x", sum[:12]))
}

// resolvedShadowReviewRate treats malformed policy as unavailable and uses
// the composed default. Numeric overrides are bounded to the declared [0,1]
// probability range rather than becoming an unbounded policy effect.
func resolvedShadowReviewRate(policy domain.ResolvedPolicy, fallback float64) float64 {
	for _, key := range policy.Keys {
		if key.Key != shadowReviewRatePolicyKey {
			continue
		}
		rate, err := strconv.ParseFloat(strings.TrimSpace(key.Value), 64)
		if err != nil || math.IsNaN(rate) || math.IsInf(rate, 0) {
			return fallback
		}
		return min(max(rate, 0), 1)
	}
	return fallback
}

func shadowReviewSelected(runID domain.RunID, round int, rate float64) bool {
	switch {
	case rate <= 0:
		return false
	case rate >= 1:
		return true
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("shadow-rate-v1\x00%s\x00%d", runID, round)))
	// Use the high 53 bits so conversion to float64 is exact and stable.
	draw := float64(binary.BigEndian.Uint64(sum[:8])>>11) / float64(uint64(1)<<53)
	return draw < rate
}

// reconcileShadowReview advances one selected observation-only pass. A
// source failure is classified, reported, and abandoned without changing the
// routed gate. Store and trust-boundary failures still fail loudly because
// silently losing a completed shadow result would invalidate the ceiling.
func (w *productionPublicationWorkflow) reconcileShadowReview(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	checkpoint productionVerificationCheckpoint,
	workspace PublicationCheckout,
	instructions exec.ReviewInstructionBinding,
	routed domain.ReviewRecord,
) (bool, error) {
	id := ProductionShadowReviewInvocationID(task.RunID, routed.Round)
	var existing domain.ShadowReviewRecord
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		existing, err = tx.GetShadowReviewRecord(ctx, id)
		return err
	}); err == nil {
		// The persisted record crossed the source/configuration trust boundary
		// before commit. Re-gate its routed bindings here, but do not require a
		// later process to compose the same optional source configuration.
		if existing.RunID != task.RunID || existing.ShadowedRound != routed.Round ||
			existing.Source != domain.ShadowReviewClaudeLocal ||
			existing.InstructionDigest != instructions.ResultDigest ||
			existing.BaseSHA != routed.BaseSHA || existing.HeadSHA != routed.HeadSHA {
			return false, fmt.Errorf("shadow review %q binding: %w", id, domain.ErrParentKeyMismatch)
		}
		if err := w.finishShadowReviewEvidence(ctx, task, routed, existing); err != nil {
			return false, err
		}
		return true, w.removeReviewWorkspace(id)
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, err
	}
	// A completed result is durable independently of whether a later process
	// composes or selects the optional arm. Recover its classifier samples and
	// safety gate before applying current launch policy, closing the crash
	// window between record persistence and evidence finalization.
	if w.shadowReviewSource == nil || w.holdOnly ||
		!shadowReviewSelected(task.RunID, routed.Round,
			resolvedShadowReviewRate(binding.resolvedPolicy, w.shadowReviewDefaultRate)) {
		return true, nil
	}

	req, authority, err := w.productionReviewRequest(
		task, binding, checkpoint, id, routed.Round, instructions,
	)
	if err != nil {
		return false, err
	}
	authorityVerifier, ok := w.shadowReviewSource.(exec.ReviewRequestAuthorityVerifier)
	if !ok {
		return w.abandonShadowReview(task, routed.Round, id, &exec.ReviewSourceFailure{
			Class: domain.ReviewFailureConfiguration,
			Err:   errors.New("shadow review source cannot verify request authority"),
		})
	}
	if supersessionVerifier, ok := w.shadowReviewSource.(exec.ReviewRequestSupersessionVerifier); ok {
		if err := supersessionVerifier.VerifyReviewRequestSupersession(ctx, id, req); err != nil &&
			!errors.Is(err, exec.ErrUnknownInvocation) {
			return w.abandonStartedShadowReview(
				ctx, task, routed.Round, id, authorityVerifier, err,
			)
		}
	}
	if err := authorityVerifier.VerifyRequestAuthority(ctx, id, authority); err != nil &&
		!errors.Is(err, exec.ErrUnknownInvocation) {
		return w.abandonStartedShadowReview(
			ctx, task, routed.Round, id, authorityVerifier, err,
		)
	}
	status, err := w.shadowReviewSource.Inspect(ctx, id)
	if errors.Is(err, exec.ErrUnknownInvocation) {
		if !w.now().Before(routed.CompletedAt.Add(shadowReviewWait)) {
			return w.abandonTimedOutShadowReview(
				ctx, task, routed.Round, id, authorityVerifier, "shadow review launch window expired",
			)
		}
		retained, retainErr := w.ensureReviewWorkspace(ctx, id, workspace, task.HeadSHA)
		if retainErr != nil {
			return w.abandonShadowReview(task, routed.Round, id, retainErr)
		}
		if retained != req.Workspace {
			return false, fmt.Errorf("retained shadow review workspace changed: %w",
				domain.ErrPathBoundaryMismatch)
		}
		if err := w.shadowReviewSource.RequestReview(ctx, id, req); err != nil {
			return w.abandonStartedShadowReview(
				ctx, task, routed.Round, id, authorityVerifier, err,
			)
		}
		status, err = w.shadowReviewSource.Inspect(ctx, id)
	}
	if err != nil {
		return w.abandonStartedShadowReview(
			ctx, task, routed.Round, id, authorityVerifier, err,
		)
	}
	if status == exec.StatusPending || status == exec.StatusRunning {
		if !w.now().Before(routed.CompletedAt.Add(shadowReviewWait)) {
			return w.abandonTimedOutShadowReview(
				ctx, task, routed.Round, id, authorityVerifier,
				"shadow review exceeded its observation window",
			)
		}
		return false, nil
	}
	if !w.now().Before(routed.CompletedAt.Add(shadowReviewWait)) {
		return w.abandonTimedOutShadowReview(
			ctx, task, routed.Round, id, authorityVerifier,
			"shadow review completed outside its observation window",
		)
	}
	result, err := w.shadowReviewSource.Poll(ctx, id)
	if errors.Is(err, exec.ErrResultNotReady) {
		if !w.now().Before(routed.CompletedAt.Add(shadowReviewWait)) {
			return w.abandonTimedOutShadowReview(
				ctx, task, routed.Round, id, authorityVerifier,
				"shadow review result exceeded its observation window",
			)
		}
		return false, nil
	}
	if err != nil {
		if errors.Is(err, exec.ErrNoResult) {
			err = normalizeTerminalReviewFailure(err)
		}
		return w.abandonStartedShadowReview(
			ctx, task, routed.Round, id, authorityVerifier, err,
		)
	}
	if !w.now().Before(routed.CompletedAt.Add(shadowReviewWait)) {
		return w.abandonTimedOutShadowReview(
			ctx, task, routed.Round, id, authorityVerifier,
			"shadow review result arrived outside its observation window",
		)
	}
	if err := result.Validate(); err != nil {
		return w.abandonStartedShadowReview(ctx, task, routed.Round, id, authorityVerifier,
			&exec.ReviewSourceFailure{
				Class: domain.ReviewFailureContradiction, Err: err,
			})
	}
	if result.InvocationID != id || result.BaseSHA != routed.BaseSHA ||
		result.HeadSHA != routed.HeadSHA ||
		result.ConfigurationDigest != w.shadowReviewConfigurationDigest ||
		result.InstructionDigest != instructions.ResultDigest ||
		result.CostOwner != w.shadowReviewCostOwner {
		return w.abandonStartedShadowReview(ctx, task, routed.Round, id, authorityVerifier,
			&exec.ReviewSourceFailure{
				Class: domain.ReviewFailureContradiction, Err: domain.ErrParentKeyMismatch,
			})
	}
	if err := authorityVerifier.VerifyRequestAuthority(ctx, id, authority); err != nil {
		return w.abandonStartedShadowReview(
			ctx, task, routed.Round, id, authorityVerifier, err,
		)
	}
	if err := w.shadowReviewSource.Verify(ctx, id, routed.BaseSHA, routed.HeadSHA); err != nil {
		return w.abandonStartedShadowReview(
			ctx, task, routed.Round, id, authorityVerifier, err,
		)
	}
	findingIDs := make([]domain.FindingID, len(result.Findings))
	for i, finding := range result.Findings {
		if finding.RunID != task.RunID {
			return w.abandonStartedShadowReview(ctx, task, routed.Round, id, authorityVerifier,
				&exec.ReviewSourceFailure{
					Class: domain.ReviewFailureContradiction, Err: domain.ErrParentKeyMismatch,
				})
		}
		if err := domain.ValidateShadowReviewFinding(domain.ShadowReviewClaudeLocal, finding); err != nil {
			return w.abandonStartedShadowReview(ctx, task, routed.Round, id, authorityVerifier,
				&exec.ReviewSourceFailure{
					Class: domain.ReviewFailureContradiction, Err: err,
				})
		}
		findingIDs[i] = finding.ID
	}
	outcome := domain.ReviewClean
	if len(findingIDs) > 0 {
		outcome = domain.ReviewFindings
	}
	record, err := domain.NewShadowReviewRecord(domain.ShadowReviewRecord{
		InvocationID: id, RunID: task.RunID, ShadowedRound: routed.Round,
		Source: domain.ShadowReviewClaudeLocal, Provider: result.Provider,
		ModelConfiguration:  result.ModelConfiguration,
		ConfigurationDigest: result.ConfigurationDigest,
		InstructionDigest:   result.InstructionDigest, CostOwner: result.CostOwner,
		BaseSHA: result.BaseSHA, HeadSHA: result.HeadSHA,
		CompletedAt: result.CompletedAt, CompletionEvidence: result.CompletionEvidence,
		Outcome: outcome, FindingIDs: findingIDs,
	})
	if err != nil {
		return w.abandonStartedShadowReview(ctx, task, routed.Round, id, authorityVerifier,
			&exec.ReviewSourceFailure{
				Class: domain.ReviewFailureContradiction, Err: err,
			})
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutShadowReviewRecord(ctx, record, result.Findings)
	}); err != nil {
		return false, err
	}
	if err := w.finishShadowReviewEvidence(ctx, task, routed, record); err != nil {
		return false, err
	}
	return true, w.removeReviewWorkspace(id)
}

func (w *productionPublicationWorkflow) abandonShadowReview(
	task productionPublicationTask, round int, id domain.InvocationID, cause error,
) (bool, error) {
	class := exec.ClassifyReviewSourceFailure(cause)
	w.shadowReviewFailure(task.RunID, round, class, cause)
	if err := w.removeReviewWorkspace(id); err != nil {
		return false, err
	}
	return true, nil
}

func (w *productionPublicationWorkflow) abandonTimedOutShadowReview(
	ctx context.Context,
	task productionPublicationTask,
	round int,
	id domain.InvocationID,
	verifier exec.ReviewRequestAuthorityVerifier,
	reason string,
) (bool, error) {
	return w.abandonStartedShadowReview(ctx, task, round, id, verifier,
		&exec.ReviewSourceFailure{
			Class: domain.ReviewFailureTransient,
			Err:   errors.New(reason),
		})
}

func (w *productionPublicationWorkflow) abandonStartedShadowReview(
	ctx context.Context,
	task productionPublicationTask,
	round int,
	id domain.InvocationID,
	verifier exec.ReviewRequestAuthorityVerifier,
	cause error,
) (bool, error) {
	// The authority verifier's contract includes durable teardown before it
	// rejects a mismatched authority. Use that existing source boundary to reap
	// a credential-bearing topology instead of merely ceasing to poll it.
	sum := sha256.Sum256([]byte("shadow-review-abort\x00" + string(id)))
	abortAuthority := domain.Digest(contentaddr.Format(sum[:]))
	if err := verifier.VerifyRequestAuthority(ctx, id, abortAuthority); err == nil {
		return false, errors.New("shadow review source accepted abort authority")
	} else if !errors.Is(err, exec.ErrUnknownInvocation) &&
		!errors.Is(err, domain.ErrParentKeyMismatch) {
		return false, err
	}
	return w.abandonShadowReview(task, round, id, cause)
}

func (w *productionPublicationWorkflow) finishShadowReviewEvidence(
	ctx context.Context,
	task productionPublicationTask,
	routed domain.ReviewRecord,
	record domain.ShadowReviewRecord,
) error {
	var existingSamples []domain.ClassifierAccuracySample
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		existingSamples, err = tx.ListClassifierAccuracySamples(ctx, task.RunID)
		return err
	}); err != nil {
		return err
	}
	existing := make(map[string]struct{}, len(existingSamples))
	for _, sample := range existingSamples {
		existing[classifierSampleKey(sample.ShadowInvocationID, sample.FindingID,
			sample.ClassificationVersion)] = struct{}{}
	}

	routedFindings, routedClassifications, err := w.reviewComparisonTrace(ctx, routed)
	if err != nil {
		return err
	}
	needsAttention := false
	attentionFindingCount := 0
	attentionClaims := make([]domain.AgentClaim, 0, len(record.FindingIDs))
	for _, findingID := range record.FindingIDs {
		finding, classification, err := w.classifyShadowFinding(ctx, task, record, findingID)
		if err != nil {
			return err
		}
		findingNeedsAttention, err := shadowReviewFindingNeedsAttention(finding, classification)
		if err != nil {
			return err
		}
		needsAttention = needsAttention || findingNeedsAttention
		if findingNeedsAttention {
			attentionFindingCount++
			claims, err := shadowReviewFindingClaims(record, finding)
			if err != nil {
				return err
			}
			attentionClaims = append(attentionClaims, claims...)
		}
		key := classifierSampleKey(record.InvocationID, finding.ID, classification.Version)
		if _, found := existing[key]; found {
			continue
		}
		if err := w.classifyMatchingRoutedFindings(
			ctx, task, finding, routed.Round, routedFindings, routedClassifications,
		); err != nil {
			return err
		}
		sample := domain.ClassifierAccuracySample{
			RunID: task.RunID, FindingID: finding.ID,
			ClassificationVersion: classification.Version,
			ShadowInvocationID:    record.InvocationID,
			Assessment: compareClassifierTrace(
				finding, classification, routedFindings, routedClassifications),
			RecordedAt: w.now().UTC(),
		}
		if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
			return tx.PutClassifierAccuracySample(ctx, sample)
		}); err != nil {
			return err
		}
	}
	if !needsAttention {
		return nil
	}
	itemID := shadowReviewAttentionItemID(record.InvocationID)
	if w.inference != nil {
		if err := w.inference.ReserveAttention(
			inference.ClassifierSiteID, string(task.ProjectID), string(task.RunID), string(itemID)); err != nil {
			return err
		}
	}
	reason := fmt.Sprintf(
		"%d observation-only shadow finding(s) require a bound Approve, Discuss, or Stop decision before ready status; routed review remains authoritative.",
		attentionFindingCount,
	)
	return w.putReviewAttentionWithActionsAndID(ctx, task, domain.ReviewRecord{
		InvocationID: record.InvocationID, RunID: record.RunID, Round: record.ShadowedRound,
		BaseSHA: record.BaseSHA, HeadSHA: record.HeadSHA,
	}, reason,
		domain.AttentionReviewDispute, itemID,
		[]domain.Action{domain.ActionApprove, domain.ActionDiscuss, domain.ActionStop},
		attentionClaims)
}

func shadowReviewFindingClaims(
	record domain.ShadowReviewRecord, finding domain.Finding,
) ([]domain.AgentClaim, error) {
	location := "review-level"
	if finding.Location != nil {
		location = finding.Location.String()
	}
	content := fmt.Sprintf(
		"**Finding ID:** `%s`\n\n**Severity:** %s\n\n**Location:** `%s`\n\n**Reviewer text**\n\n%s",
		finding.ID, finding.Severity, location, finding.RawText,
	)
	if finding.Message != finding.RawText {
		content = fmt.Sprintf(
			"**Finding ID:** `%s`\n\n**Severity:** %s\n\n**Location:** `%s`\n\n**Summary:** %s\n\n**Full reviewer text**\n\n%s",
			finding.ID, finding.Severity, location, finding.Message, finding.RawText,
		)
	}
	if !utf8.ValidString(content) {
		return nil, domain.ErrClaimTextNotUTF8
	}
	chunks := splitShadowClaimText(content, domain.MaxClaimTextBytes)
	identity := sha256.Sum256([]byte(string(record.InvocationID) + "\x00" + string(finding.ID)))
	claims := make([]domain.AgentClaim, len(chunks))
	for idx, chunk := range chunks {
		text := domain.ClaimText{MediaType: domain.MediaTypeTextMarkdown, Content: chunk}
		label := fmt.Sprintf("Shadow finding %s", finding.ID)
		if len(chunks) > 1 {
			label = fmt.Sprintf("%s (part %d/%d)", label, idx+1, len(chunks))
		}
		claims[idx] = domain.AgentClaim{
			Label: label,
			Artifact: domain.ArtifactID(fmt.Sprintf(
				"shadow-finding-%x-%d", identity[:8], idx+1,
			)),
			Digest: text.ComputeDigest(),
			Provenance: domain.Provenance{
				ProducerClass: domain.ProducerAgent, ProducerInvocationID: record.InvocationID,
				HeadBinding: domain.HeadBound, SourceHeadSHA: record.HeadSHA,
				SensitivityClass: domain.SensitivitySensitive,
			},
			Text: &text,
		}
	}
	return claims, nil
}

func splitShadowClaimText(content string, maximum int) []string {
	chunks := make([]string, 0, 1+len(content)/maximum)
	for len(content) > maximum {
		end := maximum
		for end > 0 && !utf8.RuneStart(content[end]) {
			end--
		}
		chunks = append(chunks, content[:end])
		content = content[end:]
	}
	return append(chunks, content)
}

func classifierSampleKey(id domain.InvocationID, findingID domain.FindingID, version int) string {
	return fmt.Sprintf("%s\x00%s\x00%d", id, findingID, version)
}

func (w *productionPublicationWorkflow) classifyShadowFinding(
	ctx context.Context, task productionPublicationTask, record domain.ShadowReviewRecord,
	findingID domain.FindingID,
) (domain.Finding, domain.Classification, error) {
	return w.classifySampleFinding(ctx, task, findingID, record.ShadowedRound)
}

func (w *productionPublicationWorkflow) classifySampleFinding(
	ctx context.Context, task productionPublicationTask, findingID domain.FindingID, version int,
) (domain.Finding, domain.Classification, error) {
	var (
		finding        domain.Finding
		classification domain.Classification
	)
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		finding, err = tx.GetFinding(ctx, findingID)
		if err != nil {
			return err
		}
		classification, err = tx.GetClassification(ctx, findingID, version)
		return err
	})
	if err == nil {
		return finding, classification, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return domain.Finding{}, domain.Classification{}, err
	}
	decision := inference.ConservativeClassifierDecision(finding, version)
	if w.inference != nil {
		decision, err = w.inference.ClassifyFinding(
			ctx, string(task.ProjectID), string(task.RunID), finding, version)
		if err != nil {
			return domain.Finding{}, domain.Classification{}, err
		}
	}
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutClassification(ctx, decision.Classification)
	}); err != nil {
		return domain.Finding{}, domain.Classification{}, err
	}
	return finding, decision.Classification, nil
}

func (w *productionPublicationWorkflow) reviewComparisonTrace(
	ctx context.Context, record domain.ReviewRecord,
) ([]domain.Finding, map[domain.FindingID]domain.Classification, error) {
	findings := make([]domain.Finding, 0, len(record.FindingIDs))
	classifications := make(map[domain.FindingID]domain.Classification, len(record.FindingIDs))
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		for _, id := range record.FindingIDs {
			finding, err := tx.GetFinding(ctx, id)
			if err != nil {
				return err
			}
			findings = append(findings, finding)
			classification, err := tx.GetClassification(ctx, id, record.Round)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			classifications[id] = classification
		}
		return nil
	})
	return findings, classifications, err
}

// classifyMatchingRoutedFindings completes only the comparison trace that
// this shadow finding can join. A shadow source may finish before routed
// adjudication runs, and immutable samples must not freeze that timing race as
// indeterminate; unrelated routed findings must not consume sampling budget.
func (w *productionPublicationWorkflow) classifyMatchingRoutedFindings(
	ctx context.Context,
	task productionPublicationTask,
	shadow domain.Finding,
	version int,
	routed []domain.Finding,
	classifications map[domain.FindingID]domain.Classification,
) error {
	for _, candidate := range routed {
		if _, found := classifications[candidate.ID]; found ||
			!shadowFindingMatchesRouted(shadow, candidate) {
			continue
		}
		_, classification, err := w.classifySampleFinding(ctx, task, candidate.ID, version)
		if err != nil {
			return err
		}
		classifications[candidate.ID] = classification
	}
	return nil
}

func shadowFindingMatchesRouted(shadow, routed domain.Finding) bool {
	projected := shadow
	projected.Source = routed.Source
	shadowFingerprint, shadowErr := projected.Fingerprint()
	routedFingerprint, routedErr := routed.Fingerprint()
	return shadowErr == nil && routedErr == nil && shadowFingerprint == routedFingerprint
}

func compareClassifierTrace(
	shadow domain.Finding,
	classification domain.Classification,
	routed []domain.Finding,
	routedClassifications map[domain.FindingID]domain.Classification,
) domain.ClassifierAccuracyAssessment {
	matched := false
	consistent := true
	for _, candidate := range routed {
		if !shadowFindingMatchesRouted(shadow, candidate) {
			continue
		}
		routedClassification, ok := routedClassifications[candidate.ID]
		if !ok {
			continue
		}
		if _, err := inference.EvaluateClassifierClassification(candidate, routedClassification); err != nil {
			continue
		}
		matched = true
		consistent = consistent && routedClassification.Materiality == classification.Materiality
	}
	switch {
	case !matched:
		return domain.ClassifierAssessmentIndeterminate
	case consistent:
		return domain.ClassifierAssessmentAccurate
	default:
		return domain.ClassifierAssessmentInaccurate
	}
}

func shadowFindingCriticalOrHigh(finding domain.Finding) bool {
	return finding.Severity == domain.FindingSeverityP0 ||
		finding.Severity == domain.FindingSeverityP1
}

func shadowReviewFindingNeedsAttention(
	finding domain.Finding, classification domain.Classification,
) (bool, error) {
	requiresSecond, err := inference.EvaluateClassifierClassification(finding, classification)
	if err != nil {
		return false, err
	}
	return requiresSecond || shadowFindingCriticalOrHigh(finding), nil
}

func shadowReviewAttentionDecision(
	item domain.AttentionItem, commands []domain.Command,
) (domain.Action, error) {
	if item.Type != domain.AttentionReviewDispute || !slices.Equal(
		item.RequestedDecision,
		[]domain.Action{domain.ActionApprove, domain.ActionDiscuss, domain.ActionStop},
	) {
		return "", domain.ErrParentKeyMismatch
	}
	var terminal *domain.Command
	for i := range commands {
		command := &commands[i]
		switch command.Action {
		case domain.ActionApprove, domain.ActionStop:
			if terminal != nil {
				return "", domain.ErrParentKeyMismatch
			}
			terminal = command
		case domain.ActionDiscuss:
			continue
		default:
			return "", domain.ErrParentKeyMismatch
		}
	}
	if item.Status == domain.StatusOpen {
		if terminal != nil || item.DecidedAt != nil {
			return "", domain.ErrParentKeyMismatch
		}
		return "", nil
	}
	if terminal == nil || item.Status != domain.StatusResolved || item.DecidedAt == nil ||
		terminal.ItemVersion >= item.ItemVersion || !item.Offers(terminal.Action) ||
		terminal.ItemID != item.ID || terminal.PRHeadSHA != item.PRHeadSHA ||
		!slices.Equal(terminal.ArtifactDigests, item.ArtifactDigests) {
		return "", domain.ErrParentKeyMismatch
	}
	return terminal.Action, nil
}

func shadowReviewAttentionBindsTask(
	item domain.AttentionItem, task productionPublicationTask, record domain.ShadowReviewRecord,
) bool {
	return item.ID == shadowReviewAttentionItemID(record.InvocationID) &&
		item.ProjectID == task.ProjectID && record.RunID == task.RunID &&
		item.Subject.Type == domain.SubjectRun &&
		item.Subject.ID == domain.SubjectID(task.RunID) &&
		item.Subject.RunID != nil && *item.Subject.RunID == task.RunID &&
		item.PRHeadSHA == task.HeadSHA && item.PRHeadSHA == record.HeadSHA
}

func shadowReviewAttentionPresentsClaims(
	item domain.AttentionItem, claims []domain.AgentClaim,
) bool {
	digests := make([]domain.Digest, len(claims))
	for idx := range claims {
		digests[idx] = claims[idx].Digest
	}
	slices.Sort(digests)
	digests = slices.Compact(digests)
	return reflect.DeepEqual(item.AgentClaims, claims) &&
		slices.Equal(item.ArtifactDigests, digests)
}

func (w *productionPublicationWorkflow) shadowReviewBlocksReady(
	ctx context.Context, task productionPublicationTask, binding productionBinding,
) error {
	var records []domain.ShadowReviewRecord
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		records, err = tx.ListShadowReviewRecords(ctx, task.RunID)
		return err
	}); err != nil {
		return err
	}
	for _, record := range records {
		if record.BaseSHA != binding.admission.Base.BaseSHA || record.HeadSHA != task.HeadSHA {
			continue
		}
		needsAttention := false
		var expectedClaims []domain.AgentClaim
		if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
			for _, findingID := range record.FindingIDs {
				finding, err := tx.GetFinding(ctx, findingID)
				if err != nil {
					return err
				}
				classification, err := tx.GetClassification(ctx, findingID, record.ShadowedRound)
				if err != nil {
					return err
				}
				findingNeedsAttention, err := shadowReviewFindingNeedsAttention(finding, classification)
				if err != nil {
					return err
				}
				if findingNeedsAttention {
					needsAttention = true
					claims, err := shadowReviewFindingClaims(record, finding)
					if err != nil {
						return err
					}
					expectedClaims = append(expectedClaims, claims...)
				}
			}
			return nil
		}); err != nil {
			return err
		}
		if !needsAttention {
			continue
		}
		var (
			item     domain.AttentionItem
			commands []domain.Command
		)
		if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			item, err = tx.GetAttentionItem(ctx, shadowReviewAttentionItemID(record.InvocationID))
			if err != nil || item.Status == domain.StatusOpen {
				return err
			}
			commands, err = tx.ListCommandsForItem(ctx, item.ID)
			return err
		}); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return errors.Join(errShadowReviewBlocksReady, domain.ErrParentKeyMismatch, err)
			}
			return err
		}
		if !shadowReviewAttentionBindsTask(item, task, record) ||
			!shadowReviewAttentionPresentsClaims(item, expectedClaims) {
			return errors.Join(errShadowReviewBlocksReady, domain.ErrParentKeyMismatch)
		}
		decision, err := shadowReviewAttentionDecision(item, commands)
		if err != nil {
			return errors.Join(errShadowReviewBlocksReady, domain.ErrParentKeyMismatch)
		}
		if decision == domain.ActionApprove {
			continue
		}
		if decision == domain.ActionStop {
			return errors.Join(errShadowReviewBlocksReady, errShadowReviewStopped)
		}
		if decision == "" {
			return errShadowReviewBlocksReady
		}
		return errors.Join(errShadowReviewBlocksReady, domain.ErrParentKeyMismatch)
	}
	return nil
}
