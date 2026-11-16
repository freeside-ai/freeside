package daemoncompose

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const readinessProject = domain.ProjectID("project-widgets")

var (
	readinessReviewDigest = domain.Digest("sha256:" + strings.Repeat("d", 64))
	// readinessRecipe is the recipe selectorImage builds from.
	readinessRecipe = domain.Digest("sha256:" + strings.Repeat("1", 64))
)

// fakeBotIdentity resolves the App bot, or fails with err.
type fakeBotIdentity struct{ err error }

func (f *fakeBotIdentity) Resolve(context.Context, string) (publish.AppBotIdentity, error) {
	return publish.AppBotIdentity{}, f.err
}

// readinessFixture is an evaluator over a store holding one image that
// serves selectorBase, with a GitHub whose answers each test sets.
type readinessFixture struct {
	t         *testing.T
	store     *store.Store
	attention *signet.Service
	now       time.Time
	tipErr    error
	fetchID   int64
	fetches   int
	bot       *fakeBotIdentity
	base      *baseInputsMemo
}

func newReadinessFixture(t *testing.T) *readinessFixture {
	t.Helper()
	image := selectorImage(t, selectorBase, "a", "e")
	f := &readinessFixture{
		t: t, store: selectorStore(t, image),
		now:     time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC),
		fetchID: selectorRepositoryID, bot: &fakeBotIdentity{},
		base: selectorBaseInputs("a", nil),
	}
	f.attention = signet.NewService(f.store)
	f.trust(readinessReviewDigest, selectorRepositoryID)
	return f
}

// trust activates a new trust profile revision; patterns only vary its
// digest so each call records a new revision.
func (f *readinessFixture) trust(review domain.Digest, repositoryID int64, patterns ...string) {
	f.t.Helper()
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: selectorRepo, RepositoryID: repositoryID,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review:                     domain.ReviewSettings{Mode: domain.ReviewFreesideInvoked, ConfigDigest: review},
		ProtectedPaths:             domain.ProtectedPathConfig{ExtraVerificationControlPatterns: patterns},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := f.store.WriteInternal(f.t.Context(), func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(f.t.Context(), profile, f.now)
	}); err != nil {
		f.t.Fatal(err)
	}
}

func (f *readinessFixture) evaluator(settings *domain.SettingsRevisions) *projectReadinessEvaluator {
	return &projectReadinessEvaluator{
		store: f.store, project: readinessProject, settings: settings,
		repo: selectorRepo, repositoryID: selectorRepositoryID, baseRef: "main",
		agentImage:     selectorImage(f.t, selectorBase, "a", "e").ImageRef,
		approvedRecipe: readinessRecipe, reviewConfigurationDigest: readinessReviewDigest,
		github: projectReadinessGitHub{
			observeTip: func(context.Context, string, string) (string, error) {
				if f.tipErr != nil {
					return "", f.tipErr
				}
				return selectorBase, nil
			},
			fetchTip: func(context.Context, string, string) (int64, error) {
				f.fetches++
				return f.fetchID, nil
			},
			appBotIdentity: f.bot,
			baseInputs:     f.base.read,
		},
		now: func() time.Time { return f.now },
	}
}

func (f *readinessFixture) doctor() operations.Doctor {
	return operations.Doctor{
		Store: f.store, Attention: f.attention, ProjectID: "project-system",
		Now: func() time.Time { return f.now },
	}
}

// evaluate runs a full evaluation, as the doctor pass does.
func (f *readinessFixture) evaluate(e *projectReadinessEvaluator) domain.ProjectReadiness {
	f.t.Helper()
	observation, err := e.observe(f.t.Context())
	if err != nil {
		f.t.Fatal(err)
	}
	readiness, err := e.commit(f.t.Context(), observation, f.doctor())
	if err != nil {
		f.t.Fatal(err)
	}
	return readiness
}

func (f *readinessFixture) refresh(e *projectReadinessEvaluator) {
	f.t.Helper()
	if err := e.refresh(f.t.Context(), f.doctor()); err != nil {
		f.t.Fatal(err)
	}
}

func (f *readinessFixture) stored(settings *domain.SettingsRevisions) domain.ProjectReadiness {
	f.t.Helper()
	var readiness domain.ProjectReadiness
	if err := f.store.Read(f.t.Context(), func(tx *store.ReadTx) error {
		var err error
		readiness, err = tx.ProjectReadiness(f.t.Context(), readinessProject, settings)
		return err
	}); err != nil {
		f.t.Fatal(err)
	}
	return readiness
}

