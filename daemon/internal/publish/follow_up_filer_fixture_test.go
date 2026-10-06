package publish_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

const (
	filingBotID   int64 = 4242
	filingBotSlug       = "freeside-test"
	filingRepo          = "freeside-ai/evidence-repo"
)

var filingAt = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// fakeFiledIssue is one issue the fake serves on the creator listing.
type fakeFiledIssue struct {
	Number    int
	Login     string
	UserID    int64
	UserType  string
	CreatedAt time.Time
	// UpdatedAt is what the since filter compares; zero means CreatedAt.
	UpdatedAt time.Time
	IsPR      bool

	Title     string
	Body      string
	Labels    []string
	Milestone *int
}

// fakeCreateResponse overrides one create response. The zero value is a
// plain 201.
type fakeCreateResponse struct {
	Status int
	Header map[string]string
	// Commit creates the issue whatever the response says: a create that
	// committed and whose response was lost or mangled.
	Commit bool
	// Body, when non-empty, replaces the 201 body.
	Body string
	// Drop fails the request at the transport after the handler ran.
	Drop bool
}

// fakeFiling is the fake's follow-up filing state.
type fakeFiling struct {
	issues     []fakeFiledIssue
	milestones map[string]int
	// responses are consumed one per create; none left answers a plain 201.
	responses []fakeCreateResponse
	// listFailures makes the next n creator listings answer 500.
	listFailures int
	pageSize     int
	now          func() time.Time
	creates      int
	dropNext     bool
}

func (f *fakeFiling) clock() time.Time {
	if f.now == nil {
		return time.Now().UTC()
	}
	return f.now().UTC().Truncate(time.Second)
}

func (f *fakeFiling) create(t *testing.T, w http.ResponseWriter, r *http.Request) {
	var request struct {
		Title     string   `json:"title"`
		Body      string   `json:"body"`
		Labels    []string `json:"labels"`
		Milestone *int     `json:"milestone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		t.Errorf("decode create issue request: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	f.creates++
	response := fakeCreateResponse{}
	if len(f.responses) > 0 {
		response, f.responses = f.responses[0], f.responses[1:]
	}
	issue := fakeFiledIssue{
		Number: 500 + len(f.issues), Login: filingBotSlug + "[bot]", UserID: filingBotID, UserType: "Bot",
		CreatedAt: f.clock(), Title: request.Title, Body: request.Body,
		Labels: request.Labels, Milestone: request.Milestone,
	}
	if response.Status == 0 || response.Commit {
		f.issues = append(f.issues, issue)
	}
	f.dropNext = response.Drop
	for key, value := range response.Header {
		w.Header().Set(key, value)
	}
	if response.Status != 0 {
		w.WriteHeader(response.Status)
		_, _ = w.Write([]byte(response.Body))
		return
	}
	w.WriteHeader(http.StatusCreated)
	if response.Body != "" {
		_, _ = w.Write([]byte(response.Body))
		return
	}
	_ = json.NewEncoder(w).Encode(filedIssueJSON(issue))
}

func filedIssueJSON(issue fakeFiledIssue) map[string]any {
	out := map[string]any{
		"number": issue.Number, "title": issue.Title, "body": issue.Body,
		"user":       map[string]any{"id": issue.UserID, "type": issue.UserType, "login": issue.Login},
		"created_at": issue.CreatedAt.Format(time.RFC3339),
	}
	if issue.IsPR {
		out["pull_request"] = map[string]any{"url": "https://api.github.test/pulls/" + strconv.Itoa(issue.Number)}
	}
	return out
}

func (f *fakeFiling) list(w http.ResponseWriter, r *http.Request) {
	if f.listFailures > 0 {
		f.listFailures--
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	query := r.URL.Query()
	since, err := time.Parse(time.RFC3339, query.Get("since"))
	if err != nil || query.Get("state") != "all" {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	rows := []map[string]any{}
	for _, issue := range f.issues {
		updated := issue.UpdatedAt
		if updated.IsZero() {
			updated = issue.CreatedAt
		}
		if issue.Login == query.Get("creator") && !updated.Before(since) {
			rows = append(rows, filedIssueJSON(issue))
		}
	}
	if f.pageSize > 0 {
		page, err := strconv.Atoi(query.Get("page"))
		if err != nil || page < 1 {
			page = 1
		}
		start := min((page-1)*f.pageSize, len(rows))
		end := min(start+f.pageSize, len(rows))
		if end < len(rows) {
			query.Set("page", strconv.Itoa(page+1))
			w.Header().Set("Link", fmt.Sprintf(`<http://%s%s?%s>; rel="next"`, r.Host, r.URL.Path, query.Encode()))
		}
		rows = rows[start:end]
	}
	_ = json.NewEncoder(w).Encode(rows)
}

