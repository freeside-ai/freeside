package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
)

const (
	insertAttentionItemPRReferenceSQL = `INSERT INTO attention_item_pr_references
		(item_id, repo, pr_number, body) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`
	selectAttentionItemPRReferenceBodySQL = `SELECT body
		FROM attention_item_pr_references WHERE item_id = ?`
	getAttentionItemPRReferenceSQL = `SELECT item_id, repo, pr_number, body
		FROM attention_item_pr_references WHERE item_id = ?`
	insertReadyItemPRBindingSQL = `INSERT INTO ready_item_pr_bindings
		(item_id, run_id, producing_invocation_id, publication_invocation_id, publication_identity,
		 repository_id, pr_number, body, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`
	selectReadyItemPRBindingBodySQL = `SELECT body
		FROM ready_item_pr_bindings WHERE item_id = ?`
	getReadyItemPRBindingSQL = `SELECT item_id, run_id, producing_invocation_id, publication_invocation_id,
		publication_identity, repository_id, pr_number, body, recorded_at
		FROM ready_item_pr_bindings WHERE item_id = ?`
)

const readyPublicationIntentKind = "publish.publication"

func (tx *WriteTx) putAttentionItemPRReference(
	ctx context.Context, item domain.AttentionItem,
) error {
	if item.Type != domain.AttentionReadyForFinalReview || item.PRReference == nil {
		return nil
	}
	body, err := encode(*item.PRReference)
	if err != nil {
		return err
	}
	return tx.putImmutable(ctx, insertAttentionItemPRReferenceSQL,
		[]any{item.ID, item.PRReference.Repo, item.PRReference.Number, body},
		selectAttentionItemPRReferenceBodySQL, []any{item.ID}, body)
}

func (tx *ReadTx) getAttentionItemPRReference(
	ctx context.Context, itemID domain.ItemID,
) (domain.PRReference, error) {
	var storedItemID, repo string
	var number int64
	var body []byte
	if err := tx.tx.QueryRowContext(ctx, getAttentionItemPRReferenceSQL, itemID).Scan(
		&storedItemID, &repo, &number, &body,
	); err != nil {
		return domain.PRReference{}, notFoundOr(err)
	}
	reference, err := decode[domain.PRReference](body)
	if err != nil {
		return domain.PRReference{}, err
	}
	if domain.ItemID(storedItemID) != itemID || reference.Repo != repo ||
		int64(reference.Number) != number {
		return domain.PRReference{}, errRowInconsistent
	}
	return reference, nil
}

type readyPublicationIntent = publicationrecord.Intent

func decodeReadyPublicationIntent(payload []byte) (readyPublicationIntent, error) {
	return publicationrecord.DecodeIntent(payload)
}

type readyPublicationOutcome = publicationrecord.Outcome

func decodeReadyPublicationOutcome(payload []byte) (readyPublicationOutcome, error) {
	return publicationrecord.DecodeOutcome(payload)
}

// RecordReadyItemPRBinding records the exact pull request behind a ready item.
// The publication workflow replays the same value modulo its stamped instant;
// a different resource for the same item is an immutable conflict.
func (tx *InternalTx) RecordReadyItemPRBinding(ctx context.Context, binding domain.ReadyItemPRBinding) error {
	if err := tx.validateReadyItemPRBinding(ctx, binding); err != nil {
		return fmt.Errorf("put ready item pr binding %s: %w", binding.ItemID, err)
	}
	body, err := encode(binding)
	if err != nil {
		return fmt.Errorf("put ready item pr binding: %w", err)
	}
	if err := tx.putImmutable(ctx, insertReadyItemPRBindingSQL,
		[]any{
			binding.ItemID, binding.RunID, binding.ProducingInvocationID,
			binding.PublicationInvocationID, binding.PublicationIdentity,
			binding.RepositoryID, binding.PRNumber,
			body, formatTime(binding.RecordedAt),
		},
		selectReadyItemPRBindingBodySQL, []any{binding.ItemID}, body); err != nil {
		return fmt.Errorf("put ready item pr binding %s: %w", binding.ItemID, err)
	}
	return nil
}

