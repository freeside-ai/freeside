package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// notReadyGate is a readiness gate that refuses every project, naming
// checks as the ones that failed.
func notReadyGate(checks ...domain.ProjectReadinessCheck) ProjectReadinessGate {
	return func(_ context.Context, _ *store.ReadTx, project domain.ProjectID) error {
		refusal := &domain.ProjectNotReadyError{ProjectID: project}
		for _, check := range checks {
			refusal.NotPassed = append(refusal.NotPassed, domain.ProjectReadinessResult{
				Check: check, Status: domain.ProjectReadinessFailed, Detail: "broken for the test",
			})
		}
		return refusal
	}
}

// TestImageRepairTaskWaivesOnlyWriterImage: the image-repair task is opened
// while writer_image is its project's only failed check, since that is
// what it repairs, and refused, writing nothing, when anything else failed.
func TestImageRepairTaskWaivesOnlyWriterImage(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		failed []domain.ProjectReadinessCheck
		opened bool
	}{
		"writer_image alone": {[]domain.ProjectReadinessCheck{domain.ProjectReadinessWriterImage}, true},
		"writer_image and trust_profile": {[]domain.ProjectReadinessCheck{
			domain.ProjectReadinessTrustProfile, domain.ProjectReadinessWriterImage,
		}, false},
		"repository_base alone": {[]domain.ProjectReadinessCheck{domain.ProjectReadinessRepositoryBase}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			refusal := repairTestRefusal(t, domain.ImageUpkeepRefusalRepair, repairTestRepositoryID, strings.Repeat("a", 40))
			service, st := newGatedImageRepairHarness(
				t, notReadyGate(tc.failed...), []domain.ProjectID{repairTestProject}, refusal)
			_, err := service.SubmitImageRepairTask(ctx, repairTestProject, refusal.ID)
			if tc.opened {
				if err != nil {
					t.Fatalf("submit: %v", err)
				}
				return
			}
			if !errors.Is(err, domain.ErrProjectNotReady) {
				t.Fatalf("submit = %v, want ErrProjectNotReady", err)
			}
			if err := st.Read(ctx, func(tx *store.ReadTx) error {
				_, err := tx.GetTaskSubmission(ctx, domain.ImageUpkeepSubmissionIdentity(refusal.ID))
				if !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("refused submission recorded: %v", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// TestProjectNotReadyErrorNamesTheChecks: a refusal's message names each
// check that did not pass, and the waiver reads only a record's results.
func TestProjectNotReadyErrorNamesTheChecks(t *testing.T) {
	t.Parallel()
	err := notReadyGate(domain.ProjectReadinessTrustProfile, domain.ProjectReadinessWriterImage)(
		t.Context(), nil, "project-1")
	for _, want := range []string{"project-1", "trust_profile failed", "writer_image failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("refusal %q does not name %q", err, want)
		}
	}
	missing := &domain.ProjectNotReadyError{ProjectID: "project-1", Reason: "not yet evaluated"}
	if missing.OnlyNotPassed(domain.ProjectReadinessWriterImage) {
		t.Fatal("a refusal with no record waived writer_image")
	}
	if requireProjectReady(t.Context(), nil, nil, "project-1") != nil {
		t.Fatal("a nil gate refused")
	}
}