func (f *fakeFiling) listMilestones(w http.ResponseWriter) {
	rows := []map[string]any{}
	for title, number := range f.milestones {
		rows = append(rows, map[string]any{"number": number, "title": title})
	}
	_ = json.NewEncoder(w).Encode(rows)
}

// fakeTransport serves requests from the fake in process. A real listener
// would park its connection goroutines in network reads, which a synctest
// bubble never counts as idle; in-process calls let the same harness run
// inside and outside a bubble. A request whose context ended before the send
// never reaches the fake, and one whose context ended during it loses its
// response, as a killed process would.
type fakeTransport struct{ h *filingHarness }

func (t fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.h.stepped("send " + req.Method + " " + req.URL.Path)
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	recorder := httptest.NewRecorder()
	t.h.gh.handle(recorder, req)
	t.h.gh.mu.Lock()
	drop := t.h.gh.filing.dropNext
	t.h.gh.filing.dropNext = false
	t.h.gh.mu.Unlock()
	if drop {
		return nil, errors.New("connection reset by peer")
	}
	t.h.stepped("receive " + req.Method + " " + req.URL.Path)
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	return recorder.Result(), nil
}

// steppedTokens reports each token request as a step before answering it.
type steppedTokens struct{ h *filingHarness }

func (s steppedTokens) Token(ctx context.Context, repo string) (publish.InstallationToken, error) {
	s.h.stepped("token")
	if err := ctx.Err(); err != nil {
		return publish.InstallationToken{}, err
	}
	return s.h.tokens.Token(ctx, repo)
}

type fixedBotIdentity struct {
	identity publish.AppBotIdentity
	err      error
}

func (s fixedBotIdentity) Resolve(context.Context, string) (publish.AppBotIdentity, error) {
	return s.identity, s.err
}

// errTokenSource fails every token request with a fixed error.
type errTokenSource struct{ err error }

func (s errTokenSource) Token(context.Context, string) (publish.InstallationToken, error) {
	return publish.InstallationToken{}, s.err
}

// filingHarness is one store, one fake forge, and a filer over both, with a
// clock the test moves.
type filingHarness struct {
	t      *testing.T
	path   string
	store  *store.Store
	gh     *fakeGitHub
	now    time.Time
	tokens publish.TokenSource
	filer  *publish.FollowUpFiler
	// step, when set, sees every point at which the filer reaches outside the
	// store: each token request and each side of each forge request.
	step func(name string)
}

func (h *filingHarness) stepped(name string) {
	if h.step != nil {
		h.step(name)
	}
}

func newFilingHarness(t *testing.T) *filingHarness {
	t.Helper()
	h := &filingHarness{
		t: t, path: filepath.Join(t.TempDir(), "store.db"), gh: newFakeGitHub(t),
		now: filingAt.Add(24 * time.Hour), tokens: testTokenSource(),
	}
	h.gh.filing.milestones = map[string]int{"1B": 7}
	h.gh.filing.now = func() time.Time { return h.now }
	h.open()
	if err := h.store.Write(context.Background(), func(tx *store.WriteTx) error {
		return tx.RegisterProject(context.Background(), domain.Project{
			ID: "project-filing", Repo: filingRepo, RepositoryID: testRepoID,
		})
	}); err != nil {
		t.Fatalf("register project: %v", err)
	}
	return h
}

