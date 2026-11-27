package publish

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/domain/domaintest"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type replyHarnessThread struct {
	id         string
	root       int64
	isResolved bool
}

type replyHarness struct {
	t                *testing.T
	f                replyBindingFixture
	r                *ExternalReviewReplier
	now              time.Time
	d                domain.ExternalFindingDisposition
	second           domain.ExternalFindingDisposition
	authority        domain.PublicationSuccessor
	comments         []replyCommentWire
	bodies           []string
	creates          int
	status           int
	commitUnknown    bool
	threads          []replyHarnessThread
	graphqlStatus    int
	graphqlHeader    http.Header
	graphqlErrors    bool
	graphqlErr       error
	graphqlQueries   int
	graphqlMutations int
	step             func(string)
	beforeSend       func()
	beforeGraphQL    func()
	tokenErr         error
	// tokenFor, when set, sees each token request's repository first.
	tokenFor func(repo string)
}

type replyTestBoundary struct{ h *replyHarness }

func (b replyTestBoundary) Token(ctx context.Context, repo string) (InstallationToken, error) {
	if b.h.tokenFor != nil {
		b.h.tokenFor(repo)
	}
	if b.h.step != nil {
		b.h.step("token")
	}
	if ctx.Err() != nil {
		return InstallationToken{}, ctx.Err()
	}
	if b.h.tokenErr != nil {
		return InstallationToken{}, b.h.tokenErr
	}
	return InstallationToken{Token: Secret("reply-fixture-token")}, nil
}

func (b replyTestBoundary) Resolve(ctx context.Context, _ string) (AppBotIdentity, error) {
	if b.h.step != nil {
		b.h.step("identity")
	}
	if ctx.Err() != nil {
		return AppBotIdentity{}, ctx.Err()
	}
	return AppBotIdentity{AppSlug: "test", BotUserID: 123}, nil
}

func (b replyTestBoundary) RoundTrip(req *http.Request) (*http.Response, error) {
	h := b.h
	if h.step != nil {
		h.step("before " + req.Method)
	}
	if req.Context().Err() != nil {
		return nil, req.Context().Err()
	}
	w := httptest.NewRecorder()
	if req.Method == http.MethodPost && req.URL.Path == "/graphql" {
		if h.beforeGraphQL != nil {
			h.beforeGraphQL()
		}
		if h.graphqlErr != nil {
			return nil, h.graphqlErr
		}
		var gql struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(req.Body).Decode(&gql); err != nil {
			h.t.Fatal(err)
		}
		isMutation := strings.Contains(gql.Query, "resolveReviewThread")
		if isMutation {
			h.graphqlMutations++
		} else {
			h.graphqlQueries++
		}
		for k, vals := range h.graphqlHeader {
			for _, v := range vals {
				w.Header().Add(k, v)
			}
		}
		status := h.graphqlStatus
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		if status == http.StatusOK {
			switch {
			case h.graphqlErrors:
				_ = json.NewEncoder(w).Encode(map[string]any{
					"errors": []map[string]any{{"message": "reply-fixture-token graphql error"}},
				})
			case isMutation:
				id, _ := gql.Variables["id"].(string)
				var matched *replyHarnessThread
				for i := range h.threads {
					if h.threads[i].id == id {
						h.threads[i].isResolved = true
						matched = &h.threads[i]
						break
					}
				}
				if matched == nil {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"data": map[string]any{"resolveReviewThread": map[string]any{"thread": nil}},
					})
				} else {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"data": map[string]any{
							"resolveReviewThread": map[string]any{
								"thread": map[string]any{"id": matched.id, "isResolved": matched.isResolved},
							},
						},
					})
				}
			default:
				nodes := make([]map[string]any, 0, len(h.threads))
				for _, th := range h.threads {
					nodes = append(nodes, map[string]any{
						"id":         th.id,
						"isResolved": th.isResolved,
						"comments": map[string]any{
							"nodes": []map[string]any{{"fullDatabaseId": fmt.Sprintf("%d", th.root)}},
						},
					})
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"repository": map[string]any{
							"nameWithOwner": h.f.binding.Repo,
							"pullRequest": map[string]any{
								"number": h.f.binding.PRNumber,
								"reviewThreads": map[string]any{
									"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
									"nodes":    nodes,
								},
							},
						},
					},
				})
			}
		}
	} else if req.Method == http.MethodGet {
		_ = json.NewEncoder(w).Encode(h.comments)
	} else {
		if h.beforeSend != nil {
			h.beforeSend()
		}
		h.creates++
		var body struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			h.t.Fatal(err)
		}
		h.bodies = append(h.bodies, body.Body)
		status := h.status
		if status == 0 {
			status = http.StatusCreated
		}
		c := h.comment(int64(100+h.creates), 123)
		if status == http.StatusCreated || h.commitUnknown {
			h.comments = append(h.comments, c)
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(c)
	}
	if h.step != nil {
		h.step("after " + req.Method)
	}
	if req.Context().Err() != nil {
		return nil, errors.New("transport failed: reply-fixture-token")
	}
	return w.Result(), nil
}

func (h *replyHarness) comment(id, author int64) replyCommentWire {
	c := replyCommentWire{ID: id, CreatedAt: h.now, IssueURL: "https://api.example.test/repos/owner/repo/issues/450"}
	c.User.ID, c.User.Type = author, "Bot"
	thread, _ := parseExternalReplyThread(h.thread())
	c.InReplyToID = thread.root
	return c
}

func (h *replyHarness) thread() string {
	var f domain.Finding
	h.read(func(tx *store.ReadTx) error {
		var err error
		f, err = tx.GetFinding(context.Background(), h.d.FindingID)
		return err
	})
	return f.External.ThreadID
}

func (h *replyHarness) read(fn func(*store.ReadTx) error) {
	h.t.Helper()
	if err := h.f.st.Read(context.Background(), fn); err != nil {
		h.t.Fatal(err)
	}
}

func (h *replyHarness) write(fn func(*store.WriteTx) error) {
	h.t.Helper()
	if err := h.f.st.Write(context.Background(), fn); err != nil {
		h.t.Fatal(err)
	}
}

func (h *replyHarness) pass() {
	h.t.Helper()
	if err := h.r.Pass(context.Background()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *replyHarness) restart() {
	h.t.Helper()
	if h.r != nil {
		h.f.reopen(h.t)
	}
	var err error
	b := replyTestBoundary{h}
	h.r, err = NewExternalReviewReplier(h.f.st, b, &http.Client{Transport: b}, "https://api.example.test", b, func() time.Time { return h.now })
	if err != nil {
		h.t.Fatal(err)
	}
}

func (h *replyHarness) profile(admit bool) domain.AutomationTrustProfile {
	h.t.Helper()
	var reviewers []domain.ExternalReviewer
	if admit {
		reviewers = []domain.ExternalReviewer{{Forge: domain.ExternalReviewForgeGitHub, AccountID: 41, Login: "reviewer", Authority: domain.ExternalReviewDriveRound}}
	}
	p, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: h.f.binding.Repo, RepositoryID: h.f.binding.RepositoryID,
		PRExecution: domain.PRExecutionAuditedSameRepo, CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions: domain.TokenPermissionsReadOnly, CommitPlan: domain.CommitPlanSingleCommit,
		MessageRuleset: domain.MessageRulesetGitHub1, WorkflowAuditDigest: "sha256:audit",
		Review: domain.ReviewSettings{Mode: domain.ReviewFreesideInvoked, ConfigDigest: "sha256:review"}, ExternalReviewers: reviewers,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	ctx := context.Background()
	if err := h.f.st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordInactiveTrustProfile(ctx, p, h.now); err != nil {
			return err
		}
		return tx.ActivateTrustProfile(ctx, p.Repo, p.ProfileDigest, h.now)
	}); err != nil {
		h.t.Fatal(err)
	}
	return p
}

