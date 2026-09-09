package store

import (
	"context"
	"encoding/base64"
	"errors"
	"reflect"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// PublicationContinuationIntent reauthenticates the accepted decision and its
// immutable source records on every read, including after dispatch.
func (tx *ReadTx) PublicationContinuationIntent(ctx context.Context, runID domain.RunID, commandID string) (domain.PublicationContinuationIntent, QueueEntry, error) {
	key := (domain.PublicationContinuationIntent{Successor: domain.PublicationSuccessor{RunID: runID, CommandID: commandID}}).Key()
	entry, err := tx.GetOutbox(ctx, key)
	if err != nil {
		return domain.PublicationContinuationIntent{}, entry, err
	}
	r, err := domain.DecodePublicationContinuationIntent(entry.Payload)
	if err != nil || entry.Kind != domain.PublicationContinuationRequestedKind || r.Key() != key || r.Successor.RunID != runID || r.Successor.CommandID != commandID {
		return r, entry, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	return r, entry, tx.AuthenticatePublicationContinuation(ctx, r)
}

func (tx *ReadTx) AuthenticatePublicationContinuation(ctx context.Context, r domain.PublicationContinuationIntent) error {
	if err := r.Validate(); err != nil {
		return err
	}
	s := r.Successor
	command, err := tx.GetCommand(ctx, s.CommandID)
	if err != nil {
		return err
	}
	item, err := tx.GetAttentionItemRecord(ctx, command.ItemID)
	if err != nil {
		return err
	}
	run, err := tx.GetRun(ctx, s.RunID)
	if err != nil {
		return err
	}
	if command.Action != domain.ActionApprove || command.ItemID != domain.ItemID(domain.PublicationContinuationItemPrefix+s.ReevaluationCommandID) ||
		command.Message != "" || len(command.Attachments) != 0 || command.ItemVersion+1 != item.ItemVersion ||
		command.PRHeadSHA != item.PRHeadSHA || !slices.Equal(command.ArtifactDigests, item.ArtifactDigests) ||
		item.Type != domain.AttentionReviewDispute || item.Status != domain.StatusResolved || item.DecidedAt == nil ||
		!item.Offers(domain.ActionApprove) || item.ProjectID != run.ProjectID || item.Subject.Type != domain.SubjectRun ||
		item.Subject.ID != domain.SubjectID(run.ID) || item.Subject.RunID == nil || *item.Subject.RunID != run.ID {
		return domain.ErrParentKeyMismatch
	}
	prior, err := tx.GetReviewRecord(ctx, s.PriorReviewInvocationID)
	if err != nil {
		return err
	}
	if prior.RunID != run.ID || prior.HeadSHA != item.PRHeadSHA ||
		prior.Outcome != domain.ReviewFindings || prior.Round+1 != s.ReviewRound || item.ReviewDispute == nil ||
		item.ReviewDispute.RunID != run.ID || item.ReviewDispute.Round != prior.Round ||
		item.ReviewDispute.CompletionEvidence != prior.CompletionEvidence || !slices.Equal(item.ReviewDispute.FindingIDs, prior.FindingIDs) {
		return domain.ErrParentKeyMismatch
	}
	wantTaskKey := "production-publication/" + string(run.ID)
	if s.PredecessorItemID != domain.ProductionReadyItemID(run.ID) {
		parent, err := tx.PublicationSuccessorForReadyItem(ctx, run.ID, s.PredecessorItemID)
		if err != nil {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		wantTaskKey = parent.TaskKey()
	}
	source, err := tx.GetOutbox(ctx, r.SourceTaskKey)
	if err != nil {
		return err
	}
	if r.SourceTaskKey != wantTaskKey || source.Kind != "production_publication_requested" || !source.Dispatched() ||
		domain.Digest(contentaddr.Sum(source.Payload)) != r.SourceTaskDigest {
		return domain.ErrParentKeyMismatch
	}
	adjudication, err := tx.GetFindingAdjudication(ctx, r.AdjudicationDigest)
	if err != nil {
		return err
	}
	if adjudication.RunID != run.ID || adjudication.Round != prior.Round {
		return domain.ErrParentKeyMismatch
	}
	if err := tx.authenticateContinuationReevaluation(ctx, s, item, prior); err != nil {
		return err
	}
	_, err = tx.publishedReadyAtOrBefore(ctx, run.ID, s.PredecessorItemID)
	return err
}

func (tx *ReadTx) authenticateContinuationReevaluation(ctx context.Context, s domain.PublicationSuccessor, item domain.AttentionItem, prior domain.ReviewRecord) error {
	key := "production-reevaluation/" + base64.RawURLEncoding.EncodeToString([]byte(s.RunID)) + "/" + s.ReevaluationCommandID
	entry, err := tx.GetOutbox(ctx, key)
	if err != nil {
		return err
	}
	var request domain.PublicationReevaluationRequest
	if err := strictjson.Decode(entry.Payload, &request, strictjson.RejectInvalidUTF8, strictjson.Limit(1<<20)); err != nil {
		return err
	}
	if entry.Kind != "production_publication_reevaluation_requested" || !entry.Dispatched() || request.RunID != s.RunID ||
		request.CommandID != s.ReevaluationCommandID || request.PRHeadSHA != item.PRHeadSHA || request.ReviewRound > prior.Round {
		return domain.ErrParentKeyMismatch
	}
	rerun, err := tx.GetCommand(ctx, request.CommandID)
	if err != nil {
		return err
	}
	blocked, err := tx.GetAttentionItemRecord(ctx, request.ItemID)
	if err != nil {
		return err
	}
	if rerun.Action != domain.ActionRerunTrustEvaluation || rerun.ItemID != request.ItemID || rerun.ItemVersion != request.ItemVersion ||
		rerun.PRHeadSHA != request.PRHeadSHA || !slices.Equal(rerun.ArtifactDigests, blocked.ArtifactDigests) ||
		blocked.Status != domain.StatusResolved || blocked.DecidedAt == nil || blocked.ItemVersion != request.ItemVersion+1 ||
		blocked.Subject.RunID == nil || *blocked.Subject.RunID != s.RunID || blocked.Type != domain.AttentionPublishBlocked {
		return domain.ErrParentKeyMismatch
	}
	completed, err := tx.GetOutbox(ctx, "production-reevaluation-completed/"+request.CommandID)
	if err != nil {
		return err
	}
	var completion domain.PublicationReevaluationCompletion
	if err := strictjson.Decode(completed.Payload, &completion, strictjson.RejectInvalidUTF8, strictjson.Limit(1<<20)); err != nil {
		return err
	}
	if completed.Kind != "production_publication_reevaluation_completed" || !completed.Dispatched() || completion.Validate() != nil ||
		completion.RunID != s.RunID || completion.CommandID != request.CommandID || completion.IntentKey != key ||
		completion.Outcome != domain.PublicationReevaluationReviewEscalated || completion.PRHeadSHA != request.PRHeadSHA ||
		completion.EvidenceItemID != blocked.ID || completion.EvidenceItemVersion != blocked.ItemVersion {
		return domain.ErrParentKeyMismatch
	}
	terminalEntry, err := tx.GetInbox(ctx, string(completion.TerminalInvocationID))
	if err != nil {
		return err
	}
	var terminal struct {
		InvocationID domain.InvocationID `json:"invocation_id"`
		RunID        domain.RunID        `json:"run_id"`
		StageID      domain.StageID      `json:"stage_id"`
		Status       string              `json:"status"`
		HeadSHA      string              `json:"head_sha,omitempty"`
		Artifacts    []domain.Digest     `json:"artifacts,omitempty"`
		Summary      string              `json:"summary,omitempty"`
	}
	if err := strictjson.Decode(terminalEntry.Payload, &terminal, strictjson.RejectInvalidUTF8, strictjson.Limit(1<<20)); err != nil {
		return err
	}
	if terminalEntry.Kind != "production_stage_terminal" || terminal.InvocationID != completion.TerminalInvocationID ||
		terminal.RunID != s.RunID || terminal.Status != "completed" || terminal.HeadSHA != request.PRHeadSHA {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

func (tx *ReadTx) validateContinuationSuccessor(ctx context.Context, s domain.PublicationSuccessor) error {
	r, entry, err := tx.PublicationContinuationIntent(ctx, s.RunID, s.CommandID)
	if err != nil || !entry.Dispatched() || !reflect.DeepEqual(r.Successor, s) {
		return errors.Join(err, domain.ErrParentKeyMismatch)
	}
	return nil
}