// open opens the store at the harness path and builds a fresh filer over it:
// the daemon after a restart, with nothing carried over in memory.
func (h *filingHarness) open() {
	h.t.Helper()
	s := storetest.Open(h.t, h.path, store.Options{})
	h.store = s
	var err error
	h.filer, err = publish.NewFollowUpFiler(
		s, steppedTokens{h}, &http.Client{Transport: fakeTransport{h}}, "http://github.test",
		fixedBotIdentity{identity: publish.AppBotIdentity{AppSlug: filingBotSlug, BotUserID: filingBotID}},
		func() time.Time { return h.now })
	if err != nil {
		h.t.Fatalf("NewFollowUpFiler: %v", err)
	}
}

func (h *filingHarness) restart() {
	h.t.Helper()
	if err := h.store.Close(); err != nil {
		h.t.Fatalf("store.Close: %v", err)
	}
	h.open()
}

// pass runs one pass. No pass error may carry the installation token.
func (h *filingHarness) pass() error {
	h.t.Helper()
	err := h.filer.Pass(context.Background())
	if err != nil && strings.Contains(err.Error(), fixtureTokenValue) {
		h.t.Fatalf("pass error carries the installation token: %v", err)
	}
	return err
}

func (h *filingHarness) mustPass() {
	h.t.Helper()
	if err := h.pass(); err != nil {
		h.t.Fatalf("Pass: %v", err)
	}
}

// filingRun is one seeded run whose review round defers several findings, so
// a test can approve several filings from it.
type filingRun struct {
	id           domain.RunID
	policy       domain.ResolvedPolicy
	target       domain.FollowUpFilingTarget
	adjudication domain.FindingAdjudication
	findings     []domain.Finding
}

const filingFindingsPerRun = 6

func filingDigest(fill string) domain.Digest {
	return domain.Digest("sha256:" + strings.Repeat(fill, 64))
}

