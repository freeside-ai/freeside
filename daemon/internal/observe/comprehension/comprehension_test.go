package comprehension_test

import (
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/observe/comprehension"
)

var base = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func digest(seed string) domain.Digest {
	return domain.Digest("sha256:" + strings.Repeat(seed, 64)[:64])
}

func event(t *testing.T, id string, itemID domain.ItemID, kind domain.ComprehensionEventKind, at time.Time) domain.ComprehensionEvent {
	t.Helper()
	e, err := domain.NewComprehensionEvent(domain.ComprehensionEventInput{
		DeviceID: "device-1", EventID: id, ItemID: itemID, Kind: kind,
		ItemDecisionSurfaceDigest: digest("a"), OccurredAt: at, Sequence: 1,
	}, at.Add(time.Second))
	if err != nil {
		t.Fatalf("event %s: %v", id, err)
	}
	return e
}

func command(t *testing.T, id string, itemID domain.ItemID, action domain.Action) domain.Command {
	t.Helper()
	c, err := domain.NewCommand(domain.CommandInput{
		CommandID: id, DeviceID: "device-1", ItemID: itemID, ItemVersion: 1, Action: action,
	})
	if err != nil {
		t.Fatalf("command %s: %v", id, err)
	}
	return c
}

func recommendedCommand(t *testing.T, id string, itemID domain.ItemID, action, rec domain.Action, src domain.RecommendationSource, surface domain.Digest) domain.Command {
	t.Helper()
	c := command(t, id, itemID, action)
	stamped, err := c.WithDecisionEvidence(domain.CommandDecisionEvidence{
		ActionSurfaceDigest: surface, RecommendedAction: &rec, RecommendationSource: &src,
	})
	if err != nil {
		t.Fatalf("stamp %s: %v", id, err)
	}
	return stamped
}

// surface builds a valid DecisionActionSurface offering actions (which must be
// sorted and deduplicated) for a device.
func surface(t *testing.T, deviceID domain.DeviceID, itemID domain.ItemID, actions []domain.Action) domain.DecisionActionSurface {
	t.Helper()
	s := domain.DecisionActionSurface{
		DeviceID: deviceID, ItemID: itemID,
		ItemDecisionSurfaceDigest: digest("b"), ClientCapabilityDigest: digest("c"),
		Actions: actions,
	}
	d, err := s.ComputeDigest()
	if err != nil {
		t.Fatalf("surface digest: %v", err)
	}
	s.Digest = d
	if err := s.Validate(); err != nil {
		t.Fatalf("surface: %v", err)
	}
	return s
}

func TestOpenToDecisionMedian(t *testing.T) {
	events := []domain.ComprehensionEvent{
		event(t, "e1", "item-a", domain.ComprehensionCardOpened, base),
		event(t, "e2", "item-b", domain.ComprehensionCardOpened, base),
		event(t, "e3", "item-c", domain.ComprehensionCardOpened, base),
	}
	decided := []comprehension.DecidedCommand{
		{Command: command(t, "c1", "item-a", domain.ActionApprove), ItemType: domain.AttentionReadyForFinalReview, DecidedAt: base.Add(10 * time.Second)},
		{Command: command(t, "c2", "item-b", domain.ActionApprove), ItemType: domain.AttentionReadyForFinalReview, DecidedAt: base.Add(20 * time.Second)},
		{Command: command(t, "c3", "item-c", domain.ActionApprove), ItemType: domain.AttentionReadyForFinalReview, DecidedAt: base.Add(30 * time.Second)},
	}
	got := comprehension.Compute(events, nil, decided, nil).OpenToDecision
	if len(got) != 1 || got[0].Samples != 3 || got[0].MedianSeconds != 20 {
		t.Fatalf("open-to-decision = %+v, want one type, 3 samples, median 20s", got)
	}
}

func TestReversalRate(t *testing.T) {
	// Both approved items are instrumented (card_opened), so they enter the
	// denominator.
	events := []domain.ComprehensionEvent{
		event(t, "o1", "item-a", domain.ComprehensionCardOpened, base),
		event(t, "o2", "item-b", domain.ComprehensionCardOpened, base),
	}
	decided := []comprehension.DecidedCommand{
		// run-1: an approval reversed by a later stop.
		{Command: command(t, "c1", "item-a", domain.ActionApprove), DecidedAt: base, SubjectRunID: "run-1"},
		{Command: command(t, "c2", "item-a2", domain.ActionStop), DecidedAt: base.Add(time.Minute), SubjectRunID: "run-1"},
		// run-2: an approval never returned.
		{Command: command(t, "c3", "item-b", domain.ActionApprove), DecidedAt: base, SubjectRunID: "run-2"},
	}
	got := comprehension.Compute(events, nil, decided, nil).Reversal
	if got.Approvals != 2 || got.Reversed != 1 {
		t.Fatalf("reversal = %+v, want 2 approvals, 1 reversed", got)
	}
}

