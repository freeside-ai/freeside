package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const driftAuditFailureVersion = "freeside.drift-audit-failure/v1"

// The failure fact names a coarse reason only. An inference error can quote
// model output, which has no place in a stored daemon fact.
const (
	driftAuditFailureInput = "input_unavailable"
	driftAuditFailureSite  = "site_unavailable"
	driftAuditFailureAudit = "audit_unavailable"
)

// errDriftAuditInputUnavailable marks an audit input the engine cannot build
// from what the round retained: a missing blob, a diff that does not render,
// or a round-1 candidate no stored input reconstructs. It is a fail-safe
// result, never a store integrity error.
var errDriftAuditInputUnavailable = errors.New("drift audit input unavailable")

// driftAuditFailure is the body of the fact that a round's audit failed (plan
// §7 Review Drift, Fail-safe default). Its artifact has one ID per run and
// round, so a later reconcile of the round finds it and does not call the
// site again.
type driftAuditFailure struct {
	Version string       `json:"version"`
	RunID   domain.RunID `json:"run_id"`
	Round   int          `json:"round"`
	HeadSHA string       `json:"head_sha"`
	Reason  string       `json:"reason"`
}

func driftAuditFailureArtifactID(runID domain.RunID, round int) domain.ArtifactID {
	return domain.ArtifactID(fmt.Sprintf("drift-audit-failure-%d-%s", round, runID))
}

// driftSimplification is what a simplification round adds to the round's
// remediation: the validated reversal list for the remediator, and the fixed
// dispositions the round supersedes, under the authority that ordered it.
// superseded is in reversal order.
type driftSimplification struct {
	auditDigest domain.Digest
	reversals   []remediationReversal
	superseded  []domain.ReviewDispositionRecord
	authority   domain.DispositionSupersessionAuthority
}

// reconcileDriftAudit runs the round's drift audit and routes its verdict
// (plan §7 Review Drift). It is called once the batch is adjudicated,
// convergence found no deterministic stop, and no remediation is dispatched.
//
// A non-nil simplification makes the round's remediation a simplification
// round. parked reports that the verdict raised the round's
// review_diminishing_returns item. Neither means the ordinary flow continues:
// the audit is off, below its trigger round, converged, or failed.
//
// diminishing is the round's decided item, if any. A round a deterministic
// cause stopped runs no audit, whatever the human then decided. A decided
// drift_audit item runs the simplification round only on continue_under_policy
// and only when its card promised one.
func (w *productionPublicationWorkflow) reconcileDriftAudit(
	ctx context.Context,
	task productionPublicationTask,
	record domain.ReviewRecord,
	artifact domain.FindingAdjudication,
	routes map[domain.FindingID]domain.AdjudicationRoute,
	diminishing reviewDiminishingRoute,
	baseRoot, candidateRoot string,
) (*driftSimplification, bool, error) {
	// A reevaluation rechecks a completed cycle and cannot start a
	// remediation, so a verdict could not route.
	if task.reevaluation != nil || record.Outcome != domain.ReviewFindings {
		return nil, false, nil
	}
	// The dispatch needs a routed finding, so a batch without one cannot
	// carry a reversal list.
	routedFix := len(remediationFindingIDs(artifact, routes)) > 0
	if diminishing.item != nil {
		simplification, err := w.decidedDriftSimplification(
			ctx, task, record, diminishing, routedFix, baseRoot, candidateRoot)
		return simplification, false, err
	}
	convergence, err := w.reviewConvergenceState(ctx, record)
	if err != nil {
		return nil, false, err
	}
	after := convergence.Policy.DriftAuditAfter
	if after == 0 || record.Round < after {
		return nil, false, nil
	}
	audit, err := w.driftAuditForRound(ctx, task, record, candidateRoot)
	if err != nil || audit == nil {
		return nil, false, err
	}
	facts := domain.DriftAuditFacts{
		AuditDigest: audit.Digest, Verdict: audit.Verdict, Confidence: audit.Confidence,
		Explanation: audit.Explanation,
		Reversals:   append([]domain.DriftReversal{}, audit.Reversals...),
	}
	// The switch dispatches on the verdict, so it omits default.
	switch audit.Verdict {
	case domain.DriftVerdictConverged:
		return nil, false, nil
	case domain.DriftVerdictStuck:
	case domain.DriftVerdictOverHardened:
		gate, simplification, err := w.driftSimplificationForRound(
			ctx, task, record, baseRoot, candidateRoot)
		if err != nil {
			return nil, false, err
		}
		actionable := simplification != nil && routedFix
		if actionable && gate.Auto {
			simplification.authority = domain.DispositionSupersessionAuthority{
				Kind: domain.DispositionSupersessionAutoRoute,
			}
			return simplification, false, nil
		}
		facts.SimplificationOnContinue = actionable && gate.RoundRemains
	}
	if err := w.parkReviewDiminishing(ctx, task, record, artifact, convergence,
		domain.ReviewDiminishingFacts{
			Cause: domain.ReviewDiminishingDriftAudit, DriftAudit: &facts,
		}); err != nil {
		return nil, false, err
	}
	return nil, true, nil
}

