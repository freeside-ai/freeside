package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"slices"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// RecordPublicationSuccessor seals the cycle before verification or any
// external publication effect. It shares the export/task transaction.
func (tx *WriteTx) RecordPublicationSuccessor(ctx context.Context, successor domain.PublicationSuccessor) error {
	if err := tx.validatePublicationSuccessor(ctx, successor); err != nil {
		return err
	}
	body, err := json.Marshal(successor)
	if err != nil {
		return err
	}
	entry, created, err := tx.EnqueueOutbox(ctx, successor.Key(), domain.PublicationSuccessorKind, body)
	if err != nil {
		return err
	}
	if entry.Kind != domain.PublicationSuccessorKind || !bytes.Equal(entry.Payload, body) {
		return domain.ErrImmutableTransition
	}
	if created {
		if err := tx.RequireIncompletePublication(ctx, successor.RunID); err != nil {
			return err
		}
		latest, err := tx.LatestReviewRecord(ctx, successor.RunID)
		if err != nil || latest.InvocationID != successor.PriorReviewInvocationID ||
			latest.Round+1 != successor.ReviewRound {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		failure, err := tx.LatestReviewFailure(ctx, successor.RunID)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return err
		}
		if err == nil && failure.Round >= successor.ReviewRound {
			return domain.ErrParentKeyMismatch
		}
	}
	return tx.MarkOutboxDispatched(ctx, successor.Key())
}

// ErrPublicationCompleted distinguishes an obsolete accepted request from
// damaged immutable history while preserving the transition-refusal contract.
var ErrPublicationCompleted = fmt.Errorf("publication already completed: %w", domain.ErrImmutableTransition)

// ErrNoPublishedContinuationTarget is a legitimate refusal when the original
// cycle stopped before its first publication, not damaged retained authority.
var ErrNoPublishedContinuationTarget = fmt.Errorf("no published continuation target: %w", domain.ErrImmutableTransition)

