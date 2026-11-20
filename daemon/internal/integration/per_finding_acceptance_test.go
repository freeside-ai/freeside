package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strayrequest"
)

// TestExternalReviewPerFindingAcceptance is #3436's case: a round with two
// parked findings and one auto-declined external finding shows all three on
// the card, records and replies nothing while the card is open, even across
// a restart, and then records the settled finding with the route the person
// left: the adjudicator's decline when the row is left out, or the person's
// deferral when they override it.
func TestExternalReviewPerFindingAcceptance(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		settled     []signet.FindingRouteChoice
		disposition domain.ReviewDisposition
		reply       string
	}{
		"kept": {nil, domain.ReviewDispositionDeclined, "Freeside declined this finding"},
		"overridden": {
			[]signet.FindingRouteChoice{{Route: domain.RouteDefer, Reason: externalReplyReason}},
			domain.ReviewDispositionDeferred, "A maintainer deferred this finding.\n\nReason: " + externalReplyReason,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := newProductionPublicationHarness(t, "")
			driver := p.configureAdjudicator(t)
			_, findings := p.startAdjudicatedExternalReview(t, "[P3] unnecessary rename", "fix the handle", "check the close")
			pr := p.forge.PullRequests()[0]
			p.scriptCleanReview(2, p.baseSHA, p.replay.HeadSHA)
			parked := []adjudicatorVerdict{remediateVerdict(findings[1].ID), remediateVerdict(findings[2].ID)}
			for index := range parked {
				parked[index].confidence = "low"
			}
			scriptAdjudication(t, driver, declineVerdict(findings[0].ID), parked[0], parked[1])
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatal(err)
			}
			replies := newRecordedReplies(t, p, pr.Number)
			assertWaiting := func() domain.AttentionItem {
				t.Helper()
				end := p.externalReviewEnd(t)
				if end.pending != 1 || end.readyItem || len(end.reviewItems) != 1 {
					t.Fatalf("waiting round = %#v", end)
				}
				item := end.reviewItems[0]
				rows := item.ReviewDispute.Findings
				if len(rows) != 3 || rows[0].FindingID != findings[0].ID || rows[0].SettledRoute == nil ||
					*rows[0].SettledRoute != domain.RouteDecline || rows[1].SettledRoute != nil || rows[2].SettledRoute != nil {
					t.Fatalf("dispute rows = %#v", rows)
				}
				if dispositions := p.externalDispositions(t); len(dispositions) != 0 {
					t.Fatalf("outcomes recorded while the card is open: %#v", dispositions)
				}
				if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
					rows, err := tx.ListPendingOutbox(p.ctx, engine.KindRemediationInvocationRequested)
					if err == nil && len(rows) != 0 {
						t.Fatal("remediation queued while dispute is open")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				replies.pass(t, p)
				if len(replies.bodies) != 0 {
					t.Fatalf("replied while the card is open: %q", replies.bodies)
				}
				return item
			}
			item := assertWaiting()
			if !strings.Contains(item.Reason, `"check the close"`) || strings.Contains(item.Reason, "Already answered in an earlier cycle") {
				t.Fatalf("external context confused this round with an earlier cycle: %s", item.Reason)
			}
			p.restartDurableState(t)
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatal(err)
			}
			item = assertWaiting()

			routes := []signet.FindingRouteChoice{externalChoice(findings[1].ID, domain.RouteRemediate), externalChoice(findings[2].ID, domain.RouteRemediate)}
			for _, choice := range tc.settled {
				choice.FindingID = findings[0].ID
				routes = append(routes, choice)
			}
			if _, err := p.attention.Submit(p.ctx, p.externalDisputeCommand(t, item, routes...)); err != nil {
				t.Fatal(err)
			}
			if _, err := p.reconcileLanes(); err != nil {
				t.Fatal(err)
			}
			// The answer records the settled finding's outcome: the next
			// replier sweep queues its reply, and the one after posts it.
			replies.pass(t, p)
			replies.pass(t, p)
			if len(replies.bodies) != 1 || !strings.Contains(replies.bodies[0], tc.reply) {
				t.Fatalf("replies = %q, want one containing %q", replies.bodies, tc.reply)
			}
			if strings.Contains(replies.bodies[0], "Freeside declined") != (tc.reply == "Freeside declined this finding") {
				t.Fatalf("reply credits the wrong decider: %q", replies.bodies[0])
			}
			_, input := p.remediateExternalCycle(t, 2, "production change\nfixed handle and close\n")
			assertExternalFindingsRemediated(t, input, findings[1:])
			dispositions := p.externalDispositions(t)
			if len(dispositions) != 3 {
				t.Fatalf("final outcomes = %#v", dispositions)
			}
			for _, d := range dispositions {
				want := domain.ReviewDispositionFixed
				if d.FindingID == findings[0].ID {
					want = tc.disposition
				}
				if d.Disposition != want {
					t.Fatalf("outcome = %#v, want %s", d, want)
				}
			}
			replies.pass(t, p)
			if len(replies.bodies) != 1 {
				t.Fatalf("recorded reply was sent again: %q", replies.bodies)
			}
			if prs := p.forge.PullRequests(); len(prs) != 1 || prs[0].Number != pr.Number {
				t.Fatal("remediation replaced the pull request")
			}
		})
	}
}

