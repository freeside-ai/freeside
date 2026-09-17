package publish

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func requireTaskPublicationOpen(ctx context.Context, tx *store.ReadTx, id domain.RunID) error {
	if id == "" {
		return nil // Legacy attended candidates have no task-bound reservation.
	}
	run, err := tx.GetRun(ctx, id)
	if err != nil {
		return err
	}
	task, err := tx.GetTask(ctx, run.TaskID)
	if err != nil {
		return err
	}
	if task.Cancellation != nil {
		return store.ErrTaskCancellationFenced
	}
	return nil
}

func (p *Publisher) taskEffectOpen(ctx context.Context, c Candidate) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.storeDecision == nil {
		return nil
	}
	return p.storeDecision.store.Read(ctx, func(tx *store.ReadTx) error {
		return requireTaskPublicationOpen(ctx, tx, c.RunID)
	})
}

// ReconcileCancelledRun observes exact committed identities without creating,
// repairing, or closing anything on the forge. A missing PR is not proof that
// an interrupted external request was rejected, so its intent remains pending.
func (p *Publisher) ReconcileCancelledRun(ctx context.Context, runID domain.RunID) error {
	if p.storeDecision == nil {
		return errors.New("cancelled publication has no durable decision store")
	}
	s := p.storeDecision.store
	var pending []store.QueueEntry
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		if err := requireTaskPublicationOpen(ctx, tx, runID); !errors.Is(err, store.ErrTaskCancellationFenced) {
			return errors.Join(err, errors.New("cancelled publication observation requires a task fence"))
		}
		var err error
		pending, err = tx.ListPendingOutbox(ctx, IntentKindPublication)
		return err
	}); err != nil {
		return err
	}
	var unresolved error
	for _, entry := range pending {
		intent, err := DecodeStoredIntent(entry)
		if err != nil {
			return err
		}
		if intent.ReservationRunID != runID {
			continue
		}
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			auth, err := tx.GetCandidateAuthorization(ctx, intent.AuthorizationID)
			if err != nil {
				return err
			}
			if !auth.AuthorizesPublication || auth.HeadSHA != intent.SourceHeadSHA || auth.Repo != intent.Repo {
				return domain.ErrParentKeyMismatch
			}
			exported, err := tx.GetExecutionExport(ctx, intent.ProducingInvocationID)
			if err != nil {
				return err
			}
			admission, err := tx.GetExecutionAdmission(ctx, intent.ProducingInvocationID)
			if err != nil {
				return err
			}
			if admission.RunID != runID || admission.ID != exported.AdmissionID || exported.HeadSHA != intent.SourceHeadSHA {
				return domain.ErrParentKeyMismatch
			}
			return nil
		}); err != nil {
			return err
		}
		repo, err := parseRepo(intent.Repo)
		if err != nil {
			return err
		}
		branch := publicationrecord.ExpectedBranch(intent)
		prs, err := p.forge.listPRsByHead(ctx, repo, branch)
		if err != nil {
			unresolved = errors.Join(unresolved, err)
			continue
		}
		identity := Identity{digest: intent.Identity}
		candidate := Candidate{Repo: intent.Repo, BaseRef: intent.BaseRef, HeadSHA: intent.SourceHeadSHA}
		if len(prs) != 1 || !prMatchesPublicationCoordinates(prs[0], repo, identity, candidate, branch) ||
			(intent.Successor != nil && prs[0].Number != intent.Successor.PRNumber) {
			unresolved = errors.Join(unresolved, fmt.Errorf("publication %s has no exact settled external outcome", intent.InvocationID))
			continue
		}
		if err := finalizePublicationEntry(ctx, s, entry.IdempotencyKey, intent, Result{
			Identity: identity, Branch: branch, PRNumber: prs[0].Number,
		}); err != nil {
			unresolved = errors.Join(unresolved, err)
		}
	}
	return unresolved
}
