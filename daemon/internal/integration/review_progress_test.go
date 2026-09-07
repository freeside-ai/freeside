package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type capturedReviewRequestSource struct {
	*fake.ReviewSource
	request exec.ReviewRequest
}

func (s *capturedReviewRequestSource) RequestReview(ctx context.Context, id domain.InvocationID, request exec.ReviewRequest) error {
	s.request = request
	return s.ReviewSource.RequestReview(ctx, id, request)
}

func TestProductionReviewUpgradePreservesRequestTime(t *testing.T) {
	for _, corruption := range []string{"none", "legacy", "run", "round", "base", "head", "time", "digest"} {
		t.Run(corruption, func(t *testing.T) {
			p := newProductionPublicationHarness(t, "")
			source := &capturedReviewRequestSource{ReviewSource: p.reviewer}
			p.reviewSource = source
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			id := engine.ProductionReviewInvocationID(p.runID, 1)
			p.reviewer.Script(id, fake.ReviewScript{PendingInspects: 100, Outcome: fake.OutcomeComplete})
			p.startAndRecordExport(t)
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatal(err)
			}
			original := source.request
			if original.RequestedAt.IsZero() {
				t.Fatal("review request was not captured")
			}
			retained := original
			switch corruption {
			case "run":
				retained.RunID = "other-run"
			case "round":
				retained.Round++
			case "base":
				retained.BaseSHA = "other-base"
			case "head":
				retained.HeadSHA = "other-head"
			case "time":
				retained.RequestedAt = time.Time{}
			}
			body, err := json.Marshal(retained)
			if err != nil {
				t.Fatal(err)
			}
			if corruption == "legacy" {
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(body, &fields); err != nil {
					t.Fatal(err)
				}
				delete(fields, "instructions")
				body, err = json.Marshal(fields)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := p.store.WriteInternal(p.ctx, func(tx *store.InternalTx) error {
				return tx.RecordCodexReviewRequest(p.ctx, string(id), body)
			}); err != nil {
				t.Fatal(err)
			}
			// Reconstruct schema 66 with only the in-flight provider journal,
			// then exercise the real migration and workflow restart.
			raw, err := sql.Open("sqlite", p.dbPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, query := range []string{"DROP TABLE review_requests", "DELETE FROM schema_migrations WHERE version = 67"} {
				if _, err := raw.ExecContext(p.ctx, query); err != nil {
					_ = raw.Close()
					t.Fatal(err)
				}
			}
			if corruption == "digest" {
				if _, err := raw.ExecContext(p.ctx, "UPDATE codex_review_requests SET body_digest='wrong'"); err != nil {
					_ = raw.Close()
					t.Fatal(err)
				}
			}
			if err := raw.Close(); err != nil {
				t.Fatal(err)
			}
			p.reopenStoreWithApprovedRecipes(t, map[domain.Digest]bool{p.recipeD: true})
			p.now = p.now.Add(time.Minute)
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			_, reconcileErr := p.reconcileLanes()
			var request domain.ReviewRequestRecord
			readErr := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
				var err error
				request, err = tx.GetReviewRequest(p.ctx, id)
				return err
			})
			if corruption != "none" && corruption != "legacy" {
				if reconcileErr == nil || !errors.Is(readErr, store.ErrNotFound) {
					t.Fatalf("corrupt upgrade created request facts: reconcile=%v read=%v", reconcileErr, readErr)
				}
				return
			}
			if reconcileErr != nil || readErr != nil {
				t.Fatal(errors.Join(reconcileErr, readErr))
			}
			if !request.RequestedAt.Equal(original.RequestedAt) || request.RequestedAt.Equal(p.now) {
				t.Fatalf("upgrade changed original request time: got %s want %s", request.RequestedAt, original.RequestedAt)
			}
			timeline, err := p.attention.GetRunTimeline(p.ctx, p.runID)
			if err != nil {
				t.Fatal(err)
			}
			if timeline.Review == nil || len(timeline.Review.Rounds) != 1 || timeline.Review.Rounds[0].RequestedAt == nil || !timeline.Review.Rounds[0].RequestedAt.Equal(original.RequestedAt) {
				t.Fatalf("timeline lost original request time: %#v", timeline.Review)
			}
		})
	}
}

func TestProductionReviewProgressSurvivesRestart(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	id := engine.ProductionReviewInvocationID(p.runID, 1)
	script := fake.ReviewScript{PendingInspects: 2, Outcome: fake.OutcomeComplete, Result: exec.ReviewResult{
		BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA, Provider: "openai", ModelConfiguration: "codex/test", CostOwner: "test",
		CompletedAt: p.now, CompletionEvidence: productionDigest([]byte("review timeline clean")),
	}}
	p.reviewer.Script(id, script)
	crash := errors.New("crash before review launch")
	p.workflow = p.newEngine(t, productionCrashSeams{transitionHook: func(transition engine.DurableTransition, side engine.DurableTransitionSide) error {
		if transition == engine.DurableTransitionReviewRequest && side == engine.DurableTransitionBefore {
			return crash
		}
		return nil
	}}, true)
	p.reviewer.Script(id, script)
	p.startAndRecordExport(t)
	if _, err := p.reconcileLanes(); !errors.Is(err, crash) {
		t.Fatalf("crash seam: %v", err)
	}
	assertState := func(want domain.ReviewProgressState) {
		t.Helper()
		timeline, err := p.attention.GetRunTimeline(p.ctx, p.runID)
		if err != nil {
			t.Fatal(err)
		}
		if timeline.Review == nil || len(timeline.Review.Rounds) != 1 {
			t.Fatalf("review: %#v", timeline.Review)
		}
		round := timeline.Review.Rounds[0]
		if round.State != want || round.InvocationID != id || round.HeadSHA != p.replay.HeadSHA || round.Round != 1 || round.RequestedAt == nil {
			t.Fatalf("round: %#v", round)
		}
		if want == domain.ReviewCompleted && (round.Outcome == nil || *round.Outcome != domain.ReviewClean) {
			t.Fatalf("outcome: %#v", round)
		}
	}
	assertState(domain.ReviewPending)
	p.restartDurableState(t)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.reviewer.Script(id, script)
	assertState(domain.ReviewPending)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	assertState(domain.ReviewRunning)
	// Restart the workflow while its provider session remains alive. The
	// fake's full reconstruction deliberately loses live sessions.
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	assertState(domain.ReviewRunning)
	for range 4 {
		if _, err := p.reconcileLanes(); err != nil {
			t.Fatal(err)
		}
	}
	assertState(domain.ReviewCompleted)
	p.restartDurableState(t)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	assertState(domain.ReviewCompleted)
}
