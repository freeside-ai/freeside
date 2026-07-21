package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const feedbackStageName = "conversation_feedback"

// FakeRunSpec is the fixed input needed to seed the 1A.0 walking skeleton.
// The digests are opaque here; the caller registered the approved spec and
// resolved policy before starting the run (plan §5.12).
type FakeRunSpec struct {
	RunID        domain.RunID
	ProjectID    domain.ProjectID
	SpecDigest   domain.Digest
	PolicyDigest domain.Digest
}

// StartFakeRun idempotently persists one fake run and ensures its first
// approval item exists. Existing progress is preserved; a retry with changed
// fixed bindings fails rather than retargeting the run.
func (e *Engine) StartFakeRun(ctx context.Context, spec FakeRunSpec) (domain.Run, error) {
	want := domain.Run{
		ID: spec.RunID, ProjectID: spec.ProjectID,
		SpecDigest: spec.SpecDigest, PolicyDigest: spec.PolicyDigest,
		Stages: []domain.Stage{},
	}
	if err := want.Validate(); err != nil {
		return domain.Run{}, fmt.Errorf("start fake run: %w", err)
	}

	var existing domain.Run
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		existing, err = tx.GetRun(ctx, want.ID)
		return err
	})
	switch {
	case err == nil:
		if existing.ProjectID != want.ProjectID || existing.SpecDigest != want.SpecDigest ||
			existing.PolicyDigest != want.PolicyDigest {
			return domain.Run{}, fmt.Errorf("start fake run %q: fixed bindings disagree with stored run: %w",
				want.ID, domain.ErrImmutableTransition)
		}
	case errors.Is(err, store.ErrNotFound):
		if err := e.store.Write(ctx, func(tx *store.WriteTx) error { return tx.PutRun(ctx, want) }); err != nil {
			return domain.Run{}, fmt.Errorf("start fake run %q: %w", want.ID, err)
		}
		existing = want
	case err != nil:
		return domain.Run{}, fmt.Errorf("start fake run %q: %w", want.ID, err)
	}

	if _, err := e.ensureItem(ctx, initialItem(existing)); err != nil {
		return domain.Run{}, err
	}
	return existing, nil
}

func (e *Engine) reconcileRuns(ctx context.Context) (int, error) {
	var runs []store.Snapshotted[domain.Run]
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		runs, err = tx.ListRuns(ctx)
		return err
	})
	if err != nil {
		return 0, err
	}

	transitions := 0
	for _, snapshotted := range runs {
		// A Run has no workflow-kind discriminator. StartFakeRun's deterministic
		// approval item is therefore the concrete 1A.0 ownership marker; never
		// attach this walking skeleton to unrelated runs merely because they
		// share the same store.
		owned, err := e.ownsFakeRun(ctx, snapshotted.Value)
		if err != nil {
			return transitions, err
		}
		if !owned {
			continue
		}
		advanced, err := e.reconcileRun(ctx, snapshotted.Value)
		if err != nil {
			return transitions, fmt.Errorf("run %q: %w", snapshotted.Value.ID, err)
		}
		transitions += advanced
	}
	return transitions, nil
}

func (e *Engine) ownsFakeRun(ctx context.Context, run domain.Run) (bool, error) {
	marker, err := e.signet.GetAttentionItem(ctx, initialItemID(run.ID))
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("find workflow marker for run %q: %w", run.ID, err)
	}
	if !sameWorkflowItem(marker.Item, initialItem(run)) {
		return false, fmt.Errorf("workflow marker for run %q disagrees with its binding: %w",
			run.ID, domain.ErrParentKeyMismatch)
	}
	return true, nil
}

func (e *Engine) reconcileRun(ctx context.Context, run domain.Run) (int, error) {
	created, err := e.ensureItem(ctx, initialItem(run))
	if err != nil {
		return 0, err
	}
	transitions := boolCount(created)

	approval, err := e.signet.GetAttentionItem(ctx, initialItemID(run.ID))
	if err != nil {
		return transitions, err
	}
	if approval.Item.Status != domain.StatusResolved {
		return transitions, nil
	}

	feedbackCreated, err := e.ensureItem(ctx, feedbackItem(run))
	if err != nil {
		return transitions, err
	}
	transitions += boolCount(feedbackCreated)

	stageAdded, err := e.ensureFeedbackStage(ctx, run.ID)
	if err != nil {
		return transitions, err
	}
	transitions += boolCount(stageAdded)
	return transitions, nil
}