// openCards returns the open project_not_ready cards.
func (f *readinessFixture) openCards() []domain.AttentionItem {
	f.t.Helper()
	snapshots, err := f.attention.ListAttentionItems(f.t.Context())
	if err != nil {
		f.t.Fatal(err)
	}
	var cards []domain.AttentionItem
	for _, snapshot := range snapshots {
		item := snapshot.Item
		if item.Status == domain.StatusOpen && item.HealthDiagnostic != nil &&
			item.HealthDiagnostic.Code == operations.ProjectNotReadyCode {
			cards = append(cards, item)
		}
	}
	return cards
}

// operatingGate is the store's operating-state predicate, which holds every
// unattended admission under blocking_system_health while a card is open.
func (f *readinessFixture) operatingGate() error {
	f.t.Helper()
	return f.store.Read(f.t.Context(), func(tx *store.ReadTx) error {
		return tx.RequireUnattendedOperationOpen(f.t.Context())
	})
}

func status(t *testing.T, readiness domain.ProjectReadiness, check domain.ProjectReadinessCheck) domain.ProjectReadinessResult {
	t.Helper()
	result, ok := readiness.Result(check)
	if !ok {
		t.Fatalf("record has no %s result: %+v", check, readiness)
	}
	return result
}

// TestProjectReadinessBreakAndRepair: a broken trust profile, seen by the
// pre-dispatch refresh, opens one Blocking card naming trust_profile and
// closes the operating gate; repairing it concludes the card and opens the
// gate.
func TestProjectReadinessBreakAndRepair(t *testing.T) {
	t.Parallel()
	f := newReadinessFixture(t)
	e := f.evaluator(nil)
	if readiness := f.evaluate(e); !readiness.Ready() {
		t.Fatalf("fresh project not ready: %+v", readiness.NotPassed())
	}
	if cards := f.openCards(); len(cards) != 0 {
		t.Fatalf("ready project has cards %+v", cards)
	}
	f.trust(readinessReviewDigest, selectorRepositoryID+1, "broken/**")
	f.refresh(e)
	cards := f.openCards()
	if len(cards) != 1 {
		t.Fatalf("broken profile: %d open cards, want 1", len(cards))
	}
	card := cards[0]
	if card.ProjectID != readinessProject || card.Posture == nil || *card.Posture != domain.HealthPostureBlocking ||
		!strings.Contains(card.Reason, string(readinessProject)) ||
		!strings.Contains(card.Reason, string(domain.ProjectReadinessTrustProfile)) {
		t.Fatalf("card = %+v", card)
	}
	if err := f.operatingGate(); !errors.Is(err, domain.ErrBlockingSystemHealth) {
		t.Fatalf("operating gate with the card open = %v, want ErrBlockingSystemHealth", err)
	}
	f.trust(readinessReviewDigest, selectorRepositoryID, "repaired/**")
	f.refresh(e)
	if cards := f.openCards(); len(cards) != 0 {
		t.Fatalf("repaired profile left cards %+v", cards)
	}
	if err := f.operatingGate(); err != nil {
		t.Fatalf("operating gate after repair = %v", err)
	}
}

// TestProjectReadinessReviewConfigurationDrift: a profile approving another
// review configuration fails review_configuration only.
func TestProjectReadinessReviewConfigurationDrift(t *testing.T) {
	t.Parallel()
	f := newReadinessFixture(t)
	f.trust(domain.Digest("sha256:"+strings.Repeat("9", 64)), selectorRepositoryID, "drift/**")
	readiness := f.evaluate(f.evaluator(nil))
	if got := status(t, readiness, domain.ProjectReadinessTrustProfile); got.Status != domain.ProjectReadinessPassed {
		t.Fatalf("trust_profile = %+v", got)
	}
	if got := status(t, readiness, domain.ProjectReadinessReviewConfiguration); got.Status != domain.ProjectReadinessFailed ||
		!strings.Contains(got.Detail, string(readinessReviewDigest)) {
		t.Fatalf("review_configuration = %+v", got)
	}
}

// TestProjectReadinessOpensOneCard: evaluating a project that stays not
// ready opens no second card.
func TestProjectReadinessOpensOneCard(t *testing.T) {
	t.Parallel()
	f := newReadinessFixture(t)
	f.bot.err = errors.New("multiple GitHub Apps have authority")
	e := f.evaluator(nil)
	f.evaluate(e)
	f.now = f.now.Add(time.Hour)
	f.evaluate(e)
	f.refresh(e)
	if cards := f.openCards(); len(cards) != 1 ||
		!strings.Contains(cards[0].Reason, string(domain.ProjectReadinessPublicationAuthority)) {
		t.Fatalf("open cards = %+v, want one naming publication_authority", cards)
	}
}

