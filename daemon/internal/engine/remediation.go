package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/gitrun"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	// KindRemediationInvocationRequested is the engine-owned dispatch intent
	// produced by an accepted finding-adjudication route.
	KindRemediationInvocationRequested = "remediation_invocation_requested"
	remediationRequestVersion          = "freeside.remediation-request/v1"
	remediationInputVersion            = "freeside.remediation-input/v1"
	remediationInvocationIDPrefix      = "inv-remediate-"
	remediationStageIDPrefix           = "remediate-"
	remediatorPushbackLabel            = "freeside.remediator_pushback"
	remediatorPushbackVersion          = "freeside.remediator-pushback/v1"
	remediationMarkerQuarantinePrefix  = "remediation-marker-quarantined-"
)

var (
	ErrRemediationInputUndeliverable = errors.New("remediation input cannot be delivered")
	errRemediationMarkerUnreadable   = errors.New("remediation marker unreadable")
)

const remediationQuarantineUnreadable = "A stored remediation marker could not be authenticated. " +
	"The run is held out of the remediation lane, and resumes by itself once the marker reconstructs again."

type remediatorPushback struct {
	Version    string             `json:"version"`
	FindingIDs []domain.FindingID `json:"finding_ids"`
	Reason     string             `json:"reason"`
}

type remediationReviewOutcome struct {
	dispositions []domain.ReviewDispositionRecord
	dissent      *findingAdjudicationDissent
	attention    string
	claims       []domain.AgentClaim
}

type remediationInvocationRequest struct {
	Version             string              `json:"version"`
	InvocationID        domain.InvocationID `json:"invocation_id"`
	RunID               domain.RunID        `json:"run_id"`
	StageID             domain.StageID      `json:"stage_id"`
	Round               int                 `json:"round"`
	ReviewInvocationID  domain.InvocationID `json:"review_invocation_id"`
	AdjudicationDigest  domain.Digest       `json:"adjudication_digest"`
	InputArtifactID     domain.ArtifactID   `json:"input_artifact_id"`
	InputArtifactDigest domain.Digest       `json:"input_artifact_digest"`
	BaseSHA             string              `json:"base_sha"`
	HeadSHA             string              `json:"head_sha"`
	FindingIDs          []domain.FindingID  `json:"finding_ids"`
}

type remediationInput struct {
	Version              string                     `json:"version"`
	RunID                domain.RunID               `json:"run_id"`
	Round                int                        `json:"round"`
	BaseSHA              string                     `json:"base_sha"`
	HeadSHA              string                     `json:"head_sha"`
	Instruction          string                     `json:"instruction"`
	CandidatePatchBase64 []byte                     `json:"candidate_patch_base64"`
	Adjudication         domain.FindingAdjudication `json:"adjudication"`
	Findings             []domain.Finding           `json:"findings"`
}

type preparedRemediationIntent struct {
	request      remediationInvocationRequest
	payload      []byte
	artifact     domain.Artifact
	invocation   domain.AgentInvocation
	stage        domain.Stage
	effective    map[domain.FindingID]domain.AdjudicationRoute
	publication  productionPublicationTask
	reviewRecord domain.ReviewRecord
}

func remediationStageID(runID domain.RunID, round int) domain.StageID {
	return domain.StageID(fmt.Sprintf("%s%d-%s", remediationStageIDPrefix, round, runID))
}

func remediationInvocationID(runID domain.RunID, round int) domain.InvocationID {
	return domain.InvocationID(fmt.Sprintf("%s%d-%s", remediationInvocationIDPrefix, round, runID))
}

func remediationInputArtifactID(runID domain.RunID, round int) domain.ArtifactID {
	return domain.ArtifactID(fmt.Sprintf("remediation-input-%d-%s", round, runID))
}

func remediationCoordinatesFromInvocationID(
	id domain.InvocationID,
) (domain.RunID, int, bool) {
	remainder, ok := strings.CutPrefix(string(id), remediationInvocationIDPrefix)
	if !ok {
		return "", 0, false
	}
	rawRound, rawRunID, ok := strings.Cut(remainder, "-")
	if !ok || rawRunID == "" {
		return "", 0, false
	}
	round, err := strconv.Atoi(rawRound)
	if err != nil || round < 1 {
		return "", 0, false
	}
	return domain.RunID(rawRunID), round, true
}

func remediationRoundForInvocation(
	runID domain.RunID, id domain.InvocationID,
) (int, bool) {
	gotRunID, round, ok := remediationCoordinatesFromInvocationID(id)
	return round, ok && gotRunID == runID
}

func remediationCoordinatesFromStageID(
	id domain.StageID,
) (domain.RunID, int, bool) {
	remainder, ok := strings.CutPrefix(string(id), remediationStageIDPrefix)
	if !ok {
		return "", 0, false
	}
	rawRound, rawRunID, ok := strings.Cut(remainder, "-")
	if !ok || rawRunID == "" {
		return "", 0, false
	}
	round, err := strconv.Atoi(rawRound)
	if err != nil || round < 1 {
		return "", 0, false
	}
	return domain.RunID(rawRunID), round, true
}

// activeRemediationStage derives the run's current remediation transition
// from durable workflow state. Publication-task producer identity describes
// the candidate the task currently carries; before export it deliberately
// still names the prior producer, so it cannot answer whether remediation is
// active.
func activeRemediationStage(run domain.Run) (domain.Stage, int, bool, error) {
	var (
		active      domain.Stage
		activeRound int
	)
	for _, stage := range run.Stages {
		stageRunID, round, remediation := remediationCoordinatesFromStageID(stage.ID)
		if !remediation {
			if strings.HasPrefix(string(stage.ID), remediationStageIDPrefix) {
				return domain.Stage{}, 0, false, domain.ErrParentKeyMismatch
			}
			continue
		}
		if stageRunID != run.ID || stage.Name != productionStageName {
			return domain.Stage{}, 0, false, domain.ErrParentKeyMismatch
		}
		if active.ID == "" || round > activeRound {
			active = stage
			activeRound = round
		}
	}
	return active, activeRound, active.ID != "", nil
}