// seedRun registers a run in the filing project with the given extra policy
// keys and, when boundIssue is set, a declaration bound to that issue.
func (h *filingHarness) seedRun(name string, extra map[string]string, boundIssue *int) filingRun {
	h.t.Helper()
	ctx := context.Background()
	runID := domain.RunID(name)
	provenance := domain.KeyProvenance{Source: domain.ProvenanceOverride, Digest: filingDigest("a")}
	keys := []domain.PolicyKey{
		{Key: "paths", Value: "daemon/", Provenance: provenance},
		{Key: domain.PolicyFollowUpFilingLabels, Value: "lane:spine,deferral", Provenance: provenance},
		{Key: domain.PolicyFollowUpFilingMilestone, Value: "1B", Provenance: provenance},
	}
	for key, value := range extra {
		keys = append(keys, domain.PolicyKey{Key: key, Value: value, Provenance: provenance})
	}
	policy, err := domain.NewResolvedPolicy(runID, keys)
	if err != nil {
		h.t.Fatal(err)
	}
	project := domain.Project{ID: "project-filing", Repo: filingRepo, RepositoryID: testRepoID}
	target, err := domain.DeriveFollowUpFilingTarget(project, policy)
	if err != nil {
		h.t.Fatal(err)
	}
	var findings []domain.Finding
	var ids []domain.FindingID
	var entries []domain.FindingAdjudicationEntry
	for i := range filingFindingsPerRun {
		id := domain.FindingID(fmt.Sprintf("finding-%s-%d", name, i))
		findings = append(findings, domain.Finding{
			ID: id, RunID: runID, Source: "codex_local",
			Location: &domain.FindingLocation{Path: fmt.Sprintf("daemon/%d.go", i), StartLine: 1, EndLine: 1},
			Message:  "finding " + string(id), RawText: "finding " + string(id), CreatedAt: filingAt,
		})
		ids = append(ids, id)
		entry, err := domain.NewModelAdjudicationEntry(
			id, domain.GoalAdjacent, nil, domain.RouteDefer, domain.ConfidenceHigh,
			"adjacent to the goal", nil, nil, nil, nil, nil)
		if err != nil {
			h.t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	review, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: domain.InvocationID("review-" + name), RunID: runID, Round: 1,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: filingDigest("c"), InstructionDigest: filingDigest("d"),
		CostOwner: "owner", BaseSHA: "base", HeadSHA: "head-1", CompletedAt: filingAt,
		CompletionEvidence: filingDigest("e"), Outcome: domain.ReviewFindings, FindingIDs: ids,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	adjudication, err := domain.NewFindingAdjudication(
		runID, 1, filingDigest("f"), review.InstructionDigest, policy.Digest, entries, "", filingAt.Add(time.Minute))
	if err != nil {
		h.t.Fatal(err)
	}
	if err := h.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: project.ID, SpecDigest: filingDigest("f"),
			PolicyDigest: policy.Digest, Stages: []domain.Stage{},
		}); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		input := domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundPRMerged,
			DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
		}
		if boundIssue != nil {
			input.CompletionCriterion = domain.CompletionBoundIssueClosedByMergedPR
			input.BoundIssue = boundIssue
		}
		declaration, err := domain.NewWorkUnitDeclaration(input, runID, project.ID, filingAt.Add(-time.Hour))
		if err != nil {
			return err
		}
		if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, review, findings); err != nil {
			return err
		}
		if err := tx.PutFindingAdjudication(ctx, adjudication); err != nil {
			return err
		}
		for _, finding := range findings {
			if err := tx.PutFindingDisposition(ctx, domain.ReviewDispositionRecord{
				FindingID: finding.ID, RunID: runID, Round: 1,
				Disposition: domain.ReviewDispositionDeferred, Reason: "tracked as a follow-up",
				AdjudicationDigest: adjudication.Digest, CreatedAt: filingAt.Add(2 * time.Minute),
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		h.t.Fatalf("seed run %q: %v", name, err)
	}
	return filingRun{id: runID, policy: policy, target: target, adjudication: adjudication, findings: findings}
}

func filingTitle(run filingRun, finding int) string {
	return fmt.Sprintf("Bound the retry budget (%s %d)", run.id, finding)
}

const filingBody = "The sync client retries without a budget.\n\nDeferred from review round 1."

// proposal builds the filing proposal for one of the run's findings. The
// body's screening verdict is recorded as passed whatever the text, as a
// forged row would claim; the store's gate re-screens it.
func (h *filingHarness) proposal(run filingRun, finding int, body string) domain.EffectProposal {
	h.t.Helper()
	passed := func(text string) domain.ScreenedIssueText {
		return domain.ScreenedIssueText{
			Text: text, Ruleset: domain.IssueTextRulesetGitHubIssue1, Verdict: domain.ScreeningVerdictPassed,
		}
	}
	proposal, err := domain.NewEffectProposal(domain.EffectFollowUpFiling, domain.FollowUpFilingInput{
		SubjectHandle: domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(run.id)),
		Target:        run.target,
		Source: domain.FollowUpFilingSource{
			FindingID: run.findings[finding].ID, AdjudicationDigest: run.adjudication.Digest,
			Kind: domain.FollowUpSourceDeferredDisposition,
		},
		Title: passed(filingTitle(run, finding)),
		Body:  passed(body),
	}, run.policy)
	if err != nil {
		h.t.Fatal(err)
	}
	return proposal
}

// rewriteBody replaces the stored instance's body text behind the store's
// back, keeping the row's digests consistent so only the screen can refuse.
func (h *filingHarness) rewriteBody(instance domain.ProposalInstance, run filingRun, finding int, body string) {
	h.t.Helper()
	forged := instance
	forged.Proposal = h.proposal(run, finding, body)
	row, err := json.Marshal(forged)
	if err != nil {
		h.t.Fatal(err)
	}
	h.tamper(`UPDATE effect_proposal_instances SET content_digest = ?, body = ? WHERE instance_id = ?`,
		string(forged.Proposal.Digest), string(row), string(instance.ID))
}

