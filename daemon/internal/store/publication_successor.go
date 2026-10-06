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
		if err := tx.requireCurrentExternalReviewAdmission(ctx, successor); err != nil {
			return err
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
	cacheKey := ""
	if tx.publicationSuccessorReads != nil {
		cacheKey = publicationReadPath(ctx)
		if cached, found := tx.publicationSuccessorReads[cacheKey]; found {
			if err := ctx.Err(); err != nil {
				return domain.PublicationSuccessor{}, err
			}
			return cached, nil
		}
	}
	successor, err := tx.sealedPublicationSuccessor(ctx, runID, publication)
	if err != nil {
		return successor, err
	}
	err = tx.validatePublicationSuccessor(ctx, successor)
	if err == nil && tx.publicationSuccessorReads != nil {
		tx.publicationSuccessorReads[cacheKey] = successor
	}
	return successor, err
}

// sealedPublicationSuccessor decodes the stored record and checks it against
// its own key. It runs no gate, so its result is not yet an authority.
func (tx *ReadTx) sealedPublicationSuccessor(ctx context.Context, runID domain.RunID, publication domain.InvocationID) (domain.PublicationSuccessor, error) {
	entry, err := tx.GetOutbox(ctx, "publication-successor/"+url.PathEscape(string(runID))+"/"+string(publication))
	if err != nil {
		return domain.PublicationSuccessor{}, err
	}
	successor, err := domain.DecodePublicationSuccessor(entry.Payload)
	if err != nil || entry.Kind != domain.PublicationSuccessorKind || entry.IdempotencyKey != successor.Key() ||
		!entry.Dispatched() || successor.PublicationID() != publication || successor.RunID != runID {
		return successor, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	return successor, nil
}

func (tx *ReadTx) validatePublicationSuccessor(ctx context.Context, successor domain.PublicationSuccessor) error {
	if err := successor.Validate(); err != nil {
		return err
	}
	switch successor.EffectiveOrigin() {
	case domain.PublicationSuccessorReadinessInvalidation, domain.PublicationSuccessorExternalReview:
		return tx.validateReentrySuccessor(ctx, successor)
	case domain.PublicationSuccessorRemediation:
		if err := tx.requireUninvalidatedPredecessor(ctx, successor); err != nil {
			return err
		}
		return tx.validateContinuationSuccessor(ctx, successor)
	case domain.PublicationSuccessorFeedback:
	}
	if err := tx.requireUninvalidatedPredecessor(ctx, successor); err != nil {
		return err
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

// requireCurrentExternalReviewAdmission is the seal-time half of external
// review admission. The gate proves the profile an external_review authority
// names admits its reviewer; a new cycle additionally needs that profile to
// be the repository's active one, so removing a reviewer stops new cycles. It
// runs only when the record is first sealed: a sealed cycle keeps reading
// under the profile it named, and its replay converges.
func (tx *ReadTx) requireCurrentExternalReviewAdmission(ctx context.Context, successor domain.PublicationSuccessor) error {
	if successor.EffectiveOrigin() != domain.PublicationSuccessorExternalReview {
		return nil
	}
	// The gate admits any superseded, uninvalidated ready item, which is also
	// what an operator's feedback return leaves behind. Only the run's
	// current ready item may start a cycle, or a second successor of one item
	// would branch the chain and make every later read of it fail.
	chain, err := tx.PublicationSuccessorChain(ctx, successor.RunID)
	if err != nil {
		return err
	}
	current := domain.ProductionReadyItemID(successor.RunID)
	for _, sealed := range chain {
		// One finding starts one cycle. A cycle that re-earned readiness in
		// place leaves the finding on the published head, where it would
		// otherwise pass the gate again without limit.
		if sealed.ExternalFindingID == successor.ExternalFindingID {
			return domain.ErrParentKeyMismatch
		}
		current = sealed.ReadyItemID()
	}
	if current != successor.PredecessorItemID {
		return domain.ErrParentKeyMismatch
	}
	ready, err := tx.GetReadyItemPRBinding(ctx, successor.PredecessorItemID)
	if err != nil {
		return err
	}
	active, err := tx.LatestTrustProfile(ctx, ready.Repo)
	if err != nil || active.ProfileDigest != successor.AdmittingProfileDigest {
		return fmt.Errorf("seal external review re-entry under trust profile %s: %w",
			successor.AdmittingProfileDigest, errors.Join(err, domain.ErrExternalReviewNotAdmitted))
	}
	return nil
}

// validateReentrySuccessor re-derives every coordinate of a re-entry authority
// from the superseded item's daemon-recorded invalidation fact, its
// authenticated binding, and the review pass that earned it. For head_changed
// the head is a forge observation of commits no Freeside invocation produced,
// so the record's own coordinates are never trusted: a decoded or
// caller-supplied record must restate exactly what current state proves
// (devlog/2026-10-02-0222-readiness-reentry-authority.md).
func (tx *ReadTx) validateReentrySuccessor(ctx context.Context, successor domain.PublicationSuccessor) error {
	_, err := tx.reentryPredecessorBinding(ctx, successor)
	return err
}

// reentryPredecessorBinding is the re-entry gate. It returns the predecessor
// binding it authenticated so the re-entered binding's gate can compare
// against it without authenticating the whole ancestry a second time: two
// reads per level would double the cost with every consecutive re-entry.
func (tx *ReadTx) reentryPredecessorBinding(ctx context.Context, successor domain.PublicationSuccessor) (domain.ReadyItemPRBinding, error) {
	none := domain.ReadyItemPRBinding{}
	if err := successor.Validate(); err != nil {
		return none, err
	}
	reentry := successor.Reentry
	if reentry == nil {
		return none, domain.ErrParentKeyMismatch
	}
	item, err := tx.GetAttentionItemRecord(ctx, successor.PredecessorItemID)
	if err != nil {
		return none, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	external := successor.EffectiveOrigin() == domain.PublicationSuccessorExternalReview
	var fact domain.ReadinessInvalidation
	if external {
		// Nothing invalidated this item, so no fact proves its state: it must
		// itself be a ready_for_final_review item, superseded in the
		// transaction that seals this authority and not by an invalidation,
		// which has its own origin.
		if item.Type != domain.AttentionReadyForFinalReview || item.Status != domain.StatusSuperseded ||
			item.ReadinessInvalidation != nil {
			return none, domain.ErrParentKeyMismatch
		}
	} else {
		// The fact alone proves a superseded ready_for_final_review item: the
		// item's own validation admits it on nothing else. The binding gate
		// below ties that item to the binding's run.
		if item.ReadinessInvalidation == nil || item.ReadinessInvalidation.Reason != reentry.Reason {
			return none, domain.ErrParentKeyMismatch
		}
		fact = *item.ReadinessInvalidation
	}
	ready, err := tx.GetReadyItemPRBinding(ctx, item.ID)
	if err != nil || ready.RunID != successor.RunID {
		return none, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	prior, err := tx.GetReviewRecord(ctx, successor.PriorReviewInvocationID)
	if err != nil || prior.RunID != successor.RunID || prior.HeadSHA != ready.HeadSHA ||
		prior.Round+1 != successor.ReviewRound {
		return none, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	if external {
		if err := tx.requireAdmittedExternalFinding(ctx, successor, ready); err != nil {
			return none, err
		}
		// This origin admits remediation, which rebuilds the candidate and
		// replaces the published head, so the head must be one Freeside
		// pushed. A review-only cycle on someone else's commits would need an
		// authority that says so; until one exists the finding drives nothing.
		foreign, err := tx.publishedHeadIsForeign(ctx, ready)
		if err != nil || foreign {
			return none, errors.Join(err, domain.ErrParentKeyMismatch)
		}
		// The pull request did not move: the cycle reviews the published
		// head against the base its last review used.
		if prior.BaseSHA == "" || reentry.BaseSHA != prior.BaseSHA || reentry.HeadSHA != ready.HeadSHA {
			return none, domain.ErrParentKeyMismatch
		}
		return ready, nil
	}
	wantBase, wantHead := "", ""
	switch reentry.Reason {
	case domain.ReadinessInvalidationBaseAdvanced:
		wantBase, wantHead = fact.Observed, ready.HeadSHA
	case domain.ReadinessInvalidationHeadChanged:
		if fact.Bound != ready.HeadSHA {
			return none, domain.ErrParentKeyMismatch
		}
		wantBase, wantHead = prior.BaseSHA, fact.Observed
	case domain.ReadinessInvalidationRetargeted, domain.ReadinessInvalidationIdentityChanged:
		return none, domain.ErrParentKeyMismatch
	}
	if wantBase == "" || wantHead == "" || reentry.BaseSHA != wantBase || reentry.HeadSHA != wantHead {
		return none, domain.ErrParentKeyMismatch
	}
	return ready, nil
}

// requireAdmittedExternalFinding proves the finding and profile an
// external_review authority names instead of trusting that it names them: the
// finding is an external one on this run and on the published head, and the
// named profile, re-read and re-validated, is for the bound repository and
// lists the finding's reviewer with drive_round authority. The named profile
// is checked here, not the active one, so the authority reads the same after
// the owner edits the allowlist; sealing separately requires it to be active.
func (tx *ReadTx) requireAdmittedExternalFinding(
	ctx context.Context, successor domain.PublicationSuccessor, ready domain.ReadyItemPRBinding,
) error {
	finding, err := tx.GetFinding(ctx, successor.ExternalFindingID)
	if err != nil || finding.External == nil || finding.RunID != successor.RunID ||
		finding.External.HeadSHA != ready.HeadSHA {
		return errors.Join(err, domain.ErrParentKeyMismatch)
	}
	profile, err := tx.GetTrustProfile(ctx, successor.AdmittingProfileDigest)
	if err != nil {
		return errors.Join(err, domain.ErrExternalReviewNotAdmitted)
	}
	return externalFindingAdmittedBy(finding, profile, ready)
}

// publishedHeadIsForeign reports whether the head on an authenticated ready
// binding came from a head_changed re-entry, that is, from commits no Freeside
// invocation produced. An in-place binding inherits its head from the
// authority it re-entered under: head_changed observed a foreign one,
// external_review was itself refused on one by this check, and base_advanced
// kept its predecessor's, so only that link is followed further. The walk
// reads rows without re-running their gates: authenticating the binding it
// starts from already authenticated every ancestor, and a gated read per
// level would make the cost grow with every consecutive re-entry.
func (tx *ReadTx) publishedHeadIsForeign(ctx context.Context, ready domain.ReadyItemPRBinding) (bool, error) {
	publication := ready.PublicationInvocationID
	for {
		inPlace, err := tx.reenteredInPlace(ctx, domain.ReadyItemPRBinding{PublicationInvocationID: publication})
		if err != nil || !inPlace {
			return false, err
		}
		authority, err := tx.sealedPublicationSuccessor(ctx, ready.RunID, publication)
		if err != nil || authority.Reentry == nil {
			return false, errors.Join(err, domain.ErrParentKeyMismatch)
		}
		if authority.EffectiveOrigin() == domain.PublicationSuccessorExternalReview {
			return false, nil
		}
		if authority.Reentry.Reason != domain.ReadinessInvalidationBaseAdvanced {
			return true, nil
		}
		var parent string
		if err := tx.tx.QueryRowContext(ctx, `SELECT publication_invocation_id FROM ready_item_pr_bindings
			WHERE item_id = ? AND run_id = ?`, authority.PredecessorItemID, ready.RunID).Scan(&parent); err != nil {
			return false, errors.Join(notFoundOr(err), domain.ErrParentKeyMismatch)
		}
		publication = domain.InvocationID(parent)
	}
}

// ReadyHeadIsForeign reports whether a ready item's published head is one no
// Freeside invocation produced, which is the head an external_review
// authority is refused on. It lets the trigger decide that refusal from a
// read, before it supersedes anything. The binding is read through its gate
// here, never taken from the caller.
func (tx *ReadTx) ReadyHeadIsForeign(ctx context.Context, itemID domain.ItemID) (bool, error) {
	ready, err := tx.GetReadyItemPRBinding(ctx, itemID)
	if err != nil {
		return false, err
	}
	return tx.publishedHeadIsForeign(ctx, ready)
}

// requireUninvalidatedPredecessor keeps an invalidated ready item from
// counting as the current ready item for any authority but re-entry. A cycle
// that stopped before publication has no item row, which is not an
// invalidation.
func (tx *ReadTx) requireUninvalidatedPredecessor(ctx context.Context, successor domain.PublicationSuccessor) error {
	item, err := tx.GetAttentionItemRecord(ctx, successor.PredecessorItemID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if item.ReadinessInvalidation != nil {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

// dispatchedPublicationSuccessors decodes every dispatched successor record
// stored under one run's key prefix, in insertion order. It runs no gate, so
// nothing it returns is an authority until GetPublicationSuccessor says so.
func (tx *ReadTx) dispatchedPublicationSuccessors(ctx context.Context, runID domain.RunID) ([]domain.PublicationSuccessor, error) {
	prefix := "publication-successor/" + url.PathEscape(string(runID)) + "/"
	entries, err := tx.listOutboxQuery(ctx, `SELECT id, idempotency_key, kind, payload,
		payload_version, payload_digest, status, created_at FROM outbox
		WHERE kind = ? AND status = ? AND substr(idempotency_key, 1, length(?)) = ? ORDER BY id`,
		domain.PublicationSuccessorKind, outboxStatusDispatched,
		domain.PublicationSuccessorKind, outboxStatusDispatched, prefix, prefix)
	if err != nil {
		return nil, err
	}
	sealed := make([]domain.PublicationSuccessor, 0, len(entries))
	for _, entry := range entries {
		successor, err := domain.DecodePublicationSuccessor(entry.Payload)
		if err != nil {
			return nil, err
		}
		if successor.RunID != runID {
			return nil, domain.ErrParentKeyMismatch
		}
		sealed = append(sealed, successor)
	}
	return sealed, nil
}

// linkPublicationSuccessors orders a run's sealed successors into its one
// chain from the root ready item. Two successors of one item are a branch,
// and a successor no walk from the root reaches is an orphan: both fail,
// because neither insertion order nor anything else says which row is the
// run's. It checks structure only, so a linked row is still not an authority
// until GetPublicationSuccessor says so.
func linkPublicationSuccessors(
	runID domain.RunID, sealed []domain.PublicationSuccessor,
) ([]domain.PublicationSuccessor, error) {
	byParent := make(map[domain.ItemID]domain.PublicationSuccessor, len(sealed))
	for _, successor := range sealed {
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

// CurrentPublicationSuccessor follows one unbranched, authenticated chain.
// No insertion order or caller-supplied "latest" bit grants precedence.
func (tx *ReadTx) PublicationSuccessorChain(ctx context.Context, runID domain.RunID) ([]domain.PublicationSuccessor, error) {
	sealed, err := tx.dispatchedPublicationSuccessors(ctx, runID)
	if err != nil {
		return nil, err
	}
	for _, successor := range sealed {
		verified, err := tx.GetPublicationSuccessor(ctx, runID, successor.PublicationID())
		if err != nil || !reflect.DeepEqual(verified, successor) {
			return nil, errors.Join(err, domain.ErrParentKeyMismatch)
		}
	}
	return linkPublicationSuccessors(runID, sealed)
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

// reentryPublicationPrefix is the publication ID shape of a commandless
// successor (domain.PublicationSuccessor.PublicationID).
const reentryPublicationPrefix = "publish-reentry-"

func (tx *ReadTx) PublicationSuccessorForReadyItem(ctx context.Context, runID domain.RunID, itemID domain.ItemID) (domain.PublicationSuccessor, error) {
	var publication domain.InvocationID
	if command, ok := strings.CutPrefix(string(itemID), "production-ready-feedback-"); ok {
		publication = domain.InvocationID("publish-feedback-" + command)
	} else if command, ok := strings.CutPrefix(string(itemID), "production-ready-continuation-"); ok {
		publication = domain.InvocationID("publish-continuation-" + command)
	} else if key, ok := strings.CutPrefix(string(itemID), "production-ready-reentry-"); ok {
		publication = domain.InvocationID(reentryPublicationPrefix + key)
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
	// A re-entry or continuation authority has no feedback invocation; an
	// empty producer must not match that empty field.
	if producer == "" {
		return domain.ErrParentKeyMismatch
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
		review.HeadSHA != request.HeadSHA || review.BaseSHA != request.BaseSHA {
		return errors.Join(err, domain.ErrParentKeyMismatch)
	}
	adjudication, err := tx.GetFindingAdjudication(ctx, request.AdjudicationDigest)
	if err != nil || adjudication.RunID != successor.RunID || adjudication.Round != request.Round || len(request.FindingIDs) == 0 {
		return errors.Join(err, domain.ErrParentKeyMismatch)
	}
	// An external review cycle's first round may route the cycle's admitted
	// external findings to a fix (issue #1767). No review record lists one,
	// so under that round the gate admits each such ID through the cycle's
	// own admission, and admits a clean record when every ID is one. The
	// admission is rebuilt from the authority in hand: this gate runs inside
	// the authority's own read, which must not look the authority up again.
	//
	// The admission says only that the cycle answers the finding, which is
	// also true of one the request's adjudication declined, deferred, or
	// never saw. A review finding is tied to the request through the review
	// record; an external one is tied through the adjudication, which must
	// hold it on a remediate route. An operator's alternative route can only
	// take that route away, so the recorded route is necessary, and the
	// engine's gate holds the request to the exact effective set.
	var cycle *externalReviewCycleAdmission
	if successor.EffectiveOrigin() == domain.PublicationSuccessorExternalReview &&
		successor.Reentry != nil && request.Round == successor.ReviewRound {
		admission, err := tx.externalReviewCycleAdmission(ctx, successor)
		if err != nil {
			return err
		}
		cycle = &admission
	}
	reviewed := 0
	for _, id := range request.FindingIDs {
		if slices.Contains(review.FindingIDs, id) {
			reviewed++
			continue
		}
		if cycle == nil {
			return domain.ErrParentKeyMismatch
		}
		finding, err := tx.GetFinding(ctx, id)
		if err != nil || cycle.admits(finding) != nil ||
			!slices.ContainsFunc(adjudication.Entries, func(entry domain.FindingAdjudicationEntry) bool {
				return entry.FindingID == id && entry.Route == domain.RouteRemediate
			}) {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
	}
	if review.Outcome != domain.ReviewFindings &&
		(review.Outcome != domain.ReviewClean || cycle == nil || reviewed != 0) {
		return domain.ErrParentKeyMismatch
	}
	return nil
}

// PublicationSuccessorTarget derives branch and PR coordinates from the
// predecessor's authenticated outcome, never from an agent or client. The PR
// number, branch and identity come from the last ancestor that published; the
// expected old head is the head now on the PR, which after a re-entry is the
// authority's head, not the head that ancestor pushed.
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
	// A binding re-earned in place inherits its ancestor's publication
	// identity, so the outcome under that identity records the ancestor's
	// head, not this one's. The binding's own gate already proved its head
	// against its authority.
	inPlace, inPlaceErr := tx.reenteredInPlace(ctx, ready)
	if inPlaceErr != nil {
		return publicationrecord.SuccessorTarget{}, inPlaceErr
	}
	if err != nil || entry.Kind != publicationrecord.IntentKindOutcome || outcome.Identity != ready.PublicationIdentity ||
		(!inPlace && outcome.HeadSHA != ready.HeadSHA) ||
		outcome.PRNumber != ready.PRNumber || outcome.Repo != ready.Repo || outcome.BaseRef != ready.BaseRef {
		return publicationrecord.SuccessorTarget{}, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	target := publicationrecord.SuccessorTarget{
		ItemID: ready.ItemID, Identity: ready.PublicationIdentity, HeadSHA: ready.HeadSHA,
		PRNumber: ready.PRNumber, Branch: outcome.Branch,
	}
	if successor.Reentry != nil {
		target.HeadSHA = successor.Reentry.HeadSHA
	}
	return target, target.Validate(ready.BaseRef)
}

type publicationReadKey struct{}

type publicationReadLink struct {
	key    string
	parent *publicationReadLink
}

// Include every active link: a proof under one ancestry must not suppress a
// cycle error under another. Length prefixes distinguish keys containing the
// separators. PublicationSuccessor contains only scalar fields, so a cached
// value cannot be mutated by its caller.
func publicationReadPath(ctx context.Context) string {
	var path strings.Builder
	for link, _ := ctx.Value(publicationReadKey{}).(*publicationReadLink); link != nil; link = link.parent {
		fmt.Fprintf(&path, "%d:%s", len(link.key), link.key)
	}
	return path.String()
}

// publicationReadActive reports whether the read is already reconstructing a
// ready item, feedback, or successor record.
func publicationReadActive(ctx context.Context) bool {
	link, _ := ctx.Value(publicationReadKey{}).(*publicationReadLink)
	return link != nil
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