// TestReversalExcludesUninstrumentedApprovals: an approval on an item with no
// card_opened event is pre-instrumentation history and must not enter the
// approval denominator.
func TestReversalExcludesUninstrumentedApprovals(t *testing.T) {
	events := []domain.ComprehensionEvent{
		event(t, "o1", "item-a", domain.ComprehensionCardOpened, base),
	}
	decided := []comprehension.DecidedCommand{
		// Instrumented approval, reversed by a later stop: counts.
		{Command: command(t, "c1", "item-a", domain.ActionApprove), DecidedAt: base, SubjectRunID: "run-1"},
		{Command: command(t, "c2", "item-a2", domain.ActionStop), DecidedAt: base.Add(time.Minute), SubjectRunID: "run-1"},
		// Uninstrumented approval (no card_opened): excluded.
		{Command: command(t, "c3", "item-b", domain.ActionApprove), DecidedAt: base, SubjectRunID: "run-2"},
	}
	got := comprehension.Compute(events, nil, decided, nil).Reversal
	if got.Approvals != 1 || got.Reversed != 1 {
		t.Fatalf("reversal = %+v, want 1 approval, 1 reversed (uninstrumented approval excluded)", got)
	}
}

func TestDrillDownRate(t *testing.T) {
	// Both decided items are instrumented (card_opened); item-a also drilled
	// down before deciding.
	events := []domain.ComprehensionEvent{
		event(t, "o1", "item-a", domain.ComprehensionCardOpened, base),
		event(t, "o2", "item-b", domain.ComprehensionCardOpened, base),
		event(t, "e1", "item-a", domain.ComprehensionDrillDownOpened, base),
	}
	decided := []comprehension.DecidedCommand{
		{Command: command(t, "c1", "item-a", domain.ActionApprove), DecidedAt: base.Add(time.Minute)},
		{Command: command(t, "c2", "item-b", domain.ActionApprove), DecidedAt: base.Add(time.Minute)},
	}
	got := comprehension.Compute(events, nil, decided, nil).DrillDown
	if got.DecidedItems != 2 || got.DrilledDown != 1 {
		t.Fatalf("drill-down = %+v, want 2 decided, 1 drilled down", got)
	}
}

// TestDrillDownExcludesUninstrumentedItems: a decided item with no card_opened
// event is pre-instrumentation history and must not enter the drill-down
// denominator; an instrumented item counts exactly as before.
func TestDrillDownExcludesUninstrumentedItems(t *testing.T) {
	events := []domain.ComprehensionEvent{
		// item-a is instrumented and drilled down; item-b has no card_opened.
		event(t, "o1", "item-a", domain.ComprehensionCardOpened, base),
		event(t, "e1", "item-a", domain.ComprehensionDrillDownOpened, base),
	}
	decided := []comprehension.DecidedCommand{
		{Command: command(t, "c1", "item-a", domain.ActionApprove), DecidedAt: base.Add(time.Minute)},
		{Command: command(t, "c2", "item-b", domain.ActionApprove), DecidedAt: base.Add(time.Minute)},
	}
	got := comprehension.Compute(events, nil, decided, nil).DrillDown
	if got.DecidedItems != 1 || got.DrilledDown != 1 {
		t.Fatalf("drill-down = %+v, want 1 decided, 1 drilled down (uninstrumented item-b excluded)", got)
	}
}

func TestOverrideClassification(t *testing.T) {
	src := domain.RecommendationDaemonPolicy
	// A surface offering approve and request_changes.
	full := surface(t, "device-1", "item-a", []domain.Action{domain.ActionApprove, domain.ActionRequestChanges})
	// A surface that omits the recommended approve (forced override).
	noApprove := surface(t, "device-1", "item-d", []domain.Action{domain.ActionRequestChanges, domain.ActionStop})
	// Every decided item is instrumented (card_opened), so all enter the
	// denominator as before.
	events := []domain.ComprehensionEvent{
		event(t, "o-a", "item-a", domain.ComprehensionCardOpened, base),
		event(t, "o-d", "item-d", domain.ComprehensionCardOpened, base),
		event(t, "o-e", "item-e", domain.ComprehensionCardOpened, base),
	}
	decided := []comprehension.DecidedCommand{
		// Followed: chose the recommended approve.
		{Command: recommendedCommand(t, "c1", "item-a", domain.ActionApprove, domain.ActionApprove, src, full.Digest),
			ItemType: domain.AttentionReadyForFinalReview},
		// Voluntary: chose request_changes though approve was offered.
		{Command: recommendedCommand(t, "c2", "item-a", domain.ActionRequestChanges, domain.ActionApprove, src, full.Digest),
			ItemType: domain.AttentionReadyForFinalReview},
		// Forced: approve was not on the surface.
		{Command: recommendedCommand(t, "c3", "item-d", domain.ActionRequestChanges, domain.ActionApprove, src, noApprove.Digest),
			ItemType: domain.AttentionReadyForFinalReview},
		// Unclassified: no surface digest on the evidence.
		{Command: recommendedCommand(t, "c4", "item-e", domain.ActionRequestChanges, domain.ActionApprove, src, ""),
			ItemType: domain.AttentionReadyForFinalReview},
	}
	got := comprehension.Compute(events, []domain.DecisionActionSurface{full, noApprove}, decided, nil).Overrides
	if len(got) != 1 {
		t.Fatalf("override strata = %+v, want one", got)
	}
	s := got[0]
	if s.Decisions != 3 || s.VoluntaryOverrides != 1 || s.ForcedOverrides != 1 || s.Unclassified != 1 {
		t.Fatalf("override stratum = %+v, want 3 decisions, 1 voluntary, 1 forced, 1 unclassified", s)
	}
}

