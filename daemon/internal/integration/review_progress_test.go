package integration_test

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
)

func TestProductionReviewProgressSurvivesRestart(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	id := engine.ProductionReviewInvocationID(p.runID, 1)
	script := fake.ReviewScript{PendingInspects: 2, Outcome: fake.OutcomeComplete, Result: exec.ReviewResult{
		BaseSHA: p.baseSHA, HeadSHA: p.replay.HeadSHA,
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
