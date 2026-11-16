package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// TestSubmitApplyRefusedWhileProjectNotReady: a submission through the
// daemon's control socket for a project that is not ready is refused and
// writes no run or submission; once recorded, the same submission replays
// while the project is not ready.
func TestSubmitApplyRefusedWhileProjectNotReady(t *testing.T) {
	t.Parallel()
	notReady := func(_ context.Context, _ *store.ReadTx, project domain.ProjectID) error {
		return &domain.ProjectNotReadyError{ProjectID: project, NotPassed: []domain.ProjectReadinessResult{{
			Check: domain.ProjectReadinessRepositoryBase, Status: domain.ProjectReadinessFailed, Detail: "no default branch",
		}}}
	}
	req := declareApplyRequest(t, egressApplyRequest(t, nil))
	st := storetest.Open(t, filepath.Join(t.TempDir(), "state.db"), store.Options{})
	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := applyLaunchSubmission(t.Context(), st, blobs, req, nil, notReady); !errors.Is(err, domain.ErrProjectNotReady) {
		t.Fatalf("apply = %v, want ErrProjectNotReady", err)
	}
	if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
		for _, runID := range []domain.RunID{req.ImplementationRunID, req.SpecificationRunID} {
			if _, err := tx.GetRun(t.Context(), runID); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("run %s = %v, want absent", runID, err)
			}
		}
		if _, err := tx.GetManualSubmission(t.Context(), "cli:"+req.SubmissionID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("manual submission = %v, want absent", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	recorded, err := applyLaunchSubmission(t.Context(), st, blobs, req, nil, nil)
	if err != nil {
		t.Fatalf("apply once ready: %v", err)
	}
	replayed, err := applyLaunchSubmission(t.Context(), st, blobs, req, nil, notReady)
	if err != nil || replayed.ImplementationRunID != recorded.ImplementationRunID {
		t.Fatalf("replay = %+v, %v; want the recorded submission %+v", replayed, err, recorded)
	}
}