// A completed unit cannot start a new publication cycle. Check row presence,
// not a derived current completion, so damaged completion evidence also blocks.
func (tx *ReadTx) RequireIncompletePublication(ctx context.Context, runID domain.RunID) error {
	var completed bool
	if err := tx.tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM work_unit_completions WHERE unit_id = ?)`, domain.WorkUnitIDForRun(runID)).Scan(&completed); err != nil {
		return err
	}
	if completed {
		return ErrPublicationCompleted
	}
	return nil
}

func (tx *ReadTx) GetPublicationSuccessor(ctx context.Context, runID domain.RunID, publication domain.InvocationID) (domain.PublicationSuccessor, error) {
	key := "publication-successor/" + url.PathEscape(string(runID)) + "/" + string(publication)
	ctx, err := publicationReadContext(ctx, key)
	if err != nil {
		return domain.PublicationSuccessor{}, err
	}
	entry, err := tx.GetOutbox(ctx, key)
	if err != nil {
		return domain.PublicationSuccessor{}, err
	}
	successor, err := domain.DecodePublicationSuccessor(entry.Payload)
	if err != nil || entry.Kind != domain.PublicationSuccessorKind || entry.IdempotencyKey != successor.Key() ||
		!entry.Dispatched() || successor.PublicationID() != publication || successor.RunID != runID {
		return successor, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	return successor, tx.validatePublicationSuccessor(ctx, successor)
}

func (tx *ReadTx) validatePublicationSuccessor(ctx context.Context, successor domain.PublicationSuccessor) error {
	if err := successor.Validate(); err != nil {
		return err
	}
	if successor.EffectiveOrigin() == domain.PublicationSuccessorRemediation {
		return tx.validateContinuationSuccessor(ctx, successor)
	}
	returned, ready, found, err := tx.FeedbackPublicationParent(ctx, successor.FeedbackInvocationID)
	if err != nil || !found || returned.RunID != successor.RunID || returned.CommandID != successor.CommandID ||
		returned.ItemID != successor.PredecessorItemID {
		return errors.Join(err, domain.ErrParentKeyMismatch)
	}
	exported, err := tx.GetExecutionExportRecord(ctx, successor.FeedbackInvocationID)
	if err != nil || exported.ObservedBaseSHA != returned.BaseSHA {
		return errors.Join(err, domain.ErrParentKeyMismatch)
	}
	prior, err := tx.GetReviewRecord(ctx, successor.PriorReviewInvocationID)
	if err != nil || prior.RunID != successor.RunID || prior.HeadSHA != ready.HeadSHA ||
		prior.Round+1 != successor.ReviewRound {
		return errors.Join(err, domain.ErrParentKeyMismatch)
	}
	return nil
}

// CurrentPublicationSuccessor follows one unbranched, authenticated chain.
// No insertion order or caller-supplied "latest" bit grants precedence.
func (tx *ReadTx) PublicationSuccessorChain(ctx context.Context, runID domain.RunID) ([]domain.PublicationSuccessor, error) {
	prefix := "publication-successor/" + url.PathEscape(string(runID)) + "/"
	entries, err := tx.listOutboxQuery(ctx, `SELECT id, idempotency_key, kind, payload,
		payload_version, payload_digest, status, created_at FROM outbox
		WHERE kind = ? AND status = ? AND substr(idempotency_key, 1, length(?)) = ? ORDER BY id`,
		domain.PublicationSuccessorKind, outboxStatusDispatched,
		domain.PublicationSuccessorKind, outboxStatusDispatched, prefix, prefix)
	if err != nil {
		return nil, err
	}
	byParent := make(map[domain.ItemID]domain.PublicationSuccessor)
	for _, entry := range entries {
		successor, err := domain.DecodePublicationSuccessor(entry.Payload)
		if err != nil {
			return nil, err
		}
		if successor.RunID != runID {
			return nil, domain.ErrParentKeyMismatch
		}
		verified, err := tx.GetPublicationSuccessor(ctx, runID, successor.PublicationID())
		if err != nil || !reflect.DeepEqual(verified, successor) {
			return nil, errors.Join(err, domain.ErrParentKeyMismatch)
		}
		if _, exists := byParent[successor.PredecessorItemID]; exists {
			return nil, domain.ErrParentKeyMismatch
		}
		byParent[successor.PredecessorItemID] = successor
	}
	var chain []domain.PublicationSuccessor
	parent := domain.ProductionReadyItemID(runID)
	for len(byParent) > 0 {
		next, found := byParent[parent]
		if !found {
			return nil, domain.ErrParentKeyMismatch
		}
		delete(byParent, parent)
		chain = append(chain, next)
		parent = next.ReadyItemID()
	}
	return chain, nil
}

func (tx *ReadTx) CurrentPublicationSuccessor(ctx context.Context, runID domain.RunID) (*domain.PublicationSuccessor, error) {
	chain, err := tx.PublicationSuccessorChain(ctx, runID)
	if err != nil || len(chain) == 0 {
		return nil, err
	}
	return &chain[len(chain)-1], nil
}

func (tx *ReadTx) CurrentProductionReadyItemID(ctx context.Context, runID domain.RunID) (domain.ItemID, error) {
	successor, err := tx.CurrentPublicationSuccessor(ctx, runID)
	if err != nil {
		return "", err
	}
	if successor != nil {
		return successor.ReadyItemID(), nil
	}
	return domain.ProductionReadyItemID(runID), nil
}

func (tx *ReadTx) CurrentPublicationInvocationID(ctx context.Context, runID domain.RunID) (domain.InvocationID, error) {
	successor, err := tx.CurrentPublicationSuccessor(ctx, runID)
	if err != nil {
		return "", err
	}
	if successor != nil {
		return successor.PublicationID(), nil
	}
	return domain.ProductionPublicationInvocationID(runID), nil
}

// PublishedProductionReadyItemID keeps completion authority with the last
// published cycle while a sealed successor is still being verified or reviewed.
func (tx *ReadTx) PublishedProductionReadyItemID(ctx context.Context, runID domain.RunID) (domain.ItemID, error) {
	successor, err := tx.CurrentPublicationSuccessor(ctx, runID)
	if err != nil {
		return "", err
	}
	if successor == nil {
		return domain.ProductionReadyItemID(runID), nil
	}
	ready, err := tx.publishedReadyAtOrBefore(ctx, runID, successor.ReadyItemID())
	if err != nil {
		return "", err
	}
	return ready.ItemID, nil
}

func (tx *ReadTx) PublicationSuccessorForReadyItem(ctx context.Context, runID domain.RunID, itemID domain.ItemID) (domain.PublicationSuccessor, error) {
	var publication domain.InvocationID
	if command, ok := strings.CutPrefix(string(itemID), "production-ready-feedback-"); ok {
		publication = domain.InvocationID("publish-feedback-" + command)
	} else if command, ok := strings.CutPrefix(string(itemID), "production-ready-continuation-"); ok {
		publication = domain.InvocationID("publish-continuation-" + command)
	} else {
		return domain.PublicationSuccessor{}, domain.ErrParentKeyMismatch
	}
	s, err := tx.GetPublicationSuccessor(ctx, runID, publication)
	if err != nil || s.ReadyItemID() != itemID {
		return s, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	return s, nil
}

// A rechecked cycle may have stopped before publication. Its place in the
// chain remains sealed, while only an actually published ancestor owns a head.
func (tx *ReadTx) publishedReadyAtOrBefore(ctx context.Context, runID domain.RunID, itemID domain.ItemID) (domain.ReadyItemPRBinding, error) {
	seen := make(map[domain.ItemID]bool)
	for {
		if seen[itemID] {
			return domain.ReadyItemPRBinding{}, domain.ErrParentKeyMismatch
		}
		seen[itemID] = true
		var published bool
		if err := tx.tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM ready_item_pr_bindings WHERE item_id = ?)`, itemID).Scan(&published); err != nil {
			return domain.ReadyItemPRBinding{}, err
		}
		if published {
			ready, err := tx.GetReadyItemPRBinding(ctx, itemID)
			if err != nil || ready.RunID != runID {
				return ready, errors.Join(err, domain.ErrParentKeyMismatch)
			}
			return ready, nil
		}
		if itemID == domain.ProductionReadyItemID(runID) {
			return domain.ReadyItemPRBinding{}, ErrNoPublishedContinuationTarget
		}
		parent, err := tx.PublicationSuccessorForReadyItem(ctx, runID, itemID)
		if err != nil {
			return domain.ReadyItemPRBinding{}, err
		}
		itemID = parent.PredecessorItemID
	}
}