// TestExternalReviewBlockingLabelWaitsForPerson: a rated external finding its
// reviewer labeled blocking is declined at high confidence, yet it waits on
// the card like a P0 or P1 instead of settling and replying.
func TestExternalReviewBlockingLabelWaitsForPerson(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarness(t, "")
	driver := p.configureAdjudicator(t)
	// The harness stores every external finding rated P2, as the badge here
	// reads; the blocking label alone holds it.
	_, findings := p.startAdjudicatedExternalReview(t, "[P2, blocking] Declare 401")
	p.scriptCleanReview(2, p.baseSHA, p.replay.HeadSHA)
	scriptAdjudication(t, driver, declineVerdict(findings[0].ID))
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	end := p.externalReviewEnd(t)
	if end.readyItem || len(end.reviewItems) != 1 {
		t.Fatalf("round end = %#v", end)
	}
	rows := end.reviewItems[0].ReviewDispute.Findings
	if len(rows) != 1 || rows[0].FindingID != findings[0].ID || rows[0].SettledRoute != nil {
		t.Fatalf("dispute rows = %#v", rows)
	}
	if dispositions := p.externalDispositions(t); len(dispositions) != 0 {
		t.Fatalf("blocking finding declined without a person: %#v", dispositions)
	}
}

// recordedReplies runs the external replier against a local forge fixture
// and keeps every reply body it posts to the pull request's review threads.
// Each pass builds its replier on the harness's current store, which a
// restart replaces.
type recordedReplies struct {
	server *httptest.Server
	bodies []string
}

func newRecordedReplies(t *testing.T, p *productionPublicationHarness, prNumber int) *recordedReplies {
	t.Helper()
	recorded := &recordedReplies{}
	recorded.server = httptest.NewServer(strayrequest.Ignore(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("[]"))
			return
		}
		want := fmt.Sprintf("/repos/%s/pulls/%d/comments/1/replies", p.profile.Repo, prNumber)
		if r.Method != http.MethodPost || r.URL.Path != want {
			t.Errorf("unexpected reply request: %s %s, want %s", r.Method, r.URL.Path, want)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var body struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("reply body: %v", err)
		}
		recorded.bodies = append(recorded.bodies, body.Body)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 901, "user": map[string]any{"id": 7001, "type": "Bot"}, "in_reply_to_id": 1, "created_at": p.now.Format(time.RFC3339), "pull_request_url": fmt.Sprintf("%s/repos/%s/pulls/%d", recorded.server.URL, p.profile.Repo, prNumber)})
	})))
	t.Cleanup(recorded.server.Close)
	return recorded
}

func (r *recordedReplies) pass(t *testing.T, p *productionPublicationHarness) {
	t.Helper()
	replier, err := publish.NewExternalReviewReplier(p.store, integrationTokenSource(), r.server.Client(), r.server.URL, externalDecisionIdentity{}, func() time.Time { return p.now })
	if err != nil {
		t.Fatal(err)
	}
	if err := replier.Pass(p.ctx); err != nil {
		t.Fatal(err)
	}
}