func findRemediationStage(run domain.Run, round int) (domain.Stage, bool) {
	wantID := remediationStageID(run.ID, round)
	for _, stage := range run.Stages {
		// Remediation deliberately reuses the implementation role while its
		// derived stage identity keeps the two executions distinct.
		if stage.ID == wantID && stage.Name == productionStageName {
			return stage, true
		}
	}
	return domain.Stage{}, false
}

func productionStageForInvocation(
	run domain.Run, invocationID domain.InvocationID,
) (domain.Stage, bool) {
	if strings.HasPrefix(string(invocationID), "inv-operator-feedback-") {
		return findOperatorFeedbackStage(run, invocationID)
	}
	if invocationID == productionInvocationID(run.ID) {
		if stage, ok := findProductionStage(run); ok {
			return stage, true
		}
		return domain.Stage{
			ID: productionStageID(run.ID), RunID: run.ID, Name: productionStageName,
		}, true
	}
	round, ok := remediationRoundForInvocation(run.ID, invocationID)
	if !ok {
		return domain.Stage{}, false
	}
	return findRemediationStage(run, round)
}

func (r remediationInvocationRequest) validate() error {
	if r.Version != remediationRequestVersion || r.RunID == "" || r.Round < 1 ||
		r.InvocationID != remediationInvocationID(r.RunID, r.Round) ||
		r.StageID != remediationStageID(r.RunID, r.Round) ||
		r.ReviewInvocationID != ProductionReviewInvocationID(r.RunID, r.Round) ||
		!contentaddr.Valid(string(r.AdjudicationDigest)) ||
		r.InputArtifactID != remediationInputArtifactID(r.RunID, r.Round) ||
		!contentaddr.Valid(string(r.InputArtifactDigest)) ||
		!validCommitSHA(r.BaseSHA) || !validCommitSHA(r.HeadSHA) ||
		r.BaseSHA == r.HeadSHA || len(r.FindingIDs) == 0 {
		return fmt.Errorf("invalid remediation request: %w", domain.ErrParentKeyMismatch)
	}
	if !slices.IsSorted(r.FindingIDs) {
		return fmt.Errorf("remediation finding ids are not canonical: %w", domain.ErrParentKeyMismatch)
	}
	for index, findingID := range r.FindingIDs {
		if findingID == "" || (index > 0 && findingID == r.FindingIDs[index-1]) {
			return fmt.Errorf("remediation finding ids are invalid: %w", domain.ErrParentKeyMismatch)
		}
	}
	return nil
}

func encodeRemediationRequest(request remediationInvocationRequest) ([]byte, error) {
	if err := request.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(request)
}

func decodeRemediationRequest(entry store.QueueEntry) (remediationInvocationRequest, error) {
	if entry.Kind != KindRemediationInvocationRequested {
		return remediationInvocationRequest{}, fmt.Errorf(
			"%w: remediation intent %q has kind %q: %w",
			errRemediationMarkerUnreadable,
			entry.IdempotencyKey, entry.Kind, domain.ErrParentKeyMismatch)
	}
	var request remediationInvocationRequest
	if err := strictjson.Decode(
		entry.Payload, &request, strictjson.TolerateInvalidUTF8, strictjson.NoLimit,
	); err != nil {
		return remediationInvocationRequest{}, fmt.Errorf(
			"%w: decode remediation request: %w", errRemediationMarkerUnreadable, err)
	}
	if err := request.validate(); err != nil {
		return remediationInvocationRequest{}, errors.Join(errRemediationMarkerUnreadable, err)
	}
	if entry.IdempotencyKey != string(request.InvocationID) {
		return remediationInvocationRequest{}, fmt.Errorf(
			"%w: remediation request key disagrees with invocation: %w",
			errRemediationMarkerUnreadable, domain.ErrParentKeyMismatch)
	}
	canonical, err := json.Marshal(request)
	if err != nil {
		return remediationInvocationRequest{}, errors.Join(errRemediationMarkerUnreadable, err)
	}
	if !bytes.Equal(entry.Payload, canonical) {
		return remediationInvocationRequest{}, fmt.Errorf(
			"%w: remediation request is not canonical: %w",
			errRemediationMarkerUnreadable, domain.ErrParentKeyMismatch)
	}
	return request, nil
}

func (e *Engine) quarantinePendingRemediationMarker(
	ctx context.Context, entry store.QueueEntry, cause error,
) (bool, error) {
	if !errors.Is(cause, errRemediationMarkerUnreadable) {
		return false, nil
	}
	runID, _, attributable := remediationCoordinatesFromInvocationID(
		domain.InvocationID(entry.IdempotencyKey))
	if !attributable {
		return false, nil
	}
	var run domain.Run
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, runID)
		return err
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, recordProductionQuarantine(
		ctx, e.store, e.signet, remediationMarkerQuarantinePrefix,
		run.ID, run.ProjectID, remediationQuarantineUnreadable)
}

// RemediationInvocationBackupPayloadDigests authenticates a queued
// remediation request and retains its daemon-authored input envelope.
func RemediationInvocationBackupPayloadDigests(entry store.QueueEntry) ([]domain.Digest, error) {
	request, err := decodeRemediationRequest(entry)
	if err != nil {
		return nil, err
	}
	return []domain.Digest{request.InputArtifactDigest}, nil
}

type authenticatedRemediationTransition struct {
	request       remediationInvocationRequest
	binding       invocationBinding
	inputArtifact domain.Artifact
	adjudication  domain.FindingAdjudication
	findings      []domain.Finding
	publication   ProductionPublication
}

type authenticatedProductionRunTransition struct {
	run         domain.Run
	production  *productionInvocationRequest
	remediation *authenticatedRemediationTransition
}

