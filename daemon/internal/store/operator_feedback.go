package store

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// OperatorFeedbackIntent reconstructs the command, input and retry lineage.
// Engine-specific publication task and blob checks remain at dispatch; this
// shared boundary also protects Signet's acceptance of a retry command.
func (tx *ReadTx) OperatorFeedbackIntent(
	ctx context.Context, id domain.InvocationID,
) (domain.OperatorFeedbackInvocationIntent, error) {
	seen := make(map[domain.InvocationID]bool)
	return tx.operatorFeedbackIntent(ctx, id, seen)
}

func (tx *ReadTx) operatorFeedbackIntent(
	ctx context.Context, id domain.InvocationID, seen map[domain.InvocationID]bool,
) (domain.OperatorFeedbackInvocationIntent, error) {
	if seen[id] {
		return domain.OperatorFeedbackInvocationIntent{}, domain.ErrParentKeyMismatch
	}
	seen[id] = true
	entry, err := tx.GetOutbox(ctx, string(id))
	if err != nil {
		return domain.OperatorFeedbackInvocationIntent{}, err
	}
	request, err := domain.DecodeOperatorFeedbackInvocationIntent(entry.Payload)
	if err != nil || entry.Kind != string(domain.OperatorFeedbackInvocationRequestedKind) ||
		entry.IdempotencyKey != string(request.InvocationID) || request.InvocationID != id {
		return request, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	run, err := tx.GetRun(ctx, request.RunID)
	if err != nil {
		return request, err
	}
	if !slices.ContainsFunc(run.Stages, func(stage domain.Stage) bool {
		return stage.ID == request.StageID && stage.RunID == run.ID && stage.Name == "implement"
	}) {
		return request, domain.ErrParentKeyMismatch
	}
	item, err := tx.GetAttentionItemRecord(ctx, request.ItemID)
	if err != nil {
		return request, err
	}
	command, err := tx.GetCommand(ctx, request.CommandID)
	if err != nil || command.ItemID != item.ID || command.ItemVersion+1 != item.ItemVersion ||
		command.PRHeadSHA != item.PRHeadSHA || !slices.Equal(command.ArtifactDigests, item.ArtifactDigests) ||
		item.Status != domain.StatusSuperseded || item.DecidedAt == nil ||
		item.Subject.RunID == nil || *item.Subject.RunID != run.ID || item.ProjectID != run.ProjectID ||
		strings.TrimSpace(command.Message) == "" ||
		(command.Action != domain.ActionReturnToAgent && command.Action != domain.ActionAnswerAndRetry) {
		return request, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	input, err := tx.GetArtifact(ctx, request.InputArtifactID)
	if err != nil {
		return request, err
	}
	wantBinding, wantHead := domain.HeadIndependent, ""
	if command.Action == domain.ActionReturnToAgent {
		wantBinding, wantHead = domain.HeadBound, request.HeadSHA
		ready, err := tx.GetReadyItemPRBinding(ctx, item.ID)
		if err != nil || ready.RunID != run.ID || ready.ProducingInvocationID != request.SourceInvocationID ||
			ready.HeadSHA != request.HeadSHA {
			return request, errors.Join(err, domain.ErrParentKeyMismatch)
		}
	} else if request.HeadSHA != "" || request.BaseSHA != "" {
		return request, domain.ErrParentKeyMismatch
	}
	if input.Type != domain.ArtifactKindEvidence || input.Digest != request.InputArtifactDigest ||
		input.Provenance.ProducerClass != domain.ProducerDaemon ||
		input.Provenance.ProducerInvocationID != request.SourceInvocationID ||
		input.Provenance.HeadBinding != wantBinding || input.Provenance.SourceHeadSHA != wantHead ||
		input.Provenance.SensitivityClass != domain.SensitivityNormal {
		return request, domain.ErrParentKeyMismatch
	}
	invocation, err := tx.GetAgentInvocation(ctx, request.InvocationID)
	if err != nil || invocation.ConversationID != nil || invocation.ThroughSequence != 0 {
		return request, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	source, err := tx.GetAgentInvocation(ctx, request.SourceInvocationID)
	if err != nil {
		return request, err
	}
	admission, err := tx.GetExecutionAdmissionRecord(ctx, source.ID)
	if err != nil {
		return request, err
	}
	digest, err := source.ComputeInputDigest()
	if err != nil || admission.RunID != run.ID || admission.InputDigest != digest ||
		!slices.Equal(invocation.InputIDs, append(slices.Clone(source.InputIDs), request.InputArtifactID)) {
		return request, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	if request.Retry != nil {
		parent, err := tx.operatorFeedbackIntent(ctx, request.Retry.FailedInvocationID, seen)
		if err != nil {
			return request, err
		}
		parent.Version, parent.InvocationID, parent.StageID, parent.Retry = request.Version, request.InvocationID, request.StageID, request.Retry
		if !reflect.DeepEqual(parent, request) {
			return request, domain.ErrParentKeyMismatch
		}
		failure, err := tx.GetAttentionItemRecord(ctx, request.Retry.ItemID)
		if err != nil {
			return request, err
		}
		retry, err := tx.GetCommand(ctx, request.Retry.CommandID)
		if err != nil || retry.Action != domain.ActionRetry || retry.Message != "" || len(retry.Attachments) != 0 ||
			retry.ItemID != failure.ID || retry.ItemVersion+1 != failure.ItemVersion ||
			retry.PRHeadSHA != failure.PRHeadSHA || !slices.Equal(retry.ArtifactDigests, failure.ArtifactDigests) ||
			failure.Status != domain.StatusResolved || failure.DecidedAt == nil {
			return request, errors.Join(err, domain.ErrParentKeyMismatch)
		}
		if err := tx.feedbackFailure(ctx, failure, request.Retry.FailedInvocationID, run.ID); err != nil {
			return request, err
		}
	}
	return request, nil
}

func (tx *ReadTx) feedbackFailure(
	ctx context.Context, item domain.AttentionItem, invocation domain.InvocationID, runID domain.RunID,
) error {
	if item.ID != domain.ItemID("execution-failure-"+string(invocation)) ||
		item.Type != domain.AttentionExecutionFailure || item.ExecutionFailure == nil ||
		item.ExecutionFailure.InvocationID != invocation || item.ExecutionFailure.Stage != domain.StageNameImplementation ||
		item.Subject.RunID == nil || *item.Subject.RunID != runID {
		return domain.ErrParentKeyMismatch
	}
	outcome, err := tx.GetExecutionOutcomeRecord(ctx, invocation)
	if err != nil || outcome.Status != item.ExecutionFailure.Outcome ||
		(outcome.Status != domain.ExecutionOutcomeFailed && outcome.Status != domain.ExecutionOutcomeCanceled &&
			outcome.Status != domain.ExecutionOutcomeLost) {
		return errors.Join(err, domain.ErrParentKeyMismatch)
	}
	if _, err := tx.GetExecutionExportRecord(ctx, invocation); !errors.Is(err, ErrNotFound) {
		return errors.Join(err, domain.ErrParentKeyMismatch)
	}
	return nil
}

// FeedbackPublicationParent follows answer and retry ancestry to the accepted
// return command and its immutable published candidate. A pre-publication
// question has no such ancestor and returns found=false.
func (tx *ReadTx) FeedbackPublicationParent(
	ctx context.Context, invocation domain.InvocationID,
) (domain.OperatorFeedbackInvocationIntent, domain.ReadyItemPRBinding, bool, error) {
	seen := make(map[domain.InvocationID]bool)
	for strings.HasPrefix(string(invocation), "inv-operator-feedback-") {
		if seen[invocation] {
			return domain.OperatorFeedbackInvocationIntent{}, domain.ReadyItemPRBinding{}, false, domain.ErrParentKeyMismatch
		}
		seen[invocation] = true
		request, err := tx.OperatorFeedbackIntent(ctx, invocation)
		if err != nil {
			return request, domain.ReadyItemPRBinding{}, false, err
		}
		if request.HeadSHA != "" {
			ready, err := tx.GetReadyItemPRBinding(ctx, request.ItemID)
			return request, ready, err == nil, err
		}
		invocation = request.SourceInvocationID
	}
	return domain.OperatorFeedbackInvocationIntent{}, domain.ReadyItemPRBinding{}, false, nil
}

// OperatorFeedbackRetryParent is the command-acceptance gate for the narrow
// post-publication retry route. It does not manufacture retry authority for
// generic failed stages or for a terminal export interpreted as a failure.
func (tx *ReadTx) OperatorFeedbackRetryParent(
	ctx context.Context, item domain.AttentionItem,
) (domain.OperatorFeedbackInvocationIntent, error) {
	if item.Status != domain.StatusOpen || item.DecidedAt != nil || item.ExecutionFailure == nil {
		return domain.OperatorFeedbackInvocationIntent{}, domain.ErrParentKeyMismatch
	}
	parent, err := tx.OperatorFeedbackIntent(ctx, item.ExecutionFailure.InvocationID)
	if err != nil {
		return parent, err
	}
	if err := tx.feedbackFailure(ctx, item, parent.InvocationID, parent.RunID); err != nil {
		return parent, err
	}
	if _, _, found, err := tx.FeedbackPublicationParent(ctx, parent.InvocationID); err != nil || !found {
		return parent, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	if err := tx.RequireIncompletePublication(ctx, parent.RunID); err != nil {
		return parent, err
	}
	return parent, nil
}
