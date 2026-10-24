package integration_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestExternalReviewPerFindingAcceptance(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	driver := p.configureAdjudicator(t)
	_, findings := p.startAdjudicatedExternalReview(t, "[P2] unnecessary rename", "fix the handle", "check the close")
	pr := p.forge.PullRequests()[0]
	p.scriptCleanReview(2, p.baseSHA, p.replay.HeadSHA)
	waiting := remediateVerdict(findings[2].ID)
	waiting.confidence = "low"
	scriptAdjudication(t, driver, declineVerdict(findings[0].ID), remediateVerdict(findings[1].ID), waiting)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	end := p.externalReviewEnd(t)
	if end.pending != 1 || end.readyItem || len(end.reviewItems) != 1 {
		t.Fatalf("waiting round = %#v", end)
	}
	item := end.reviewItems[0]
	if item.ReviewDispute == nil || len(item.ReviewDispute.FindingIDs) != 3 || len(item.ReviewDispute.Findings) != 1 || item.ReviewDispute.Findings[0].FindingID != findings[2].ID {
		t.Fatalf("dispute = %#v", item.ReviewDispute)
	}
	if !strings.Contains(item.Reason, `"check the close"`) || strings.Contains(item.Reason, "Already answered in an earlier cycle") {
		t.Fatalf("external context confused this round with an earlier cycle: %s", item.Reason)
	}
	dispositions := p.externalDispositions(t)
	if len(dispositions) != 1 || dispositions[0].FindingID != findings[0].ID || dispositions[0].Disposition != domain.ReviewDispositionDeclined {
		t.Fatalf("early outcomes = %#v", dispositions)
	}
	early := dispositions[0]
	p.assertExternalReplyQueued(t, findings[0])
	assertNoRemediation := func() {
		t.Helper()
		if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
			rows, err := tx.ListPendingOutbox(p.ctx, engine.KindRemediationInvocationRequested)
			if err == nil && len(rows) != 0 {
				t.Fatal("remediation queued while dispute is open")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	assertNoRemediation()

	// Exercise the existing publisher through the actual thread POST while
	// the dispute is still open, using a local forge fixture.
	posts := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte("[]"))
			return
		}
		want := fmt.Sprintf("/repos/%s/pulls/%d/comments/1/replies", p.profile.Repo, pr.Number)
		if r.Method != http.MethodPost || r.URL.Path != want {
			t.Errorf("unexpected reply request: %s %s, want %s", r.Method, r.URL.Path, want)
			w.WriteHeader(400)
			return
		}
		var body struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !strings.Contains(body.Body, "Freeside declined this finding") {
			t.Errorf("reply body = %q, %v", body.Body, err)
		}
		posts++
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 901, "user": map[string]any{"id": 7001, "type": "Bot"}, "in_reply_to_id": 1, "created_at": p.now.Format(time.RFC3339), "pull_request_url": fmt.Sprintf("%s/repos/%s/pulls/%d", server.URL, p.profile.Repo, pr.Number)})
	}))
	t.Cleanup(server.Close)
	replier, err := publish.NewExternalReviewReplier(p.store, integrationTokenSource(), server.Client(), server.URL, externalDecisionIdentity{}, func() time.Time { return p.now })
	if err != nil {
		t.Fatal(err)
	}
	if err := replier.Pass(p.ctx); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatalf("thread replies = %d", posts)
	}
	if err := replier.Pass(p.ctx); err != nil {
		t.Fatal(err)
	}
	if posts != 1 {
		t.Fatal("recorded reply was sent twice")
	}
	p.restartDurableState(t)
	p.workflow = p.newEngine(t, productionCrashSeams{}, true)
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	if again := p.externalReviewEnd(t); again.pending != 1 || again.readyItem || len(again.reviewItems) != 1 {
		t.Fatalf("restart lost waiting dispute: %#v", again)
	}
	assertNoRemediation()
	command := p.externalDisputeCommand(t, item, signet.AlternativeChoice{FindingID: findings[2].ID, Route: domain.RouteRemediate})
	if _, err := p.attention.Submit(p.ctx, command); err != nil {
		t.Fatal(err)
	}
	if _, err := p.reconcileLanes(); err != nil {
		t.Fatal(err)
	}
	_, input := p.remediateExternalCycle(t, 2, "production change\nfixed handle and close\n")
	assertExternalFindingsRemediated(t, input, findings[1:])
	dispositions = p.externalDispositions(t)
	if len(dispositions) != 3 {
		t.Fatalf("final outcomes = %#v", dispositions)
	}
	for _, d := range dispositions {
		if d.FindingID == early.FindingID {
			if !reflect.DeepEqual(d, early) {
				t.Fatal("human answer changed the earlier decline")
			}
		} else if d.Disposition != domain.ReviewDispositionFixed {
			t.Fatalf("unfixed outcome = %#v", d)
		}
	}
	prs := p.forge.PullRequests()
	if len(prs) != 1 || prs[0].Number != pr.Number {
		t.Fatal("remediation replaced the pull request")
	}
}
