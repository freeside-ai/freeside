package signet

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestRunSnapshotLifecycleAndSupersession pins the summary's derived split
// (#1134): a superseded run is finished whatever its outcome and names its
// successor; an unsuperseded run follows domain.LifecycleOf.
func TestRunSnapshotLifecycleAndSupersession(t *testing.T) {
	run := domain.Run{ID: "run-1", ProjectID: "proj-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
	successor := domain.RunID("run-2")
	for _, outcome := range domain.AllRunOutcomes {
		conclusion := domain.RunConclusion{Outcome: outcome}
		alone := runSnapshot(run, store.Snapshot{EntityVersion: 1}, domain.RunObservation{RunID: run.ID},
			conclusion, 1, nil, runProjectionFacts{}).Run
		if alone.Lifecycle != domain.LifecycleOf(conclusion, false) || alone.SupersededBy != nil {
			t.Errorf("%s alone: lifecycle %s superseded_by %v", outcome, alone.Lifecycle, alone.SupersededBy)
		}
		retried := runSnapshot(run, store.Snapshot{EntityVersion: 1}, domain.RunObservation{RunID: run.ID},
			conclusion, 1, nil, runProjectionFacts{supersededBy: &successor}).Run
		if retried.Lifecycle != domain.RunLifecycleFinished || retried.SupersededBy == nil || *retried.SupersededBy != successor {
			t.Errorf("%s retried: lifecycle %s superseded_by %v, want finished by %s",
				outcome, retried.Lifecycle, retried.SupersededBy, successor)
		}
	}
}
