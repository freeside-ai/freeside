package ward

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

func TestTaskCancellationStopsReviewAndRecoversPartialCleanup(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "restart"}[restart], func(t *testing.T) {
			ctx := t.Context()
			fx := newHandoffFixture(t)
			seed := fx.seed(t)
			cfg, spec := testCodexReview(t)
			journal := &fakeCodexReviewJournal{}
			config := codexReviewSourceConfigForTest(t, fx.codexReviewLifecycle(t), cfg, spec, journal)
			source, err := NewCodexReviewSource(config)
			if err != nil {
				t.Fatal(err)
			}
			id := domain.InvocationID("review-task-cancellation")
			request := exec.ReviewRequest{
				RunID: "run-cancelled", Round: 1, Repo: seed.Seed.Base.Repo,
				RepositoryID: seed.Seed.Base.RepositoryID, BaseRef: seed.Seed.Base.BaseRef,
				BaseSHA: strings.Repeat("a", 40), HeadSHA: seed.Seed.Base.BaseSHA,
				Workspace: seed.Seed.SourceDir, Verification: testReviewVerificationEvidence(),
				Instructions: testReviewInstructionBinding(), RequestedAt: codexReviewEpoch.Add(-time.Minute),
			}
			if err := source.RequestReview(ctx, id, request); err != nil {
				t.Fatal(err)
			}
			intent, err := journal.GetCodexReviewIntent(ctx, string(id))
			if err != nil {
				t.Fatal(err)
			}
			fx.rt.onDeleteContainer = func(container string) (bool, error) {
				if container == intent.ReviewContainer {
					return true, errors.New("owned container is temporarily unavailable")
				}
				return false, nil
			}
			if err := source.CancelReview(ctx, id); err == nil {
				t.Fatal("failed cleanup was reported as quiescent")
			}
			outcome, ready, err := journal.GetCodexReviewOutcome(ctx, string(id))
			if err != nil || ready || !outcome.AbortRequired || outcome.FailureClass != domain.ReviewFailureTransient ||
				outcome.Failure != "review stopped by task cancellation" {
				t.Fatalf("cancellation fence: %#v, ready=%v, error=%v", outcome, ready, err)
			}
			if restart {
				source, err = NewCodexReviewSource(config)
				if err != nil {
					t.Fatal(err)
				}
			}
			fx.rt.onDeleteContainer = nil
			if err := source.CancelReview(ctx, id); err != nil {
				t.Fatal(err)
			}
			if err := source.CancelReview(ctx, id); err != nil {
				t.Fatalf("replay: %v", err)
			}
			after, ready, err := journal.GetCodexReviewOutcome(ctx, string(id))
			if err != nil || !ready || !reflect.DeepEqual(after, outcome) {
				t.Fatalf("recovered outcome changed: %#v, ready=%v, error=%v", after, ready, err)
			}
			if err := source.RequestReview(ctx, id, request); !errors.Is(err, exec.ErrDuplicateStart) {
				t.Fatalf("cancelled review could restart: %v", err)
			}
			containers, _ := fx.rt.ListContainers(ctx)
			volumes, _ := fx.rt.ListVolumes(ctx)
			networks, _ := fx.rt.ListNetworks(ctx)
			if len(containers)+len(volumes)+len(networks) != 0 {
				t.Fatalf("owned resources remain: %v %v %v", containers, volumes, networks)
			}
		})
	}
}

func TestTaskCancellationDoesNotInventUnknownReview(t *testing.T) {
	fx := newHandoffFixture(t)
	cfg, spec := testCodexReview(t)
	journal := &fakeCodexReviewJournal{}
	source, err := NewCodexReviewSource(codexReviewSourceConfigForTest(t, fx.codexReviewLifecycle(t), cfg, spec, journal))
	if err != nil {
		t.Fatal(err)
	}
	id := domain.InvocationID("review-never-started")
	if err := source.CancelReview(t.Context(), id); !errors.Is(err, exec.ErrUnknownInvocation) {
		t.Fatalf("unknown review = %v", err)
	}
	if _, _, err := journal.GetCodexReviewOutcome(t.Context(), string(id)); !errors.Is(err, ErrCodexReviewOutcomeNotFound) {
		t.Fatalf("invented outcome for unknown review: %v", err)
	}
}
