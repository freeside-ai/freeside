package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
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
	insertHeldItemPRBindingSQL = `INSERT INTO held_item_pr_bindings
		(item_id, run_id, producing_invocation_id, publication_invocation_id, publication_identity,
		 repository_id, pr_number, body, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT DO NOTHING`
	selectHeldItemPRBindingBodySQL = `SELECT body
		FROM held_item_pr_bindings WHERE item_id = ?`
	getHeldItemPRBindingSQL = `SELECT item_id, run_id, producing_invocation_id, publication_invocation_id,
		publication_identity, repository_id, pr_number, body, recorded_at
		FROM held_item_pr_bindings WHERE item_id = ?`
)

const readyPublicationIntentKind = "publish.publication"

// putAttentionItemPRReference anchors the item's pull request coordinates the
// first time a version carries them: at creation for a ready item, and for a
// publish_blocked item either at creation or on the one later version that
// attaches them. putImmutable makes the anchor write-once, so a later version
// naming another pull request is an immutable conflict.
func (tx *WriteTx) putAttentionItemPRReference(
	ctx context.Context, item domain.AttentionItem,
) error {
	if item.PRReference == nil ||
		(item.Type != domain.AttentionReadyForFinalReview && item.Type != domain.AttentionPublishBlocked) {
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

func decodeReadyPublicationIntent(entry QueueEntry) (readyPublicationIntent, error) {
	intent, err := publicationrecord.DecodeIntent(entry.Payload)
	if err != nil {
		return readyPublicationIntent{}, err
	}
	if intent.FormatVersion != entry.PayloadVersion {
		return readyPublicationIntent{}, domain.ErrParentKeyMismatch
	}
	return intent, nil
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
	ctx, err := publicationReadContext(ctx, "ready/"+string(itemID))
	if err != nil {
		return domain.ReadyItemPRBinding{}, err
	}
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

// RecordHeldItemPRBinding records the exact pull request behind a
// publish_blocked item whose run already published. A replay of the same
// value converges modulo its stamped instant; a different resource for the
// same item is an immutable conflict.
func (tx *InternalTx) RecordHeldItemPRBinding(ctx context.Context, binding domain.HeldItemPRBinding) error {
	if err := tx.validateHeldItemPRBinding(ctx, binding); err != nil {
		return fmt.Errorf("put held item pr binding %s: %w", binding.ItemID, err)
	}
	body, err := encode(binding)
	if err != nil {
		return fmt.Errorf("put held item pr binding: %w", err)
	}
	if err := tx.putImmutable(ctx, insertHeldItemPRBindingSQL,
		[]any{
			binding.ItemID, binding.RunID, binding.ProducingInvocationID,
			binding.PublicationInvocationID, binding.PublicationIdentity,
			binding.RepositoryID, binding.PRNumber,
			body, formatTime(binding.RecordedAt),
		},
		selectHeldItemPRBindingBodySQL, []any{binding.ItemID}, body); err != nil {
		return fmt.Errorf("put held item pr binding %s: %w", binding.ItemID, err)
	}
	return nil
}

// GetHeldItemPRBinding reconstructs the held resource and re-anchors it to the
// item, run, and publication records it claims to describe. Stored
// coordinates are data, never authority to point reconciliation at another
// pull request.
func (tx *ReadTx) GetHeldItemPRBinding(ctx context.Context, itemID domain.ItemID) (domain.HeldItemPRBinding, error) {
	ctx, err := publicationReadContext(ctx, "held/"+string(itemID))
	if err != nil {
		return domain.HeldItemPRBinding{}, err
	}
	var (
		storedItemID, storedRunID, producingInvocationID, publicationInvocationID string
		publicationIdentity, recordedAt                                           string
		repositoryID, prNumber                                                    int64
		body                                                                      []byte
	)
	if err := tx.tx.QueryRowContext(ctx, getHeldItemPRBindingSQL, itemID).Scan(
		&storedItemID, &storedRunID, &producingInvocationID, &publicationInvocationID,
		&publicationIdentity,
		&repositoryID, &prNumber, &body, &recordedAt,
	); err != nil {
		return domain.HeldItemPRBinding{}, fmt.Errorf("get held item pr binding %s: %w", itemID, notFoundOr(err))
	}
	binding, err := decode[domain.HeldItemPRBinding](body)
	if err != nil {
		return domain.HeldItemPRBinding{}, fmt.Errorf("get held item pr binding %s: %w", itemID, err)
	}
	if binding.ItemID != domain.ItemID(storedItemID) || binding.RunID != domain.RunID(storedRunID) ||
		binding.ProducingInvocationID != domain.InvocationID(producingInvocationID) ||
		binding.PublicationInvocationID != domain.InvocationID(publicationInvocationID) ||
		binding.PublicationIdentity != domain.Digest(publicationIdentity) ||
		binding.RepositoryID != repositoryID || int64(binding.PRNumber) != prNumber ||
		formatTime(binding.RecordedAt) != recordedAt || binding.ItemID != itemID {
		return domain.HeldItemPRBinding{}, fmt.Errorf("get held item pr binding %s: %w", itemID, errRowInconsistent)
	}
	if err := tx.validateHeldItemPRBinding(ctx, binding); err != nil {
		// The row exists, so a record its proof needs that is missing makes
		// the binding corrupt. Reporting that as ErrNotFound would let a
		// caller read a corrupt binding as an absent one.
		if errors.Is(err, ErrNotFound) {
			return domain.HeldItemPRBinding{}, fmt.Errorf(
				"get held item pr binding %s: %w: %s", itemID, errRowInconsistent, err.Error())
		}
		return domain.HeldItemPRBinding{}, fmt.Errorf("get held item pr binding %s: %w", itemID, err)
	}
	return binding, nil
}

// validateHeldItemPRBinding authenticates a held binding on write and on
// every read. The item must be a publish_blocked item of the binding's run
// and head that carries the binding's pull request, and the run's publication
// records must prove those coordinates exactly as they prove a ready binding.
func (tx *ReadTx) validateHeldItemPRBinding(
	ctx context.Context, binding domain.HeldItemPRBinding,
) error {
	item, err := tx.GetAttentionItemRecord(ctx, binding.ItemID)
	if err != nil {
		return fmt.Errorf("item: %w", err)
	}
	// The record tier above skips the anchor gate, so compare the body's
	// reference to the store-owned anchor here: the binding must describe the
	// pull request the item is anchored to, not one a synced body names.
	anchored, err := tx.getAttentionItemPRReference(ctx, item.ID)
	if err != nil {
		return fmt.Errorf("item pr reference: %w", err)
	}
	if item.PRReference == nil || *item.PRReference != anchored ||
		anchored.Repo != binding.Repo || anchored.Number != binding.PRNumber {
		return errRowInconsistent
	}
	return tx.validateItemPRBindingAgainst(ctx, item, domain.ReadyItemPRBinding(binding), domain.AttentionPublishBlocked)
}

func (tx *ReadTx) validateReadyItemPRBinding(
	ctx context.Context, binding domain.ReadyItemPRBinding,
) error {
	item, err := tx.GetAttentionItemRecord(ctx, binding.ItemID)
	if err != nil {
		return fmt.Errorf("item: %w", err)
	}
	return tx.validateReadyItemPRBindingAgainst(ctx, item, binding)
}

// validateReadyItemPRBindingAgainst authenticates a binding against an item
// the caller already reconstructed. The 0062 data migration needs this split:
// it runs before 0063 added readiness_detail, so the head-schema item read
// above cannot execute there, while its own scan projects the column as NULL.
func (tx *ReadTx) validateReadyItemPRBindingAgainst(
	ctx context.Context, item domain.AttentionItem, binding domain.ReadyItemPRBinding,
) error {
	return tx.validateItemPRBindingAgainst(ctx, item, binding, domain.AttentionReadyForFinalReview)
}

// validateItemPRBindingAgainst is the publication proof a ready binding and a
// held binding share: the item is of itemType on the binding's run and head,
// and the producing admission, producing export, dispatched publication
// intent, and recorded outcome (or, for an in-place re-entry, its authority)
// all agree with the binding's coordinates.
//
// A successor cycle's ready item has one derivable identity, so a ready
// binding must name it. A held binding has no such check: one publication can
// have several hold items (a rerun of trust evaluation holds under its own
// item ID on the same publication invocation), so the item's run, head, and
// anchored pull request tie a hold to its cycle instead.
func (tx *ReadTx) validateItemPRBindingAgainst(
	ctx context.Context, item domain.AttentionItem, binding domain.ReadyItemPRBinding,
	itemType domain.AttentionType,
) error {
	ready := itemType == domain.AttentionReadyForFinalReview
	if item.ID != binding.ItemID {
		return errRowInconsistent
	}
	if item.Type != itemType || item.ProjectID == "" ||
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
	inPlace, err := tx.reenteredInPlace(ctx, binding)
	if err != nil {
		return err
	}
	if inPlace {
		return tx.validateReenteredItemPRBinding(ctx, item, binding, ready)
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
	intent, err := decodeReadyPublicationIntent(intentEntry)
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
	if outcome.Identity != binding.PublicationIdentity || outcome.Repo != binding.Repo ||
		outcome.BaseRef != binding.BaseRef || outcome.HeadSHA != binding.HeadSHA ||
		outcome.PRNumber != binding.PRNumber || outcome.Branch != publicationrecord.ExpectedBranch(intent) ||
		!reflect.DeepEqual(outcome.Successor, intent.Successor) {
		return errRowInconsistent
	}
	if intent.Successor != nil {
		successor, err := tx.GetPublicationSuccessor(ctx, binding.RunID, binding.PublicationInvocationID)
		if err != nil || (ready && successor.ReadyItemID() != item.ID) {
			return domain.ErrParentKeyMismatch
		}
		target, err := tx.PublicationSuccessorTarget(ctx, binding.RunID, binding.PublicationInvocationID)
		if err != nil || !reflect.DeepEqual(target, *intent.Successor) {
			return domain.ErrParentKeyMismatch
		}
	}
	return nil
}

// reenteredInPlace reports whether the binding claims a re-entry authority as
// its publication and that cycle published nothing: it re-earned readiness on
// the head already on the pull request. The claim is only a route to the
// authority's gate. With no such authenticated record the binding is refused,
// and every other binding still needs its own producing export and
// publication outcome. That includes a re-entered cycle whose remediation
// pushed a new head: it has a publication intent, so the ordinary successor
// gate proves it.
func (tx *ReadTx) reenteredInPlace(ctx context.Context, binding domain.ReadyItemPRBinding) (bool, error) {
	if !strings.HasPrefix(string(binding.PublicationInvocationID), reentryPublicationPrefix) {
		return false, nil
	}
	_, err := tx.GetOutbox(ctx, "publish/"+string(binding.PublicationInvocationID)+"/"+readyPublicationIntentKind)
	if errors.Is(err, ErrNotFound) {
		return true, nil
	}
	return false, err
}

// validateReenteredItemPRBinding proves an in-place re-entered cycle's
// binding by its authority instead of an export and outcome it never had: the
// cycle pushed nothing, so its head is the one the authority re-entered for
// and every resource coordinate is its predecessor's. The authority's gate
// authenticated that predecessor binding, so by induction those coordinates
// are the last actually published ancestor's.
func (tx *ReadTx) validateReenteredItemPRBinding(
	ctx context.Context, item domain.AttentionItem, binding domain.ReadyItemPRBinding, ready bool,
) error {
	authority, err := tx.sealedPublicationSuccessor(ctx, binding.RunID, binding.PublicationInvocationID)
	if err != nil || authority.Reentry == nil || (ready && authority.ReadyItemID() != item.ID) ||
		authority.Reentry.HeadSHA != binding.HeadSHA {
		return errors.Join(err, domain.ErrParentKeyMismatch)
	}
	published, err := tx.reentryPredecessorBinding(ctx, authority)
	if err != nil || published.RunID != binding.RunID || published.Repo != binding.Repo ||
		published.RepositoryID != binding.RepositoryID || published.PRNumber != binding.PRNumber ||
		published.BaseRef != binding.BaseRef || published.ProducingInvocationID != binding.ProducingInvocationID ||
		published.PublicationIdentity != binding.PublicationIdentity {
		return errors.Join(err, domain.ErrParentKeyMismatch)
	}
	return nil
}
