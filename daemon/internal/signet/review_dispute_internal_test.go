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
	entry, err := domain.NewHumanAdjudicationEntry("finding-1", domain.RouteDecline, "")
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

// TestReviewDisputeDecisionRequiresReasonOnExternalReplies pins the reason
// rule: a decline or defer of an external finding posts a reply, so it needs
// the person's reason, and every other choice refuses one (#2352).
func TestReviewDisputeDecisionRequiresReasonOnExternalReplies(t *testing.T) {
	item := domain.AttentionItem{
		ID: "dispute", Type: domain.AttentionReviewDispute, Status: domain.StatusOpen,
		ItemVersion: 1, RequestedDecision: []domain.Action{domain.ActionAdjudicateFindings},
		ReviewDispute: &domain.ReviewDisputeBinding{
			FindingIDs: []domain.FindingID{"external", "internal"},
			Findings: []domain.DisputedFinding{
				{
					FindingID: "external", External: &domain.DisputedFindingExternal{ReviewerLogin: "reviewer", ThreadID: "review_comment/1"},
					OfferedRoutes: []domain.AdjudicationRoute{domain.RouteRemediate, domain.RouteDecline, domain.RouteDefer},
				},
				{FindingID: "internal", OfferedRoutes: []domain.AdjudicationRoute{domain.RouteDecline, domain.RouteDefer}},
			},
		},
	}
	internal := FindingRouteChoice{FindingID: "internal", Route: domain.RouteDecline}
	for name, tc := range map[string]struct {
		external   FindingRouteChoice
		internal   FindingRouteChoice
		want       error
		wantReason string
	}{
		"decline with reason":   {FindingRouteChoice{"external", domain.RouteDecline, "Out of scope."}, internal, nil, "Out of scope."},
		"defer with reason":     {FindingRouteChoice{"external", domain.RouteDefer, "Tracked in #12."}, internal, nil, "Tracked in #12."},
		"reason is trimmed":     {FindingRouteChoice{"external", domain.RouteDecline, " \n Out of scope.\t "}, internal, nil, "Out of scope."},
		"at the byte bound":     {FindingRouteChoice{"external", domain.RouteDecline, strings.Repeat("é", domain.MaxHumanAdjudicationReasonBytes/2)}, internal, nil, strings.Repeat("é", domain.MaxHumanAdjudicationReasonBytes/2)},
		"decline without":       {FindingRouteChoice{"external", domain.RouteDecline, ""}, internal, ErrFindingRouteReasonInvalid, ""},
		"defer without":         {FindingRouteChoice{"external", domain.RouteDefer, ""}, internal, ErrFindingRouteReasonInvalid, ""},
		"blank reason":          {FindingRouteChoice{"external", domain.RouteDecline, " \t\n "}, internal, ErrFindingRouteReasonInvalid, ""},
		"over the byte bound":   {FindingRouteChoice{"external", domain.RouteDecline, strings.Repeat("x", domain.MaxHumanAdjudicationReasonBytes+1)}, internal, ErrFindingRouteReasonInvalid, ""},
		"invalid UTF-8":         {FindingRouteChoice{"external", domain.RouteDecline, "why \xff"}, internal, ErrFindingRouteReasonInvalid, ""},
		"reason on remediate":   {FindingRouteChoice{"external", domain.RouteRemediate, "Fix it."}, internal, ErrFindingRouteReasonInvalid, ""},
		"reason on an internal": {FindingRouteChoice{"external", domain.RouteDecline, "Out of scope."}, FindingRouteChoice{"internal", domain.RouteDecline, "Also out."}, ErrFindingRouteReasonInvalid, ""},
		// The reply would refuse these for good after the decision, so the
		// decision refuses them while the person can rewrite the reason.
		"a mention":            {FindingRouteChoice{"external", domain.RouteDecline, "Agreed with @alice."}, internal, ErrFindingRouteReasonUnpublishable, ""},
		"a slash command line": {FindingRouteChoice{"external", domain.RouteDefer, "Later.\n/close"}, internal, ErrFindingRouteReasonUnpublishable, ""},
		"a line separator":     {FindingRouteChoice{"external", domain.RouteDecline, "One\u2028two"}, internal, ErrFindingRouteReasonUnpublishable, ""},
		"a publisher marker":   {FindingRouteChoice{"external", domain.RouteDecline, "freeside: skip"}, internal, ErrFindingRouteReasonUnpublishable, ""},
	} {
		t.Run(name, func(t *testing.T) {
			message, err := decisionMessage(DecisionPayload{
				Action: domain.ActionAdjudicateFindings, FindingRoutes: []FindingRouteChoice{tc.external, tc.internal},
			})
			if err == nil {
				var command domain.Command
				command, err = domain.NewCommand(domain.CommandInput{
					CommandID: "decide", DeviceID: "device", ItemID: item.ID,
					ItemVersion: item.ItemVersion, Action: domain.ActionAdjudicateFindings, Message: message,
				})
				if err != nil {
					t.Fatal(err)
				}
				err = validateReviewDisputeDecision(command, item)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if err != nil {
				if !isCommandRequestError(err) {
					t.Fatalf("refusal %v is not a 400", err)
				}
				return
			}
			choices, err := decodeFindingRoutes(message)
			if err != nil || choices[0].Reason != tc.wantReason || choices[1].Reason != "" {
				t.Fatalf("stored choices = %#v, %v", choices, err)
			}
		})
	}
}

// TestFindingRoutesKeepTheReasonlessMessage pins the stored form a command
// had before reasons existed, which the store gate re-marshals byte for byte,
// and that a closed card's routes drop the reason.
func TestFindingRoutesKeepTheReasonlessMessage(t *testing.T) {
	const reasonless = `[{"finding_id":"a","route":"remediate"},{"finding_id":"b","route":"decline"}]`
	message, err := decisionMessage(DecisionPayload{
		Action: domain.ActionAdjudicateFindings,
		FindingRoutes: []FindingRouteChoice{
			{FindingID: "b", Route: domain.RouteDecline}, {FindingID: "a", Route: domain.RouteRemediate},
		},
	})
	if err != nil || message != reasonless {
		t.Fatalf("message = %s, %v", message, err)
	}
	if _, err := decodeFindingRoutes(`[{"finding_id":"b","route":"decline","reason":" why"}]`); err == nil {
		t.Fatal("accepted an untrimmed stored reason")
	}
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: "decide", DeviceID: "device", ItemID: "item", ItemVersion: 1,
		Action:  domain.ActionAdjudicateFindings,
		Message: `[{"finding_id":"b","route":"decline","reason":"Out of scope."}]`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Service{}).validateCommandContent(command); err != nil {
		t.Fatalf("content gate refused a reasoned message: %v", err)
	}
	routes, err := concludedRoutes(command)
	if err != nil || len(routes) != 1 || routes[0] != (domain.ConcludedRoute{FindingID: "b", Route: domain.RouteDecline}) {
		t.Fatalf("concluded routes = %#v, %v", routes, err)
	}
}