// propose admits a filing for one of the run's findings and opens its card,
// as the producer does. A decision of "" leaves the card open; approve or
// decline records the decision and concludes the card the way the attention
// service does.
func (h *filingHarness) propose(run filingRun, finding int, decision domain.Action) domain.ProposalInstance {
	h.t.Helper()
	ctx := context.Background()
	proposal := h.proposal(run, finding, filingBody)
	created := filingAt.Add(time.Hour + time.Duration(finding)*time.Minute)
	var instance domain.ProposalInstance
	if err := h.store.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		instance, _, err = tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{
				Source:          domain.ProposalSourceUpstreamEvent,
				UpstreamEventID: fmt.Sprintf("filing-%s-%d", run.id, finding),
			},
			domain.ProposalBatchID(fmt.Sprintf("batch-%s-%d", run.id, finding)), proposal, created)
		if err != nil {
			return err
		}
		item, err := signet.OpenFollowUpFilingItem(ctx, tx, instance.ID, created)
		if err != nil || decision == "" {
			return err
		}
		command, err := domain.NewCommand(domain.CommandInput{
			CommandID: "command-" + string(instance.ID), DeviceID: "device-1",
			ItemID: item.ID, ItemVersion: item.ItemVersion, ArtifactDigests: item.ArtifactDigests, Action: decision,
		})
		if err != nil {
			return err
		}
		if err := tx.PutCommand(ctx, command); err != nil {
			return err
		}
		status, selected := domain.StatusDismissed, (*domain.Digest)(nil)
		if decision == domain.ActionApprove {
			status, selected = domain.StatusResolved, &instance.Proposal.Digest
		}
		decidedAt := created.Add(time.Hour)
		if err := tx.RecordProposalDecision(ctx, instance.ID, command.CommandID, decision, selected, decidedAt); err != nil {
			return err
		}
		item.ItemVersion++
		item.Status = status
		item, err = item.WithDecidedAt(decidedAt)
		if err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		h.t.Fatalf("propose filing %s/%d: %v", run.id, finding, err)
	}
	return instance
}

func (h *filingHarness) approve(run filingRun, finding int) domain.ProposalInstance {
	h.t.Helper()
	return h.propose(run, finding, domain.ActionApprove)
}

// intent reads the filing's intent, or nil when none is open.
func (h *filingHarness) intent(id domain.ProposalInstanceID) *domain.FollowUpFilingIntent {
	h.t.Helper()
	var intent *domain.FollowUpFilingIntent
	if err := h.store.Read(context.Background(), func(tx *store.ReadTx) error {
		got, err := tx.GetFollowUpFilingIntent(context.Background(), id)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		intent = &got
		return err
	}); err != nil {
		h.t.Fatalf("read intent %q: %v", id, err)
	}
	return intent
}

// filed reads the ledger row for the filing, or nil.
func (h *filingHarness) filed(id domain.ProposalInstanceID) *domain.FiledFollowUpIssue {
	h.t.Helper()
	var filed *domain.FiledFollowUpIssue
	if err := h.store.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		filed, err = tx.FiledFollowUpIssueForInstance(context.Background(), id)
		return err
	}); err != nil {
		h.t.Fatalf("read ledger row %q: %v", id, err)
	}
	return filed
}