// TestProjectReadinessCardConcludedByAnotherComposition: a readiness card
// is kept by a daemon evaluating its project and concluded by one that
// evaluates another project or none, so a settings change cannot leave a
// card holding unattended admission with nothing left to converge it.
func TestProjectReadinessCardConcludedByAnotherComposition(t *testing.T) {
	t.Parallel()
	f := newReadinessFixture(t)
	f.bot.err = errors.New("multiple GitHub Apps have authority")
	f.evaluate(f.evaluator(nil))
	if err := f.doctor().ConcludeProjectReadinessExcept(t.Context(), readinessProject); err != nil {
		t.Fatal(err)
	}
	if len(f.openCards()) != 1 || f.operatingGate() == nil {
		t.Fatal("the daemon evaluating the project concluded its card")
	}
	if err := f.doctor().ConcludeProjectReadinessExcept(t.Context(), "project-other"); err != nil {
		t.Fatal(err)
	}
	if cards := f.openCards(); len(cards) != 0 {
		t.Fatalf("open cards = %+v, want the other project's card concluded", cards)
	}
	if err := f.operatingGate(); err != nil {
		t.Fatalf("operating gate = %v, want open", err)
	}
}

// TestProjectReadinessNewRevisionsStartNotReady: revisions no evaluation has
// recorded read as not ready, and an evaluation under them leaves the
// record of other revisions alone.
func TestProjectReadinessNewRevisionsStartNotReady(t *testing.T) {
	t.Parallel()
	f := newReadinessFixture(t)
	f.evaluate(f.evaluator(nil))
	revisions := &domain.SettingsRevisions{Instance: 2, Project: 1}
	e := f.evaluator(revisions)
	f.refresh(e)
	readiness := f.stored(revisions)
	if readiness.Ready() {
		t.Fatalf("unevaluated revisions read as ready: %+v", readiness)
	}
	for _, result := range readiness.NotPassed() {
		if result.Check.StoreLocal() || result.Status != domain.ProjectReadinessUnavailable {
			t.Fatalf("unevaluated result = %+v", result)
		}
	}
	if len(f.openCards()) != 1 {
		t.Fatal("unevaluated revisions opened no card")
	}
	if readiness := f.evaluate(e); !readiness.Ready() {
		t.Fatalf("evaluated revisions not ready: %+v", readiness.NotPassed())
	}
	if len(f.openCards()) != 0 {
		t.Fatal("evaluated revisions left the card open")
	}
	if !f.stored(nil).Ready() {
		t.Fatal("the file-settings record changed")
	}
}

// TestProjectReadinessUnreachableGitHub: a GitHub that gives no answer keeps
// an earlier pass with its time, and without one records unavailable, which
// is not ready; a GitHub that answers no fails the check.
func TestProjectReadinessUnreachableGitHub(t *testing.T) {
	t.Parallel()
	unreachable := &url.Error{Op: "Get", URL: "https://api.github.com/repos/owner/repo", Err: errors.New("connection refused")}

	f := newReadinessFixture(t)
	e := f.evaluator(nil)
	first := f.evaluate(e)
	checkedAt := status(t, first, domain.ProjectReadinessRepositoryBase).CheckedAt
	f.now = f.now.Add(time.Hour)
	f.tipErr = unreachable
	kept := f.evaluate(e)
	if got := status(t, kept, domain.ProjectReadinessRepositoryBase); got.Status != domain.ProjectReadinessPassed ||
		!got.CheckedAt.Equal(checkedAt) {
		t.Fatalf("unreachable GitHub after a pass: repository_base = %+v, want the pass from %v", got, checkedAt)
	}
	if !kept.Ready() {
		t.Fatalf("a kept pass left the project not ready: %+v", kept.NotPassed())
	}

	fresh := newReadinessFixture(t)
	fresh.tipErr = unreachable
	readiness := fresh.evaluate(fresh.evaluator(nil))
	if got := status(t, readiness, domain.ProjectReadinessRepositoryBase); got.Status != domain.ProjectReadinessUnavailable {
		t.Fatalf("unreachable GitHub with no earlier pass: repository_base = %+v", got)
	}
	if readiness.Ready() || len(fresh.openCards()) != 1 {
		t.Fatal("an unavailable check let the project read as ready")
	}

	answered := newReadinessFixture(t)
	answered.evaluate(answered.evaluator(nil))
	answered.tipErr = &publish.APIError{Status: 404, RequestPath: "/repos/owner/repo/git/ref/heads/main"}
	readiness = answered.evaluate(answered.evaluator(nil))
	if got := status(t, readiness, domain.ProjectReadinessRepositoryBase); got.Status != domain.ProjectReadinessFailed {
		t.Fatalf("GitHub answering 404: repository_base = %+v, want failed despite the earlier pass", got)
	}
}