func (h *replyHarness) review(round int, head string) domain.ReviewRecord {
	h.t.Helper()
	digest := domain.Digest("sha256:" + strings.Repeat("c", 64))
	invocationID := domain.InvocationID(fmt.Sprintf("reply-review-%s-%d", h.f.run.ID, round))
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: invocationID, RunID: h.f.run.ID, Round: round,
		Admission: fake.RecordAdmission(domain.RoleReviewer, h.f.run.ID, invocationID), ConfigurationDigest: digest, InstructionDigest: digest,
		BaseSHA: h.f.admission.Base.BaseSHA, HeadSHA: head, CompletedAt: h.now, CompletionEvidence: digest, Outcome: domain.ReviewClean,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.write(func(tx *store.WriteTx) error { return tx.PutReviewRecord(context.Background(), record, nil) })
	return record
}

func newReplyHarness(t *testing.T, thread string, disposition domain.ReviewDisposition, reason string) *replyHarness {
	return newReplyHarnessThreads(t, thread, "review_comment/44", disposition, reason)
}

func newReplyHarnessThreads(t *testing.T, thread, secondThread string, disposition domain.ReviewDisposition, reason string) *replyHarness {
	t.Helper()
	f := seedReplyBinding(t, "feat/test-replies", strings.Repeat("1", 40), strings.Repeat("a", 40))
	return newReplyHarnessForBinding(t, f, thread, secondThread, disposition, reason)
}

func newReplyHarnessForBinding(t *testing.T, f replyBindingFixture, thread, secondThread string, disposition domain.ReviewDisposition, reason string) *replyHarness {
	t.Helper()
	return newReplyHarnessDecided(t, f, thread, secondThread, disposition, reason, false)
}

