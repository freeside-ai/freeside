package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// putArtifactIdempotent write-once registers a content-addressed research
// artifact without conflicting on a replay whose only difference is the
// recorded created_at. The reconcile transitions that build these artifacts
// (enqueueSpecificationAnswer, enqueueSpecRevision, enqueueSpecDiscussion) re-run
// on every engine tick while their answered item and command persist, and they
// write the artifact before their outbox idempotence guard; a plain PutArtifact
// of the byte-different (fresh created_at) artifact would raise
// ErrImmutableConflict and wedge the reconcile loop. The artifact's identity is
// a deterministic function of the immutable command and its content, so an
// existing row with the same id names the same bytes: its digest must match,
// and the original recorded time stays authoritative. A genuine content
// divergence is still surfaced as a conflict.
func putArtifactIdempotent(ctx context.Context, tx *store.WriteTx, artifact domain.Artifact) error {
	existing, err := tx.GetArtifact(ctx, artifact.ID)
	if errors.Is(err, store.ErrNotFound) {
		return tx.PutArtifact(ctx, artifact)
	}
	if err != nil {
		return err
	}
	if existing.Digest != artifact.Digest {
		return fmt.Errorf("artifact %s digest %s, existing row %s: %w",
			artifact.ID, artifact.Digest, existing.Digest, store.ErrImmutableConflict)
	}
	return nil
}

const (
	// KindOperatorFeedbackInvocationRequested is the durable invocation intent
	// created from answer_and_retry or return_to_agent.
	KindOperatorFeedbackInvocationRequested = "operator_feedback_invocation_requested"
	operatorFeedbackRequestVersion          = "freeside.operator-feedback-request/v1"
	operatorFeedbackInputVersion            = "freeside.operator-feedback-input/v1"
	operatorFeedbackInstruction             = "Use the operator feedback as recorded input. Preserve the existing candidate, apply the supplied patch when present, and return a complete revised candidate."
	operatorFeedbackMarkerQuarantinePrefix  = "operator-feedback-marker-quarantined-"
	operatorFeedbackQuarantineUnreadable    = "A stored operator-feedback marker could not be authenticated. The run is held out of the feedback lane, and resumes by itself once the marker reconstructs again."
)

var errOperatorFeedbackMarkerUnreadable = errors.New("operator-feedback marker unreadable")

type operatorFeedbackRequest struct {
	Version             string              `json:"version"`
	InvocationID        domain.InvocationID `json:"invocation_id"`
	RunID               domain.RunID        `json:"run_id"`
	StageID             domain.StageID      `json:"stage_id"`
	CommandID           string              `json:"command_id"`
	ItemID              domain.ItemID       `json:"item_id"`
	SourceInvocationID  domain.InvocationID `json:"source_invocation_id"`
	InputArtifactID     domain.ArtifactID   `json:"input_artifact_id"`
	InputArtifactDigest domain.Digest       `json:"input_artifact_digest"`
	BaseSHA             string              `json:"base_sha,omitempty"`
	HeadSHA             string              `json:"head_sha,omitempty"`
}

type operatorFeedbackInput struct {
	Version              string        `json:"version"`
	RunID                domain.RunID  `json:"run_id"`
	Action               domain.Action `json:"action"`
	Feedback             string        `json:"feedback"`
	Instruction          string        `json:"instruction"`
	BaseSHA              string        `json:"base_sha,omitempty"`
	HeadSHA              string        `json:"head_sha,omitempty"`
	CandidatePatchBase64 []byte        `json:"candidate_patch_base64,omitempty"`
}

func newOperatorFeedbackInput(
	runID domain.RunID,
	command domain.Command,
	base, head string,
	patch []byte,
) operatorFeedbackInput {
	return operatorFeedbackInput{
		Version: operatorFeedbackInputVersion, RunID: runID, Action: command.Action,
		Feedback: strings.TrimSpace(command.Message), Instruction: operatorFeedbackInstruction,
		BaseSHA: base, HeadSHA: head, CandidatePatchBase64: patch,
	}
}

func operatorFeedbackInvocationID(commandID string) domain.InvocationID {
	return domain.InvocationID("inv-operator-feedback-" + commandID)
}

func operatorFeedbackStageID(invocationID domain.InvocationID) domain.StageID {
	return domain.StageID("operator-feedback-" + string(invocationID))
}

func operatorFeedbackArtifactID(commandID string) domain.ArtifactID {
	return domain.ArtifactID("operator-feedback-" + commandID)
}

func operatorFeedbackUndeliverableItemID(commandID string) domain.ItemID {
	return domain.ItemID("operator-feedback-undeliverable-" + commandID)
}

