package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publicationrecord"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// A retained checkpoint proves authenticated publication history, not permission
// to continue work. Only a current open ready binding proves current readiness.
// A pre-publication checkpoint proves only that the admitted run has not
// published; it carries no binding and grants nothing.
type realRunCheckpoint struct {
	State      string                     `json:"state"`
	Binding    domain.ReadyItemPRBinding  `json:"binding"`
	Branch     string                     `json:"branch"`
	Review     *domain.ReviewRecord       `json:"review,omitempty"`
	Completion *domain.WorkUnitCompletion `json:"completion,omitempty"`
	ready      domain.AttentionItem
	outcome    publish.Outcome
}

func readRealRunCheckpoint(ctx context.Context, tx *store.ReadTx, runID domain.RunID, retained bool) (realRunCheckpoint, error) {
	var result realRunCheckpoint
	current, err := tx.CurrentProductionReadyItemID(ctx, runID)
	if err != nil {
		return result, err
	}
	selected := current
	if retained {
		selected, err = tx.PublishedProductionReadyItemID(ctx, runID)
		if err != nil {
			return result, err
		}
	}
	result.Binding, err = tx.GetReadyItemPRBinding(ctx, selected)
	if errors.Is(err, store.ErrNotFound) && retained && selected == domain.ProductionReadyItemID(runID) {
		return realRunPrePublicationCheckpoint(ctx, tx, runID, err)
	}
	if err != nil {
		return result, err
	}
	result.ready, err = tx.GetAttentionItem(ctx, selected)
	if err != nil {
		return result, err
	}
	entry, err := tx.GetInbox(ctx, publicationrecord.OutcomeKey(result.Binding.PublicationIdentity))
	if err != nil {
		return result, err
	}
	result.outcome, err = publish.DecodeOutcome(entry.Payload)
	if err != nil || entry.Kind != publish.IntentKindOutcome || !result.outcome.EvidenceEligible {
		return result, errors.Join(err, domain.ErrParentKeyMismatch)
	}
	result.Branch = result.outcome.Branch
	completion, err := realRunCompletedCheckpoint(ctx, tx, runID, result.Binding)
	if err != nil {
		return result, err
	}
	if completion != nil {
		if !retained {
			return result, store.ErrPublicationCompleted
		}
		result.State, result.Completion = "completed", completion
		return result, nil
	}
	if selected != current || result.ready.Status != domain.StatusOpen {
		if !retained {
			return result, fmt.Errorf("current publication has no open ready item")
		}
		if err := tx.RequireIncompletePublication(ctx, runID); err != nil {
			return result, err
		}
		if selected == current {
			if result.ready.Status == domain.StatusSuperseded {
				if err := realRunFeedbackContinuation(ctx, tx, result.Binding); err != nil {
					return result, err
				}
			} else if result.ready.Status != domain.StatusDismissed {
				return result, fmt.Errorf("retained ready item is neither dismissed nor awaiting feedback")
			}
		}
		// A different current ID was obtained through the authenticated sealed
		// successor chain. Its predecessor is history, never current readiness.
		result.State = "retained"
		return result, nil
	}
	review, err := tx.LatestReviewRecord(ctx, runID)
	if err != nil || review.HeadSHA != result.Binding.HeadSHA {
		return result, errors.Join(err, fmt.Errorf("latest review does not cover current publication"))
	}
	successor, err := tx.CurrentPublicationSuccessor(ctx, runID)
	if err != nil {
		return result, err
	}
	if successor != nil && (review.Round < successor.ReviewRound || result.Binding.PublicationInvocationID != successor.PublicationID()) {
		return result, domain.ErrParentKeyMismatch
	}
	result.State, result.Review = "ready", &review
	return result, nil
}

// A run interrupted after admission and before publication, such as at a
// review_dispute gate, has no ready binding to select. The caller has already
// anchored on the run's admission. Absence of a ready item is not proof: a
// publish_blocked hold after publication records no ready binding. Proof is
// the run's publication slot, which holds its reservation until the publisher
// commits an intent; a committed intent, and so any successor chain, means
// publication began. A ready item whose binding is missing, or a completion,
// is damaged history either way.
func realRunPrePublicationCheckpoint(ctx context.Context, tx *store.ReadTx, runID domain.RunID, missing error) (realRunCheckpoint, error) {
	reservation, err := publish.NewReservation(domain.ProductionPublicationInvocationID(runID), runID)
	if err != nil {
		return realRunCheckpoint{}, err
	}
	if err := publish.CheckInvocationAvailable(ctx, tx, reservation); err != nil {
		return realRunCheckpoint{}, fmt.Errorf("run publication began without a ready binding: %w", errors.Join(err, missing))
	}
	// Availability also passes a free slot. The run reserves its slot in the
	// transaction that creates it and nothing frees it, so a free slot is
	// damage that could equally have taken a committed intent. Only the
	// reservation itself, which the check above matched to this run, proves it.
	key, err := reservation.Key()
	if err != nil {
		return realRunCheckpoint{}, err
	}
	slot, err := tx.GetOutbox(ctx, key)
	if err != nil || slot.Kind != publish.IntentKindReservation {
		return realRunCheckpoint{}, fmt.Errorf("run publication slot holds no reservation: %w", errors.Join(err, missing))
	}
	items, err := tx.ListAttentionItems(ctx)
	if err != nil {
		return realRunCheckpoint{}, err
	}
	for _, snapshot := range items {
		item := snapshot.Value
		if item.Type == domain.AttentionReadyForFinalReview && item.Subject.RunID != nil && *item.Subject.RunID == runID {
			return realRunCheckpoint{}, fmt.Errorf("published ready item has no binding: %w", missing)
		}
	}
	if err := tx.RequireIncompletePublication(ctx, runID); err != nil {
		return realRunCheckpoint{}, err
	}
	return realRunCheckpoint{State: "pre-publication"}, nil
}

