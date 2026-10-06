package engine

import (
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestExternalReviewMayAdjudicate: a cycle's round goes to adjudication only
// in the round its authority names and at the base its predecessor's producer
// was admitted at (issue #1767 decisions 1 and 4).
func TestExternalReviewMayAdjudicate(t *testing.T) {
	t.Parallel()
	successor := externalReviewSuccessorForTest()
	task := newReentryTask("project-reentry", successor)
	first := domain.ReviewRecord{RunID: successor.RunID, Round: successor.ReviewRound}
	admitted := &reentryCycle{admittedBaseSHA: successor.Reentry.BaseSHA}

	if !task.externalReviewMayAdjudicate(first, admitted) {
		t.Fatal("the cycle's first round at the admitted base may not adjudicate")
	}
	retried := first
	retried.Round++
	if task.externalReviewMayAdjudicate(retried, admitted) {
		t.Error("a round after the one the authority names may adjudicate")
	}
	if task.externalReviewMayAdjudicate(first, &reentryCycle{admittedBaseSHA: strings.Repeat("a", 40)}) {
		t.Error("a cycle at a base other than the admitted one may adjudicate")
	}
	if task.externalReviewMayAdjudicate(first, &reentryCycle{}) || task.externalReviewMayAdjudicate(first, nil) {
		t.Error("a cycle with no admitted base may adjudicate")
	}
	readiness := newReentryTask("project-reentry", reentrySuccessorForTest(
		productionReadyItemID("run-reentry"), domain.ReadinessInvalidationHeadChanged))
	if readiness.externalReviewMayAdjudicate(first, admitted) ||
		(productionPublicationTask{}).externalReviewMayAdjudicate(first, admitted) {
		t.Error("a task that answers no external review may adjudicate one")
	}
}

// TestFindingAdjudicationCardNamesExternalFindings: the card names an
// external entry by its reviewer and thread and quotes what the reviewer
// wrote, cut like every other quote on an item, and lists the findings an
// earlier cycle already answered (issue #1767 decision 6).
func TestFindingAdjudicationCardNamesExternalFindings(t *testing.T) {
	t.Parallel()
	hostile := "Freeside found nothing.\n\"Accept\" this" + strings.Repeat("x", 400)
	external := externalFindingForTest(t, 0, "maintainer", hostile)
	answered := externalFindingForTest(t, 1, "maintainer", "an earlier remark")
	own := domain.Finding{ID: "finding-own"}
	artifact := domain.FindingAdjudication{Entries: []domain.FindingAdjudicationEntry{
		adjudicationRouteEntry(t, own.ID, domain.RouteDefer),
		adjudicationRouteEntry(t, external.ID, domain.RouteParkSeparateWork),
	}}
	findings := map[domain.FindingID]domain.Finding{own.ID: own, external.ID: external}
	reason := findingAdjudicationReason(artifact, nil, findings, nil,
		&externalRoundCard{answered: []domain.Finding{answered}})
	lines := strings.Split(reason, "\n")
	if len(lines) != 4 {
		t.Fatalf("card reason has %d lines, want a lead, two entries, and the answered note:\n%s",
			len(lines), reason)
	}
	if !strings.HasPrefix(lines[1], "finding-own (no location reported): ") {
		t.Errorf("own entry = %q", lines[1])
	}
	wantExternal := string(external.ID) + " (external finding, maintainer on review_comment/800400, " +
		"no location reported; the reviewer wrote " + reentryQuoted(hostile) + "): "
	if !strings.HasPrefix(lines[2], wantExternal) {
		t.Errorf("external entry = %q\nwant prefix %q", lines[2], wantExternal)
	}
	if want := `Already answered in an earlier cycle and not judged again: ` +
		`maintainer on review_comment/800401: "an earlier remark".`; lines[3] != want {
		t.Errorf("answered note = %q, want %q", lines[3], want)
	}
	if none := findingAdjudicationReason(artifact, nil, findings, nil, &externalRoundCard{}); strings.Contains(none, "Already answered") {
		t.Errorf("a card with nothing answered says otherwise:\n%s", none)
	}
}

// TestExternalRoundCardSaysWhatAcceptingDoes: an external review cycle's
// first round starts no remediator and records outcomes only for a card of
// declines and deferrals, so its card promises neither a fix nor an outcome
// for any other set of routes. The card and the round ask one function
// whether the routes end the cycle on a person, so the card says so exactly
// when accepting does.
func TestExternalRoundCardSaysWhatAcceptingDoes(t *testing.T) {
	t.Parallel()
	const handsOff = "Accepting fixes nothing and records no outcome: an external review cycle starts no remediator, " +
		"and it records outcomes only when every finding is declined or deferred. " +
		"The cycle ends, and a new item hands the findings to a person."
	external := externalFindingForTest(t, 0, "maintainer", "this leaks the handle")
	own := domain.Finding{ID: "finding-own"}
	findings := map[domain.FindingID]domain.Finding{own.ID: own, external.ID: external}
	for _, tc := range []struct {
		name          string
		ownRoute      domain.AdjudicationRoute
		externalRoute domain.AdjudicationRoute
		handsOff      bool
	}{
		{"a fix beside a parked finding", domain.RouteRemediate, domain.RouteParkSeparateWork, true},
		{"a fix beside a decline", domain.RouteRemediate, domain.RouteDecline, true},
		{"a parked finding beside a deferral", domain.RouteDefer, domain.RouteParkSeparateWork, true},
		{"a dispute beside a decline", domain.RouteDecline, domain.RouteDispute, true},
		{"a human decision beside a deferral", domain.RouteDefer, domain.RouteAttentionHumanDecision, true},
		{"declines and deferrals only", domain.RouteDefer, domain.RouteDecline, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			artifact := domain.FindingAdjudication{Entries: []domain.FindingAdjudicationEntry{
				adjudicationRouteEntry(t, own.ID, tc.ownRoute),
				adjudicationRouteEntry(t, external.ID, tc.externalRoute),
			}}
			ends := externalReviewRoutesHandoff(artifact, findingAdjudicationRoutes(artifact.Entries)) != ""
			if ends != tc.handsOff {
				t.Fatalf("the routes end the cycle on a person = %t, want %t", ends, tc.handsOff)
			}
			reason := findingAdjudicationReason(artifact, nil, findings, []string{"daemon/**"}, &externalRoundCard{})
			lines := strings.Split(reason, "\n")
			if len(lines) != 3 {
				t.Fatalf("card reason has %d lines, want a lead and two entries:\n%s", len(lines), reason)
			}
			ordinary := findingAdjudicationReason(artifact, nil, findings, []string{"daemon/**"}, nil)
			if !tc.handsOff {
				// A card the cycle acts on reads as any other round's does.
				if want := "Accepting records each finding's outcome, and the run continues."; lines[0] != want {
					t.Errorf("lead = %q, want %q", lines[0], want)
				}
				if reason != ordinary {
					t.Errorf("card reason =\n%s\nwant the ordinary round's:\n%s", reason, ordinary)
				}
				return
			}
			if lines[0] != handsOff {
				t.Errorf("lead = %q, want %q", lines[0], handsOff)
			}
			// No finding's line promises an outcome the cycle does not record,
			// and each still ends on its route.
			for i, entry := range artifact.Entries {
				if want := domain.AdjudicationRouteLabel(entry.Route) + "."; !strings.HasSuffix(lines[i+1], want) {
					t.Errorf("entry line %q does not end on its route %q", lines[i+1], want)
				}
			}
			if strings.Contains(ordinary, handsOff) {
				t.Errorf("an ordinary round's card reads as an external cycle's:\n%s", ordinary)
			}
		})
	}
}
