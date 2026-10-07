package publish

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type replyHarness struct {
	t             *testing.T
	f             replyBindingFixture
	r             *ExternalReviewReplier
	now           time.Time
	d             domain.ExternalFindingDisposition
	second        domain.ExternalFindingDisposition
	authority     domain.PublicationSuccessor
	comments      []replyCommentWire
	bodies        []string
	creates       int
	status        int
	commitUnknown bool
	step          func(string)
	beforeSend    func()
}

type replyTestBoundary struct{ h *replyHarness }

func (b replyTestBoundary) Token(ctx context.Context, _ string) (InstallationToken, error) {
	if b.h.step != nil {
		b.h.step("token")
	}
	if ctx.Err() != nil {
		return InstallationToken{}, ctx.Err()
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
	if req.Method == http.MethodGet {
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
	record, err := domain.NewReviewRecord(domain.ReviewRecord{
		InvocationID: domain.InvocationID(fmt.Sprintf("reply-review-%d", round)), RunID: h.f.run.ID, Round: round,
		Provider: "openai", ModelConfiguration: "test", ConfigurationDigest: digest, InstructionDigest: digest, CostOwner: "owner",
		BaseSHA: h.f.admission.Base.BaseSHA, HeadSHA: head, CompletedAt: h.now, CompletionEvidence: digest, Outcome: domain.ReviewClean,
	})
	if err != nil {
		h.t.Fatal(err)
	}
	h.write(func(tx *store.WriteTx) error { return tx.PutReviewRecord(context.Background(), record, nil) })
	return record
}

func newReplyHarness(t *testing.T, thread string, disposition domain.ReviewDisposition, reason string) *replyHarness {
	t.Helper()
	h := &replyHarness{t: t, now: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC), comments: []replyCommentWire{}}
	h.f = seedReplyBinding(t, "feat/test-replies", strings.Repeat("1", 40), strings.Repeat("a", 40))
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
		return tx.PutAttentionItem(ctx, item)
	})
	second, err := domain.NewExternalFinding(domain.ExternalFindingInput{
		RunID: h.f.run.ID, Forge: domain.ExternalReviewForgeGitHub,
		ReviewerAccountID: 41, ReviewerLogin: "reviewer", ThreadID: "review_comment/44", HeadSHA: h.f.binding.HeadSHA,
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
		d.AdjudicationDigest = &a.Digest
	}
	h.write(func(tx *store.WriteTx) error { return tx.PutExternalFindingDisposition(ctx, d) })
	h.d = d
	h.second = d
	h.second.FindingID = second.ID
	h.restart()
	return h
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
	for _, thread := range []string{"review/42", "review_comment/43"} {
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
		})
	}
}

func TestExternalReviewReplierSurvivesEachBoundary(t *testing.T) {
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
	h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, "Already checked.")
	h.pass()
	h.now = h.now.Add(-time.Second)
	h.pass()
	if h.creates != 1 || h.outcome().CommentID != 101 {
		t.Fatalf("outcome = %+v, creates = %d", h.outcome(), h.creates)
	}
}

func TestExternalReviewReplierDoesNotAdoptSkewedUnknownResponse(t *testing.T) {
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

func TestExternalReviewReplierIgnoresNeverListedFinding(t *testing.T) {
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
	body, err := externalReviewReplyText(h.d, thread, binding, i.Ruleset)
	if err != nil {
		t.Fatal(err)
	}
	i.TextDigest = contentaddr.Sum([]byte(body))
	if err := h.r.send(context.Background(), h.f.run, h.d, source, i); err == nil || h.creates != 0 {
		t.Fatalf("substituted head accepted: %v", err)
	}
}

func TestExternalReviewReplierDoesNotAdoptExistingOrForeignComment(t *testing.T) {
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
	synctest.Test(t, func(t *testing.T) {
		h := newReplyHarness(t, "review/42", domain.ReviewDispositionDeclined, "Already checked.")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- h.r.Run(ctx, time.Minute, func(err error) { t.Error(err) }) }()
		synctest.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()
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
