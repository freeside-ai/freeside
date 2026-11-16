package integration_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// refuseNotReady is a readiness gate that refuses every project with
// trust_profile failed.
func refuseNotReady(_ context.Context, _ *store.ReadTx, project domain.ProjectID) error {
	return &domain.ProjectNotReadyError{ProjectID: project, NotPassed: []domain.ProjectReadinessResult{{
		Check: domain.ProjectReadinessTrustProfile, Status: domain.ProjectReadinessFailed, Detail: "no approved trust profile",
	}}}
}

// TestReentryWaitsWhileProjectNotReady: no re-entry cycle starts for a
// project that is not ready, and the refusal writes nothing, so the trigger
// that was due is still due and starts the cycle once the project is ready.
// Each helper treats a readiness refusal as a cycle not started and fails
// on any other error.
func TestReentryWaitsWhileProjectNotReady(t *testing.T) {
	t.Parallel()
	t.Run("hand-back", func(t *testing.T) {
		t.Parallel()
		p := newProductionPublicationHarness(t, "")
		p.configureAdjudicator(t)
		first := p.publishReady(t)
		maintainer := externalMaintainer()
		p.listExternalReviewers(t, maintainer)
		head := p.pushToPullRequest(t, "REWORK.md", "the lander's rework\n")

		p.reentryGate = refuseNotReady
		if _, started := p.handBack(t, first, 1, maintainer, head, p.baseSHA, "the rebase broke the build"); started {
			t.Fatal("a hand-back cycle started while the project was not ready")
		}
		p.reentryGate = nil
		if _, started := p.handBack(t, first, 1, maintainer, head, p.baseSHA, "the rebase broke the build"); !started {
			t.Fatal("the refused hand-back did not start once the project was ready")
		}
	})
	t.Run("external review", func(t *testing.T) {
		t.Parallel()
		p := newProductionPublicationHarness(t, "")
		p.publishReady(t)
		maintainer := externalMaintainer()
		p.listExternalReviewers(t, maintainer)
		p.leaveExternalFinding(t, 1, maintainer, p.replay.HeadSHA, "this leaks the handle")

		p.reentryGate = refuseNotReady
		if p.startExternalReview(t) {
			t.Fatal("an external review cycle started while the project was not ready")
		}
		p.reentryGate = nil
		if !p.startExternalReview(t) {
			t.Fatal("the refused external review did not start once the project was ready")
		}
	})
	t.Run("readiness", func(t *testing.T) {
		t.Parallel()
		p := newProductionPublicationHarnessWithBoundIssue(t, "", 1986)
		p.publishReady(t)
		head := p.pushToPullRequest(t, "FOLLOWUP.md", "pushed after readiness\n")
		fact := domain.ReadinessInvalidation{
			Reason: domain.ReadinessInvalidationHeadChanged, Bound: p.replay.HeadSHA, Observed: head,
		}

		p.reentryGate = refuseNotReady
		if _, started := p.invalidateReady(t, fact); started {
			t.Fatal("a readiness re-entry started while the project was not ready")
		}
		p.reentryGate = nil
		if _, started := p.invalidateReady(t, fact); !started {
			t.Fatal("the refused re-entry did not start once the project was ready")
		}
	})
}

// TestMovedReentryWaitsWhileProjectNotReady: a cycle whose head moved while
// it ran starts the next cycle as it ends. While the project is not ready
// that start is refused: the pass does not fail, nothing is written, and the
// run is held under blocking_system_health. The first pass after the
// project is ready supersedes the cycle's item and starts the next cycle.
func TestMovedReentryWaitsWhileProjectNotReady(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	p.publishReady(t)
	second := p.pushToPullRequest(t, "FOLLOWUP.md", "pushed after readiness\n")
	if _, started := p.invalidateReady(t, domain.ReadinessInvalidation{
		Reason: domain.ReadinessInvalidationHeadChanged, Bound: p.replay.HeadSHA, Observed: second,
	}); !started {
		t.Fatal("a head change started no re-entry")
	}
	var notReady atomic.Bool
	notReady.Store(true)
	p.engineOptions = append(p.engineOptions, engine.WithProjectReadinessGate(
		func(ctx context.Context, tx *store.ReadTx, project domain.ProjectID) error {
			if !notReady.Load() {
				return nil
			}
			return refuseNotReady(ctx, tx, project)
		}))
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	p.scriptCleanReview(2, p.baseSHA, second)
	var third string
	p.transport.OnHeadFetch(func(call int) {
		// The second fetch of the cycle is its check of the live head just
		// before readiness.
		if call == 2 {
			third = p.pushToPullRequest(t, "LATER.md", "pushed during the cycle\n")
			p.scriptCleanReview(3, p.baseSHA, third)
		}
	})
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatalf("a refused next cycle failed the pass: %v", err)
	}
	if third == "" {
		t.Fatal("the cycle never checked the live head before readiness")
	}
	chainLength := func() int {
		t.Helper()
		var length int
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			chain, err := tx.PublicationSuccessorChain(p.ctx, p.runID)
			length = len(chain)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return length
	}
	if n := chainLength(); n != 1 {
		t.Fatalf("successor chain length = %d, want 1: the refused cycle wrote nothing", n)
	}
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		hold, held, err := tx.GetRunHold(p.ctx, p.runID)
		if err != nil || !held || hold.Reason != domain.HoldBlockingSystemHealth {
			t.Fatalf("run hold = %#v held %t, %v; want blocking_system_health", hold, held, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	notReady.Store(false)
	p.transport.OnHeadFetch(nil)
	p.now = p.now.Add(time.Hour)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	if n := chainLength(); n != 2 {
		t.Fatalf("successor chain length = %d, want 2 once the project is ready", n)
	}
}
