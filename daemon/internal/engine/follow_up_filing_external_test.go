package engine

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestFollowUpFilingSkipsExternalFindings: a deferred external finding
// proposes no follow-up issue (issue #1767 decision 5), while a deferred
// finding of Freeside's own in the same adjudication still does.
func TestFollowUpFilingSkipsExternalFindings(t *testing.T) {
	f := newFollowUpFilingFixture(t, domain.FindingSeverityP2, domain.RouteDefer, nil)
	seedFollowUpFilingSubject(t, f, true)
	external, err := domain.NewExternalFinding(domain.ExternalFindingInput{
		RunID: f.task.RunID, Forge: domain.ExternalReviewForgeGitHub,
		ReviewerAccountID: 4101, ReviewerLogin: "maintainer",
		ThreadID: "review_comment/1", HeadSHA: f.record.HeadSHA,
		Severity: domain.FindingSeverityP2, Message: "rename this too", RawText: "rename this too",
		CreatedAt: f.finding.CreatedAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(f.ctx, func(tx *store.WriteTx) error {
		return tx.PutExternalFinding(f.ctx, external)
	}); err != nil {
		t.Fatal(err)
	}
	artifact := domain.FindingAdjudication{
		RunID: f.task.RunID, Round: f.record.Round, Digest: adjudicationDigest("9"),
		Entries: []domain.FindingAdjudicationEntry{
			modelRouteEntry(t, f.finding.ID, domain.RouteDefer, domain.ConfidenceHigh),
			modelRouteEntry(t, external.ID, domain.RouteDefer, domain.ConfidenceHigh),
		},
	}
	routes := map[domain.FindingID]domain.AdjudicationRoute{
		f.finding.ID: domain.RouteDefer, external.ID: domain.RouteDefer,
	}
	entries := followUpFilingEntries(artifact, routes, domain.FollowUpSourceDeferredDisposition)
	if len(entries) != 2 {
		t.Fatalf("deferred entries = %#v, want both findings", entries)
	}
	if err := f.store.Read(f.ctx, func(tx *store.ReadTx) error {
		plan, err := planFollowUpFilings(f.ctx, tx, artifact, entries)
		if err != nil {
			return err
		}
		if len(plan.pending) != 1 || plan.pending[0].entry.FindingID != f.finding.ID {
			t.Errorf("planned filings = %#v, want only Freeside's own finding", plan.pending)
		}
		// With nothing else to propose the plan resolves no authority at all.
		alone, err := planFollowUpFilings(f.ctx, tx, artifact, entries[1:])
		if err != nil {
			return err
		}
		if len(alone.pending) != 0 || alone.handle != "" {
			t.Errorf("an external finding alone planned %#v", alone)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
