package signet

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestAdjudicateFindingsCommandIDStaysWithinArtifactBudget(t *testing.T) {
	commandID := strings.Repeat("\x00", MaxAdjudicateFindingsCommandIDBytes)
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: commandID, DeviceID: "device-1", ItemID: "item-1", ItemVersion: 1,
		Action:  domain.ActionAdjudicateFindings,
		Message: `[{"finding_id":"finding-1","route":"decline"}]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Service{}).validateCommandContent(command); err != nil {
		t.Fatalf("maximum command id rejected: %v", err)
	}

	digest := domain.Digest("sha256:" + strings.Repeat("a", 64))
	entry, err := domain.NewHumanAdjudicationEntry("finding-1", domain.RouteDecline)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := domain.NewHumanFindingAdjudication("run-1", 1, digest, digest, digest, []domain.FindingAdjudicationEntry{entry},
		domain.AdjudicationDecision{CommandID: commandID, ItemID: domain.ProductionFindingAdjudicationItemID("run-1", 1, 1), ItemVersion: 1},
		time.Date(2026, 10, 13, 16, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	body, err := artifact.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if len(body) > domain.MaxFindingAdjudicationBytes {
		t.Fatalf("artifact body is %d bytes, limit %d", len(body), domain.MaxFindingAdjudicationBytes)
	}
	if _, err := domain.DecodeFindingAdjudication(body); err != nil {
		t.Fatalf("maximum command id artifact does not reconstruct: %v", err)
	}

	command.CommandID += "x"
	if err := (&Service{}).validateCommandContent(command); !errors.Is(err, domain.ErrClaimTextTooLarge) {
		t.Fatalf("overlong command id error = %v, want ErrClaimTextTooLarge", err)
	}
}

func TestReviewDisputeDecisionCountsOnlyListedFindings(t *testing.T) {
	item := domain.AttentionItem{
		ID: "subset", Type: domain.AttentionReviewDispute, Status: domain.StatusOpen,
		ItemVersion: 1, RequestedDecision: []domain.Action{domain.ActionAdjudicateFindings},
		ReviewDispute: &domain.ReviewDisputeBinding{
			FindingIDs: []domain.FindingID{"first", "second", "omitted"},
			Findings: []domain.DisputedFinding{
				{FindingID: "first", OfferedRoutes: []domain.AdjudicationRoute{domain.RouteDecline}},
				{FindingID: "second", OfferedRoutes: []domain.AdjudicationRoute{domain.RouteDecline}},
			},
		},
	}
	for _, tc := range []struct {
		message string
		want    error
	}{
		{`[{"finding_id":"first","route":"decline"},{"finding_id":"second","route":"decline"}]`, nil},
		{`[{"finding_id":"first","route":"decline"}]`, ErrFindingRoutesIncomplete},
		{`[{"finding_id":"first","route":"decline"},{"finding_id":"omitted","route":"decline"}]`, ErrFindingRouteNotOffered},
	} {
		command, err := domain.NewCommand(domain.CommandInput{
			CommandID: "answer", DeviceID: "device", ItemID: item.ID,
			ItemVersion: item.ItemVersion, Action: domain.ActionAdjudicateFindings, Message: tc.message,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := validateReviewDisputeDecision(command, item); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.message, err, tc.want)
		}
	}
}