// TestOverrideExcludesUninstrumentedItems: a decided item with no card_opened
// event is pre-instrumentation history and must not enter the override
// denominator; an instrumented item classifies exactly as before.
func TestOverrideExcludesUninstrumentedItems(t *testing.T) {
	src := domain.RecommendationDaemonPolicy
	full := surface(t, "device-1", "item-a", []domain.Action{domain.ActionApprove, domain.ActionRequestChanges})
	other := surface(t, "device-1", "item-b", []domain.Action{domain.ActionApprove, domain.ActionRequestChanges})
	events := []domain.ComprehensionEvent{
		event(t, "o-a", "item-a", domain.ComprehensionCardOpened, base),
	}
	decided := []comprehension.DecidedCommand{
		// Instrumented voluntary override: counts.
		{
			Command:  recommendedCommand(t, "c1", "item-a", domain.ActionRequestChanges, domain.ActionApprove, src, full.Digest),
			ItemType: domain.AttentionReadyForFinalReview,
		},
		// Uninstrumented item (no card_opened): excluded from the denominator.
		{
			Command:  recommendedCommand(t, "c2", "item-b", domain.ActionRequestChanges, domain.ActionApprove, src, other.Digest),
			ItemType: domain.AttentionReadyForFinalReview,
		},
	}
	got := comprehension.Compute(events, []domain.DecisionActionSurface{full, other}, decided, nil).Overrides
	if len(got) != 1 {
		t.Fatalf("override strata = %+v, want one", got)
	}
	s := got[0]
	if s.Decisions != 1 || s.VoluntaryOverrides != 1 || s.ForcedOverrides != 0 || s.Unclassified != 0 {
		t.Fatalf("override stratum = %+v, want 1 decision, 1 voluntary (uninstrumented item-b excluded)", s)
	}
}

func TestOverrideExcludesRecordOnlyActions(t *testing.T) {
	src := domain.RecommendationDaemonPolicy
	full := surface(t, "device-1", "item-a", []domain.Action{domain.ActionApprove, domain.ActionRequestChanges})
	events := []domain.ComprehensionEvent{
		event(t, "o-a", "item-a", domain.ComprehensionCardOpened, base),
	}
	decided := []comprehension.DecidedCommand{
		// Record-only: viewing the PR on a recommendation-bearing item carries
		// the recommendation's evidence but does not conclude the decision, so
		// it must not enter the denominator (neither override nor unclassified).
		{
			Command:  recommendedCommand(t, "c1", "item-a", domain.ActionOpenPR, domain.ActionApprove, src, full.Digest),
			ItemType: domain.AttentionReadyForFinalReview,
		},
		// A concluding action that differs from the recommendation still counts
		// as a voluntary override, exactly as before.
		{
			Command:  recommendedCommand(t, "c2", "item-a", domain.ActionRequestChanges, domain.ActionApprove, src, full.Digest),
			ItemType: domain.AttentionReadyForFinalReview,
		},
	}
	got := comprehension.Compute(events, []domain.DecisionActionSurface{full}, decided, nil).Overrides
	if len(got) != 1 {
		t.Fatalf("override strata = %+v, want one", got)
	}
	s := got[0]
	if s.Decisions != 1 || s.VoluntaryOverrides != 1 || s.ForcedOverrides != 0 || s.Unclassified != 0 {
		t.Fatalf("override stratum = %+v, want 1 decision, 1 voluntary, 0 forced, 0 unclassified (record-only open_pr excluded)", s)
	}
}

func TestDefectCount(t *testing.T) {
	defects := []domain.ComprehensionDefect{
		{ItemID: "item-a", ClaimDigest: digest("d"), RecordedAt: base, Reason: "x"},
		{ItemID: "item-b", ClaimDigest: digest("e"), RecordedAt: base, Reason: "y"},
	}
	if got := comprehension.Compute(nil, nil, nil, defects).DefectCount; got != 2 {
		t.Fatalf("defect count = %d, want 2", got)
	}
}