// authenticateProductionRunInput completes active-transition authentication at
// the artifact boundary. Durable rows name the daemon-authored remediation
// input, but the transition is not authenticated until those bytes are opened,
// digest-verified, strictly decoded, and compared with the reconstructed rows.
func authenticateProductionRunInput(
	artifacts ArtifactStore,
	transition authenticatedProductionRunTransition,
) error {
	if transition.remediation == nil {
		return nil
	}
	return authenticateRemediationInput(artifacts, *transition.remediation)
}

// authenticateProductionRunTransition reconstructs the complete active
// production transition from durable run state. The run's highest remediation
// stage is the lifecycle index; a publication task's producer is intentionally
// not consulted here because it still names the prior candidate before export.
func authenticateProductionRunTransition(
	ctx context.Context,
	tx *store.ReadTx,
	runID domain.RunID,
) (authenticatedProductionRunTransition, error) {
	run, err := tx.GetRun(ctx, runID)
	if err != nil {
		return authenticatedProductionRunTransition{}, err
	}
	transition := authenticatedProductionRunTransition{run: run}
	activeStage, activeRound, active, err := activeRemediationStage(run)
	if err != nil {
		return transition, classifyRemediationMarkerError(err)
	}
	entry, err := tx.GetOutbox(ctx, string(productionInvocationID(run.ID)))
	if errors.Is(err, store.ErrNotFound) && !active {
		return transition, nil
	}
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return transition, fmt.Errorf(
				"%w: production marker for run %q with active remediation is absent: %w",
				errProductionMarkerUnreadable, run.ID, err,
			)
		}
		return transition, err
	}
	production, err := authenticateProductionMarker(entry, run.ID)
	if err != nil {
		return transition, err
	}
	transition.production = &production
	if !active {
		return transition, nil
	}
	remediationEntry, err := tx.GetOutbox(
		ctx, string(remediationInvocationID(run.ID, activeRound)))
	if err != nil {
		return transition, classifyRemediationMarkerError(err)
	}
	verified, err := authenticateRemediationInvocationTransition(
		ctx, tx, remediationEntry, run.ID, activeStage.ID,
	)
	if err != nil || verified.request.Round != activeRound {
		return transition, classifyRemediationMarkerError(
			errors.Join(err, domain.ErrParentKeyMismatch))
	}
	transition.remediation = &verified
	return transition, nil
}

// AuthenticateRemediationInvocationTransition binds a remediation dispatch
// marker to the admitted run and stage, reconstructs its daemon-authored
// review/adjudication chain, and returns the original production publication
// authority. The returned commit author is never taken from remediation output.
func AuthenticateRemediationInvocationTransition(
	ctx context.Context,
	tx *store.ReadTx,
	entry store.QueueEntry,
	runID domain.RunID,
	stageID domain.StageID,
) (ProductionPublication, error) {
	verified, err := authenticateRemediationInvocationTransition(
		ctx, tx, entry, runID, stageID,
	)
	if err != nil {
		return ProductionPublication{}, err
	}
	return verified.publication, nil
}

