package publish

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// GatedUpdate authorizes one old-head/new-head transition on the predecessor's
// branch. Its sealed GatedHead retains the per-publisher transport binding.
type GatedUpdate struct {
	head            GatedHead
	expectedOldHead string
}

func (g GatedUpdate) Branch() string          { return g.head.Branch() }
func (g GatedUpdate) SourceHeadSHA() string   { return g.head.SourceHeadSHA() }
func (g GatedUpdate) ExpectedOldHead() string { return g.expectedOldHead }

func validateSuccessorCandidate(ctx context.Context, tx *store.ReadTx, candidate Candidate, producer *domain.InvocationID) error {
	if candidate.Successor == nil {
		return nil
	}
	if candidate.DispositionHistory == nil {
		return ErrUnauthorizedPublication
	}
	successor, err := tx.GetPublicationSuccessor(ctx, candidate.RunID, candidate.InvocationID)
	if err != nil || successor.RunID != candidate.RunID {
		return errors.Join(err, ErrUnauthorizedPublication)
	}
	if producer == nil {
		return ErrUnauthorizedPublication
	}
	if err := tx.AuthenticateSuccessorProducer(ctx, successor, *producer); err != nil {
		return errors.Join(err, ErrUnauthorizedPublication)
	}
	review, err := tx.LatestReviewRecord(ctx, candidate.RunID)
	if err != nil || review.Round < successor.ReviewRound || review.HeadSHA != candidate.HeadSHA {
		return errors.Join(err, ErrUnauthorizedPublication)
	}
	target, err := tx.PublicationSuccessorTarget(ctx, candidate.RunID, candidate.InvocationID)
	if err != nil || !reflect.DeepEqual(target, *candidate.Successor) || candidate.Branch != target.Branch {
		return errors.Join(err, ErrUnauthorizedPublication)
	}
	ready, err := tx.GetReadyItemPRBinding(ctx, target.ItemID)
	if err != nil || ready.Repo != candidate.Repo || ready.BaseRef != candidate.BaseRef {
		return errors.Join(err, ErrUnauthorizedPublication)
	}
	current, err := tx.CurrentPublicationSuccessor(ctx, candidate.RunID)
	if err != nil || current == nil || current.PublicationID() != candidate.InvocationID {
		return errors.Join(err, ErrUnauthorizedPublication)
	}
	return nil
}

// PublishSuccessorAfterGateAndFinalize updates the one predecessor PR. It uses
// the same candidate/readiness/trust gates as initial publication, but never
// grants ordinary create-only transport authority to replace a branch.
func (p *Publisher) PublishSuccessorAfterGateAndFinalize(
	ctx context.Context, candidate ExecutionCandidate, approvedRecipes map[domain.Digest]bool,
	updateHead func(context.Context, GatedUpdate) error,
) (Result, error) {
	if candidate.Successor == nil || updateHead == nil || p.storeDecision == nil {
		return Result{}, ErrUnauthorizedPublication
	}
	result, err := p.publishWithTransport(ctx, candidate.Candidate, approvedRecipes,
		func(ctx context.Context, head GatedHead) error {
			return updateHead(ctx, GatedUpdate{head: head, expectedOldHead: candidate.Successor.HeadSHA})
		}, &candidate.ProducingInvocationID)
	if err != nil {
		return Result{}, err
	}
	if err := finalizePublicationResult(ctx, p.storeDecision.store, candidate.Candidate, result, candidate.ProducingInvocationID); err != nil {
		return Result{}, err
	}
	return result, nil
}

// observeSuccessorPR permits only the two durable recovery states: the prior
// identity, before or after the leased push, or the exact successor identity
// at the new head. A missing/closed/foreign PR is never a create request.
func (p *Publisher) observeSuccessorPR(ctx context.Context, repo repoRef, identity Identity, candidate Candidate) (prState, error) {
	target := candidate.Successor
	if target == nil {
		return prState{}, ErrUnauthorizedPublication
	}
	prs, err := p.forge.listPRsByHead(ctx, repo, target.Branch)
	if err != nil {
		return prState{}, err
	}
	if len(prs) != 1 {
		return prState{}, ErrPublicationConflict
	}
	pr := prs[0]
	return validateSuccessorPR(pr, repo, identity, candidate)
}

func validateSuccessorPR(pr prState, repo repoRef, identity Identity, candidate Candidate) (prState, error) {
	target := candidate.Successor
	if target == nil {
		return prState{}, ErrUnauthorizedPublication
	}
	marker, ok := ParseMarker(pr.Body)
	if !ok || (marker != target.Identity && marker != identity.Digest()) {
		return prState{}, ErrForeignResource
	}
	if pr.Number != target.PRNumber || pr.State != "open" {
		return prState{}, ErrPublicationConflict
	}
	// Reuse the exact repository/base/head ownership validator with the
	// observed identity and one of the only two permitted heads.
	expected := candidate
	observedIdentity := identity
	if marker == target.Identity {
		observedIdentity = Identity{digest: target.Identity}
		if prMatchesCandidate(pr, repo, observedIdentity, expected, target.Branch) {
			return pr, nil
		}
		expected.HeadSHA = target.HeadSHA
	}
	if !prMatchesCandidate(pr, repo, observedIdentity, expected, target.Branch) {
		return prState{}, ErrPublicationConflict
	}
	return pr, nil
}

func (p *Publisher) convergeSuccessorPR(ctx context.Context, repo repoRef, identity Identity, candidate Candidate, title, body string) (int, error) {
	pr, err := p.observeSuccessorPR(ctx, repo, identity, candidate)
	if err != nil {
		return 0, err
	}
	// Updating the body before the branch has converged would describe
	// evidence for a head the PR does not contain.
	expected := pr
	expected.Body = body
	if !prMatchesCandidate(expected, repo, identity, candidate, candidate.Successor.Branch) {
		return 0, ErrPublicationConflict
	}
	if pr.Title != title || pr.Body != body {
		pr, err = p.forge.updatePR(ctx, repo, pr.Number, title, body)
		if err != nil {
			return 0, err
		}
	}
	if pr.Number != candidate.Successor.PRNumber || pr.Title != title || pr.Body != body ||
		!prMatchesCandidate(pr, repo, identity, candidate, candidate.Successor.Branch) {
		return 0, fmt.Errorf("successor PR moved or changed during publication: %w", ErrPublicationConflict)
	}
	return pr.Number, nil
}