func (tx *ReadTx) PublishedPublicationInvocationID(ctx context.Context, runID domain.RunID) (domain.InvocationID, error) {
	itemID, err := tx.PublishedProductionReadyItemID(ctx, runID)
	if err != nil {
		return "", err
	}
	if itemID == domain.ProductionReadyItemID(runID) {
		return domain.ProductionPublicationInvocationID(runID), nil
	}
	ready, err := tx.GetReadyItemPRBinding(ctx, itemID)
	return ready.PublicationInvocationID, err
}

func (tx *ReadTx) PublicationSuccessorForBlockedItem(ctx context.Context, runID domain.RunID, itemID domain.ItemID) (*domain.PublicationSuccessor, error) {
	chain, err := tx.PublicationSuccessorChain(ctx, runID)
	if err != nil {
		return nil, err
	}
	for _, successor := range chain {
		if successor.BlockedItemID() == itemID {
			return &successor, nil
		}
	}
	return nil, nil
}

// EffectiveWorkUnitPRBinding derives the current head without changing the
// immutable first-publication record. A pending successor has no merge authority.
func (tx *ReadTx) EffectiveWorkUnitPRBinding(ctx context.Context, unitID domain.WorkUnitID) (domain.WorkUnitPRBinding, error) {
	binding, err := tx.GetWorkUnitPRBinding(ctx, unitID)
	if err != nil {
		return binding, err
	}
	declaration, err := tx.GetWorkUnitDeclaration(ctx, unitID)
	if err != nil {
		return binding, err
	}
	successor, err := tx.CurrentPublicationSuccessor(ctx, declaration.RunID)
	if err != nil || successor == nil {
		return binding, err
	}
	itemID, err := tx.PublishedProductionReadyItemID(ctx, declaration.RunID)
	if err != nil {
		return binding, err
	}
	ready, err := tx.GetReadyItemPRBinding(ctx, itemID)
	if err != nil {
		return binding, err
	}
	if binding.Repo != ready.Repo || binding.RepositoryID != ready.RepositoryID ||
		binding.PRNumber != ready.PRNumber || binding.BaseRef != ready.BaseRef {
		return binding, domain.ErrParentKeyMismatch
	}
	binding.HeadSHA = ready.HeadSHA
	return binding, nil
}

