package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type issueReadTestTokens struct{}

func (issueReadTestTokens) Token(context.Context, string) (publish.InstallationToken, error) {
	return publish.InstallationToken{Token: publish.Secret("issue-read-token")}, nil
}

// stubNoOpenIssues gives the next engine a lister whose repository has no
// open issues, so each follow-up filing's duplicate search records "new"
// without a site call and the filing is proposed.
func (p *productionPublicationHarness) stubNoOpenIssues(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(srv.Close)
	p.openIssues = publish.NewOpenIssueLister(issueReadTestTokens{}, srv.Client(), srv.URL)
}

// openFollowUpFilings returns the open follow_up_filing cards and the filings
// their batches hold. A filing proposal needs the run's work-unit
// declaration, so these tests submit with a bound issue.
func (p *productionPublicationHarness) openFollowUpFilings(
	t *testing.T,
) ([]domain.AttentionItem, []domain.FollowUpFilingParameters) {
	t.Helper()
	var (
		cards   []domain.AttentionItem
		filings []domain.FollowUpFilingParameters
		batches []domain.ProposalBatchID
	)
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		items, err := tx.ListAttentionItems(p.ctx)
		if err != nil {
			return err
		}
		for _, snapshot := range items {
			item := snapshot.Value
			if item.Type != domain.AttentionEffectProposal || item.Status != domain.StatusOpen {
				continue
			}
			cards = append(cards, item)
			batch := domain.ProposalBatchID(item.Subject.ID)
			if slices.Contains(batches, batch) {
				continue
			}
			batches = append(batches, batch)
			instances, err := tx.ListProposalBatch(p.ctx, batch)
			if err != nil {
				return err
			}
			for _, instance := range instances {
				if instance.Proposal.FilingProposal != nil {
					filings = append(filings, *instance.Proposal.FilingProposal)
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return cards, filings
}

// assertExternalFiling checks that one filing names the finding as its source
// under kind and carries the reviewer's words only quoted.
func assertExternalFiling(
	t *testing.T, filing domain.FollowUpFilingParameters, finding domain.Finding, kind domain.FollowUpSourceKind,
) {
	t.Helper()
	if filing.Source.FindingID != finding.ID || filing.Source.Kind != kind {
		t.Errorf("filing source = %#v, want %s for %q", filing.Source, kind, finding.ID)
	}
	quoted := `"` + finding.Message + `"`
	if filing.Title.Text != "External review finding: "+quoted {
		t.Errorf("filing title = %q", filing.Title.Text)
	}
	for _, want := range []string{
		`A reviewer outside Freeside, "maintainer", left this finding`,
		"Reviewer's words: `" + quoted + "`",
	} {
		if !strings.Contains(filing.Body.Text, want) {
			t.Errorf("filing body lacks %q:\n%s", want, filing.Body.Text)
		}
	}
}

// TestExternalReviewDeferralProposesOneFollowUpFiling: adjudication defers an
// admitted external finding, so the cycle's first round records its deferred
// outcome and, in the same transaction, proposes one follow-up issue for it
// (issue #1787). The run is ready again, and a replay proposes nothing more.
func TestExternalReviewDeferralProposesOneFollowUpFiling(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarnessWithBoundIssue(t, "", 79)
	p.stubNoOpenIssues(t)
	driver := p.configureAdjudicator(t)
	_, findings := p.startAdjudicatedExternalReview(t, "rename this too")

	p.scriptCleanReview(2, p.baseSHA, p.replay.HeadSHA)
	scriptAdjudication(t, driver, deferVerdict(findings[0].ID))
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
		t.Fatalf("cycle result = %#v, %v", result, err)
	}
	dispositions := p.externalDispositions(t)
	if len(dispositions) != 1 || dispositions[0].Disposition != domain.ReviewDispositionDeferred {
		t.Fatalf("external dispositions = %#v, want one deferral", dispositions)
	}
	cards, filings := p.openFollowUpFilings(t)
	if len(cards) != 1 || len(filings) != 1 {
		t.Fatalf("follow-up filings = %d cards, %d proposals, want one of each", len(cards), len(filings))
	}
	assertExternalFiling(t, filings[0], findings[0], domain.FollowUpSourceDeferredDisposition)
	if filings[0].Source.AdjudicationDigest != dispositions[0].Adjudication() {
		t.Errorf("filing adjudication = %q, want the deferral's %q",
			filings[0].Source.AdjudicationDigest, dispositions[0].Adjudication())
	}

	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("converged replay = %#v, %v", result, err)
	}
	if cards, filings := p.openFollowUpFilings(t); len(cards) != 1 || len(filings) != 1 {
		t.Fatalf("after replay: %d cards, %d proposals, want one of each", len(cards), len(filings))
	}
}

// TestExternalReviewSeparateWorkProposesOneFollowUpFiling: adjudication parks
// an admitted external finding as separate work. Once the person accepts the
// card, the cycle proposes one follow-up issue for it and still ends on the
// handoff item, because the finding has no outcome in this pull request.
func TestExternalReviewSeparateWorkProposesOneFollowUpFiling(t *testing.T) {
	t.Parallel()
	p := newProductionPublicationHarnessWithBoundIssue(t, "", 79)
	p.stubNoOpenIssues(t)
	driver := p.configureAdjudicator(t)
	_, findings := p.startAdjudicatedExternalReview(t, "this leaks the handle")

	p.scriptCleanReview(2, p.baseSHA, p.replay.HeadSHA)
	scriptAdjudication(t, driver, parkVerdict(findings[0].ID))
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("cycle result = %#v, %v", result, err)
	}
	if cards, filings := p.openFollowUpFilings(t); len(cards) != 0 || len(filings) != 0 {
		t.Fatalf("an undecided card proposed %d filings", len(filings))
	}
	var card domain.AttentionItem
	if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
		var err error
		card, err = tx.GetAttentionItem(p.ctx, productionReviewItemIDForTest(p.runID, 2))
		return err
	}); err != nil {
		t.Fatal(err)
	}

	p.decideAdjudicationCard(t, card, domain.ActionAcceptRecommendedRoute)
	if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 0 {
		t.Fatalf("decided cycle = %#v, %v", result, err)
	}
	cards, filings := p.openFollowUpFilings(t)
	if len(cards) != 1 || len(filings) != 1 {
		t.Fatalf("follow-up filings = %d cards, %d proposals, want one of each", len(cards), len(filings))
	}
	assertExternalFiling(t, filings[0], findings[0], domain.FollowUpSourceSeparateWorkVerdict)
	end := p.externalReviewEnd(t)
	if end.readyItem || end.pending != 0 || len(end.reviewItems) != 1 ||
		end.reviewItems[0].ID != domain.ItemID("production-external-review-handoff-"+string(p.runID)+"-2") {
		t.Fatalf("decided cycle end = %#v", end)
	}
	if dispositions := p.externalDispositions(t); len(dispositions) != 0 {
		t.Fatalf("a parked finding got an outcome: %#v", dispositions)
	}
}