// decidedDriftSimplification answers a decided item. The card's promise is
// recomputed from stored records instead of trusted: the store proved it when
// it loaded the item, and the engine's own two conditions (a routed fix, a
// surface for every reversal) are deterministic for the round. Anything short
// of the full promise keeps the action's landed meaning, an ordinary round.
func (w *productionPublicationWorkflow) decidedDriftSimplification(
	ctx context.Context,
	task productionPublicationTask,
	record domain.ReviewRecord,
	diminishing reviewDiminishingRoute,
	routedFix bool,
	baseRoot, candidateRoot string,
) (*driftSimplification, error) {
	facts := diminishing.item.ReviewDiminishing
	if diminishing.cause != domain.ReviewDiminishingDriftAudit ||
		diminishing.action != domain.ActionContinueUnderPolicy || diminishing.command == nil ||
		facts == nil || facts.DriftAudit == nil || !facts.DriftAudit.SimplificationOnContinue ||
		!routedFix {
		return nil, nil
	}
	gate, simplification, err := w.driftSimplificationForRound(
		ctx, task, record, baseRoot, candidateRoot)
	if err != nil {
		return nil, err
	}
	if simplification == nil || !gate.RoundRemains ||
		gate.Audit.Digest != facts.DriftAudit.AuditDigest {
		return nil, nil
	}
	simplification.authority = domain.DispositionSupersessionAuthority{
		Kind: domain.DispositionSupersessionHumanCommand,
		Command: &domain.DispositionSupersessionCommand{
			ItemID:      diminishing.item.ID,
			ItemVersion: diminishing.command.ItemVersion,
			CommandID:   diminishing.command.CommandID,
		},
	}
	return simplification, nil
}

// driftSimplificationForRound reads the store's route gate and, for a list the
// gate validated, derives each reversal's surface from the cited finding's
// normalized location. The audit's undo text is advisory and never chooses a
// path. The simplification is nil when the list is not valid or a cited
// finding has no derivable surface.
func (w *productionPublicationWorkflow) driftSimplificationForRound(
	ctx context.Context,
	task productionPublicationTask,
	record domain.ReviewRecord,
	baseRoot, candidateRoot string,
) (store.DriftRouteGate, *driftSimplification, error) {
	var (
		gate     store.DriftRouteGate
		findings []domain.Finding
	)
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		gate, err = tx.DriftAuditRouteGate(ctx, task.RunID, record.Round)
		if err != nil || !gate.ListValid {
			return err
		}
		findings = make([]domain.Finding, len(gate.Audit.Reversals))
		for index, reversal := range gate.Audit.Reversals {
			findings[index], err = tx.GetFinding(ctx, reversal.FindingID)
			if err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return store.DriftRouteGate{}, nil, err
	}
	if !gate.ListValid {
		return gate, nil, nil
	}
	reversals := make([]remediationReversal, len(findings))
	for index, finding := range findings {
		surface, err := domain.DeriveRemediationSurface(finding.Location,
			func(path string) (bool, bool, error) {
				inBase, baseErr := pathExists(baseRoot, path)
				if baseErr != nil {
					return false, false, baseErr
				}
				inCandidate, candidateErr := pathExists(candidateRoot, path)
				return inBase, inCandidate, candidateErr
			})
		if err != nil {
			return store.DriftRouteGate{}, nil, err
		}
		if surface == nil {
			return gate, nil, nil
		}
		reversals[index] = remediationReversalFor(gate.Audit.Reversals[index], finding)
	}
	return gate, &driftSimplification{
		auditDigest: gate.Audit.Digest, reversals: reversals,
		superseded: gate.Superseded,
	}, nil
}