// GetReadyItemPRBinding reconstructs the ready resource and re-anchors it to
// the item and run records it claims to describe. Stored coordinates are data,
// never authority to retarget a ready item.
func (tx *ReadTx) GetReadyItemPRBinding(ctx context.Context, itemID domain.ItemID) (domain.ReadyItemPRBinding, error) {
	var (
		storedItemID, storedRunID, producingInvocationID, publicationInvocationID string
		publicationIdentity, recordedAt                                           string
		repositoryID, prNumber                                                    int64
		body                                                                      []byte
	)
	if err := tx.tx.QueryRowContext(ctx, getReadyItemPRBindingSQL, itemID).Scan(
		&storedItemID, &storedRunID, &producingInvocationID, &publicationInvocationID,
		&publicationIdentity,
		&repositoryID, &prNumber, &body, &recordedAt,
	); err != nil {
		return domain.ReadyItemPRBinding{}, fmt.Errorf("get ready item pr binding %s: %w", itemID, notFoundOr(err))
	}
	binding, err := decode[domain.ReadyItemPRBinding](body)
	if err != nil {
		return domain.ReadyItemPRBinding{}, fmt.Errorf("get ready item pr binding %s: %w", itemID, err)
	}
	if binding.ItemID != domain.ItemID(storedItemID) || binding.RunID != domain.RunID(storedRunID) ||
		binding.ProducingInvocationID != domain.InvocationID(producingInvocationID) ||
		binding.PublicationInvocationID != domain.InvocationID(publicationInvocationID) ||
		binding.PublicationIdentity != domain.Digest(publicationIdentity) ||
		binding.RepositoryID != repositoryID || int64(binding.PRNumber) != prNumber ||
		formatTime(binding.RecordedAt) != recordedAt || binding.ItemID != itemID {
		return domain.ReadyItemPRBinding{}, fmt.Errorf("get ready item pr binding %s: %w", itemID, errRowInconsistent)
	}
	if err := tx.validateReadyItemPRBinding(ctx, binding); err != nil {
		return domain.ReadyItemPRBinding{}, fmt.Errorf("get ready item pr binding %s: %w", itemID, err)
	}
	return binding, nil
}

func (tx *ReadTx) validateReadyItemPRBinding(
	ctx context.Context, binding domain.ReadyItemPRBinding,
) error {
	item, err := tx.GetAttentionItemRecord(ctx, binding.ItemID)
	if err != nil {
		return fmt.Errorf("item: %w", err)
	}
	if item.Type != domain.AttentionReadyForFinalReview || item.ProjectID == "" ||
		item.Subject.Type != domain.SubjectRun || item.Subject.RunID == nil ||
		*item.Subject.RunID != binding.RunID || item.Subject.ID != domain.SubjectID(binding.RunID) ||
		item.PRHeadSHA != binding.HeadSHA {
		return errRowInconsistent
	}
	run, err := tx.GetRun(ctx, binding.RunID)
	if err != nil {
		return fmt.Errorf("run: %w", err)
	}
	if run.ProjectID != item.ProjectID {
		return errRowInconsistent
	}
	admission, err := tx.GetExecutionAdmissionRecord(ctx, binding.ProducingInvocationID)
	if err != nil {
		return fmt.Errorf("producing admission: %w", err)
	}
	if admission.RunID != binding.RunID || admission.Base.Repo != binding.Repo ||
		admission.Base.RepositoryID != binding.RepositoryID || admission.Base.BaseRef != binding.BaseRef {
		return errRowInconsistent
	}
	export, err := tx.GetExecutionExportRecord(ctx, binding.ProducingInvocationID)
	if err != nil {
		return fmt.Errorf("producing export: %w", err)
	}
	if export.HeadSHA != binding.HeadSHA {
		return errRowInconsistent
	}
	intentKey := "publish/" + string(binding.PublicationInvocationID) + "/" + readyPublicationIntentKind
	intentEntry, err := tx.GetOutbox(ctx, intentKey)
	if err != nil {
		return fmt.Errorf("publication intent: %w", err)
	}
	if intentEntry.IdempotencyKey != intentKey || intentEntry.Kind != readyPublicationIntentKind ||
		!intentEntry.Dispatched() {
		return errRowInconsistent
	}
	intent, err := decodeReadyPublicationIntent(intentEntry.Payload)
	if err != nil {
		return fmt.Errorf("publication intent: %w", err)
	}
	if intent.Identity != binding.PublicationIdentity ||
		intent.InvocationID != binding.PublicationInvocationID ||
		intent.Repo != binding.Repo || intent.BaseRef != binding.BaseRef ||
		intent.SourceHeadSHA != binding.HeadSHA ||
		intent.ProducingInvocationID != binding.ProducingInvocationID ||
		intent.ReservationRunID != binding.RunID {
		return errRowInconsistent
	}
	outcomeKey := "publish.outcome/" + string(binding.PublicationIdentity)
	entry, err := tx.GetInbox(ctx, outcomeKey)
	if err != nil {
		return fmt.Errorf("publication outcome: %w", err)
	}
	if entry.IdempotencyKey != outcomeKey || entry.Kind != "publish.outcome" {
		return errRowInconsistent
	}
	outcome, err := decodeReadyPublicationOutcome(entry.Payload)
	if err != nil {
		return fmt.Errorf("publication outcome: %w", err)
	}
	hexIdentity := strings.TrimPrefix(string(binding.PublicationIdentity), "sha256:")
	if outcome.Identity != binding.PublicationIdentity || outcome.Repo != binding.Repo ||
		outcome.BaseRef != binding.BaseRef || outcome.HeadSHA != binding.HeadSHA ||
		outcome.PRNumber != binding.PRNumber || outcome.Branch != "freeside/publish/"+hexIdentity[:16] {
		return errRowInconsistent
	}
	return nil
}
