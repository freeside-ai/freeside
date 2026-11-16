package daemoncompose

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestIntakeServesOtherRepositoryWhileOneStalls: while one repository's label
// listing hangs on GitHub, the pass still admits another repository's
// labeled issue (#3215). The stalled initiator comes first, so a pass that
// visits initiators in turn fails this.
func TestIntakeServesOtherRepositoryWhileOneStalls(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	stalled := intakeInitiatorFor(t, domain.InitiatorModePropose, domain.ProvenanceOverride, 5)
	stalled.Repo, stalled.RepositoryID, stalled.ProjectID = "example/stalled", intakeTestRepoID+1, "project-stalled"
	served := intakeInitiatorFor(t, domain.InitiatorModePropose, domain.ProvenanceOverride, 5)
	r := f.reconciler([]IntakeInitiator{stalled, served}, labeledOpen(7), nil)
	observe := r.observeLabel
	reached, release := make(chan struct{}), make(chan struct{})
	r.observeLabel = func(ctx context.Context, repo, label string) (publish.LabelIssuesObservation, error) {
		if repo == stalled.Repo {
			close(reached)
			<-release
			return publish.LabelIssuesObservation{}, errors.New("stalled listing released")
		}
		return observe(ctx, repo, label)
	}
	passed := make(chan struct{})
	go func() {
		defer close(passed)
		r.reconcile(t.Context(), nil)
	}()
	<-reached
	defer func() {
		close(release)
		<-passed
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var (
			o     domain.IntakeOccurrence
			found bool
		)
		if err := f.store.Read(t.Context(), func(tx *store.ReadTx) (err error) {
			o, found, err = tx.LatestIntakeOccurrence(t.Context(), intakeTestRepoID, 7, intakeTestLabel)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if found && o.Admission != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the served repository waited on the stalled repository's listing")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
