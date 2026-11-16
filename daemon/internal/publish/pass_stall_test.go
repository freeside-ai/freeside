package publish_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// stalledProject is a second project whose repository the fake forge does not
// serve: its token request stalls before any request reaches the forge.
var stalledProject = domain.Project{ID: "project-stalled", Repo: "example/stalled", RepositoryID: testRepoID + 1}

// stallingTokens holds every token request for repo until release closes,
// and closes reached on the first. Other repositories' requests pass through.
type stallingTokens struct {
	inner            publish.TokenSource
	repo             string
	reached, release chan struct{}
	once             sync.Once
}

func (s *stallingTokens) Token(ctx context.Context, repo string) (publish.InstallationToken, error) {
	if repo != s.repo {
		return s.inner.Token(ctx, repo)
	}
	s.once.Do(func() { close(s.reached) })
	<-s.release
	return publish.InstallationToken{}, errors.New("stalled token request released")
}

// stallOtherRepository registers stalledProject, seeds a run in it, and makes
// its token requests stall. A test proposes the stalled run's item first, so
// it is the older one and a pass that visits items in turn meets it first.
func (h *filingHarness) stallOtherRepository() (filingRun, *stallingTokens) {
	h.t.Helper()
	if err := h.store.Write(context.Background(), func(tx *store.WriteTx) error {
		return tx.RegisterProject(context.Background(), stalledProject)
	}); err != nil {
		h.t.Fatalf("register project: %v", err)
	}
	run := seedFilingRun(h.t, h.store, stalledProject, "run-stalled", map[string]string{
		domain.PolicyFollowUpFilingLabels:    "lane:spine,deferral",
		domain.PolicyFollowUpFilingMilestone: "1B",
	}, nil)
	stall := &stallingTokens{inner: h.tokens, repo: stalledProject.Repo, reached: make(chan struct{}), release: make(chan struct{})}
	h.tokens = stall
	return run, stall
}

// whileStalled runs pass in the background, waits until the stalled
// repository's token request is in flight, and then until done reports true.
// It releases the stall and waits for pass before returning.
func whileStalled(t *testing.T, stall *stallingTokens, pass func() error, done func() bool) {
	t.Helper()
	passed := make(chan struct{})
	go func() {
		defer close(passed)
		_ = pass()
	}()
	defer func() {
		close(stall.release)
		<-passed
	}()
	select {
	case <-stall.reached:
	case <-passed:
		t.Fatal("the pass never asked for the stalled repository's token")
	}
	for deadline := time.Now().Add(10 * time.Second); !done(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the served repository waited on the stalled repository's token request")
		}
	}
}

// TestFollowUpFilerServesOtherRepositoryWhileOneStalls: while one
// repository's GitHub calls hang, the pass still files another repository's
// approved filing (#3215).
func TestFollowUpFilerServesOtherRepositoryWhileOneStalls(t *testing.T) {
	t.Parallel()
	h := newFilingHarness(t)
	stalledRun, stall := h.stallOtherRepository()
	h.approve(stalledRun, 0)
	approved := h.approve(h.seedRun("run-served", nil, nil), 1)
	whileStalled(t, stall, func() error { return h.filer.Pass(context.Background()) },
		func() bool { return h.filed(approved.ID) != nil })
}

// TestIssueCommentDispatcherServesOtherRepositoryWhileOneStalls: while one
// repository's GitHub calls hang, the pass still posts another repository's
// approved comment (#3215).
func TestIssueCommentDispatcherServesOtherRepositoryWhileOneStalls(t *testing.T) {
	t.Parallel()
	h := newFilingHarness(t)
	h.trustComments()
	stalledRun, stall := h.stallOtherRepository()
	h.proposeComment(stalledRun, 0, 41, domain.ActionApprove)
	approved := h.proposeComment(h.seedRun("run-served", nil, nil), 1, 41, domain.ActionApprove)
	whileStalled(t, stall, func() error { return h.comments.Pass(context.Background()) },
		func() bool { return h.posted(approved.ID) != nil })
}