// newReplyHarnessDecided builds the harness with the first finding's decline
// or defer decided by the model, or, when byPerson is set, by a person
// answering the review dispute over the model's judgment.
func newReplyHarnessDecided(t *testing.T, f replyBindingFixture, thread, secondThread string, disposition domain.ReviewDisposition, reason string, byPerson bool) *replyHarness {
	t.Helper()
	h := &replyHarness{t: t, f: f, now: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC), comments: []replyCommentWire{}}
	if parsed, err := parseExternalReplyThread(thread); err == nil && parsed.root > 0 {
		h.threads = append(h.threads, replyHarnessThread{id: fmt.Sprintf("PRRT_%d", parsed.root), root: parsed.root})
	}
	if parsed, err := parseExternalReplyThread(secondThread); err == nil && parsed.root > 0 &&
		(len(h.threads) == 0 || h.threads[0].root != parsed.root) {
		h.threads = append(h.threads, replyHarnessThread{id: fmt.Sprintf("PRRT_%d", parsed.root), root: parsed.root})
	}
	prior := h.review(1, h.f.binding.HeadSHA)
	profile := h.profile(true)
	finding, err := domain.NewExternalFinding(domain.ExternalFindingInput{
		RunID: h.f.run.ID, Forge: domain.ExternalReviewForgeGitHub,
		ReviewerAccountID: 41, ReviewerLogin: "reviewer", ThreadID: thread, HeadSHA: h.f.binding.HeadSHA, Message: "Unchecked error", RawText: "P2: unchecked error", CreatedAt: h.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	h.write(func(tx *store.WriteTx) error {
		if err := tx.PutExternalFinding(ctx, finding); err != nil {
			return err
		}
		item := h.f.item
		item.Status = domain.StatusSuperseded
		item.ItemVersion++
		item = domaintest.MustConclude(item)
		return tx.PutAttentionItem(ctx, item)
	})
	second, err := domain.NewExternalFinding(domain.ExternalFindingInput{
		RunID: h.f.run.ID, Forge: domain.ExternalReviewForgeGitHub,
		ReviewerAccountID: 41, ReviewerLogin: "reviewer", ThreadID: secondThread, HeadSHA: h.f.binding.HeadSHA,
		Message: "Second finding", RawText: "P2: second finding", CreatedAt: h.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.write(func(tx *store.WriteTx) error { return tx.PutExternalFinding(ctx, second) })
	h.authority = domain.PublicationSuccessor{
		Version: domain.PublicationExternalReviewVersion, Origin: domain.PublicationSuccessorExternalReview,
		RunID: h.f.run.ID, PredecessorItemID: h.f.item.ID, PriorReviewInvocationID: prior.InvocationID, ReviewRound: 2,
		Reentry: &domain.PublicationSuccessorReentry{BaseSHA: prior.BaseSHA, HeadSHA: prior.HeadSHA}, ExternalFindingID: finding.ID, AdmittingProfileDigest: profile.ProfileDigest,
	}
	h.write(func(tx *store.WriteTx) error { return tx.RecordPublicationSuccessor(ctx, h.authority) })
	h.review(2, prior.HeadSHA)
	d := domain.ExternalFindingDisposition{FindingID: finding.ID, RunID: h.f.run.ID, Round: 2, Disposition: disposition, Reason: reason, CreatedAt: h.now}
	if disposition == domain.ReviewDispositionFixed {
		fix := h.review(3, strings.Repeat("b", 40))
		d.RemediationInvocationID = &fix.InvocationID
	} else {
		route, goal := domain.RouteDecline, domain.GoalContradictory
		if disposition == domain.ReviewDispositionDeferred {
			route, goal = domain.RouteDefer, domain.GoalAdjacent
		}
		entry, err := domain.NewModelAdjudicationEntry(finding.ID, goal, nil, route, domain.ConfidenceHigh, "decision", nil, nil, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		secondEntry, err := domain.NewModelAdjudicationEntry(second.ID, goal, nil, route, domain.ConfidenceHigh, "decision", nil, nil, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		a, err := domain.NewFindingAdjudication(h.f.run.ID, 2, h.f.run.SpecDigest, prior.InstructionDigest, h.f.run.PolicyDigest, []domain.FindingAdjudicationEntry{entry, secondEntry}, "", h.now)
		if err != nil {
			t.Fatal(err)
		}
		h.write(func(tx *store.WriteTx) error { return tx.PutFindingAdjudication(ctx, a) })
		if byPerson {
			a = h.personDecides(a, finding, route)
		}
		d.AdjudicationDigest = &a.Digest
	}
	h.write(func(tx *store.WriteTx) error { return tx.PutExternalFindingDisposition(ctx, d) })
	h.d = d
	h.second = d
	h.second.FindingID = second.ID
	h.restart()
	return h
}

// personDecides records a person's adjudicate_findings answer to a review
// dispute over finding and the human successor of prior it decides, as the
// store accepts them, and returns that successor.
func (h *replyHarness) personDecides(prior domain.FindingAdjudication, finding domain.Finding, route domain.AdjudicationRoute) domain.FindingAdjudication {
	h.t.Helper()
	ctx := context.Background()
	ids := make([]domain.FindingID, 0, len(prior.Entries))
	for _, entry := range prior.Entries {
		ids = append(ids, entry.FindingID)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID:        domain.ProductionFindingAdjudicationItemID(h.f.run.ID, prior.Round, 1),
		ProjectID: h.f.run.ProjectID, Subject: h.f.item.Subject,
		Type: domain.AttentionReviewDispute, Priority: domain.PriorityNormal, Reason: "choose routes",
		RequestedDecision: []domain.Action{domain.ActionAdjudicateFindings, domain.ActionStop},
		PRHeadSHA:         h.f.binding.HeadSHA, ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate,
		Status: domain.StatusOpen, ReviewDispute: &domain.ReviewDisputeBinding{
			RunID: h.f.run.ID, Round: prior.Round, FindingIDs: ids,
			CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("c", 64)),
			Findings: []domain.DisputedFinding{{
				FindingID: finding.ID, Message: finding.Message,
				External:      &domain.DisputedFindingExternal{ReviewerLogin: finding.External.ReviewerLogin, ThreadID: finding.External.ThreadID},
				OfferedRoutes: []domain.AdjudicationRoute{domain.RouteDecline, domain.RouteDefer},
			}},
		},
	}, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: "person-answer", DeviceID: "device-test", ItemID: item.ID, ItemVersion: item.ItemVersion,
		PRHeadSHA: item.PRHeadSHA, ArtifactDigests: item.ArtifactDigests, Action: domain.ActionAdjudicateFindings,
		Message: `[{"finding_id":"` + string(finding.ID) + `","route":"` + string(route) + `"}]`,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	decidedAt := h.now.Add(time.Minute)
	h.write(func(tx *store.WriteTx) error {
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return err
		}
		if err := tx.PutCommand(ctx, command); err != nil {
			return err
		}
		item.Status = domain.StatusResolved
		item.ItemVersion++
		decided, err := item.WithDecidedAt(decidedAt)
		if err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, domaintest.MustConclude(decided))
	})
	entries := slices.Clone(prior.Entries)
	for index, entry := range entries {
		if entry.FindingID == finding.ID {
			if entries[index], err = domain.NewHumanAdjudicationEntry(finding.ID, route, ""); err != nil {
				h.t.Fatal(err)
			}
		}
	}
	decision := domain.AdjudicationDecision{CommandID: command.CommandID, ItemID: item.ID, ItemVersion: command.ItemVersion}
	human, err := domain.NewHumanSuccessorFindingAdjudication(prior, entries, decision, decidedAt)
	if err != nil {
		h.t.Fatal(err)
	}
	h.write(func(tx *store.WriteTx) error { return tx.PutFindingAdjudication(ctx, human) })
	return human
}

func (h *replyHarness) outcome() externalReplyOutcome {
	h.t.Helper()
	var outcome externalReplyOutcome
	h.read(func(tx *store.ReadTx) error {
		row, err := tx.GetOutbox(context.Background(), externalReplyKey(h.d.FindingID, h.d.Round)+"/outcome")
		if err != nil {
			return err
		}
		return json.Unmarshal(row.Payload, &outcome)
	})
	return outcome
}

func TestExternalReviewReplierPostsOnce(t *testing.T) {
	t.Parallel()
	for _, thread := range []string{"review/42", "review_comment/43", "issue_comment/44"} {
		t.Run(thread, func(t *testing.T) {
			h := newReplyHarness(t, thread, domain.ReviewDispositionDeclined, "Already checked.")
			h.pass()
			h.pass()
			if got := h.outcome(); got.CommentID != 101 || got.Code != "" {
				t.Fatalf("outcome: %+v", got)
			}
			h.restart()
			h.pass()
			if h.creates != 1 {
				t.Fatalf("creates = %d", h.creates)
			}
			// A conversation comment's reply is a conversation comment that
			// links it, as a review body's links its review (#3127).
			if want := "In response to [comment 44](/owner/repo/pull/450#issuecomment-44):\n\n"; thread == "issue_comment/44" &&
				(len(h.bodies) != 1 || !strings.HasPrefix(h.bodies[0], want)) {
				t.Fatalf("conversation reply bodies = %q, want one starting %q", h.bodies, want)
			}
		})
	}
}

// TestExternalReviewReplierNamesAPersonsDecision posts a person's decline
// and defer in the target repository's terms: a maintainer decided, with no
// placeholder reason and no adjudication digest (#2349).
func TestExternalReviewReplierNamesAPersonsDecision(t *testing.T) {
	t.Parallel()
	for disposition, want := range map[domain.ReviewDisposition]string{
		domain.ReviewDispositionDeclined: "A maintainer declined this finding.\n",
		domain.ReviewDispositionDeferred: "A maintainer deferred this finding.\n",
	} {
		t.Run(string(disposition), func(t *testing.T) {
			f := seedReplyBinding(t, "feat/test-replies", strings.Repeat("1", 40), strings.Repeat("a", 40))
			h := newReplyHarnessDecided(t, f, "review_comment/43", "review_comment/44", disposition, "Human decision recorded. (finding adjudication sha256:3f7b)", true)
			h.pass()
			h.pass()
			if got := h.outcome(); got.CommentID != 101 || got.Code != "" {
				t.Fatalf("outcome: %+v", got)
			}
			if len(h.bodies) != 1 || h.bodies[0] != want {
				t.Fatalf("posted %q, want %q", h.bodies, want)
			}
		})
	}
}

func TestExternalReviewReplierSurvivesEachBoundary(t *testing.T) {
	t.Parallel()
	for kill := 1; kill < 20; kill++ {
		stopped := false
		t.Run(fmt.Sprint(kill), func(t *testing.T) {
			h := newReplyHarness(t, "review_comment/43", domain.ReviewDispositionDeferred, "Tracked separately.")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			steps := 0
			h.step = func(string) {
				steps++
				if steps == kill {
					stopped = true
					cancel()
				}
			}
			_ = h.r.Pass(ctx)
			_ = h.r.Pass(ctx)
			h.step = nil
			h.restart()
			h.pass()
			h.pass()
			h.now = h.now.Add(11 * time.Minute)
			h.pass()
			h.pass()
			outcome := h.outcome()
			if h.creates > 1 || len(h.comments) > 1 {
				t.Fatalf("duplicate: %d creates", h.creates)
			}
			if outcome.CommentID == 0 && outcome.Code != externalReplyAmbiguous {
				t.Fatalf("outcome: %+v", outcome)
			}
		})
		if !stopped {
			break
		}
	}
}

func TestExternalReviewReplierUnknownOutcomes(t *testing.T) {
	t.Parallel()
	for _, count := range []int{0, 1, 2} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			h := newReplyHarness(t, "review_comment/43", domain.ReviewDispositionDeclined, "Already checked.")
			h.status = 500
			h.pass()
			h.pass()
			for n := range count {
				h.comments = append(h.comments, h.comment(int64(200+n), 123))
			}
			wrong := h.comment(300, 999)
			h.comments = append(h.comments, wrong)
			wrongThread := h.comment(301, 123)
			wrongThread.InReplyToID = 999
			h.comments = append(h.comments, wrongThread)
			h.restart()
			h.pass()
			h.now = h.now.Add(11 * time.Minute)
			h.pass()
			outcome := h.outcome()
			if (count == 1 && outcome.CommentID != 200) || (count != 1 && outcome.Code != externalReplyAmbiguous) {
				t.Fatalf("outcome: %+v", outcome)
			}
			h.restart()
			h.pass()
			if h.creates != 1 {
				t.Fatalf("creates = %d", h.creates)
			}
		})
	}
}

func TestExternalReviewReplierRefusals(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{"@codex review", "Already checked."} {
		t.Run(reason, func(t *testing.T) {
			h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, reason)
			h.status = 404
			h.pass()
			h.pass()
			if h.outcome().Code != externalReplyRefused {
				t.Fatal("expected refusal")
			}
			want := 1
			if strings.HasPrefix(reason, "@") {
				want = 0
			}
			if h.creates != want {
				t.Fatalf("creates = %d", h.creates)
			}
		})
	}
}

