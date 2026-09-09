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
type realRunCheckpoint struct {
	State   string                    `json:"state"`
	Binding domain.ReadyItemPRBinding `json:"binding"`
	Branch  string                    `json:"branch"`
	Review  *domain.ReviewRecord      `json:"review,omitempty"`
	ready   domain.AttentionItem
	outcome publish.Outcome
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