// AuthenticateSuccessorProducer excludes the original export and other
// feedback cycles even if their old evidence still validates independently.
func (tx *ReadTx) AuthenticateSuccessorProducer(ctx context.Context, successor domain.PublicationSuccessor, producer domain.InvocationID) error {
	if err := tx.validatePublicationSuccessor(ctx, successor); err != nil {
		return err
	}
	if producer == successor.FeedbackInvocationID {
		return tx.validatePublicationSuccessor(ctx, successor)
	}
	entry, err := tx.GetOutbox(ctx, string(producer))
	if err != nil {
		return err
	}
	var request domain.RemediationInvocationIntent
	if err := strictjson.Decode(entry.Payload, &request, strictjson.RejectInvalidUTF8, strictjson.NoLimit); err != nil {
		return err
	}
	canonical, err := json.Marshal(request)
	if err != nil || !bytes.Equal(canonical, entry.Payload) || entry.Kind != "remediation_invocation_requested" ||
		!entry.Dispatched() || request.Version != "freeside.remediation-request/v1" ||
		request.InvocationID != producer || request.RunID != successor.RunID ||
		!successor.AllowsRemediation(request) ||
		string(producer) != fmt.Sprintf("inv-remediate-%d-%s", request.Round, request.RunID) {
		return domain.ErrParentKeyMismatch
	}
	admission, err := tx.GetExecutionAdmissionRecord(ctx, producer)
	if err != nil || admission.RunID != successor.RunID || admission.StageID != request.StageID {
		return errors.Join(err, domain.ErrParentKeyMismatch)
	}
	invocation, err := tx.GetAgentInvocation(ctx, producer)
	if err != nil || len(invocation.InputIDs) != 2 || invocation.InputIDs[1] != request.InputArtifactID {
		return errors.Join(err, domain.ErrParentKeyMismatch)
	}
	inputDigest, err := invocation.ComputeInputDigest()
	if err != nil || inputDigest != admission.InputDigest {
		return errors.Join(err, domain.ErrParentKeyMismatch)
	}
	input, err := tx.GetArtifact(ctx, request.InputArtifactID)
	if err != nil || input.Digest != request.InputArtifactDigest || input.Provenance.ProducerClass != domain.ProducerDaemon ||
		input.Provenance.ProducerInvocationID != request.ReviewInvocationID || input.Provenance.SourceHeadSHA != request.HeadSHA {
		return errors.Join(err, domain.ErrParentKeyMismatch)
	}
	review, err := tx.GetReviewRecord(ctx, request.ReviewInvocationID)
	if err != nil || review.RunID != successor.RunID || review.Round != request.Round ||
		review.HeadSHA != request.HeadSHA || review.BaseSHA != request.BaseSHA || review.Outcome != domain.ReviewFindings {
		return errors.Join(err, domain.ErrParentKeyMismatch)
	}
	adjudication, err := tx.GetFindingAdjudication(ctx, request.AdjudicationDigest)
	if err != nil || adjudication.RunID != successor.RunID || adjudication.Round != request.Round || len(request.FindingIDs) == 0 {
		return errors.Join(err, domain.ErrParentKeyMismatch)
	}
	for _, finding := range request.FindingIDs {
		if !slices.Contains(review.FindingIDs, finding) {
			return domain.ErrParentKeyMismatch
		}
	}
	return nil
}

// PublicationSuccessorTarget derives branch and PR coordinates from the
// predecessor's authenticated outcome, never from an agent or client.
func (tx *ReadTx) PublicationSuccessorTarget(ctx context.Context, runID domain.RunID, publication domain.InvocationID) (publicationrecord.SuccessorTarget, error) {
	successor, err := tx.GetPublicationSuccessor(ctx, runID, publication)
	if err != nil {
		return publicationrecord.SuccessorTarget{}, err
	}
	ready, err := tx.publishedReadyAtOrBefore(ctx, runID, successor.PredecessorItemID)
	if err != nil {
		return publicationrecord.SuccessorTarget{}, err
	}
	entry, err := tx.GetInbox(ctx, publicationrecord.OutcomeKey(ready.PublicationIdentity))
	if err != nil {
		return publicationrecord.SuccessorTarget{}, err
	}
	outcome, err := publicationrecord.DecodeOutcome(entry.Payload)
	if err != nil || entry.Kind != publicationrecord.IntentKindOutcome || outcome.Identity != ready.PublicationIdentity ||
		outcome.HeadSHA != ready.HeadSHA || outcome.PRNumber != ready.PRNumber || outcome.Repo != ready.Repo || outcome.BaseRef != ready.BaseRef {
		return publicationrecord.SuccessorTarget{}, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	target := publicationrecord.SuccessorTarget{
		ItemID: ready.ItemID, Identity: ready.PublicationIdentity, HeadSHA: ready.HeadSHA,
		PRNumber: ready.PRNumber, Branch: outcome.Branch,
	}
	return target, target.Validate(ready.BaseRef)
}

type publicationReadKey struct{}

type publicationReadLink struct {
	key    string
	parent *publicationReadLink
}

// Carry the active reconstruction path across ready, feedback and successor
// reads. A forged cycle fails closed before recursively trusting its own row.
func publicationReadContext(ctx context.Context, key string) (context.Context, error) {
	parent, _ := ctx.Value(publicationReadKey{}).(*publicationReadLink)
	for link := parent; link != nil; link = link.parent {
		if link.key == key {
			return ctx, domain.ErrParentKeyMismatch
		}
	}
	return context.WithValue(ctx, publicationReadKey{}, &publicationReadLink{key: key, parent: parent}), nil
}