func TestExternalReviewReplierRevokedReviewer(t *testing.T) {
	t.Parallel()
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprint(pending), func(t *testing.T) {
			h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, "Already checked.")
			if pending {
				h.pass()
			}
			h.profile(false)
			h.pass()
			if h.creates != 0 {
				t.Fatal("revoked reviewer caused a send")
			}
			h.read(func(tx *store.ReadTx) error {
				items, err := tx.ListOpenAttentionItems(context.Background(), domain.AttentionSystemHealth)
				if len(items) != 0 {
					t.Fatalf("revocation raised %d items", len(items))
				}
				return err
			})
		})
	}
}

func TestExternalReviewReplierReconcilesUnknownSendAfterRevocation(t *testing.T) {
	t.Parallel()
	h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, "Already checked.")
	h.status = http.StatusInternalServerError
	h.commitUnknown = true
	h.pass()
	h.pass()
	h.profile(false)
	h.now = h.now.Add(11 * time.Minute)
	h.pass()
	if h.creates != 1 || h.outcome().CommentID != 101 {
		t.Fatalf("outcome = %+v, creates = %d", h.outcome(), h.creates)
	}
}

func TestExternalReviewReplierRejectsMismatchedDispatchingIntentAfterRevocation(t *testing.T) {
	t.Parallel()
	h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, "Already checked.")
	h.status = http.StatusInternalServerError
	h.pass()
	h.pass()
	var intent externalReplyIntent
	h.read(func(tx *store.ReadTx) error {
		row, err := tx.GetOutbox(context.Background(), externalReplyKey(h.d.FindingID, h.d.Round))
		if err != nil {
			return err
		}
		intent, err = decodeExternalReplyIntent(row)
		return err
	})
	intent.Thread = "review/999"
	payload, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.f.st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", h.f.path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(context.Background(), `UPDATE outbox SET payload = ?, payload_digest = ? WHERE idempotency_key = ?`,
		payload, contentaddr.Sum(payload), externalReplyKey(h.d.FindingID, h.d.Round))
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("rewrite intent: %v, %v", err, closeErr)
	}
	h.f.reopen(t)
	h.r, err = NewExternalReviewReplier(h.f.st, replyTestBoundary{h}, &http.Client{Transport: replyTestBoundary{h}}, "https://api.example.test", replyTestBoundary{h}, func() time.Time { return h.now })
	if err != nil {
		t.Fatal(err)
	}
	h.profile(false)
	if err := h.r.Pass(context.Background()); err == nil || h.creates != 1 {
		t.Fatalf("mismatched dispatching intent reconciled: %v, creates = %d", err, h.creates)
	}
}

func TestExternalReviewReplierAcceptsSkewedSuccessfulResponse(t *testing.T) {
	t.Parallel()
	h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, "Already checked.")
	h.pass()
	h.now = h.now.Add(-time.Second)
	h.pass()
	if h.creates != 1 || h.outcome().CommentID != 101 {
		t.Fatalf("outcome = %+v, creates = %d", h.outcome(), h.creates)
	}
}

func TestExternalReviewReplierDoesNotAdoptSkewedUnknownResponse(t *testing.T) {
	t.Parallel()
	h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, "Already checked.")
	h.status = http.StatusInternalServerError
	h.commitUnknown = true
	h.pass()
	h.now = h.now.Add(-time.Second)
	h.pass()
	h.now = h.now.Add(11 * time.Minute)
	h.pass()
	if h.creates != 1 || h.outcome().Code != externalReplyAmbiguous {
		t.Fatalf("outcome = %+v, creates = %d", h.outcome(), h.creates)
	}
}

func TestExternalReviewReplierRechecksAdmissionAfterTokenResolution(t *testing.T) {
	t.Parallel()
	h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, "Already checked.")
	h.pass()
	h.step = func(name string) {
		if name == "token" {
			h.profile(false)
		}
	}
	h.pass()
	if h.creates != 0 || h.outcome().Code != externalReplyRevoked {
		t.Fatal("revocation during request preparation did not stop send")
	}
}

func TestExternalReviewReplierWorkflowClosureBoundaries(t *testing.T) {
	t.Parallel()
	for _, boundary := range []string{"before intent", "before send", "in flight"} {
		t.Run(boundary, func(t *testing.T) {
			h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, "Already checked.")
			closeRun := func() {
				h.write(func(tx *store.WriteTx) error {
					run, err := tx.GetRun(t.Context(), h.f.run.ID)
					if err != nil {
						return err
					}
					task, err := tx.GetTask(t.Context(), run.TaskID)
					if err != nil {
						return err
					}
					if task.CurrentStart() == nil {
						if err := tx.RecordTaskStart(t.Context(), run.ID, h.now); err != nil {
							return err
						}
						task, err = tx.GetTask(t.Context(), run.TaskID)
						if err != nil {
							return err
						}
					}
					if _, err := tx.AbandonTask(t.Context(), task.ID, h.now); err != nil {
						return err
					}
					_, _, err = tx.CloseTaskWorkflow(t.Context(), task.ID, task.CurrentStart().Ordinal, h.now)
					return err
				})
			}
			switch boundary {
			case "before intent":
				closeRun()
			case "before send":
				h.pass()
				h.step = func(name string) {
					if name == "token" {
						h.step = nil
						closeRun()
					}
				}
			case "in flight":
				h.pass()
				h.beforeSend = closeRun
			}
			h.pass()
			h.step, h.beforeSend = nil, nil
			h.restart()
			h.pass()
			want := 0
			if boundary == "in flight" {
				want = 1
				if h.outcome().CommentID == 0 {
					t.Fatal("already dispatched reply outcome was lost")
				}
			}
			if h.creates != want {
				t.Fatalf("reply creates = %d, want %d", h.creates, want)
			}
			if boundary == "before send" {
				h.read(func(tx *store.ReadTx) error {
					entry, err := tx.GetOutbox(t.Context(), externalReplyKey(h.d.FindingID, h.d.Round))
					if err == nil && !entry.Dispatched() {
						t.Fatalf("losing claim left an active reservation: %s", entry.Status)
					}
					return err
				})
			}
		})
	}
}

func TestExternalReviewReplierWorkflowClosureAfterDispatchClaim(t *testing.T) {
	t.Parallel()
	for _, response := range []string{"success", "unproven", "transient rejection"} {
		t.Run(response, func(t *testing.T) {
			h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, "Already checked.")
			h.pass()
			CloseWorkflowAtDispatchForTest(t, h.f.st, h.f.path, h.f.run.ID, h.now, false)
			h.restart()
			h.beforeSend = func() {
				if err := h.r.retireClosed(t.Context(), h.f.run, h.d); err != nil {
					t.Fatal(err)
				}
				h.read(func(tx *store.ReadTx) error {
					key := externalReplyKey(h.d.FindingID, h.d.Round)
					entry, err := tx.GetOutbox(t.Context(), key)
					if err == nil && entry.Status != "dispatching" {
						t.Fatalf("retirement took an active dispatch claim: %s", entry.Status)
					}
					return err
				})
			}
			switch response {
			case "unproven":
				h.status, h.commitUnknown = http.StatusInternalServerError, true
			case "transient rejection":
				h.status = http.StatusTooManyRequests
			}
			h.pass()
			h.beforeSend = nil
			if h.creates != 1 {
				t.Fatalf("winning claim sent %d requests, want 1", h.creates)
			}
			if err := h.f.st.Read(t.Context(), func(tx *store.ReadTx) error {
				return tx.RequireRunWorkflowOpen(t.Context(), h.f.run.ID)
			}); !errors.Is(err, store.ErrTaskWorkflowClosed) {
				t.Fatalf("dispatch did not restore an authentic closure: %v", err)
			}
			h.restart()
			h.pass()
			h.now = h.now.Add(11 * time.Minute)
			h.pass()
			h.pass()
			if h.creates != 1 {
				t.Fatalf("closure permitted a fresh dispatch: %d requests", h.creates)
			}
			if response == "transient rejection" {
				if got := h.outcome(); got.Code != externalReplyRefused || got.CommentID != 0 {
					t.Fatalf("actual rejection was not retired: %+v", got)
				}
			} else if got := h.outcome(); got.CommentID != 101 || got.Code != "" {
				t.Fatalf("lost actual reply or recorded false ambiguity: %+v", got)
			}
		})
	}
}

