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

// TestBoundImplementationRun pins the specification hand-off rule (#1183): a
// specification run whose production attempt is approved is superseded by that
// attempt's implementation run, and nothing else is.
func TestBoundImplementationRun(t *testing.T) {
	const specRun, implRun = domain.RunID("run-spec"), domain.RunID("run-impl")
	approved := domain.ProductionAttempt{
		SpecificationRunID: specRun, ImplementationRunID: implRun, ApprovedSpecDigest: "sha256:spec",
	}
	unapproved := domain.ProductionAttempt{SpecificationRunID: specRun, ImplementationRunID: implRun}
	foreign := domain.ProductionAttempt{
		SpecificationRunID: "run-other", ImplementationRunID: implRun, ApprovedSpecDigest: "sha256:spec",
	}
	cases := []struct {
		name    string
		run     domain.RunID
		attempt domain.ProductionAttempt
		want    domain.RunID
		bound   bool
	}{
		{"approved specification run is bound", specRun, approved, implRun, true},
		{"unapproved specification run is not bound", specRun, unapproved, "", false},
		{"implementation run of its own attempt is not bound", implRun, approved, "", false},
		{"attempt naming a different specification run is not bound", specRun, foreign, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, bound := boundImplementationRun(domain.Run{ID: tc.run}, tc.attempt)
			if bound != tc.bound || got != tc.want {
				t.Errorf("boundImplementationRun = (%q, %v), want (%q, %v)", got, bound, tc.want, tc.bound)
			}
		})
	}
}