func (r operatorFeedbackRequest) validate() error {
	if r.Version != operatorFeedbackRequestVersion || r.RunID == "" || r.CommandID == "" ||
		r.ItemID == "" || r.SourceInvocationID == "" ||
		r.InvocationID != operatorFeedbackInvocationID(r.CommandID) ||
		r.StageID != operatorFeedbackStageID(r.InvocationID) ||
		r.InputArtifactID != operatorFeedbackArtifactID(r.CommandID) ||
		!contentaddr.Valid(string(r.InputArtifactDigest)) {
		return domain.ErrParentKeyMismatch
	}
	if (r.BaseSHA == "") != (r.HeadSHA == "") ||
		(r.BaseSHA != "" && (!validCommitSHA(r.BaseSHA) || !validCommitSHA(r.HeadSHA))) {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

func encodeOperatorFeedbackRequest(request operatorFeedbackRequest) ([]byte, error) {
	if err := request.validate(); err != nil {
		return nil, err
	}
	return json.Marshal(request)
}

func decodeOperatorFeedbackRequest(entry store.QueueEntry) (operatorFeedbackRequest, error) {
	if entry.Kind != KindOperatorFeedbackInvocationRequested {
		return operatorFeedbackRequest{}, errors.Join(
			errOperatorFeedbackMarkerUnreadable, domain.ErrParentKeyMismatch)
	}
	var request operatorFeedbackRequest
	if err := strictjson.Decode(entry.Payload, &request, strictjson.RejectInvalidUTF8, strictjson.Limit(1<<20)); err != nil {
		return operatorFeedbackRequest{}, errors.Join(errOperatorFeedbackMarkerUnreadable, err)
	}
	if err := request.validate(); err != nil || entry.IdempotencyKey != string(request.InvocationID) {
		return operatorFeedbackRequest{}, errors.Join(
			errOperatorFeedbackMarkerUnreadable, err, domain.ErrParentKeyMismatch)
	}
	canonical, err := encodeOperatorFeedbackRequest(request)
	if err != nil || !bytes.Equal(canonical, entry.Payload) {
		return operatorFeedbackRequest{}, errors.Join(
			errOperatorFeedbackMarkerUnreadable, err, domain.ErrParentKeyMismatch)
	}
	return request, nil
}

// OperatorFeedbackInvocationBackupPayloadDigests authenticates a queued
// feedback intent. The payload references a store artifact rather than a raw
// backup blob, so it contributes no additional digest closure.
func OperatorFeedbackInvocationBackupPayloadDigests(entry store.QueueEntry) ([]domain.Digest, error) {
	if _, err := decodeOperatorFeedbackRequest(entry); err != nil {
		return nil, err
	}
	return nil, nil
}

func findOperatorFeedbackStage(run domain.Run, invocationID domain.InvocationID) (domain.Stage, bool) {
	want := operatorFeedbackStageID(invocationID)
	for _, stage := range run.Stages {
		if stage.ID == want && stage.Name == productionStageName {
			return stage, true
		}
	}
	return domain.Stage{}, false
}

func operatorFeedbackCommandMatchesItem(command domain.Command, item domain.AttentionItem) bool {
	return command.ItemID == item.ID && command.ItemVersion+1 == item.ItemVersion &&
		command.PRHeadSHA == item.PRHeadSHA &&
		slices.Equal(command.ArtifactDigests, item.ArtifactDigests) &&
		item.Status == domain.StatusSuperseded && item.DecidedAt != nil
}

func feedbackSourceInvocation(
	item domain.AttentionItem,
	accept func(domain.InvocationID) bool,
) (domain.InvocationID, error) {
	if item.Type == domain.AttentionAgentQuestion {
		if item.AgentQuestion == nil || !accept(item.AgentQuestion.InvocationID) {
			return "", domain.ErrParentKeyMismatch
		}
		return item.AgentQuestion.InvocationID, nil
	}
	seen := map[domain.InvocationID]struct{}{}
	for _, claim := range item.AgentClaims {
		id := claim.Provenance.ProducerInvocationID
		if accept(id) {
			seen[id] = struct{}{}
		}
	}
	if len(seen) != 1 {
		return "", domain.ErrParentKeyMismatch
	}
	for id := range seen {
		return id, nil
	}
	return "", domain.ErrParentKeyMismatch
}

func specificationSourceInvocation(item domain.AttentionItem, run domain.Run) (domain.InvocationID, error) {
	stage, ok := findSpecificationStage(run)
	if !ok {
		return "", domain.ErrParentKeyMismatch
	}
	return feedbackSourceInvocation(item, func(id domain.InvocationID) bool {
		for _, attempt := range stage.Attempts {
			if attempt.InvocationID == id {
				return true
			}
		}
		return false
	})
}

func implementationSourceInvocation(item domain.AttentionItem, run domain.Run) (domain.InvocationID, error) {
	return feedbackSourceInvocation(item, func(id domain.InvocationID) bool {
		_, ok := productionStageForInvocation(run, id)
		return ok
	})
}

func (e *Engine) reconcileOperatorFeedback(ctx context.Context) (int, error) {
	return e.reconcileOperatorFeedbackActions(ctx, domain.ActionAnswerAndRetry)
}

func (e *Engine) reconcileOperatorFeedbackActions(
	ctx context.Context, actions ...domain.Action,
) (int, error) {
	var selected []domain.Command
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		selected, err = tx.ListCommandsForActions(ctx, actions...)
		return err
	}); err != nil {
		return 0, err
	}
	created := 0
	var joined error
	seen := make(map[domain.ItemID]struct{}, len(selected))
	for _, selectedCommand := range selected {
		if _, duplicate := seen[selectedCommand.ItemID]; duplicate {
			continue
		}
		seen[selectedCommand.ItemID] = struct{}{}
		var item domain.AttentionItem
		if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			item, err = tx.GetAttentionItemRecord(ctx, selectedCommand.ItemID)
			return err
		}); err != nil {
			joined = errors.Join(joined, fmt.Errorf(
				"operator feedback command %q: %w", selectedCommand.CommandID, err))
			continue
		}
		if item.Status != domain.StatusSuperseded || item.Subject.RunID == nil {
			continue
		}
		commands, err := e.commandsForOperatorFeedback(ctx, item)
		if err != nil {
			joined = errors.Join(joined, fmt.Errorf("operator feedback item %q: %w", item.ID, err))
			continue
		}
		commands = slices.DeleteFunc(commands, func(command domain.Command) bool {
			return !operatorFeedbackCommandMatchesItem(command, item) ||
				(command.Action != domain.ActionAnswerAndRetry && command.Action != domain.ActionReturnToAgent)
		})
		if len(commands) == 0 {
			continue
		}
		if len(commands) != 1 {
			joined = errors.Join(joined, fmt.Errorf(
				"operator feedback item %q has %d matching commands: %w",
				item.ID, len(commands), domain.ErrParentKeyMismatch))
			continue
		}
		command := commands[0]
		if item.Type == domain.AttentionAgentQuestion {
			var run domain.Run
			if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				run, err = tx.GetRun(ctx, *item.Subject.RunID)
				return err
			}); err != nil {
				joined = errors.Join(joined, fmt.Errorf(
					"operator feedback command %q: %w", command.CommandID, err))
				continue
			}
			if owned, err := e.ownsSpecificationRun(ctx, run); err != nil {
				joined = errors.Join(joined, fmt.Errorf(
					"operator feedback command %q: %w", command.CommandID, err))
				continue
			} else if owned {
				made, err := e.enqueueSpecificationAnswer(ctx, run, item, command)
				created += boolCount(made)
				if err != nil {
					joined = errors.Join(joined, fmt.Errorf(
						"operator feedback command %q: %w", command.CommandID, err))
				}
				continue
			}
		}
		made, err := e.enqueueImplementationFeedback(ctx, item, command)
		created += boolCount(made)
		if err != nil {
			joined = errors.Join(joined, fmt.Errorf(
				"operator feedback command %q: %w", command.CommandID, err))
		}
	}
	return created, joined
}