// driftAuditForRound returns the round's audit, calling the site at most once
// per round. A nil audit with no error is the fail-safe result: the audit
// failed, now or on an earlier reconcile, and the round continues as it would
// with the audit off.
func (w *productionPublicationWorkflow) driftAuditForRound(
	ctx context.Context,
	task productionPublicationTask,
	record domain.ReviewRecord,
	candidateRoot string,
) (*domain.DriftAudit, error) {
	var (
		stored *domain.DriftAudit
		failed bool
	)
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		audit, err := tx.GetDriftAuditForRound(ctx, task.RunID, record.Round)
		if err == nil {
			stored = &audit
			return nil
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		_, err = tx.GetArtifact(ctx, driftAuditFailureArtifactID(task.RunID, record.Round))
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		failed = err == nil
		return err
	}); err != nil {
		return nil, err
	}
	if stored != nil || failed {
		return stored, nil
	}
	if w.inference == nil || !w.inference.SupportsSite(inference.DriftAuditorSiteID) {
		return nil, w.recordDriftAuditFailure(ctx, task, record, driftAuditFailureSite)
	}
	project, input, err := w.driftAuditorInput(ctx, task, record, candidateRoot)
	if errors.Is(err, errDriftAuditInputUnavailable) {
		return nil, w.recordDriftAuditFailure(ctx, task, record, driftAuditFailureInput)
	}
	if err != nil {
		return nil, err
	}
	audit, err := w.inference.AuditDrift(ctx, project, string(task.RunID), input)
	if err != nil {
		// The site folds every failure into one sentinel, a cancelled context
		// included. A cancellation is this reconcile ending, not the audit
		// failing, so it must not use up the round's one call.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, w.recordDriftAuditFailure(ctx, task, record, driftAuditFailureAudit)
	}
	// The artifact's binding fields come from the stored rows the input was
	// built from, and the site refuses a reversal naming a finding it was not
	// shown. A store refusal here is therefore a contradiction between stored
	// rows, not a failed audit, and it propagates.
	if err := w.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutDriftAudit(ctx, audit)
	}); err != nil {
		return nil, err
	}
	return &audit, nil
}

// recordDriftAuditFailure stores the round's failure fact as an engine
// evidence artifact. It reads before writing: re-putting the artifact with a
// fresh created_at would trip the immutable-row guard.
func (w *productionPublicationWorkflow) recordDriftAuditFailure(
	ctx context.Context,
	task productionPublicationTask,
	record domain.ReviewRecord,
	reason string,
) error {
	body, err := json.Marshal(driftAuditFailure{
		Version: driftAuditFailureVersion, RunID: task.RunID, Round: record.Round,
		HeadSHA: record.HeadSHA, Reason: reason,
	})
	if err != nil {
		return err
	}
	digest := domain.Digest(contentaddr.Sum(body))
	if _, err := w.artifacts.Put(digest, bytes.NewReader(body)); err != nil {
		return fmt.Errorf("store drift audit failure: %w", err)
	}
	artifactID := driftAuditFailureArtifactID(task.RunID, record.Round)
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: artifactID, Type: domain.ArtifactKindEvidence, Digest: digest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerDaemon, ProducerInvocationID: record.InvocationID,
			HeadBinding: domain.HeadBound, SourceHeadSHA: record.HeadSHA,
			SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: int64(len(body)),
			CreatedAt: w.now().UTC(), Source: domain.EvidenceSourceRun,
			Availability: domain.EvidenceAvailable,
		},
	}, w.approvedRecipes)
	if err != nil {
		return err
	}
	return w.store.Write(ctx, func(tx *store.WriteTx) error {
		_, err := tx.GetArtifact(ctx, artifactID)
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return tx.PutArtifact(ctx, artifact)
	})
}

// driftAuditEntries collects the current entries of every adjudicated round,
// leaving out the entries for external findings. The adjudication list is in
// round then revision order, so the last artifact of a round is its current
// one.
func driftAuditEntries(
	adjudications []domain.FindingAdjudication, external []domain.Finding,
) []domain.FindingAdjudicationEntry {
	var entries []domain.FindingAdjudicationEntry
	for index, adjudication := range adjudications {
		if index+1 < len(adjudications) && adjudications[index+1].Round == adjudication.Round {
			continue
		}
		for _, entry := range adjudication.Entries {
			if slices.ContainsFunc(external, func(finding domain.Finding) bool {
				return finding.ID == entry.FindingID
			}) {
				continue
			}
			entries = append(entries, entry)
		}
	}
	return entries
}