func authenticateRemediationInvocationTransition(
	ctx context.Context,
	tx *store.ReadTx,
	entry store.QueueEntry,
	runID domain.RunID,
	stageID domain.StageID,
) (authenticatedRemediationTransition, error) {
	request, err := decodeRemediationRequest(entry)
	if err != nil {
		return authenticatedRemediationTransition{}, err
	}
	if request.RunID != runID || request.StageID != stageID {
		return authenticatedRemediationTransition{}, fmt.Errorf(
			"remediation invocation marker disagrees with admitted run or stage: %w",
			domain.ErrParentKeyMismatch,
		)
	}
	verified := authenticatedRemediationTransition{request: request}
	verified.binding.invocation, err = tx.GetAgentInvocation(ctx, request.InvocationID)
	if err != nil {
		return authenticatedRemediationTransition{}, err
	}
	verified.binding.run, err = tx.GetRun(ctx, request.RunID)
	if err != nil {
		return authenticatedRemediationTransition{}, err
	}
	stage, ok := findRemediationStage(verified.binding.run, request.Round)
	if !ok || stage.ID != stageID {
		return authenticatedRemediationTransition{}, domain.ErrParentKeyMismatch
	}
	verified.inputArtifact, err = tx.GetArtifact(ctx, request.InputArtifactID)
	if err != nil {
		return authenticatedRemediationTransition{}, err
	}
	initial, err := tx.GetAgentInvocation(ctx, productionInvocationID(request.RunID))
	if err != nil {
		return authenticatedRemediationTransition{}, err
	}
	if len(initial.InputIDs) != 1 {
		return authenticatedRemediationTransition{}, domain.ErrParentKeyMismatch
	}
	initialInput, err := tx.GetArtifact(ctx, initial.InputIDs[0])
	if err != nil {
		return authenticatedRemediationTransition{}, err
	}
	verified.adjudication, err = tx.GetFindingAdjudication(ctx, request.AdjudicationDigest)
	if err != nil {
		return authenticatedRemediationTransition{}, err
	}
	record, err := tx.GetReviewRecord(ctx, request.ReviewInvocationID)
	if err != nil {
		return authenticatedRemediationTransition{}, err
	}
	verified.findings = make([]domain.Finding, len(request.FindingIDs))
	for index, findingID := range request.FindingIDs {
		verified.findings[index], err = tx.GetFinding(ctx, findingID)
		if err != nil {
			return authenticatedRemediationTransition{}, err
		}
		if verified.findings[index].RunID != request.RunID {
			return authenticatedRemediationTransition{}, domain.ErrParentKeyMismatch
		}
	}
	provenance := verified.inputArtifact.Provenance
	if verified.binding.invocation.ConversationID != nil || initial.ConversationID != nil ||
		!slices.Equal(verified.binding.invocation.InputIDs,
			[]domain.ArtifactID{initial.InputIDs[0], request.InputArtifactID}) ||
		initialInput.Type != domain.ArtifactKindSpecification ||
		initialInput.Digest != verified.binding.run.SpecDigest ||
		verified.inputArtifact.Type != domain.ArtifactKindEvidence ||
		verified.inputArtifact.Digest != request.InputArtifactDigest ||
		provenance.ProducerClass != domain.ProducerDaemon ||
		provenance.ProducerInvocationID != request.ReviewInvocationID ||
		provenance.HeadBinding != domain.HeadBound || provenance.SourceHeadSHA != request.HeadSHA ||
		provenance.SensitivityClass != domain.SensitivityNormal ||
		verified.adjudication.RunID != request.RunID ||
		verified.adjudication.Round != request.Round ||
		verified.adjudication.Digest != request.AdjudicationDigest ||
		verified.adjudication.ApprovedSpecDigest != verified.binding.run.SpecDigest ||
		verified.adjudication.ResolvedPolicyDigest != verified.binding.run.PolicyDigest ||
		record.RunID != request.RunID || record.Round != request.Round ||
		record.InvocationID != request.ReviewInvocationID ||
		record.BaseSHA != request.BaseSHA || record.HeadSHA != request.HeadSHA ||
		record.Outcome != domain.ReviewFindings {
		return authenticatedRemediationTransition{}, domain.ErrParentKeyMismatch
	}
	routes, err := effectiveFindingRoutesTx(ctx, tx, verified.adjudication)
	if err != nil || !slices.Equal(
		remediationFindingIDs(verified.adjudication, routes), request.FindingIDs,
	) {
		return authenticatedRemediationTransition{}, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	initialMarker, err := tx.GetOutbox(ctx, string(productionInvocationID(request.RunID)))
	if err != nil {
		return authenticatedRemediationTransition{}, err
	}
	verified.publication, ok, err = ProductionInvocationPublication(initialMarker)
	if err != nil {
		return authenticatedRemediationTransition{}, err
	}
	if !ok {
		return authenticatedRemediationTransition{}, domain.ErrParentKeyMismatch
	}
	return verified, nil
}

func effectiveFindingRoutesTx(
	ctx context.Context,
	tx *store.ReadTx,
	artifact domain.FindingAdjudication,
) (map[domain.FindingID]domain.AdjudicationRoute, error) {
	itemID := productionFindingAdjudicationItemID(artifact.RunID, artifact.Round, artifact.Revision)
	item, err := tx.GetAttentionItem(ctx, itemID)
	if errors.Is(err, store.ErrNotFound) {
		return findingRoutesFromDecision(artifact, nil)
	}
	if err != nil {
		return nil, err
	}
	if item.Type == domain.AttentionReviewDispute {
		return nil, domain.ErrParentKeyMismatch
	}
	_, command, err := tx.FindingAdjudicationDecision(ctx, itemID)
	if err != nil {
		return nil, err
	}
	if command == nil {
		return nil, domain.ErrParentKeyMismatch
	}
	return findingRoutesFromDecision(artifact, command)
}

func (w *productionPublicationWorkflow) remediationSupersedesReview(
	ctx context.Context,
	task productionPublicationTask,
	record domain.ReviewRecord,
) (bool, error) {
	round, ok := remediationRoundForInvocation(task.RunID, task.ProducingInvocationID)
	if !ok {
		return false, nil
	}
	var entry store.QueueEntry
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, string(task.ProducingInvocationID))
		return err
	}); err != nil {
		return false, err
	}
	request, err := decodeRemediationRequest(entry)
	if err != nil {
		return false, err
	}
	return entry.Dispatched() && request.Round == round &&
		request.ReviewInvocationID == record.InvocationID &&
		request.BaseSHA == record.BaseSHA && request.HeadSHA == record.HeadSHA &&
		task.Replay.ObservedBaseSHA == record.BaseSHA, nil
}

func (w *productionPublicationWorkflow) completeRemediationImportDissent(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	paths []string,
) (productionTaskOutcome, error) {
	round, ok := remediationRoundForInvocation(task.RunID, task.ProducingInvocationID)
	if !ok || len(paths) == 0 {
		return productionTaskOutcome{}, domain.ErrParentKeyMismatch
	}
	var (
		entry  store.QueueEntry
		record domain.ReviewRecord
	)
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, string(task.ProducingInvocationID))
		if err != nil {
			return err
		}
		record, err = tx.GetReviewRecord(ctx, ProductionReviewInvocationID(task.RunID, round))
		return err
	}); err != nil {
		return productionTaskOutcome{}, err
	}
	request, err := decodeRemediationRequest(entry)
	if err != nil || !entry.Dispatched() || request.Round != round {
		return productionTaskOutcome{}, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	slices.Sort(paths)
	paths = slices.Compact(paths)
	dissent := findingAdjudicationDissent{
		Kind: findingDissentImportPathRejected, FindingIDs: slices.Clone(request.FindingIDs),
		Evidence: "The import boundary rejected remediation paths: " + strings.Join(paths, ", "),
	}
	if err := validateFindingAdjudicationDissent(dissent); err != nil {
		return productionTaskOutcome{}, err
	}
	itemID := domain.ItemID(fmt.Sprintf("production-remediation-dissent-%s-%d", task.RunID, round))
	if err := w.putReviewAttentionWithID(
		ctx, task, record, dissent.Evidence, domain.AttentionReviewDispute, itemID,
	); err != nil {
		return productionTaskOutcome{}, err
	}
	return w.completeReviewEscalationTask(ctx, task, binding)
}