func (w *productionPublicationWorkflow) reconcileOperatorFeedback(ctx context.Context) (int, error) {
	return (&Engine{
		store: w.store, signet: w.signet, productionPublication: w,
	}).reconcileOperatorFeedbackActions(ctx, domain.ActionReturnToAgent)
}

func (e *Engine) commandsForOperatorFeedback(
	ctx context.Context, item domain.AttentionItem,
) ([]domain.Command, error) {
	var commands []domain.Command
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		commands, err = tx.ListCommandsForItem(ctx, item.ID)
		return err
	})
	return commands, err
}

func (e *Engine) enqueueSpecificationAnswer(
	ctx context.Context, run domain.Run, item domain.AttentionItem, command domain.Command,
) (bool, error) {
	if e.specification == nil || !operatorFeedbackCommandMatchesItem(command, item) ||
		command.Action != domain.ActionAnswerAndRetry || strings.TrimSpace(command.Message) == "" {
		return false, domain.ErrParentKeyMismatch
	}
	source, err := specificationSourceInvocation(item, run)
	if err != nil {
		return false, err
	}
	var entry store.QueueEntry
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, string(source))
		return err
	}); err != nil {
		return false, err
	}
	request, binding, err := e.loadSpecificationBinding(ctx, entry)
	if err != nil || binding.run.ID != run.ID {
		return false, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	var resolved domain.ResolvedPolicy
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		resolved, err = tx.GetResolvedPolicy(ctx, run.ID)
		return err
	}); err != nil {
		return false, err
	}
	settings, err := specify.ParsePolicy(resolved)
	if err != nil {
		return false, err
	}
	if request.Iteration >= settings.MaxIterations {
		return false, e.recordSpecificationRevisionFailure(
			ctx, run, request, exec.StatusFailed, ErrSpecificationIterationsExhausted.Error())
	}
	feedbackBody := strings.TrimSpace(command.Message)
	digest := domain.Digest(contentaddr.Sum([]byte(feedbackBody)))
	if _, err := e.specification.blobs.Put(digest, strings.NewReader(feedbackBody)); err != nil {
		return false, err
	}
	feedbackID := domain.ArtifactID("answer-" + command.CommandID)
	feedback, err := domain.NewArtifact(domain.ArtifactInput{
		ID: feedbackID, Type: domain.ArtifactKindResearch, Digest: digest,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerDaemon, ProducerInvocationID: source,
			HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaTextPlain, SizeBytes: int64(len(feedbackBody)),
			CreatedAt: e.specification.now().UTC(), Source: domain.EvidenceSourceRun,
			Availability: domain.EvidenceAvailable,
		},
	}, nil)
	if err != nil {
		return false, err
	}
	next := nextSpecificationAnswerRequest(request, feedbackID)
	payload, err := encodeSpecificationRequest(next)
	if err != nil {
		return false, err
	}
	invocation, err := domain.NewAgentInvocation(next.InvocationID, next.InputArtifactIDs, nil, 0)
	if err != nil {
		return false, err
	}
	if err := e.validateProspectiveDelivery(ctx, run, invocation,
		e.specification.promptPackage, true, map[domain.ArtifactID]domain.Artifact{feedback.ID: feedback}); err != nil {
		return false, err
	}
	if err := runDurableTransitionHook(e.specification.transitionHook,
		DurableTransitionSpecificationAnswer, DurableTransitionBefore); err != nil {
		return false, err
	}
	inserted := false
	err = e.store.Write(ctx, func(tx *store.WriteTx) error {
		current, err := tx.GetAttentionItem(ctx, item.ID)
		if err != nil {
			return err
		}
		stored, err := tx.GetCommand(ctx, command.CommandID)
		if err != nil || !reflect.DeepEqual(current, item) || !reflect.DeepEqual(stored, command) ||
			!operatorFeedbackCommandMatchesItem(stored, current) {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		if err := putArtifactIdempotent(ctx, tx, feedback); err != nil {
			return err
		}
		if err := tx.PutAgentInvocation(ctx, invocation); err != nil {
			return err
		}
		queued, made, err := tx.EnqueueOutbox(
			ctx, string(next.InvocationID), KindSpecificationInvocationRequested, payload)
		if err != nil {
			return err
		}
		if !made && (queued.Kind != KindSpecificationInvocationRequested || !bytes.Equal(queued.Payload, payload)) {
			return domain.ErrImmutableTransition
		}
		inserted = made
		if !made {
			return errReplay
		}
		return nil
	})
	if errors.Is(err, errReplay) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := runDurableTransitionHook(e.specification.transitionHook,
		DurableTransitionSpecificationAnswer, DurableTransitionAfter); err != nil {
		return false, err
	}
	return inserted, nil
}

