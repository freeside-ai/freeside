package publish

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestExternalReviewReplierServesOtherRepositoryWhileOneStalls: while one
// repository's token request hangs, the pass still replies on another
// repository's pull request (#3215). The stalled run sorts first, so a pass
// that visits runs in turn meets it first and never returns.
func TestExternalReviewReplierServesOtherRepositoryWhileOneStalls(t *testing.T) {
	t.Parallel()
	served := seedReplyBinding(t, "feat/test-replies", strings.Repeat("1", 40), strings.Repeat("a", 40))
	stalledFixture := seedReplyBindingIn(t, served.st, replyBindingSeed{"run-a-stalled", "example/stalled", 515151, 451},
		"feat/stalled-replies", strings.Repeat("1", 40), strings.Repeat("a", 40))
	stalledFixture.path = served.path
	newReplyHarnessForBinding(t, stalledFixture, "review/45", "review_comment/46", domain.ReviewDispositionDeclined, "Checked.")
	h := newReplyHarnessForBinding(t, served, "review/42", "review_comment/44", domain.ReviewDispositionDeclined, "Already checked.")
	var once sync.Once
	reached, release := make(chan struct{}), make(chan struct{})
	h.tokenFor = func(repo string) {
		if repo == "example/stalled" {
			once.Do(func() { close(reached) })
			<-release
		}
	}
	passed := make(chan struct{})
	go func() {
		defer close(passed)
		_ = h.r.Pass(context.Background())
	}()
	defer func() {
		close(release)
		<-passed
	}()
	select {
	case <-reached:
	case <-passed:
		t.Fatal("the pass never asked for the stalled repository's token")
	}
	replied := func() bool {
		var found bool
		h.read(func(tx *store.ReadTx) error {
			_, err := tx.GetOutbox(context.Background(), externalReplyKey(h.d.FindingID, h.d.Round)+"/outcome")
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			found = err == nil
			return err
		})
		return found
	}
	// A reply's outcome lands on the pass after its post. Later passes skip
	// the stalled project, whose run has not returned, as the Run loop's do.
	deadline := time.After(10 * time.Second)
	for !replied() {
		later := make(chan error, 1)
		go func() { later <- h.r.Pass(context.Background()) }()
		select {
		case err := <-later:
			if err != nil {
				t.Fatal(err)
			}
		case <-deadline:
			t.Fatal("the served repository waited on the stalled repository's token request")
		}
	}
}

// TestHandBackLabelPassServesOtherProjectWhileOneStalls: while one project's
// removal hangs on GitHub, the pass still removes another project's label
// (#3215). Seeding a due hand-back cycle needs the store's own fixtures, so
// this drives the pass's fan-out with a stand-in removal; the removal itself
// is TestHandBackLabelRemoval's.
func TestHandBackLabelPassServesOtherProjectWhileOneStalls(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var r HandBackLabelRemover
		runs := []store.Snapshotted[domain.Run]{
			{Value: domain.Run{ID: "run-stalled", ProjectID: "project-stalled"}},
			{Value: domain.Run{ID: "run-served", ProjectID: "project-served"}},
		}
		release := make(chan struct{})
		removed := make(chan domain.RunID, 1)
		passed := make(chan error, 1)
		go func() {
			passed <- r.removeEach(context.Background(), runs, func(_ context.Context, id domain.RunID) error {
				if id == "run-stalled" {
					<-release
					return nil
				}
				removed <- id
				return nil
			})
		}()
		synctest.Wait()
		select {
		case id := <-removed:
			if id != "run-served" {
				t.Fatalf("removed %q", id)
			}
		default:
			t.Fatal("the served project waited on the stalled project's removal")
		}
		close(release)
		if err := <-passed; err != nil {
			t.Fatal(err)
		}
	})
}

// TestProposalNoticesKeepAStalledDrivesHolds: a notice the last finished
// drive held stays held while the next drive of the same proposal runs, even
// across a pass that no longer finds the proposal, so an overlapping pass
// never resolves it. The drive's own finish decides.
func TestProposalNoticesKeepAStalledDrivesHolds(t *testing.T) {
	t.Parallel()
	n := newProposalNotices("test/", []string{"stalled"}, "cleared", time.Now)
	stalled, other := filingNotice{"proposal-a", "stalled"}, filingNotice{"proposal-b", "stalled"}
	for _, notice := range []filingNotice{stalled, other} {
		n.begin(notice.instanceID)
		n.hold(notice.instanceID, notice.code)
		n.finish(notice.instanceID)
	}
	n.begin(stalled.instanceID)
	n.startPass(map[domain.ProposalInstanceID]bool{})
	if !n.holding(stalled) {
		t.Fatal("a pass resolved the notice of a drive still in progress")
	}
	if n.holding(other) {
		t.Fatal("a proposal the pass no longer finds kept its notice")
	}
	n.finish(stalled.instanceID)
	if n.holding(stalled) {
		t.Fatal("a drive that saw no hold left its notice held")
	}
}