// A completion is historical authority for restoring access, never permission
// to execute again. Reuse the store's full declaration/fact-timeline re-gate,
// then bind its result to the publication this run actually produced.
func realRunCompletedCheckpoint(ctx context.Context, tx *store.ReadTx, runID domain.RunID, ready domain.ReadyItemPRBinding) (*domain.WorkUnitCompletion, error) {
	declaration, err := tx.GetWorkUnitDeclarationByRun(ctx, runID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	completion, err := tx.GetWorkUnitCompletion(ctx, declaration.ID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	binding, err := tx.EffectiveWorkUnitPRBinding(ctx, declaration.ID)
	if err != nil {
		return nil, err
	}
	if ready.RunID != runID || binding.Repo != ready.Repo || binding.RepositoryID != ready.RepositoryID ||
		binding.PRNumber != ready.PRNumber || binding.BaseRef != ready.BaseRef || binding.HeadSHA != ready.HeadSHA {
		return nil, fmt.Errorf("completed work-unit binding disagrees with published ready resource: %w", domain.ErrParentKeyMismatch)
	}
	return &completion, nil
}

func realRunFeedbackContinuation(ctx context.Context, tx *store.ReadTx, published domain.ReadyItemPRBinding) error {
	items, err := tx.ListAttentionItems(ctx)
	if err != nil {
		return err
	}
	for _, snapshot := range items {
		item := snapshot.Value
		if item.Type != domain.AttentionExecutionFailure || item.Subject.RunID == nil ||
			*item.Subject.RunID != published.RunID || item.ExecutionFailure == nil ||
			!strings.HasPrefix(string(item.ExecutionFailure.InvocationID), "inv-operator-feedback-") {
			continue
		}
		_, parent, found, err := tx.FeedbackPublicationParent(ctx, item.ExecutionFailure.InvocationID)
		if err != nil {
			return err
		}
		if !found || parent.ItemID != published.ItemID {
			continue
		}
		if item.Status == domain.StatusOpen {
			_, err := tx.OperatorFeedbackRetryParent(ctx, item)
			return err
		}
		commands, err := tx.ListCommandsForItem(ctx, item.ID)
		if err != nil {
			return err
		}
		for _, command := range commands {
			if command.Action != domain.ActionRetry {
				continue
			}
			invocation := domain.InvocationID("inv-operator-feedback-" + command.CommandID)
			_, err := tx.GetOutbox(ctx, string(invocation))
			if errors.Is(err, store.ErrNotFound) {
				// Acceptance precedes engine dispatch. The authenticated command
				// and still-preserved failure prove this short pending interval.
				_, err = tx.GetExecutionOutcomeRecord(ctx, item.ExecutionFailure.InvocationID)
				return err
			}
			if err != nil {
				return err
			}
			intent, err := tx.OperatorFeedbackIntent(ctx, invocation)
			if err != nil || intent.Retry == nil || intent.Retry.FailedInvocationID != item.ExecutionFailure.InvocationID {
				return errors.Join(err, domain.ErrParentKeyMismatch)
			}
			if _, err := tx.GetExecutionOutcomeRecord(ctx, invocation); errors.Is(err, store.ErrNotFound) {
				return nil
			} else if err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("superseded publication has no authenticated retry continuation")
}

func writeRealRunCheckpoint(path string, checkpoint realRunCheckpoint) error {
	if path == "" {
		return nil
	}
	body, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	writeErr := root.WriteFile(filepath.Base(path), append(body, '\n'), 0o600)
	return errors.Join(writeErr, root.Close())
}

func TestRealRunCheckpointOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.json")
	if err := writeRealRunCheckpoint(path, realRunCheckpoint{State: "retained"}); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	body, err := root.ReadFile("checkpoint.json")
	if err := errors.Join(err, root.Close()); err != nil {
		t.Fatal(err)
	}
	var checkpoint realRunCheckpoint
	if err := json.Unmarshal(body, &checkpoint); err != nil {
		t.Fatal(err)
	}
	if checkpoint.State != "retained" {
		t.Fatalf("checkpoint state = %q, want retained", checkpoint.State)
	}
}

func TestRealRunRetainedSchema(t *testing.T) {
	if os.Getenv("FREESIDE_REAL_RUN_SCHEMA_TEST") != "1" {
		t.Skip("operator-only retained schema check")
	}
	root := os.Getenv("FREESIDE_REAL_RUN_STATE_ROOT")
	if root == "" {
		t.Fatal("retained state root is required")
	}
	st, err := store.OpenReadOnly(context.Background(), filepath.Join(root, "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}