func TestExternalReviewReplierClosureRetiresProvenUnsentReservation(t *testing.T) {
	t.Parallel()
	for _, rejected := range []bool{false, true} {
		t.Run(fmt.Sprintf("transient-rejection-%t", rejected), func(t *testing.T) {
			h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, "Already checked.")
			h.pass()
			if rejected {
				h.status = http.StatusTooManyRequests
				h.pass()
			}
			closeWorkflowForTest(t, h.f.st, h.f.run.ID, h.now)
			h.restart()
			nextFixture := seedReplyBindingIn(t, h.f.st, replyBindingSeed{"later-run", "owner/repo", 424242, 450},
				"feat/later-reply", strings.Repeat("1", 40), strings.Repeat("a", 40))
			nextFixture.path = h.f.path
			next := newReplyHarnessForBinding(t, nextFixture, "review/45", "review_comment/46", domain.ReviewDispositionDeclined, "Checked later.")
			next.pass()
			next.pass()
			next.pass()
			if next.creates != 1 {
				t.Fatalf("closed reservation blocked later workflow: creates = %d", next.creates)
			}
			if got := h.outcome(); got.Code != externalReplyRefused || got.CommentID != 0 {
				t.Fatalf("proven-unsent reply was not retired: %+v", got)
			}
			if got := next.outcome(); got.CommentID != 101 || got.Code != "" {
				t.Fatalf("later workflow outcome = %+v", got)
			}
			next.read(func(tx *store.ReadTx) error {
				items, err := tx.ListOpenAttentionItems(t.Context(), domain.AttentionSystemHealth)
				if err == nil && len(items) != 0 {
					t.Fatalf("closed workflow acquired notices: %+v", items)
				}
				return err
			})
			next.restart()
			next.pass()
			if next.creates != 1 {
				t.Fatal("restart retried retired work")
			}
		})
	}
}

func TestExternalReviewReplierIgnoresNeverListedFinding(t *testing.T) {
	t.Parallel()
	h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, "Already checked.")
	f, err := domain.NewExternalFinding(domain.ExternalFindingInput{
		RunID: h.f.run.ID, Forge: domain.ExternalReviewForgeGitHub,
		ReviewerAccountID: 999, ReviewerLogin: "unlisted", ThreadID: "review/999", HeadSHA: h.f.binding.HeadSHA,
		Message: "Unlisted finding", RawText: "P2: unlisted", CreatedAt: h.now,
	})
	if err != nil {
		t.Fatal(err)
	}
	h.write(func(tx *store.WriteTx) error { return tx.PutExternalFinding(context.Background(), f) })
	h.pass()
	h.pass()
	h.pass()
	if h.creates != 1 || !strings.Contains(h.bodies[0], "review 42") {
		t.Fatalf("unexpected replies: %v", h.bodies)
	}
}

func TestExternalReviewReplierRetriesOnlyProvenRateLimitRejections(t *testing.T) {
	t.Parallel()
	h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, "Already checked.")
	h.status = 429
	h.pass()
	h.pass()
	h.pass()
	if h.creates != 1 {
		t.Fatal("retry ignored backoff")
	}
	h.now = h.now.Add(time.Minute)
	h.pass()
	h.now = h.now.Add(time.Minute)
	h.pass()
	if h.creates != 3 || h.outcome().Code != externalReplyRefused {
		t.Fatal("rate-limit attempt bound did not hold")
	}
	h.restart()
	h.pass()
	if h.creates != 3 {
		t.Fatal("terminal refusal retried after restart")
	}
}

func TestExternalReviewReplierFixedWaitsForPublication(t *testing.T) {
	t.Parallel()
	h := newReplyHarness(t, "review/42", domain.ReviewDispositionFixed, "Reviewed a remediated head.")
	h.pass()
	h.pass()
	if h.creates != 0 {
		t.Fatal("fixed reply preceded publication")
	}
	// A later round can return to the already-published head. The reply must
	// name that reviewed, published head, never the intervening fix's head.
	h.review(4, h.f.binding.HeadSHA)
	h.pass()
	h.pass()
	if h.creates != 1 || !strings.Contains(h.bodies[0], h.f.binding.HeadSHA) || strings.Contains(h.bodies[0], strings.Repeat("b", 40)) {
		t.Fatalf("fixed reply = %v", h.bodies)
	}
}

func TestExternalReviewReplierSerializesPullRequest(t *testing.T) {
	t.Parallel()
	h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, "Already checked.")
	h.write(func(tx *store.WriteTx) error { return tx.PutExternalFindingDisposition(context.Background(), h.second) })
	h.status = 500
	h.pass()
	h.pass()
	h.pass()
	if h.creates != 1 {
		t.Fatalf("overlapping creates = %d", h.creates)
	}
	h.read(func(tx *store.ReadTx) error {
		rows, err := tx.ListPendingOutbox(context.Background(), ExternalReplyIntentKind)
		if len(rows) != 1 {
			t.Fatalf("pending intents = %d", len(rows))
		}
		return err
	})
	h.now = h.now.Add(11 * time.Minute)
	h.pass()
	h.status = 201
	h.pass()
	h.pass()
	if h.creates != 2 {
		t.Fatalf("second reply did not resume: %d", h.creates)
	}
}

func TestExternalReviewReplierScreensAgainBeforeSend(t *testing.T) {
	t.Parallel()
	h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, "Already checked.")
	h.pass()
	var i externalReplyIntent
	var source externalReplySource
	h.read(func(tx *store.ReadTx) error {
		row, err := tx.GetOutbox(context.Background(), externalReplyKey(h.d.FindingID, h.d.Round))
		if err != nil {
			return err
		}
		i, err = decodeExternalReplyIntent(row)
		if err != nil {
			return err
		}
		source, err = replySource(context.Background(), tx, h.d)
		return err
	})
	d := h.d
	d.Reason = "@codex review"
	if err := h.r.send(context.Background(), h.f.run, d, source, i); err != nil {
		t.Fatal(err)
	}
	if h.creates != 0 || h.outcome().Code != externalReplyRefused {
		t.Fatal("unsafe send-time reason accepted")
	}
}

func TestExternalReviewReplierNeverEchoesTransportToken(t *testing.T) {
	t.Parallel()
	h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, "Already checked.")
	h.pass()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.step = func(name string) {
		if name == "after POST" {
			cancel()
		}
	}
	err := h.r.Pass(ctx)
	if err == nil || strings.Contains(err.Error(), "reply-fixture-token") {
		t.Fatalf("error: %v", err)
	}
	h.step = nil
	h.restart()
	h.pass()
	h.now = h.now.Add(11 * time.Minute)
	h.pass()
	if h.outcome().CommentID == 0 || h.creates != 1 {
		t.Fatal("lost response was not adopted")
	}
}