func (w *productionPublicationWorkflow) parseRemediatorPushback(
	task productionPublicationTask,
	claims []domain.AgentClaim,
) (*remediatorPushback, []domain.AgentClaim, string, error) {
	var matchedClaims []domain.AgentClaim
	for index := range claims {
		if claims[index].Label != remediatorPushbackLabel {
			continue
		}
		matchedClaims = append(matchedClaims, claims[index])
	}
	if len(matchedClaims) == 0 {
		return nil, nil, "", nil
	}
	if len(matchedClaims) > 1 {
		return nil, matchedClaims, "The remediation export carried multiple remediator-pushback claims.", nil
	}
	matched := matchedClaims[0]
	if matched.Validate() != nil || !contentaddr.Valid(string(matched.Digest)) ||
		matched.Provenance.ProducerInvocationID != task.ProducingInvocationID ||
		matched.Provenance.HeadBinding != domain.HeadBound ||
		matched.Provenance.SourceHeadSHA != task.HeadSHA {
		return nil, matchedClaims, "The remediator-pushback claim was not bound to this remediation invocation and head.", nil
	}
	body, err := w.remediatorPushbackBody(matched)
	switch {
	case errors.Is(err, domain.ErrClaimTextTooLarge):
		return nil, matchedClaims, "The remediator-pushback claim was malformed.", nil
	case errors.Is(err, domain.ErrClaimTextDigestMismatch):
		return nil, matchedClaims, "The remediator-pushback claim could not be authenticated.", nil
	case err != nil:
		return nil, matchedClaims, "", fmt.Errorf("load remediator-pushback claim: %w", err)
	}
	var pushback remediatorPushback
	if err := strictjson.Decode(
		body, &pushback, strictjson.RejectInvalidUTF8,
		strictjson.Limit(domain.MaxClaimTextBytes),
	); err != nil || pushback.Version != remediatorPushbackVersion ||
		len(pushback.FindingIDs) == 0 || !slices.IsSorted(pushback.FindingIDs) ||
		strings.TrimSpace(pushback.Reason) == "" {
		return nil, matchedClaims, "The remediator-pushback claim was malformed.", nil
	}
	for index, findingID := range pushback.FindingIDs {
		if findingID == "" || (index > 0 && findingID == pushback.FindingIDs[index-1]) {
			return nil, matchedClaims, "The remediator-pushback claim named invalid finding identities.", nil
		}
	}
	return &pushback, matchedClaims, "", nil
}

func (w *productionPublicationWorkflow) remediatorPushbackBody(
	claim domain.AgentClaim,
) ([]byte, error) {
	if claim.Text != nil {
		return []byte(claim.Text.Content), nil
	}
	if w.artifacts == nil {
		return nil, domain.ErrParentKeyMismatch
	}
	reader, err := w.artifacts.Open(claim.Digest)
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(reader, domain.MaxClaimTextBytes+1))
	closeErr := reader.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	if len(body) > domain.MaxClaimTextBytes {
		return nil, domain.ErrClaimTextTooLarge
	}
	if digestProductionBytes(body) != claim.Digest {
		return nil, domain.ErrClaimTextDigestMismatch
	}
	return body, nil
}

func (w *productionPublicationWorkflow) reconcileRemediationReview(
	ctx context.Context,
	task productionPublicationTask,
	record domain.ReviewRecord,
	current []domain.Finding,
	claims []domain.AgentClaim,
) (remediationReviewOutcome, error) {
	round, remediated := remediationRoundForInvocation(task.RunID, task.ProducingInvocationID)
	if !remediated {
		return remediationReviewOutcome{}, nil
	}
	pushback, pushbackClaims, attention, err := w.parseRemediatorPushback(task, claims)
	if err != nil {
		return remediationReviewOutcome{}, err
	}
	var outcome remediationReviewOutcome
	if attention != "" {
		outcome.attention = attention
		outcome.claims = pushbackClaims
	} else if pushback != nil {
		outcome.claims = pushbackClaims
	}
	err = w.store.Read(ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetOutbox(ctx, string(task.ProducingInvocationID))
		if err != nil {
			return err
		}
		request, err := decodeRemediationRequest(entry)
		if err != nil {
			return err
		}
		if !entry.Dispatched() || request.Round != round || record.Round <= round ||
			request.BaseSHA != record.BaseSHA ||
			task.Replay.ObservedBaseSHA != record.BaseSHA || task.HeadSHA != record.HeadSHA {
			return domain.ErrParentKeyMismatch
		}
		if pushback != nil {
			for _, findingID := range pushback.FindingIDs {
				if !slices.Contains(request.FindingIDs, findingID) {
					outcome.attention = "The remediator-pushback claim named a finding outside its adjudicated route set."
					break
				}
			}
		}
		if outcome.attention != "" {
			return nil
		}
		records, err := tx.ListReviewRecords(ctx, task.RunID)
		if err != nil {
			return err
		}
		for _, priorRecord := range records {
			if priorRecord.Round > request.Round && priorRecord.Round < record.Round {
				return domain.ErrParentKeyMismatch
			}
		}
		dispositions, err := tx.ListFindingDispositions(ctx, task.RunID)
		if err != nil {
			return err
		}
		final := make(map[domain.FindingID]struct{}, len(dispositions))
		for _, disposition := range dispositions {
			final[disposition.FindingID] = struct{}{}
		}
		reemitted := make(map[domain.FindingID]struct{})
		for _, priorRecord := range records {
			if priorRecord.Round >= record.Round || priorRecord.Outcome != domain.ReviewFindings {
				continue
			}
			adjudication, err := tx.GetFindingAdjudicationForRound(
				ctx, task.RunID, priorRecord.Round)
			if errors.Is(err, store.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			routes, err := effectiveFindingRoutesTx(ctx, tx, adjudication)
			if err != nil {
				return err
			}
			for _, findingID := range remediationFindingIDs(adjudication, routes) {
				if _, ok := final[findingID]; ok {
					continue
				}
				if pushback != nil && slices.Contains(pushback.FindingIDs, findingID) {
					continue
				}
				prior, err := tx.GetFinding(ctx, findingID)
				if err != nil {
					return err
				}
				absent, err := domain.FindingIdentityAbsent(prior, current)
				if errors.Is(err, domain.ErrUnfingerprintableFinding) {
					outcome.attention = "The remediated review contains a finding whose cross-round identity cannot be proven."
					continue
				}
				if err != nil {
					return err
				}
				if absent {
					outcome.dispositions = append(outcome.dispositions, domain.ReviewDispositionRecord{
						FindingID: findingID, RunID: task.RunID, Round: priorRecord.Round,
						Disposition:             domain.ReviewDispositionFixed,
						Reason:                  "Absent from the independent review of the remediated head.",
						RemediationInvocationID: record.InvocationID,
						CreatedAt:               record.CompletedAt,
					})
					final[findingID] = struct{}{}
					continue
				}
				priorFingerprint, _ := prior.Fingerprint()
				for _, finding := range current {
					fingerprint, fingerprintErr := finding.Fingerprint()
					if fingerprintErr == nil && fingerprint == priorFingerprint {
						reemitted[finding.ID] = struct{}{}
					}
				}
			}
		}
		if len(reemitted) > 0 {
			ids := make([]domain.FindingID, 0, len(reemitted))
			for findingID := range reemitted {
				ids = append(ids, findingID)
			}
			slices.Sort(ids)
			if pushback == nil {
				outcome.dissent = &findingAdjudicationDissent{
					Kind:       findingDissentRemediationReemitted,
					FindingIDs: ids,
					Evidence:   "The independent reviewer re-emitted an adjudicated remediation finding on the new head.",
				}
			}
		}
		if pushback != nil && outcome.attention == "" {
			ids := make([]string, len(pushback.FindingIDs))
			for index, findingID := range pushback.FindingIDs {
				ids[index] = string(findingID)
			}
			outcome.attention = fmt.Sprintf(
				"The remediator reported pushback for findings %s: %s",
				strings.Join(ids, ", "), pushback.Reason,
			)
			outcome.claims = pushbackClaims
		}
		return nil
	})
	return outcome, err
}