// healthItem reads the filer's notice for the filing under code, or nil.
func (h *filingHarness) healthItem(id domain.ProposalInstanceID, code string) *domain.AttentionItem {
	h.t.Helper()
	var item *domain.AttentionItem
	itemID := domain.ItemID("follow-up-filing/" + string(id) + "/" + code)
	if err := h.store.Read(context.Background(), func(tx *store.ReadTx) error {
		got, err := tx.GetAttentionItem(context.Background(), itemID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		item = &got
		return err
	}); err != nil {
		h.t.Fatalf("read health item %q: %v", itemID, err)
	}
	return item
}

// assertNotice fails unless the filing's notice under code has the status. A
// transient notice raised again after it was resolved is code + "/2".
func (h *filingHarness) assertNotice(id domain.ProposalInstanceID, code string, want domain.ItemStatus) {
	h.t.Helper()
	item := h.healthItem(id, code)
	if item == nil || item.Status != want {
		h.t.Fatalf("filing %q %s notice = %+v, want status %s", id, code, item, want)
	}
}

// assertLedgered fails unless the filing ended with one ledger row naming
// issue number.
func (h *filingHarness) assertLedgered(id domain.ProposalInstanceID, number int) {
	h.t.Helper()
	intent, filed := h.intent(id), h.filed(id)
	if intent == nil || intent.Terminal == nil || intent.Terminal.Outcome != domain.FollowUpFilingLedgered ||
		filed == nil || filed.IssueNumber != number || filed.RepositoryID != testRepoID {
		h.t.Fatalf("filing %q: intent = %+v, ledger row = %+v, want ledgered as issue %d", id, intent, filed, number)
	}
}

// assertTerminal fails unless the filing ended with the outcome and reason,
// no ledger row, and the notice under code.
func (h *filingHarness) assertTerminal(
	id domain.ProposalInstanceID, outcome domain.FollowUpFilingOutcome,
	reason domain.FollowUpFilingRefusalReason, code string,
) domain.AttentionItem {
	h.t.Helper()
	intent := h.intent(id)
	if intent == nil || intent.Terminal == nil || intent.Terminal.Outcome != outcome ||
		(reason == "") != (intent.Terminal.Reason == nil) ||
		(reason != "" && *intent.Terminal.Reason != reason) {
		h.t.Fatalf("filing %q: intent = %+v, want terminal %s %q", id, intent, outcome, reason)
	}
	if filed := h.filed(id); filed != nil {
		h.t.Fatalf("filing %q: ledger row %+v beside a %s outcome", id, filed, outcome)
	}
	item := h.healthItem(id, code)
	if item == nil {
		h.t.Fatalf("filing %q: no %s notice", id, code)
	}
	if item.Type != domain.AttentionSystemHealth || item.Status != domain.StatusOpen ||
		item.Posture == nil || *item.Posture != domain.HealthPostureAdvisory ||
		len(item.RequestedDecision) != 1 || item.RequestedDecision[0] != domain.ActionAcknowledge ||
		item.HealthDiagnostic == nil || item.HealthDiagnostic.Code != code ||
		!strings.Contains(item.Reason, string(id)) || strings.Contains(item.Reason, fixtureTokenValue) {
		h.t.Fatalf("filing %q notice = %+v", id, item)
	}
	return *item
}

// tamper rewrites rows behind the store's back, then reopens the store.
func (h *filingHarness) tamper(statement string, args ...any) {
	h.t.Helper()
	if err := h.store.Close(); err != nil {
		h.t.Fatalf("store.Close: %v", err)
	}
	raw, err := sql.Open("sqlite", h.path)
	if err != nil {
		h.t.Fatal(err)
	}
	result, err := raw.Exec(statement, args...)
	if err != nil {
		h.t.Fatalf("tamper: %v", err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		h.t.Fatalf("tamper changed %d rows (err %v), want 1", rows, err)
	}
	if err := raw.Close(); err != nil {
		h.t.Fatal(err)
	}
	h.open()
}

// creates is how many create requests reached the forge.
func (h *filingHarness) creates() int {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	return h.gh.filing.creates
}

// forgeIssues is a copy of the issues the forge holds.
func (h *filingHarness) forgeIssues() []fakeFiledIssue {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	return slices.Clone(h.gh.filing.issues)
}

// respond queues create responses, consumed one per create request.
func (h *filingHarness) respond(responses ...fakeCreateResponse) {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	h.gh.filing.responses = append(h.gh.filing.responses, responses...)
}

// plant adds an issue to the forge as if something else created it.
func (h *filingHarness) plant(issue fakeFiledIssue) {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	if issue.Login == "" {
		issue.Login = filingBotSlug + "[bot]"
	}
	h.gh.filing.issues = append(h.gh.filing.issues, issue)
}

// botIssue is a planted issue the App's bot account authored at the
// harness's current time.
func (h *filingHarness) botIssue(number int) fakeFiledIssue {
	return fakeFiledIssue{
		Number: number, UserID: filingBotID, UserType: "Bot", CreatedAt: h.now.UTC().Truncate(time.Second),
	}
}

func (h *filingHarness) advance(d time.Duration) { h.now = h.now.Add(d) }