func TestExternalReviewReplierRejectsSubstitutedPublishedHead(t *testing.T) {
	t.Parallel()
	h := newReplyHarness(t, "review/42", domain.ReviewDispositionFixed, "Reviewed a remediated head.")
	h.review(4, h.f.binding.HeadSHA)
	h.pass()
	var i externalReplyIntent
	var source externalReplySource
	h.read(func(tx *store.ReadTx) error {
		row, err := tx.GetOutbox(context.Background(), externalReplyKey(h.d.FindingID, h.d.Round))
		if err != nil {
			return err
		}
		i, err = decodeExternalReplyIntent(row)
		if err != nil {
			return err
		}
		source, err = replySource(context.Background(), tx, h.d)
		return err
	})
	i.HeadSHA = strings.Repeat("c", 40)
	binding := source.binding
	binding.HeadSHA = i.HeadSHA
	thread, _ := parseExternalReplyThread(i.Thread)
	body, err := externalReviewReplyText(h.d, source.adjudication, thread, binding, i.Ruleset)
	if err != nil {
		t.Fatal(err)
	}
	i.TextDigest = contentaddr.Sum([]byte(body))
	if err := h.r.send(context.Background(), h.f.run, h.d, source, i); err == nil || h.creates != 0 {
		t.Fatalf("substituted head accepted: %v", err)
	}
}

func TestExternalReviewReplierDoesNotAdoptExistingOrForeignComment(t *testing.T) {
	t.Parallel()
	h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, "Already checked.")
	h.comments = append(h.comments, h.comment(80, 123))
	h.pass()
	h.status = 500
	h.pass()
	foreign := h.comment(81, 123)
	foreign.IssueURL = "https://api.example.test/repos/owner/repo/issues/999"
	h.comments = append(h.comments, foreign)
	h.now = h.now.Add(11 * time.Minute)
	h.pass()
	if h.outcome().Code != externalReplyAmbiguous || h.creates != 1 {
		t.Fatal("foreign or existing comment adopted")
	}
}

func TestExternalReviewReplierUnreadableDispositionRaisesOneSafeNotice(t *testing.T) {
	t.Parallel()
	h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, "Already checked.")
	if err := h.f.st.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", h.f.path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(context.Background(), "UPDATE external_finding_dispositions SET reason = ? WHERE finding_id = ?", "reply-fixture-token", h.d.FindingID)
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("corrupt fixture: %v, %v", err, closeErr)
	}
	h.restart()
	for range 2 {
		err := h.r.Pass(context.Background())
		if err == nil || strings.Contains(err.Error(), "reply-fixture-token") {
			t.Fatalf("unsafe error: %v", err)
		}
	}
	if h.creates != 0 {
		t.Fatal("unreadable disposition sent")
	}
	h.read(func(tx *store.ReadTx) error {
		items, err := tx.ListOpenAttentionItems(context.Background(), domain.AttentionSystemHealth)
		if len(items) != 1 || items[0].HealthDiagnostic.Code != externalReplyUnreadable || strings.Contains(items[0].Reason, "reply-fixture-token") {
			t.Fatalf("notices: %+v", items)
		}
		return err
	})
}

func TestExternalReviewReplyBackupRecordsHaveNoArtifactDigests(t *testing.T) {
	t.Parallel()
	h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, "Already checked.")
	h.pass()
	h.pass()
	h.read(func(tx *store.ReadTx) error {
		key := externalReplyKey(h.d.FindingID, h.d.Round)
		for _, key := range []string{key, key + "/outcome"} {
			row, err := tx.GetOutbox(context.Background(), key)
			if err != nil {
				return err
			}
			digests, err := ExternalReplyBackupPayloadDigests(row)
			if err != nil || len(digests) != 0 {
				t.Fatalf("backup: %v, %v", digests, err)
			}
			row.Kind = "other"
			if _, err := ExternalReplyBackupPayloadDigests(row); err == nil {
				t.Fatal("foreign kind accepted")
			}
		}
		return nil
	})
}

func TestExternalReviewReplierRun(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, "Already checked.")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- h.r.Run(ctx, time.Minute, func(err error) { t.Error(err) }) }()
		synctest.Wait()
		synctest.Sleep(time.Minute)
		cancel()
		synctest.Wait()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if h.creates != 1 {
			t.Fatalf("creates = %d", h.creates)
		}
	})
}

func (h *replyHarness) resolution() (externalReviewThreadResolution, bool) {
	h.t.Helper()
	return h.resolutionFor(h.d)
}

func (h *replyHarness) resolutionFor(d domain.ExternalFindingDisposition) (externalReviewThreadResolution, bool) {
	h.t.Helper()
	var res externalReviewThreadResolution
	var found bool
	h.read(func(tx *store.ReadTx) error {
		row, err := tx.GetOutbox(context.Background(), externalReplyKey(d.FindingID, d.Round)+"/resolve")
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		res, err = decodeExternalReviewThreadResolution(row)
		found = err == nil
		return err
	})
	return res, found
}

func TestExternalReviewReplierResolvesDeclinedAndDeferredOnce(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		disposition     domain.ReviewDisposition
		alreadyResolved bool
		wantMutations   int
	}{
		{name: "declined", disposition: domain.ReviewDispositionDeclined, wantMutations: 1},
		{name: "deferred", disposition: domain.ReviewDispositionDeferred, wantMutations: 1},
		{name: "already resolved", disposition: domain.ReviewDispositionDeclined, alreadyResolved: true, wantMutations: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newReplyHarness(t, "review_comment/43", tc.disposition, "Already checked.")
			h.threads[0].isResolved = tc.alreadyResolved
			h.pass() // open reply intent
			h.pass() // send reply and record outcome
			if h.graphqlQueries != 0 || h.graphqlMutations != 0 {
				t.Fatalf("unexpected GraphQL before resolution pass: queries=%d mutations=%d", h.graphqlQueries, h.graphqlMutations)
			}
			h.pass() // resolve thread
			if h.graphqlQueries != 1 || h.graphqlMutations != tc.wantMutations || !h.threads[0].isResolved {
				t.Fatalf("queries=%d mutations=%d resolved=%t", h.graphqlQueries, h.graphqlMutations, h.threads[0].isResolved)
			}
			res, found := h.resolution()
			if !found || res.Code != "" {
				t.Fatalf("resolution = (%+v, %t), want empty code", res, found)
			}
			// Reopening on GitHub or restarting must not trigger another GraphQL call.
			h.threads[0].isResolved = false
			h.pass()
			h.restart()
			h.pass()
			if h.graphqlQueries != 1 || h.graphqlMutations != tc.wantMutations {
				t.Fatalf("re-resolved after restart: queries=%d mutations=%d", h.graphqlQueries, h.graphqlMutations)
			}
			h.read(func(tx *store.ReadTx) error {
				items, err := tx.ListOpenAttentionItems(context.Background(), domain.AttentionSystemHealth)
				if err == nil && len(items) != 0 {
					t.Fatalf("unexpected notices: %+v", items)
				}
				row, err := tx.GetOutbox(context.Background(), externalReplyKey(h.d.FindingID, h.d.Round)+"/resolve")
				if err != nil {
					return err
				}
				digests, err := ExternalReviewThreadResolutionBackupPayloadDigests(row)
				if err != nil || len(digests) != 0 {
					t.Fatalf("backup digests = (%v, %v)", digests, err)
				}
				row.Kind = "other"
				if _, err := ExternalReviewThreadResolutionBackupPayloadDigests(row); err == nil {
					t.Fatal("foreign kind accepted")
				}
				return nil
			})
		})
	}
}

