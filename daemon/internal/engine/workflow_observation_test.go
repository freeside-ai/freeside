package engine

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type closedReviewSource struct {
	immediateShadowSource
	failure error
}

func (s *closedReviewSource) Inspect(context.Context, domain.InvocationID) (exec.Status, error) {
	panic("closed observation must not call a recovery-capable Inspect")
}

func (s *closedReviewSource) RequestReview(context.Context, domain.InvocationID, exec.ReviewRequest) error {
	panic("closed observation must not request a review")
}

func (s *closedReviewSource) Poll(ctx context.Context, id domain.InvocationID) (exec.ReviewResult, error) {
	if s.failure != nil {
		return exec.ReviewResult{}, s.failure
	}
	return s.immediateShadowSource.Poll(ctx, id)
}

func TestWorkflowClosureRetainsReviewHistoryAcrossConfigurationChange(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "result", true: "failure with usage"}[failed], func(t *testing.T) {
			e, run, attempt := usageEngineFixture(t, true)
			st := e.store
			id := attempt.InvocationID
			now := time.Date(2026, 10, 20, 15, 0, 0, 0, time.UTC)
			w := &productionPublicationWorkflow{store: st, reviewConfigurationDigest: adjudicationDigest("e")}
			_, instructions, err := exec.ComposeCodexReviewInstructions(exec.ReviewHostInstructionInput{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			request := exec.ReviewRequest{
				RunID: run.ID, Round: 2, Repo: "owner/repo", RepositoryID: 1, BaseRef: "main",
				BaseSHA: strings.Repeat("a", 40), HeadSHA: strings.Repeat("b", 40), Workspace: t.TempDir(),
				Instructions: instructions, Admission: *execfake.RecordAdmission(domain.RoleReviewer, run.ID, id),
				Verification: exec.ReviewVerificationEvidence{
					Outcome: domain.VerificationPassed, RecipeDigest: adjudicationDigest("a"),
					EvidenceSnapshotDigest: adjudicationDigest("b"),
				},
				RequestedAt: now,
			}
			body, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			binding := domain.ReviewRequestRecord{
				InvocationID: id, RunID: request.RunID, Round: request.Round,
				BaseSHA: request.BaseSHA, HeadSHA: request.HeadSHA, RequestedAt: request.RequestedAt,
			}
			if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
				if err := tx.PutReviewAdmission(t.Context(), request.Admission); err != nil {
					return err
				}
				return tx.RecordCodexReviewRequest(t.Context(), string(id), body)
			}); err != nil {
				t.Fatal(err)
			}
			usage := []exec.UsageMeasurement{{
				Source: domain.UsageSourceReviewSource, Kind: domain.UsageMeasurementBillableCost,
				Metric: "cost", Unit: "usd_micros", Quantity: 42, Sequence: 1,
				ObservedAt: now.Add(time.Minute),
			}}
			source := &closedReviewSource{immediateShadowSource: immediateShadowSource{
				requested: true, request: request, id: id,
				result: exec.ReviewResult{
					ConfigurationDigest: adjudicationDigest("c"), CompletionEvidence: adjudicationDigest("d"),
					CompletedAt: now.Add(time.Minute), Usage: usage,
				},
			}}
			if failed {
				source.failure = &exec.ReviewSourceFailure{
					Class: domain.ReviewFailureQuota, Err: exec.ErrNoResult, Usage: usage,
				}
			}
			closeWorkflowForTest(t, st, run.ID, true)
			terminalFailure := source.failure
			source.failure = errors.Join(exec.ErrResultNotReady, &exec.ReviewSourceFailure{
				Class: domain.ReviewFailureTransient, Err: errors.New("journal temporarily unavailable"),
			})
			if err := w.observeClosedReview(t.Context(), binding, id, source); err != nil {
				t.Fatal(err)
			}
			if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
				_, err := tx.GetInbox(t.Context(), "closed-workflow-review/"+string(id))
				return err
			}); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("transient observation claimed the terminal key: %v", err)
			}
			source.failure = terminalFailure
			for range 2 {
				if err := w.observeClosedReview(t.Context(), binding, id, source); err != nil {
					t.Fatal(err)
				}
			}
			if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
				if _, err := tx.GetInbox(t.Context(), "closed-workflow-review/"+string(id)); err != nil {
					return err
				}
				if _, err := tx.GetReviewRecord(t.Context(), id); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("historical result granted review authority: %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if err := st.ReadUsage(t.Context(), func(tx *store.UsageReadTx) error {
				rows, err := tx.ListRunUsageObservations(t.Context(), run.ID)
				if err == nil && (len(rows) != 1 || rows[0].Quantity != 42) {
					t.Fatalf("late review usage = %+v", rows)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