// TestProjectReadinessRepositoryIdentity: a base whose fetch carries another
// repository id fails, and a verified tip is not fetched again.
func TestProjectReadinessRepositoryIdentity(t *testing.T) {
	t.Parallel()
	f := newReadinessFixture(t)
	e := f.evaluator(nil)
	f.evaluate(e)
	f.evaluate(e)
	if f.fetches != 1 {
		t.Fatalf("a verified tip was fetched %d times, want 1", f.fetches)
	}
	other := newReadinessFixture(t)
	other.fetchID = selectorRepositoryID + 1
	readiness := other.evaluate(other.evaluator(nil))
	if got := status(t, readiness, domain.ProjectReadinessRepositoryBase); got.Status != domain.ProjectReadinessFailed {
		t.Fatalf("foreign repository id: repository_base = %+v", got)
	}
}

// TestProjectReadinessWriterImage: writer_image fails on a recipe other than
// the approved one or a preparation that does not resolve, is unavailable on
// a base it cannot read, and passes when no image serves the base.
func TestProjectReadinessWriterImage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		setup func(*readinessFixture, *projectReadinessEvaluator)
		want  domain.ProjectReadinessStatus
	}{
		{"serving image", func(*readinessFixture, *projectReadinessEvaluator) {}, domain.ProjectReadinessPassed},
		{"no image serves the base", func(f *readinessFixture, e *projectReadinessEvaluator) {
			e.github.baseInputs = selectorBaseInputs("z", nil).read
		}, domain.ProjectReadinessPassed},
		{"another recipe", func(_ *readinessFixture, e *projectReadinessEvaluator) {
			e.approvedRecipe = domain.Digest("sha256:" + strings.Repeat("2", 64))
		}, domain.ProjectReadinessFailed},
		{"unrecorded agent image", func(_ *readinessFixture, e *projectReadinessEvaluator) {
			e.agentImage = selectorImage(t, selectorBase, "a", "f").ImageRef
		}, domain.ProjectReadinessFailed},
		{"unreadable base", func(f *readinessFixture, e *projectReadinessEvaluator) {
			// The image was built at selectorBase, so only another tip makes
			// selection read the base.
			e.github.observeTip = func(context.Context, string, string) (string, error) {
				return strings.Repeat("c", 40), nil
			}
			e.github.baseInputs = selectorBaseInputs("a", errors.New("fetch exact base: unreachable")).read
		}, domain.ProjectReadinessUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReadinessFixture(t)
			e := f.evaluator(nil)
			tc.setup(f, e)
			got := status(t, f.evaluate(e), domain.ProjectReadinessWriterImage)
			if got.Status != tc.want {
				t.Fatalf("writer_image = %+v, want %s", got, tc.want)
			}
		})
	}
}

// TestProjectReadinessRetriesRemote: a dispatch pass re-observes a
// GitHub-side check that did not pass, unavailable or failed, at most once
// per interval, so a repair concludes the card without the daily doctor
// pass; it does not re-observe a record whose GitHub-side checks passed.
func TestProjectReadinessRetriesRemote(t *testing.T) {
	t.Parallel()
	for name, tipErr := range map[string]error{
		"unavailable": &url.Error{Op: "Get", URL: "https://api.github.com", Err: errors.New("timeout")},
		"failed":      &publish.APIError{Status: 404, RequestPath: "/repos/owner/repo/git/ref/heads/main"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newReadinessFixture(t)
			f.tipErr = tipErr
			e := f.evaluator(nil)
			if f.evaluate(e).Ready() {
				t.Fatal("a broken tip read evaluated ready")
			}
			if _, retried, err := e.retryRemote(t.Context()); err != nil || retried {
				t.Fatalf("retry inside the interval: retried %v, err %v", retried, err)
			}
			f.tipErr = nil
			f.now = f.now.Add(projectReadinessRetryInterval)
			observation, retried, err := e.retryRemote(t.Context())
			if err != nil || !retried {
				t.Fatalf("retry after the interval: retried %v, err %v", retried, err)
			}
			if readiness, err := e.commit(t.Context(), observation, f.doctor()); err != nil || !readiness.Ready() {
				t.Fatalf("retried readiness = %+v, err %v", readiness.NotPassed(), err)
			}
			if len(f.openCards()) != 0 {
				t.Fatal("a ready retry left the card open")
			}
			f.now = f.now.Add(projectReadinessRetryInterval)
			if _, retried, err := e.retryRemote(t.Context()); err != nil || retried {
				t.Fatalf("retry of a record that passed: retried %v, err %v", retried, err)
			}
		})
	}
}