func TestExternalReviewReplierFailedThreadResolutionRaisesOneNotice(t *testing.T) {
	t.Parallel()
	h := newReplyHarness(t, "review_comment/43", domain.ReviewDispositionDeclined, "Already checked.")
	h.pass()
	h.pass()
	h.graphqlErrors = true
	h.pass()
	res, found := h.resolution()
	if !found || res.Code != externalReplyThreadUnresolved {
		t.Fatalf("resolution = (%+v, %t), want %s", res, found, externalReplyThreadUnresolved)
	}
	h.pass()
	h.restart()
	h.pass()
	if h.graphqlQueries != 1 || h.graphqlMutations != 0 {
		t.Fatalf("failed resolution retried: queries=%d mutations=%d", h.graphqlQueries, h.graphqlMutations)
	}
	h.read(func(tx *store.ReadTx) error {
		items, err := tx.ListOpenAttentionItems(context.Background(), domain.AttentionSystemHealth)
		if err != nil {
			return err
		}
		if len(items) != 1 {
			t.Fatalf("notices = %d, want 1", len(items))
		}
		item := items[0]
		if item.HealthDiagnostic == nil || item.HealthDiagnostic.Code != externalReplyThreadUnresolved ||
			item.Posture == nil || *item.Posture != domain.HealthPostureAdvisory ||
			!slices.Equal(item.RequestedDecision, []domain.Action{domain.ActionAcknowledge}) ||
			strings.Contains(item.Reason, "reply-fixture-token") {
			t.Fatalf("notice = %+v", item)
		}
		return nil
	})
}

func TestExternalReviewReplierRetriesThreadResolutionWithinBound(t *testing.T) {
	t.Parallel()

	t.Run("three attempt bound with backoff", func(t *testing.T) {
		h := newReplyHarness(t, "review_comment/43", domain.ReviewDispositionDeclined, "Already checked.")
		h.pass()
		h.pass()
		h.graphqlStatus = http.StatusTooManyRequests
		h.pass()
		h.pass()
		if h.graphqlQueries != 1 {
			t.Fatalf("retry ignored backoff: queries=%d", h.graphqlQueries)
		}
		if _, found := h.resolution(); found {
			t.Fatal("recorded resolution before attempt bound")
		}
		h.now = h.now.Add(time.Minute)
		h.pass()
		if h.graphqlQueries != 2 {
			t.Fatalf("second attempt queries=%d", h.graphqlQueries)
		}
		h.now = h.now.Add(time.Minute)
		h.pass()
		if h.graphqlQueries != 3 {
			t.Fatalf("third attempt queries=%d", h.graphqlQueries)
		}
		res, found := h.resolution()
		if !found || res.Code != externalReplyThreadUnresolved {
			t.Fatalf("resolution = (%+v, %t), want %s", res, found, externalReplyThreadUnresolved)
		}
		h.restart()
		h.pass()
		if h.graphqlQueries != 3 {
			t.Fatalf("terminal resolution failure retried after restart: queries=%d", h.graphqlQueries)
		}
		h.read(func(tx *store.ReadTx) error {
			items, err := tx.ListOpenAttentionItems(context.Background(), domain.AttentionSystemHealth)
			if err == nil && (len(items) != 1 || items[0].HealthDiagnostic.Code != externalReplyThreadUnresolved) {
				t.Fatalf("notices = %+v", items)
			}
			return err
		})
	})

	t.Run("token failure and canceled context record nothing", func(t *testing.T) {
		h := newReplyHarness(t, "review_comment/43", domain.ReviewDispositionDeclined, "Already checked.")
		h.pass()
		h.pass()

		h.tokenErr = errors.New("mint failed: reply-fixture-token")
		err := h.r.Pass(context.Background())
		if err == nil || strings.Contains(err.Error(), "reply-fixture-token") {
			t.Fatalf("token error = %v", err)
		}
		h.tokenErr = nil

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := h.r.Pass(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled pass = %v", err)
		}
		if h.graphqlQueries != 0 || h.graphqlMutations != 0 {
			t.Fatalf("unexpected GraphQL: queries=%d mutations=%d", h.graphqlQueries, h.graphqlMutations)
		}
		if _, found := h.resolution(); found {
			t.Fatal("recorded resolution after pre-send error")
		}

		// Later pass resolves normally without having spent any retry attempts.
		h.pass()
		if res, found := h.resolution(); !found || res.Code != "" || h.graphqlMutations != 1 {
			t.Fatalf("later pass failed to resolve: res=(%+v, %t) mutations=%d", res, found, h.graphqlMutations)
		}
	})
}

func TestExternalReviewReplierSkipsThreadResolutionWhereInapplicable(t *testing.T) {
	t.Parallel()

	t.Run("fixed reply", func(t *testing.T) {
		h := newReplyHarness(t, "review_comment/43", domain.ReviewDispositionFixed, "Reviewed a remediated head.")
		h.review(4, h.f.binding.HeadSHA)
		h.pass()
		h.pass()
		h.pass()
		if h.creates != 1 || h.graphqlQueries != 0 || h.graphqlMutations != 0 {
			t.Fatalf("creates=%d queries=%d mutations=%d", h.creates, h.graphqlQueries, h.graphqlMutations)
		}
	})

	for _, thread := range []string{"review/42", "issue_comment/44"} {
		t.Run(thread, func(t *testing.T) {
			h := newReplyHarness(t, thread, domain.ReviewDispositionDeclined, "Already checked.")
			h.pass()
			h.pass()
			h.pass()
			if h.creates != 1 || h.graphqlQueries != 0 || h.graphqlMutations != 0 {
				t.Fatalf("creates=%d queries=%d mutations=%d", h.creates, h.graphqlQueries, h.graphqlMutations)
			}
		})
	}

	t.Run("refused reply", func(t *testing.T) {
		h := newReplyHarness(t, "review_comment/43", domain.ReviewDispositionDeclined, "Already checked.")
		h.status = http.StatusNotFound
		h.pass()
		h.pass()
		h.pass()
		if h.outcome().Code != externalReplyRefused || h.graphqlQueries != 0 || h.graphqlMutations != 0 {
			t.Fatalf("outcome=%+v queries=%d mutations=%d", h.outcome(), h.graphqlQueries, h.graphqlMutations)
		}
	})

	t.Run("ambiguous reply", func(t *testing.T) {
		h := newReplyHarness(t, "review_comment/43", domain.ReviewDispositionDeclined, "Already checked.")
		h.status = http.StatusInternalServerError
		h.pass()
		h.pass()
		h.now = h.now.Add(11 * time.Minute)
		h.pass()
		h.pass()
		if h.outcome().Code != externalReplyAmbiguous || h.graphqlQueries != 0 || h.graphqlMutations != 0 {
			t.Fatalf("outcome=%+v queries=%d mutations=%d", h.outcome(), h.graphqlQueries, h.graphqlMutations)
		}
	})

	t.Run("revoked reply", func(t *testing.T) {
		h := newReplyHarness(t, "review_comment/43", domain.ReviewDispositionDeclined, "Already checked.")
		h.pass()
		h.profile(false)
		h.pass()
		h.profile(true)
		h.pass()
		if h.outcome().Code != externalReplyRevoked || h.graphqlQueries != 0 || h.graphqlMutations != 0 {
			t.Fatalf("outcome=%+v queries=%d mutations=%d", h.outcome(), h.graphqlQueries, h.graphqlMutations)
		}
	})

	t.Run("closed run before resolve", func(t *testing.T) {
		h := newReplyHarness(t, "review_comment/43", domain.ReviewDispositionDeclined, "Already checked.")
		h.pass()
		h.pass()
		closeWorkflowForTest(t, h.f.st, h.f.run.ID, h.now)
		h.pass()
		if h.graphqlQueries != 0 || h.graphqlMutations != 0 {
			t.Fatalf("closed run called GraphQL: queries=%d mutations=%d", h.graphqlQueries, h.graphqlMutations)
		}
		if _, found := h.resolution(); found {
			t.Fatal("closed run recorded resolution")
		}
	})

	t.Run("closed run during resolve records nothing", func(t *testing.T) {
		h := newReplyHarness(t, "review_comment/43", domain.ReviewDispositionDeclined, "Already checked.")
		h.pass()
		h.pass()
		h.graphqlErrors = true
		closed := false
		h.beforeGraphQL = func() {
			if !closed {
				closed = true
				closeWorkflowForTest(t, h.f.st, h.f.run.ID, h.now)
			}
		}
		h.pass()
		if _, found := h.resolution(); found {
			t.Fatal("closed run wrote resolution row")
		}
		h.read(func(tx *store.ReadTx) error {
			items, err := tx.ListOpenAttentionItems(context.Background(), domain.AttentionSystemHealth)
			if err == nil && len(items) != 0 {
				t.Fatalf("closed run raised notices: %+v", items)
			}
			return err
		})
	})

	t.Run("reviewer no longer admitted", func(t *testing.T) {
		h := newReplyHarness(t, "review_comment/43", domain.ReviewDispositionDeclined, "Already checked.")
		h.pass()
		h.pass()
		h.profile(false)
		h.pass()
		if h.graphqlQueries != 0 || h.graphqlMutations != 0 {
			t.Fatalf("revoked reviewer called GraphQL: queries=%d mutations=%d", h.graphqlQueries, h.graphqlMutations)
		}
		if _, found := h.resolution(); found {
			t.Fatal("revoked reviewer recorded resolution")
		}
	})

	t.Run("contradictory stored intent fails closed", func(t *testing.T) {
		h := newReplyHarness(t, "review_comment/43", domain.ReviewDispositionDeclined, "Already checked.")
		h.pass()
		h.pass()
		var intent externalReplyIntent
		h.read(func(tx *store.ReadTx) error {
			row, err := tx.GetOutbox(context.Background(), externalReplyKey(h.d.FindingID, h.d.Round))
			if err != nil {
				return err
			}
			intent, err = decodeExternalReplyIntent(row)
			return err
		})
		intent.Thread = "review_comment/999"
		payload, err := json.Marshal(intent)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.f.st.Close(); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", h.f.path)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.ExecContext(context.Background(), `UPDATE outbox SET payload = ?, payload_digest = ? WHERE idempotency_key = ?`,
			payload, contentaddr.Sum(payload), externalReplyKey(h.d.FindingID, h.d.Round))
		closeErr := db.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("rewrite intent: %v, %v", err, closeErr)
		}
		h.restart()
		if err := h.r.Pass(context.Background()); err == nil || h.graphqlQueries != 0 || h.graphqlMutations != 0 {
			t.Fatalf("contradictory intent resolved: err=%v queries=%d mutations=%d", err, h.graphqlQueries, h.graphqlMutations)
		}
	})
}