func (w *productionPublicationWorkflow) completeRemediationNoop(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	imported importer.Result,
	sourceTree string,
) (productionTaskOutcome, error) {
	round, ok := remediationRoundForInvocation(task.RunID, task.ProducingInvocationID)
	if !ok || binding.remediation == nil || imported.CommitSHA != task.HeadSHA ||
		!validCommitSHA(sourceTree) || imported.TreeSHA != sourceTree {
		return productionTaskOutcome{}, domain.ErrParentKeyMismatch
	}
	var record domain.ReviewRecord
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		record, err = tx.GetReviewRecord(ctx, binding.remediation.request.ReviewInvocationID)
		return err
	}); err != nil {
		return productionTaskOutcome{}, err
	}
	verified := *binding.remediation
	if verified.request.Round != round || verified.request.BaseSHA != task.Replay.ObservedBaseSHA ||
		!reflect.DeepEqual(verified.publication, task.Publication) {
		return productionTaskOutcome{}, domain.ErrParentKeyMismatch
	}
	pushback, claims, attention, err := w.parseRemediatorPushback(task, imported.Claims)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	attention = remediationNoopAttention(pushback, verified.request.FindingIDs, attention)
	itemID := domain.ItemID(fmt.Sprintf("production-remediation-dissent-%s-%d", task.RunID, round))
	if err := w.putReviewAttentionWithActionsAndID(
		ctx, task, record, attention, domain.AttentionReviewDispute, itemID,
		[]domain.Action{domain.ActionDiscuss, domain.ActionStop}, claims,
	); err != nil {
		return productionTaskOutcome{}, err
	}
	return w.completeReviewEscalationTask(ctx, task, binding)
}

func (w *productionPublicationWorkflow) completeRemediationSourceIdentityDissent(
	ctx context.Context,
	task productionPublicationTask,
	binding productionBinding,
	imported importer.Result,
) (productionTaskOutcome, error) {
	round, ok := remediationRoundForInvocation(task.RunID, task.ProducingInvocationID)
	if !ok || binding.remediation == nil || imported.CommitSHA != task.HeadSHA {
		return productionTaskOutcome{}, domain.ErrParentKeyMismatch
	}
	verified := *binding.remediation
	var record domain.ReviewRecord
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		record, err = tx.GetReviewRecord(ctx, verified.request.ReviewInvocationID)
		return err
	}); err != nil {
		return productionTaskOutcome{}, err
	}
	if verified.request.Round != round || verified.request.BaseSHA != task.Replay.ObservedBaseSHA ||
		!reflect.DeepEqual(verified.publication, task.Publication) {
		return productionTaskOutcome{}, domain.ErrParentKeyMismatch
	}
	_, claims, _, err := w.parseRemediatorPushback(task, imported.Claims)
	if err != nil {
		return productionTaskOutcome{}, err
	}
	itemID := domain.ItemID(fmt.Sprintf("production-remediation-dissent-%s-%d", task.RunID, round))
	if err := w.putReviewAttentionWithActionsAndID(
		ctx, task, record,
		"The prior reviewed candidate's imported tree identity could not be authenticated, so remediation change detection stopped.",
		domain.AttentionReviewDispute, itemID,
		[]domain.Action{domain.ActionDiscuss, domain.ActionStop}, claims,
	); err != nil {
		return productionTaskOutcome{}, err
	}
	return w.completeReviewEscalationTask(ctx, task, binding)
}

func remediationNoopAttention(
	pushback *remediatorPushback,
	routed []domain.FindingID,
	parseAttention string,
) string {
	if parseAttention != "" {
		return parseAttention
	}
	if pushback == nil {
		return "The remediation export left the candidate unchanged without a remediator-pushback claim."
	}
	if !slices.Equal(pushback.FindingIDs, routed) {
		return "The unchanged remediation export's pushback did not cover every finding in its adjudicated route set."
	}
	ids := make([]string, len(pushback.FindingIDs))
	for index, findingID := range pushback.FindingIDs {
		ids[index] = string(findingID)
	}
	return fmt.Sprintf(
		"The remediator reported pushback for findings %s: %s",
		strings.Join(ids, ", "), pushback.Reason,
	)
}