func (e *Engine) ensureItem(ctx context.Context, want domain.AttentionItem) (bool, error) {
	existing, err := e.signet.GetAttentionItem(ctx, want.ID)
	switch {
	case err == nil:
		if !sameWorkflowItem(existing.Item, want) {
			return false, fmt.Errorf("attention item %q disagrees with workflow binding: %w",
				want.ID, domain.ErrImmutableTransition)
		}
		return false, nil
	case errors.Is(err, store.ErrNotFound):
		if err := e.signet.PutItem(ctx, want); err != nil {
			return false, fmt.Errorf("create attention item %q: %w", want.ID, err)
		}
		return true, nil
	default:
		return false, err
	}
}

func (e *Engine) ensureFeedbackStage(ctx context.Context, runID domain.RunID) (bool, error) {
	added := false
	err := e.store.Write(ctx, func(tx *store.WriteTx) error {
		run, err := tx.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		for _, stage := range run.Stages {
			if stage.ID != feedbackStageID(runID) {
				continue
			}
			if stage.Name != feedbackStageName {
				return fmt.Errorf("feedback stage %q has name %q: %w",
					stage.ID, stage.Name, domain.ErrImmutableTransition)
			}
			return errReplay
		}
		run.Stages = append(run.Stages, domain.Stage{
			ID: feedbackStageID(runID), RunID: runID,
			Name: feedbackStageName, Attempts: []domain.Attempt{},
		})
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		added = true
		return nil
	})
	if errors.Is(err, errReplay) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("ensure feedback stage for run %q: %w", runID, err)
	}
	return added, nil
}

func initialItem(run domain.Run) domain.AttentionItem {
	runID := run.ID
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: initialItemID(run.ID), ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &runID},
		Type:    domain.AttentionSpecApproval, Priority: domain.PriorityNormal,
		Reason: "Approve the fake run before conversation feedback.",
		// Approval is the only concluding choice in this walking skeleton. A
		// stop command also resolves an item, and the current immutable command
		// store intentionally has no item-indexed query; offering stop here
		// would make the two outcomes indistinguishable to the engine.
		RequestedDecision: []domain.Action{domain.ActionApprove},
		ItemVersion:       1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		panic(fmt.Sprintf("engine initial item invariant: %v", err))
	}
	return item
}

func feedbackItem(run domain.Run) domain.AttentionItem {
	runID := run.ID
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: feedbackItemID(run.ID), ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &runID},
		Type:    domain.AttentionSpecApproval, Priority: domain.PriorityNormal,
		Reason:            "Discuss the approved fake run with the agent.",
		RequestedDecision: []domain.Action{domain.ActionDiscuss},
		ItemVersion:       1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		panic(fmt.Sprintf("engine feedback item invariant: %v", err))
	}
	return item
}

func initialItemID(runID domain.RunID) domain.ItemID {
	return domain.ItemID("approval-" + string(runID))
}

func feedbackItemID(runID domain.RunID) domain.ItemID {
	return domain.ItemID("feedback-" + string(runID))
}

func feedbackStageID(runID domain.RunID) domain.StageID {
	return domain.StageID("feedback-" + string(runID))
}

func boolCount(v bool) int {
	if v {
		return 1
	}
	return 0
}

func sameWorkflowItem(got, want domain.AttentionItem) bool {
	return got.ID == want.ID && got.ProjectID == want.ProjectID &&
		got.Subject.Type == want.Subject.Type && got.Subject.ID == want.Subject.ID &&
		sameRunID(got.Subject.RunID, want.Subject.RunID) && got.Type == want.Type &&
		got.Priority == want.Priority && got.Reason == want.Reason &&
		slices.Equal(got.RequestedDecision, want.RequestedDecision) &&
		len(got.EvidenceSnapshot) == 0 && len(got.AgentClaims) == 0 &&
		len(got.ArtifactDigests) == 0 && got.PRHeadSHA == "" &&
		got.CommitPlanNotice == nil && got.InterruptionClass == want.InterruptionClass &&
		got.ExpiresWhen == nil
}

func sameRunID(a, b *domain.RunID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
