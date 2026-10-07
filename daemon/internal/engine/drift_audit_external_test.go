package engine

import (
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestDriftAuditEntriesLeaveOutExternalFindings: the drift audit judges the
// routes Freeside's own findings took. An external entry names a finding no
// review record lists, which the store refuses in a reversal, so the input
// leaves it out and keeps the rest of its round in order.
func TestDriftAuditEntriesLeaveOutExternalFindings(t *testing.T) {
	t.Parallel()
	external := externalFindingForTest(t, 0, "maintainer", "this leaks the handle")
	first := adjudicationRouteEntry(t, "finding-first", domain.RouteDefer)
	second := adjudicationRouteEntry(t, "finding-second", domain.RouteRemediate)
	quoted := adjudicationRouteEntry(t, external.ID, domain.RouteRemediate)
	adjudications := []domain.FindingAdjudication{
		{Round: 1, Entries: []domain.FindingAdjudicationEntry{first}},
		{Round: 2, Revision: 1, Entries: []domain.FindingAdjudicationEntry{quoted}},
		{Round: 2, Revision: 2, Entries: []domain.FindingAdjudicationEntry{quoted, second}},
	}
	got := driftAuditEntries(adjudications, []domain.Finding{external})
	if len(got) != 2 || got[0].FindingID != first.FindingID || got[1].FindingID != second.FindingID {
		t.Fatalf("filtered entries = %#v, want the two review findings", got)
	}
	if all := driftAuditEntries(adjudications, nil); len(all) != 3 {
		t.Fatalf("unfiltered entries = %d, want 3", len(all))
	}
}