func (e *Engine) enqueueImplementationFeedback(
	ctx context.Context, item domain.AttentionItem, command domain.Command,
) (bool, error) {
	if e.productionPublication == nil || !operatorFeedbackCommandMatchesItem(command, item) ||
		strings.TrimSpace(command.Message) == "" || item.Subject.RunID == nil {
		return false, domain.ErrParentKeyMismatch
	}
	parked, err := e.operatorFeedbackUndeliverableRecorded(ctx, item, command)
	if err != nil || parked {
		return false, err
	}
	recorded, err := e.operatorFeedbackInvocationRecorded(ctx, item, command)
	if err != nil || recorded {
		return false, err
	}
	runID := *item.Subject.RunID
	var (
		run        domain.Run
		root       domain.AgentInvocation
		source     domain.InvocationID
		task       *productionPublicationTask
		base, head string
	)
	err = e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		root, err = tx.GetAgentInvocation(ctx, productionInvocationID(runID))
		if err != nil {
			return err
		}
		if command.Action == domain.ActionReturnToAgent {
			entry, err := tx.GetOutbox(ctx, productionPublicationTaskKey(runID))
			if err != nil {
				return err
			}
			decoded, err := decodeProductionPublicationTask(entry)
			if err != nil || !entry.Dispatched() || decoded.HeadSHA != item.PRHeadSHA {
				return errors.Join(err, domain.ErrParentKeyMismatch)
			}
			task = &decoded
			source, base, head = decoded.ProducingInvocationID, decoded.Replay.ObservedBaseSHA, decoded.HeadSHA
			return nil
		}
		source, err = implementationSourceInvocation(item, run)
		return err
	})
	if err != nil || len(root.InputIDs) != 1 {
		return false, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	patch := []byte(nil)
	if task != nil {
		patch, err = e.productionPublication.operatorFeedbackPatch(ctx, *task)
		if err != nil {
			return false, err
		}
	}
	return e.persistImplementationFeedback(
		ctx, item, command, run, root, source, base, head, patch,
	)
}