func (w *productionPublicationWorkflow) reviewRecordFindings(
	ctx context.Context,
	record domain.ReviewRecord,
) ([]domain.Finding, error) {
	findings := make([]domain.Finding, len(record.FindingIDs))
	err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		for index, findingID := range record.FindingIDs {
			findings[index], err = tx.GetFinding(ctx, findingID)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return findings, err
}

func remediationFindingIDs(
	artifact domain.FindingAdjudication,
	routes map[domain.FindingID]domain.AdjudicationRoute,
) []domain.FindingID {
	ids := make([]domain.FindingID, 0, len(artifact.Entries))
	for _, entry := range artifact.Entries {
		if routes[entry.FindingID] == domain.RouteRemediate {
			ids = append(ids, entry.FindingID)
		}
	}
	slices.Sort(ids)
	return ids
}

func remediationCandidatePatch(
	ctx context.Context,
	workDir, candidateRoot, baseSHA, headSHA string,
) ([]byte, error) {
	scratch, err := os.MkdirTemp(workDir, ".remediation-patch-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // daemon-owned scratch
	runner, err := gitrun.New(gitrun.Options{Scratch: scratch})
	if err != nil {
		return nil, err
	}
	if _, err := runner.PinCheckout(ctx, candidateRoot); err != nil {
		return nil, fmt.Errorf("bind remediation candidate checkout: %w", err)
	}
	patch, err := runner.Run(
		ctx, nil, "diff", "--binary", "--full-index", "--no-ext-diff", "--no-textconv",
		baseSHA, headSHA, "--",
	)
	if err != nil {
		return nil, fmt.Errorf("render remediation candidate patch: %w", err)
	}
	return patch, nil
}

func (w *productionPublicationWorkflow) prepareRemediationIntent(
	ctx context.Context,
	task productionPublicationTask,
	record domain.ReviewRecord,
	artifact domain.FindingAdjudication,
	routes map[domain.FindingID]domain.AdjudicationRoute,
	candidateRoot string,
) (*preparedRemediationIntent, error) {
	findingIDs := remediationFindingIDs(artifact, routes)
	if len(findingIDs) == 0 {
		return nil, nil
	}
	var (
		initialInvocation domain.AgentInvocation
		findings          []domain.Finding
	)
	if err := w.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		initialInvocation, err = tx.GetAgentInvocation(ctx, productionInvocationID(task.RunID))
		if err != nil {
			return err
		}
		if len(initialInvocation.InputIDs) != 1 {
			return domain.ErrParentKeyMismatch
		}
		findings = make([]domain.Finding, len(findingIDs))
		for index, findingID := range findingIDs {
			findings[index], err = tx.GetFinding(ctx, findingID)
			if err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	candidatePatch, err := remediationCandidatePatch(
		ctx, w.workDir, candidateRoot, task.Replay.ObservedBaseSHA, task.HeadSHA)
	if err != nil {
		return nil, err
	}
	input := remediationInput{
		Version: remediationInputVersion, RunID: task.RunID, Round: record.Round,
		BaseSHA: task.Replay.ObservedBaseSHA, HeadSHA: task.HeadSHA,
		Instruction:          "Decode candidate_patch_base64 using standard base64 and apply the resulting binary patch to the exact-base workspace before remediating the adjudicated findings; preserve all prior candidate changes.",
		CandidatePatchBase64: candidatePatch,
		Adjudication:         artifact, Findings: findings,
	}
	inputBody, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	if int64(len(inputBody)) > exec.ProductionMaxInputBytes {
		// This deterministic size refusal fires during finding adjudication,
		// before any remediation invocation, stage, admission, or outbox intent
		// exists, so the dispatch-phase delivery-refusal terminalizer cannot
		// classify it. Surface the first-class undeliverable sentinel so the
		// caller raises a durable per-run escalation instead of returning a
		// lane-fatal error that halts every production publication.
		return nil, errors.Join(ErrRemediationInputUndeliverable, fmt.Errorf(
			"remediation input is %d bytes, limit %d: %w",
			len(inputBody), exec.ProductionMaxInputBytes, strictjson.ErrLimitExceeded,
		))
	}
	inputDigest := domain.Digest(contentaddr.Sum(inputBody))
	if _, err := w.artifacts.Put(inputDigest, bytes.NewReader(inputBody)); err != nil {
		return nil, fmt.Errorf("store remediation input: %w", err)
	}
	inputArtifactID := remediationInputArtifactID(task.RunID, record.Round)
	inputArtifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: inputArtifactID, Type: domain.ArtifactKindEvidence, Digest: inputDigest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerDaemon, ProducerInvocationID: record.InvocationID,
			HeadBinding: domain.HeadBound, SourceHeadSHA: task.HeadSHA,
			SensitivityClass: domain.SensitivityNormal,
		},
	}, w.approvedRecipes)
	if err != nil {
		return nil, err
	}
	invocationID := remediationInvocationID(task.RunID, record.Round)
	invocation, err := domain.NewAgentInvocation(
		invocationID, []domain.ArtifactID{initialInvocation.InputIDs[0], inputArtifactID}, nil, 0)
	if err != nil {
		return nil, err
	}
	request := remediationInvocationRequest{
		Version: remediationRequestVersion, InvocationID: invocationID,
		RunID: task.RunID, StageID: remediationStageID(task.RunID, record.Round),
		Round: record.Round, ReviewInvocationID: record.InvocationID,
		AdjudicationDigest: artifact.Digest,
		InputArtifactID:    inputArtifactID, InputArtifactDigest: inputDigest,
		BaseSHA: task.Replay.ObservedBaseSHA, HeadSHA: task.HeadSHA,
		FindingIDs: findingIDs,
	}
	payload, err := encodeRemediationRequest(request)
	if err != nil {
		return nil, err
	}
	return &preparedRemediationIntent{
		request: request, payload: payload,
		artifact: inputArtifact, invocation: invocation,
		stage:     domain.Stage{ID: request.StageID, RunID: request.RunID, Name: productionStageName},
		effective: mapsClone(routes), publication: task, reviewRecord: record,
	}, nil
}