// driftAuditorInput loads the audit's whole input from daemon records. A store
// read failure propagates: the rest of the round reads the same rows. Only
// content the round did not retain is reported as unavailable.
func (w *productionPublicationWorkflow) driftAuditorInput(
	ctx context.Context,
	task productionPublicationTask,
	record domain.ReviewRecord,
	candidateRoot string,
) (string, inference.DriftAuditorInput, error) {
	var (
		run           domain.Run
		policy        domain.ResolvedPolicy
		dispositions  []domain.ReviewDispositionRecord
		adjudications []domain.FindingAdjudication
		external      []domain.Finding
		records       []domain.ReviewRecord
		diffMetrics   *domain.ReviewRoundDiffMetrics
		roundOneInput *domain.Artifact
	)
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if run, err = tx.GetRun(ctx, task.RunID); err != nil {
			return err
		}
		if policy, err = tx.GetResolvedPolicy(ctx, task.RunID); err != nil {
			return err
		}
		if dispositions, err = tx.ListFindingDispositions(ctx, task.RunID); err != nil {
			return err
		}
		if adjudications, err = tx.ListFindingAdjudications(ctx, task.RunID); err != nil {
			return err
		}
		if external, err = tx.ListExternalFindings(ctx, task.RunID); err != nil {
			return err
		}
		if records, err = tx.ListReviewRecords(ctx, task.RunID); err != nil {
			return err
		}
		input, err := tx.GetArtifact(ctx, remediationInputArtifactID(task.RunID, 1))
		if err == nil {
			roundOneInput = &input
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		// A round recorded without metrics is a gap, not a failure.
		metrics, err := tx.GetReviewRoundDiffMetrics(ctx, task.RunID, record.Round)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		diffMetrics = &metrics
		return nil
	}); err != nil {
		return "", inference.DriftAuditorInput{}, fmt.Errorf("load drift-auditor bindings: %w", err)
	}
	unavailable := func(what string, err error) (string, inference.DriftAuditorInput, error) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", inference.DriftAuditorInput{}, ctxErr
		}
		return "", inference.DriftAuditorInput{}, fmt.Errorf(
			"%w: %s: %w", errDriftAuditInputUnavailable, what, err)
	}
	specification, err := loadFakePublicationBlob(w.artifacts, run.SpecDigest)
	if err != nil {
		return unavailable("approved specification", err)
	}
	instructions, err := loadFakePublicationBlob(w.artifacts, record.InstructionDigest)
	if err != nil {
		return unavailable("instruction snapshot", err)
	}
	currentDiff, err := remediationCandidatePatch(
		ctx, w.workDir, candidateRoot, record.BaseSHA, record.HeadSHA)
	if err != nil {
		return unavailable("current diff", err)
	}
	roundOneDiff, err := w.driftRoundOneDiff(records, record, roundOneInput, currentDiff)
	if err != nil {
		return unavailable("round-1 diff", err)
	}
	// An external review cycle's first round also judged the cycle's external
	// findings (issue #1767); no review record lists one, and the store
	// refuses a reversal that names a finding outside the records, so their
	// entries stay out of the input.
	entries := driftAuditEntries(adjudications, external)
	return string(run.ProjectID), inference.DriftAuditorInput{
		RunID: task.RunID, Round: record.Round,
		BaseSHA: record.BaseSHA, HeadSHA: record.HeadSHA,
		ApprovedSpecDigest: run.SpecDigest, ApprovedSpecification: string(specification),
		InstructionSnapshotDigest: record.InstructionDigest,
		InstructionSnapshot:       string(instructions),
		ResolvedPolicyDigest:      policy.Digest,
		DeclaredPaths:             domain.CanonicalDeclaredPaths(policy),
		RoundOneDiff:              string(roundOneDiff), CurrentDiff: string(currentDiff),
		Dispositions: dispositions, AdjudicationEntries: entries,
		DiffMetrics: diffMetrics,
	}, nil
}

// driftRoundOneDiff returns the bound base against the round-1 candidate head.
// Each pass rebuilds the candidate as a fresh commit on the base, so an
// earlier head is not in the review workspace. When round 1 reviewed the
// current head the current diff is the answer. Otherwise the round-1
// remediation input retained that candidate as a full base-to-candidate patch.
// A round-1 head no stored input covers (an operator-feedback successor after
// a clean round 1) cannot be rebuilt, and the audit fails safe.
func (w *productionPublicationWorkflow) driftRoundOneDiff(
	records []domain.ReviewRecord,
	record domain.ReviewRecord,
	roundOneInput *domain.Artifact,
	currentDiff []byte,
) ([]byte, error) {
	var roundOne *domain.ReviewRecord
	for index := range records {
		if records[index].Round == 1 {
			roundOne = &records[index]
			break
		}
	}
	if roundOne == nil || roundOne.BaseSHA != record.BaseSHA {
		return nil, fmt.Errorf("round-1 review record on base %q: %w", record.BaseSHA, store.ErrNotFound)
	}
	if roundOne.HeadSHA == record.HeadSHA {
		return currentDiff, nil
	}
	if roundOneInput == nil {
		return nil, fmt.Errorf("round-1 head %q has no remediation input: %w",
			roundOne.HeadSHA, store.ErrNotFound)
	}
	input, _, err := loadRemediationInput(w.artifacts, roundOneInput.Digest)
	if err != nil {
		return nil, err
	}
	if input.RunID != record.RunID || input.Round != 1 ||
		input.BaseSHA != roundOne.BaseSHA || input.HeadSHA != roundOne.HeadSHA {
		return nil, fmt.Errorf("round-1 remediation input names %q..%q: %w",
			input.BaseSHA, input.HeadSHA, domain.ErrParentKeyMismatch)
	}
	return input.CandidatePatchBase64, nil
}