func (e *Engine) persistImplementationFeedback(
	ctx context.Context,
	item domain.AttentionItem,
	command domain.Command,
	run domain.Run,
	root domain.AgentInvocation,
	source domain.InvocationID,
	base, head string,
	patch []byte,
) (bool, error) {
	runID := run.ID
	if !operatorFeedbackCommandMatchesItem(command, item) || item.Subject.RunID == nil ||
		*item.Subject.RunID != runID || item.ProjectID != run.ProjectID {
		return false, domain.ErrParentKeyMismatch
	}
	input := newOperatorFeedbackInput(runID, command, base, head, patch)
	body, err := json.Marshal(input)
	if err != nil {
		return false, err
	}
	if int64(len(body)) > exec.ProductionMaxInputBytes {
		return e.recordOperatorFeedbackUndeliverable(ctx, item, command)
	}
	digest := domain.Digest(contentaddr.Sum(body))
	if _, err := e.productionPublication.artifacts.Put(digest, bytes.NewReader(body)); err != nil {
		return false, err
	}
	request := operatorFeedbackRequest{
		Version: operatorFeedbackRequestVersion, InvocationID: operatorFeedbackInvocationID(command.CommandID),
		RunID: runID, CommandID: command.CommandID, ItemID: item.ID,
		SourceInvocationID: source, InputArtifactID: operatorFeedbackArtifactID(command.CommandID),
		InputArtifactDigest: digest, BaseSHA: base, HeadSHA: head,
	}
	request.StageID = operatorFeedbackStageID(request.InvocationID)
	payload, err := encodeOperatorFeedbackRequest(request)
	if err != nil {
		return false, err
	}
	provenance := domain.Provenance{
		ProducerClass: domain.ProducerDaemon, ProducerInvocationID: source,
		HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
	}
	if command.Action == domain.ActionReturnToAgent {
		provenance.HeadBinding = domain.HeadBound
		provenance.SourceHeadSHA = head
	}
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: request.InputArtifactID, Type: domain.ArtifactKindEvidence, Digest: digest,
		Provenance: provenance,
		// body is the JSON operator-feedback input the digest names.
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: int64(len(body)),
			CreatedAt: e.productionPublication.now().UTC(), Source: domain.EvidenceSourceRun,
			Availability: domain.EvidenceAvailable,
		},
	}, e.productionPublication.approvedRecipes)
	if err != nil {
		return false, err
	}
	invocation, err := domain.NewAgentInvocation(
		request.InvocationID, []domain.ArtifactID{root.InputIDs[0], request.InputArtifactID}, nil, 0)
	if err != nil {
		return false, err
	}
	stage := domain.Stage{ID: request.StageID, RunID: runID, Name: productionStageName}
	if err := runDurableTransitionHook(e.productionPublication.transitionHook,
		DurableTransitionOperatorFeedback, DurableTransitionBefore); err != nil {
		return false, err
	}
	inserted := false
	err = e.store.Write(ctx, func(tx *store.WriteTx) error {
		current, err := tx.GetAttentionItem(ctx, item.ID)
		if err != nil {
			return err
		}
		stored, err := tx.GetCommand(ctx, command.CommandID)
		if err != nil || !reflect.DeepEqual(current, item) || !reflect.DeepEqual(stored, command) ||
			!operatorFeedbackCommandMatchesItem(stored, current) {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		currentRun, err := tx.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		if existing, found := findOperatorFeedbackStage(currentRun, request.InvocationID); found {
			if existing.ID != stage.ID {
				return domain.ErrParentKeyMismatch
			}
		} else {
			currentRun.Stages = append(currentRun.Stages, stage)
			if err := tx.PutRun(ctx, currentRun); err != nil {
				return err
			}
		}
		if err := tx.PutArtifact(ctx, artifact); err != nil {
			return err
		}
		if err := tx.PutAgentInvocation(ctx, invocation); err != nil {
			return err
		}
		queued, made, err := tx.EnqueueOutbox(
			ctx, string(request.InvocationID), KindOperatorFeedbackInvocationRequested, payload)
		if err != nil {
			return err
		}
		if !made && (queued.Kind != KindOperatorFeedbackInvocationRequested || !bytes.Equal(queued.Payload, payload)) {
			return domain.ErrImmutableTransition
		}
		inserted = made
		if !made {
			return errReplay
		}
		return nil
	})
	if errors.Is(err, errReplay) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := runDurableTransitionHook(e.productionPublication.transitionHook,
		DurableTransitionOperatorFeedback, DurableTransitionAfter); err != nil {
		return false, err
	}
	return inserted, nil
}

func (e *Engine) operatorFeedbackUndeliverableRecorded(
	ctx context.Context, source domain.AttentionItem, command domain.Command,
) (bool, error) {
	itemID := operatorFeedbackUndeliverableItemID(command.CommandID)
	recorded := false
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(ctx, itemID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := verifyOperatorFeedbackUndeliverableItem(item, source, command); err != nil {
			return err
		}
		recorded = true
		return nil
	}); err != nil {
		return false, err
	}
	return recorded, nil
}

func (e *Engine) operatorFeedbackInvocationRecorded(
	ctx context.Context, item domain.AttentionItem, command domain.Command,
) (bool, error) {
	if item.Subject.RunID == nil {
		return false, domain.ErrParentKeyMismatch
	}
	invocationID := operatorFeedbackInvocationID(command.CommandID)
	recorded := false
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		entry, err := tx.GetOutbox(ctx, string(invocationID))
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		request, _, err := authenticateOperatorFeedbackTransition(
			ctx, tx, entry, *item.Subject.RunID, operatorFeedbackStageID(invocationID),
		)
		if err != nil || request.CommandID != command.CommandID || request.ItemID != item.ID {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		recorded = true
		return nil
	})
	return recorded, err
}