func (intent *preparedRemediationIntent) persist(
	ctx context.Context,
	tx *store.WriteTx,
) error {
	currentRecord, err := tx.GetReviewRecord(ctx, intent.reviewRecord.InvocationID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(currentRecord, intent.reviewRecord) {
		return domain.ErrParentKeyMismatch
	}
	artifact, err := tx.GetFindingAdjudication(ctx, intent.request.AdjudicationDigest)
	if err != nil {
		return err
	}
	if artifact.RunID != intent.request.RunID || artifact.Round != intent.request.Round {
		return domain.ErrParentKeyMismatch
	}
	routes, err := effectiveFindingRoutesTx(ctx, &tx.ReadTx, artifact)
	if err != nil || !reflect.DeepEqual(routes, intent.effective) ||
		!slices.Equal(remediationFindingIDs(artifact, routes), intent.request.FindingIDs) {
		return errors.Join(err, domain.ErrParentKeyMismatch)
	}
	taskEntry, err := tx.GetOutbox(ctx, productionPublicationTaskKey(intent.request.RunID))
	if err != nil {
		return err
	}
	currentTask, err := decodeProductionPublicationTask(taskEntry)
	if err != nil {
		return err
	}
	if taskEntry.Dispatched() || !reflect.DeepEqual(currentTask, intent.publication) {
		return domain.ErrImmutableTransition
	}
	run, err := tx.GetRun(ctx, intent.request.RunID)
	if err != nil {
		return err
	}
	if stage, found := findRemediationStage(run, intent.request.Round); found {
		if stage.ID != intent.stage.ID || stage.Name != intent.stage.Name {
			return domain.ErrParentKeyMismatch
		}
	} else {
		run.Stages = append(run.Stages, intent.stage)
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
	}
	if err := tx.PutArtifact(ctx, intent.artifact); err != nil {
		return err
	}
	if err := tx.PutAgentInvocation(ctx, intent.invocation); err != nil {
		return err
	}
	entry, _, err := tx.EnqueueOutbox(
		ctx, string(intent.request.InvocationID), KindRemediationInvocationRequested, intent.payload)
	if err != nil {
		return err
	}
	if entry.Kind != KindRemediationInvocationRequested || !bytes.Equal(entry.Payload, intent.payload) {
		return domain.ErrImmutableTransition
	}
	return nil
}

func (e *Engine) loadRemediationBinding(
	ctx context.Context,
	request remediationInvocationRequest,
) (invocationBinding, error) {
	var verified authenticatedRemediationTransition
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetOutbox(ctx, string(request.InvocationID))
		if err != nil {
			return err
		}
		verified, err = authenticateRemediationInvocationTransition(
			ctx, tx, entry, request.RunID, request.StageID,
		)
		if err == nil && !reflect.DeepEqual(verified.request, request) {
			return domain.ErrParentKeyMismatch
		}
		return err
	}); err != nil {
		return invocationBinding{}, classifyRemediationMarkerError(err)
	}
	if e.productionPublication == nil || e.productionPublication.artifacts == nil {
		// Composition absence is an operational/configuration failure, not
		// evidence that the durable marker is malformed. Keep it loud so a
		// correctly stored row is never quarantined for a process-local gap.
		return invocationBinding{}, domain.ErrParentKeyMismatch
	}
	if err := authenticateRemediationInput(
		e.productionPublication.artifacts, verified,
	); err != nil {
		return invocationBinding{}, err
	}
	return verified.binding, nil
}

func authenticateRemediationInput(
	artifacts ArtifactStore,
	verified authenticatedRemediationTransition,
) error {
	if artifacts == nil {
		// Composition absence is operational, not evidence that durable state
		// is malformed. Callers keep this loud instead of quarantining the run.
		return errors.New("remediation artifact store is unavailable")
	}
	request := verified.request
	body, err := loadFakePublicationBlob(
		artifacts, request.InputArtifactDigest)
	if err != nil {
		return classifyRemediationMarkerError(err)
	}
	var input remediationInput
	if err := strictjson.Decode(
		body, &input, strictjson.RejectInvalidUTF8,
		strictjson.Limit(exec.ProductionMaxInputBytes),
	); err != nil {
		return fmt.Errorf(
			"%w: decode remediation input: %w", errRemediationMarkerUnreadable, err)
	}
	canonical, err := json.Marshal(input)
	if err != nil || !bytes.Equal(body, canonical) ||
		input.Version != remediationInputVersion || input.RunID != request.RunID ||
		input.Round != request.Round || input.BaseSHA != request.BaseSHA ||
		input.HeadSHA != request.HeadSHA || strings.TrimSpace(input.Instruction) == "" ||
		!reflect.DeepEqual(input.Adjudication, verified.adjudication) ||
		!reflect.DeepEqual(input.Findings, verified.findings) {
		return errors.Join(
			errRemediationMarkerUnreadable, err, domain.ErrParentKeyMismatch)
	}
	return nil
}

func classifyRemediationMarkerError(err error) error {
	if err == nil || errors.Is(err, errRemediationMarkerUnreadable) {
		return err
	}
	if errors.Is(err, store.ErrNotFound) ||
		errors.Is(err, domain.ErrParentKeyMismatch) ||
		errors.Is(err, domain.ErrImmutableTransition) ||
		errors.Is(err, signet.ErrBlobNotFound) ||
		errors.Is(err, signet.ErrInvalidDigest) ||
		errors.Is(err, signet.ErrDigestMismatch) {
		return errors.Join(errRemediationMarkerUnreadable, err)
	}
	return err
}
