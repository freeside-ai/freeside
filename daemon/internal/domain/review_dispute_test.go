package domain_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func TestReviewDisputeRoutesRespectCanonicalScope(t *testing.T) {
	for _, path := range []string{"daemon/a.go", "app/a.go", "daemon/../app/a.go", "daemon//a.go", "/daemon/a.go", "daemon/./a.go", "daemon/a\\b.go", "daemon/a\n.go"} {
		finding := domain.Finding{Location: &domain.FindingLocation{Path: path}}
		routes := domain.ReviewDisputeRoutes(finding, []string{"daemon/**"})
		if slices.Contains(routes, domain.RouteRemediate) != (path == "daemon/a.go") {
			t.Errorf("%q routes = %v", path, routes)
		}
	}
	if slices.Contains(domain.ReviewDisputeRoutes(domain.Finding{}, []string{"**"}), domain.RouteRemediate) {
		t.Fatal("locationless remediation offered")
	}
}

func TestReviewDisputeBindingPreservesUnfingerprintableMessage(t *testing.T) {
	row := domain.DisputedFinding{
		FindingID: "finding-1", Message: " \t\n", Severity: domain.FindingSeverityP1,
		OfferedRoutes: []domain.AdjudicationRoute{domain.RouteDecline, domain.RouteDefer},
	}
	binding := domain.ReviewDisputeBinding{
		RunID: "run-human", Round: 1, FindingIDs: []domain.FindingID{row.FindingID},
		CompletionEvidence: adjDigest("completion"), Findings: []domain.DisputedFinding{row},
	}
	if err := binding.Validate(); err != nil {
		t.Fatalf("unfingerprintable message rejected: %v", err)
	}
}

func TestHumanFindingAdjudicationGoldenAndRoundTrip(t *testing.T) {
	decision := domain.AdjudicationDecision{CommandID: "human-command", ItemID: domain.ProductionFindingAdjudicationItemID("run-human", 1, 1), ItemVersion: 2}
	entry, err := domain.NewHumanAdjudicationEntry("finding-1", domain.RouteDecline)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := domain.NewHumanFindingAdjudication("run-human", 1, adjDigest("spec"), adjDigest("instruction"), adjDigest("policy"),
		[]domain.FindingAdjudicationEntry{entry}, decision, time.Date(2026, 10, 13, 14, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "finding_adjudication_human", append(body, '\n'))
	var decoded domain.FindingAdjudication
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.Validate(); err != nil {
		t.Fatal(err)
	}
	if decoded.Digest != artifact.Digest {
		t.Fatal("human round-trip changed digest")
	}
	row := domain.DisputedFinding{
		FindingID: "finding-1", Message: "The update can be applied twice.", Severity: domain.FindingSeverityP1,
		Location:      &domain.FindingLocation{Path: "daemon/update.go", StartLine: 42, EndLine: 45},
		External:      &domain.DisputedFindingExternal{ReviewerLogin: "reviewer", ThreadID: "thread-1"},
		OfferedRoutes: []domain.AdjudicationRoute{domain.RouteRemediate, domain.RouteDecline, domain.RouteDefer},
	}
	binding := domain.ReviewDisputeBinding{RunID: "run-human", Round: 1, FindingIDs: []domain.FindingID{row.FindingID}, CompletionEvidence: adjDigest("completion"), Findings: []domain.DisputedFinding{row}}
	if err := binding.Validate(); err != nil {
		t.Fatal(err)
	}
	body, err = json.MarshalIndent(row, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "disputed_finding", append(body, '\n'))
	binding.FindingIDs = append(binding.FindingIDs, "finding-2")
	if err := binding.Validate(); err == nil {
		t.Fatal("incomplete finding rows accepted")
	}
}