func (e *Engine) recordOperatorFeedbackUndeliverable(
	ctx context.Context, source domain.AttentionItem, command domain.Command,
) (bool, error) {
	if e.productionPublication == nil || !operatorFeedbackCommandMatchesItem(command, source) ||
		source.Subject.RunID == nil {
		return false, domain.ErrParentKeyMismatch
	}
	itemID := operatorFeedbackUndeliverableItemID(command.CommandID)
	created := false
	err := e.store.Write(ctx, func(tx *store.WriteTx) error {
		existing, err := tx.GetAttentionItem(ctx, itemID)
		if err == nil {
			return verifyOperatorFeedbackUndeliverableItem(existing, source, command)
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		runID := *source.Subject.RunID
		subject := domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID}
		names, err := tx.DisplayNamesFor(ctx, source.ProjectID, subject)
		if err != nil {
			return err
		}
		createdAt := e.productionPublication.attentionCreatedAt()
		item, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID: itemID, ProjectID: source.ProjectID,
			Subject: subject, Type: domain.AttentionExecutionFailure, Priority: domain.PriorityHigh,
			Reason:            "Operator feedback input cannot be delivered to the implementation agent because the candidate is too large.",
			RequestedDecision: []domain.Action{domain.ActionAcknowledge},
			ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional,
			CreatedAt: &createdAt, DisplayNames: names, Status: domain.StatusOpen,
		}, e.productionPublication.approvedRecipes)
		if err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return err
		}
		created = true
		return nil
	})
	return created, err
}