func TestExternalReviewReplierThreadResolutionRespectsSiblingFindings(t *testing.T) {
	t.Parallel()

	t.Run("fixed sibling keeps thread open", func(t *testing.T) {
		h := newReplyHarnessThreads(t, "review_comment/43", "review_comment/43", domain.ReviewDispositionDeclined, "Already checked.")
		fix := h.review(3, strings.Repeat("b", 40))
		h.review(4, h.f.binding.HeadSHA)
		fixedSibling := domain.ExternalFindingDisposition{
			FindingID:               h.second.FindingID,
			RunID:                   h.f.run.ID,
			Round:                   2,
			Disposition:             domain.ReviewDispositionFixed,
			Reason:                  "Reviewed a remediated head.",
			RemediationInvocationID: &fix.InvocationID,
			CreatedAt:               h.now,
		}
		h.write(func(tx *store.WriteTx) error {
			return tx.PutExternalFindingDisposition(context.Background(), fixedSibling)
		})
		// Drive enough passes for both replies to post and for later resolution passes to run.
		for range 5 {
			h.pass()
		}
		if h.outcome().CommentID == 0 || h.outcome().Code != "" {
			t.Fatalf("declined outcome = %+v, want posted reply", h.outcome())
		}
		if h.creates != 2 || h.graphqlQueries != 0 || h.graphqlMutations != 0 {
			t.Fatalf("creates=%d queries=%d mutations=%d", h.creates, h.graphqlQueries, h.graphqlMutations)
		}
		if _, found := h.resolution(); found {
			t.Fatal("fixed sibling allowed thread resolution")
		}
	})

	t.Run("sibling with no disposition keeps thread open", func(t *testing.T) {
		h := newReplyHarnessThreads(t, "review_comment/43", "review_comment/43", domain.ReviewDispositionDeclined, "Already checked.")
		h.pass()
		h.pass()
		h.pass()
		if h.outcome().CommentID == 0 || h.outcome().Code != "" {
			t.Fatalf("declined outcome = %+v, want posted reply", h.outcome())
		}
		if h.graphqlQueries != 0 || h.graphqlMutations != 0 {
			t.Fatalf("undispositioned sibling allowed GraphQL: queries=%d mutations=%d", h.graphqlQueries, h.graphqlMutations)
		}
		if _, found := h.resolution(); found {
			t.Fatal("undispositioned sibling allowed thread resolution")
		}
	})

	t.Run("app reply sibling is ignored", func(t *testing.T) {
		h := newReplyHarness(t, "review_comment/43", domain.ReviewDispositionDeclined, "Already checked.")
		// 123 matches the BotUserID returned by replyTestBoundary.Resolve.
		botReply, err := domain.NewExternalFinding(domain.ExternalFindingInput{
			RunID: h.f.run.ID, Forge: domain.ExternalReviewForgeGitHub,
			ReviewerAccountID: 123, ReviewerLogin: "test[bot]", ThreadID: "review_comment/43",
			HeadSHA: h.f.binding.HeadSHA, Message: "Declined: Already checked.", RawText: "Declined: Already checked.", CreatedAt: h.now,
		})
		if err != nil {
			t.Fatal(err)
		}
		h.write(func(tx *store.WriteTx) error { return tx.PutExternalFinding(context.Background(), botReply) })
		h.pass()
		h.pass()
		h.pass()
		h.pass()
		if h.graphqlQueries != 1 || h.graphqlMutations != 1 || !h.threads[0].isResolved {
			t.Fatalf("queries=%d mutations=%d resolved=%t", h.graphqlQueries, h.graphqlMutations, h.threads[0].isResolved)
		}
		if res, found := h.resolution(); !found || res.Code != "" {
			t.Fatalf("resolution = (%+v, %t), want empty code", res, found)
		}
	})

	t.Run("two declined siblings resolve once", func(t *testing.T) {
		h := newReplyHarnessThreads(t, "review_comment/43", "review_comment/43", domain.ReviewDispositionDeclined, "Already checked.")
		h.write(func(tx *store.WriteTx) error { return tx.PutExternalFindingDisposition(context.Background(), h.second) })
		// Passes 1-3 open and post both replies; neither resolves before both post.
		h.pass()
		h.pass()
		h.pass()
		if h.creates != 2 || h.graphqlQueries != 0 || h.graphqlMutations != 0 {
			t.Fatalf("before both settled: creates=%d queries=%d mutations=%d", h.creates, h.graphqlQueries, h.graphqlMutations)
		}
		// Pass 4 resolves on the first sibling and observes already-resolved on the second.
		h.pass()
		h.pass()
		if h.graphqlMutations != 1 || !h.threads[0].isResolved {
			t.Fatalf("queries=%d mutations=%d resolved=%t", h.graphqlQueries, h.graphqlMutations, h.threads[0].isResolved)
		}
		for name, d := range map[string]domain.ExternalFindingDisposition{"first": h.d, "second": h.second} {
			if res, found := h.resolutionFor(d); !found || res.Code != "" {
				t.Fatalf("%s resolution = (%+v, %t), want empty code", name, res, found)
			}
		}
	})
}