func verifyOperatorFeedbackUndeliverableItem(
	item domain.AttentionItem, source domain.AttentionItem, command domain.Command,
) error {
	validSubject := source.Subject.RunID != nil && item.Subject.Type == domain.SubjectRun &&
		item.Subject.ID == domain.SubjectID(*source.Subject.RunID) && item.Subject.RunID != nil &&
		*item.Subject.RunID == *source.Subject.RunID
	if item.ID != operatorFeedbackUndeliverableItemID(command.CommandID) ||
		item.Type != domain.AttentionExecutionFailure || item.ProjectID != source.ProjectID || !validSubject {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

func (w *productionPublicationWorkflow) operatorFeedbackPatch(
	ctx context.Context, task productionPublicationTask,
) ([]byte, error) {
	binding, err := w.loadBinding(ctx, task)
	if err != nil {
		return nil, err
	}
	scratch, err := os.MkdirTemp(w.workDir, ".operator-feedback-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(scratch) //nolint:errcheck // daemon-owned scratch
	parentInfo, err := os.Stat(scratch)
	if err != nil {
		return nil, err
	}
	checkout, err := w.transport.FetchBase(
		ctx, binding.admission.Base.Repo, binding.admission.Base.BaseRef,
		binding.admission.Base.BaseSHA, filepath.Join(scratch, "checkout"))
	if err != nil {
		return nil, err
	}
	checkoutDir, err := validatePublicationCheckoutBinding(
		checkout, binding.admission.Base.Repo, binding.admission.Base.BaseRef,
		binding.admission.Base.BaseSHA, filepath.Join(scratch, "checkout"), parentInfo)
	if err != nil {
		return nil, err
	}
	handoffDir := filepath.Join(scratch, "handoff")
	if err := w.materializeReplay(task.Replay, handoffDir); err != nil {
		return nil, err
	}
	imported, err := importer.Import(ctx, handoffDir, checkoutDir, task.Replay.ImportOptions)
	if err != nil {
		return nil, err
	}
	if imported.CommitSHA != task.HeadSHA {
		return nil, domain.ErrParentKeyMismatch
	}
	return remediationCandidatePatch(
		ctx, w.workDir, checkoutDir, task.Replay.ObservedBaseSHA, task.HeadSHA)
}

func authenticateOperatorFeedbackTransition(
	ctx context.Context, tx *store.ReadTx, entry store.QueueEntry,
	runID domain.RunID, stageID domain.StageID,
) (operatorFeedbackRequest, ProductionPublication, error) {
	request, err := decodeOperatorFeedbackRequest(entry)
	if err != nil || request.RunID != runID || request.StageID != stageID {
		return operatorFeedbackRequest{}, ProductionPublication{}, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	run, err := tx.GetRun(ctx, runID)
	if err != nil {
		return operatorFeedbackRequest{}, ProductionPublication{}, err
	}
	if _, found := findOperatorFeedbackStage(run, request.InvocationID); !found {
		return operatorFeedbackRequest{}, ProductionPublication{}, domain.ErrParentKeyMismatch
	}
	item, err := tx.GetAttentionItemRecord(ctx, request.ItemID)
	if err != nil {
		return operatorFeedbackRequest{}, ProductionPublication{}, err
	}
	command, err := tx.GetCommand(ctx, request.CommandID)
	if err != nil || !operatorFeedbackCommandMatchesItem(command, item) ||
		(command.Action != domain.ActionAnswerAndRetry && command.Action != domain.ActionReturnToAgent) ||
		item.Subject.RunID == nil || *item.Subject.RunID != runID {
		return operatorFeedbackRequest{}, ProductionPublication{}, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	invocation, err := tx.GetAgentInvocation(ctx, request.InvocationID)
	if err != nil {
		return operatorFeedbackRequest{}, ProductionPublication{}, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	artifact, err := tx.GetArtifact(ctx, request.InputArtifactID)
	if err != nil {
		return operatorFeedbackRequest{}, ProductionPublication{}, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	initial, err := tx.GetAgentInvocation(ctx, productionInvocationID(runID))
	if err != nil {
		return operatorFeedbackRequest{}, ProductionPublication{}, err
	}
	if len(initial.InputIDs) != 1 {
		return operatorFeedbackRequest{}, ProductionPublication{}, domain.ErrParentKeyMismatch
	}
	initialInput, err := tx.GetArtifact(ctx, initial.InputIDs[0])
	if err != nil {
		return operatorFeedbackRequest{}, ProductionPublication{}, err
	}
	provenance := artifact.Provenance
	wantHeadBinding := domain.HeadIndependent
	wantSourceHead := ""
	if command.Action == domain.ActionReturnToAgent {
		wantHeadBinding = domain.HeadBound
		wantSourceHead = request.HeadSHA
	}
	if invocation.ConversationID != nil || initial.ConversationID != nil ||
		!slices.Equal(invocation.InputIDs,
			[]domain.ArtifactID{initial.InputIDs[0], request.InputArtifactID}) ||
		initialInput.Type != domain.ArtifactKindSpecification || initialInput.Digest != run.SpecDigest ||
		artifact.Type != domain.ArtifactKindEvidence || artifact.Digest != request.InputArtifactDigest ||
		provenance.ProducerClass != domain.ProducerDaemon ||
		provenance.ProducerInvocationID != request.SourceInvocationID ||
		provenance.HeadBinding != wantHeadBinding || provenance.SourceHeadSHA != wantSourceHead ||
		provenance.SensitivityClass != domain.SensitivityNormal {
		return operatorFeedbackRequest{}, ProductionPublication{}, domain.ErrParentKeyMismatch
	}
	rootEntry, err := tx.GetOutbox(ctx, string(productionInvocationID(runID)))
	if err != nil {
		return operatorFeedbackRequest{}, ProductionPublication{}, err
	}
	rootMarker, err := authenticateProductionMarker(rootEntry, runID)
	if err != nil || rootMarker.Legacy {
		return operatorFeedbackRequest{}, ProductionPublication{}, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	if command.Action == domain.ActionReturnToAgent {
		taskEntry, err := tx.GetOutbox(ctx, productionPublicationTaskKey(runID))
		if err != nil {
			return operatorFeedbackRequest{}, ProductionPublication{}, err
		}
		task, err := decodeProductionPublicationTask(taskEntry)
		if err != nil || !taskEntry.Dispatched() || task.ProducingInvocationID != request.SourceInvocationID ||
			task.HeadSHA != request.HeadSHA || task.Replay.ObservedBaseSHA != request.BaseSHA {
			return operatorFeedbackRequest{}, ProductionPublication{}, errors.Join(err, domain.ErrParentKeyMismatch)
		}
	} else {
		if request.BaseSHA != "" || request.HeadSHA != "" {
			return operatorFeedbackRequest{}, ProductionPublication{}, domain.ErrParentKeyMismatch
		}
		if stage, ok := productionStageForInvocation(run, request.SourceInvocationID); !ok || stage.ID == request.StageID {
			return operatorFeedbackRequest{}, ProductionPublication{}, domain.ErrParentKeyMismatch
		}
	}
	return request, rootMarker.Publication, nil
}

// AuthenticateOperatorFeedbackInvocationTransition returns the immutable
// publication authority inherited by a feedback invocation after rebuilding
// the item, command, run, artifact, and source-task bindings.
func AuthenticateOperatorFeedbackInvocationTransition(
	ctx context.Context, tx *store.ReadTx, entry store.QueueEntry,
	runID domain.RunID, stageID domain.StageID,
) (ProductionPublication, error) {
	_, publication, err := authenticateOperatorFeedbackTransition(ctx, tx, entry, runID, stageID)
	return publication, err
}

func authenticateOperatorFeedbackAttempt(
	ctx context.Context,
	tx *store.ReadTx,
	invocationID domain.InvocationID,
	runID domain.RunID,
	stageID domain.StageID,
) (bool, error) {
	entry, err := tx.GetOutbox(ctx, string(invocationID))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if entry.Kind != KindOperatorFeedbackInvocationRequested {
		return false, nil
	}
	_, _, err = authenticateOperatorFeedbackTransition(ctx, tx, entry, runID, stageID)
	return true, err
}

func (e *Engine) loadOperatorFeedbackBinding(
	ctx context.Context, entry store.QueueEntry,
) (operatorFeedbackRequest, invocationBinding, error) {
	request, err := decodeOperatorFeedbackRequest(entry)
	if err != nil {
		return operatorFeedbackRequest{}, invocationBinding{}, err
	}
	var binding invocationBinding
	err = e.store.Read(ctx, func(tx *store.ReadTx) error {
		if _, _, err := authenticateOperatorFeedbackTransition(
			ctx, tx, entry, request.RunID, request.StageID); err != nil {
			return err
		}
		var err error
		binding.run, err = tx.GetRun(ctx, request.RunID)
		if err != nil {
			return err
		}
		binding.item, err = tx.GetAttentionItemRecord(ctx, request.ItemID)
		if err != nil {
			return err
		}
		binding.invocation, err = tx.GetAgentInvocation(ctx, request.InvocationID)
		return err
	})
	if err == nil {
		err = e.authenticateOperatorFeedbackInput(ctx, request)
	}
	return request, binding, classifyOperatorFeedbackMarkerError(err)
}

func (e *Engine) authenticateOperatorFeedbackInput(
	ctx context.Context,
	request operatorFeedbackRequest,
) error {
	if e.productionPublication == nil || e.productionPublication.artifacts == nil {
		return errors.New("operator-feedback artifact store is unavailable")
	}
	var command domain.Command
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		command, err = tx.GetCommand(ctx, request.CommandID)
		if err != nil {
			return err
		}
		if command.Action != domain.ActionReturnToAgent {
			return nil
		}
		entry, err := tx.GetOutbox(ctx, productionPublicationTaskKey(request.RunID))
		if err != nil {
			return err
		}
		decoded, err := decodeProductionPublicationTask(entry)
		if err != nil || !entry.Dispatched() ||
			decoded.ProducingInvocationID != request.SourceInvocationID ||
			decoded.Replay.ObservedBaseSHA != request.BaseSHA || decoded.HeadSHA != request.HeadSHA {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		return nil
	}); err != nil {
		return classifyOperatorFeedbackMarkerError(err)
	}
	body, err := loadFakePublicationBlob(
		e.productionPublication.artifacts, request.InputArtifactDigest)
	if err != nil {
		return classifyOperatorFeedbackMarkerError(err)
	}
	return authenticateOperatorFeedbackInputBody(request, command, body)
}

// authenticateOperatorFeedbackInputBody binds the stored feedback input to the
// immutable accepted command. Every command-derived field is recomputed and
// compared, so the operator text delivered to the agent is always the accepted
// one. The candidate patch is taken from the stored input rather than rebuilt:
// it is daemon-authored content sealed by the same content-addressed digest in
// the same store as the command, so regenerating it would only move the root
// of trust while putting forge I/O on the global dispatch path.
func authenticateOperatorFeedbackInputBody(
	request operatorFeedbackRequest,
	command domain.Command,
	body []byte,
) error {
	var stored operatorFeedbackInput
	if err := strictjson.Decode(
		body, &stored, strictjson.RejectInvalidUTF8, strictjson.NoLimit,
	); err != nil {
		return errors.Join(errOperatorFeedbackMarkerUnreadable, err)
	}
	if command.Action != domain.ActionReturnToAgent && len(stored.CandidatePatchBase64) > 0 {
		return errors.Join(errOperatorFeedbackMarkerUnreadable, domain.ErrParentKeyMismatch)
	}
	expected, err := json.Marshal(newOperatorFeedbackInput(
		request.RunID, command, request.BaseSHA, request.HeadSHA, stored.CandidatePatchBase64,
	))
	if err != nil {
		return err
	}
	if domain.Digest(contentaddr.Sum(expected)) != request.InputArtifactDigest ||
		!bytes.Equal(body, expected) {
		return errors.Join(errOperatorFeedbackMarkerUnreadable, domain.ErrParentKeyMismatch)
	}
	return nil
}

func classifyOperatorFeedbackMarkerError(err error) error {
	if err == nil || errors.Is(err, errOperatorFeedbackMarkerUnreadable) {
		return err
	}
	if errors.Is(err, store.ErrNotFound) ||
		errors.Is(err, domain.ErrParentKeyMismatch) ||
		errors.Is(err, domain.ErrImmutableTransition) ||
		errors.Is(err, signet.ErrBlobNotFound) ||
		errors.Is(err, signet.ErrInvalidDigest) ||
		errors.Is(err, signet.ErrDigestMismatch) {
		return errors.Join(errOperatorFeedbackMarkerUnreadable, err)
	}
	return err
}

func (e *Engine) quarantinePendingOperatorFeedbackMarker(
	ctx context.Context, entry store.QueueEntry, cause error,
) (bool, error) {
	if !errors.Is(cause, errOperatorFeedbackMarkerUnreadable) {
		return false, nil
	}
	const prefix = "inv-operator-feedback-"
	raw := entry.IdempotencyKey
	if !strings.HasPrefix(raw, prefix) || len(raw) == len(prefix) {
		return false, nil
	}
	commandID := strings.TrimPrefix(raw, prefix)
	var (
		item domain.AttentionItem
		run  domain.Run
	)
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		command, err := tx.GetCommand(ctx, commandID)
		if err != nil {
			return err
		}
		item, err = tx.GetAttentionItemRecord(ctx, command.ItemID)
		if err != nil || item.Subject.RunID == nil {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		run, err = tx.GetRun(ctx, *item.Subject.RunID)
		if err != nil || run.ProjectID != item.ProjectID {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		return nil
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, domain.ErrParentKeyMismatch) {
			return false, nil
		}
		return false, err
	}
	return true, recordProductionQuarantine(
		ctx, e.store, e.signet, operatorFeedbackMarkerQuarantinePrefix,
		run.ID, run.ProjectID, operatorFeedbackQuarantineUnreadable)
}
