// Package comprehension computes the plan §9 comprehension measures from the
// telemetry the store records (issue #924). It is pure: it takes slices and
// returns typed results with explicit numerators and denominators (§9
// normalization by volume), importing only the domain vocabulary. The operator
// command fetches the rows through the store's observation-only surface and
// hands them here; nothing in this package reaches the store, so it stays
// trivially testable and structurally unable to influence policy.
package comprehension

import (
	"sort"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// DecidedCommand is one accepted command joined to its item's type, decided
// instant, and subject run: the store's DecidedCommand projection restated in
// domain-only terms so this package need not import the store.
type DecidedCommand struct {
	Command      domain.Command
	ItemType     domain.AttentionType
	DecidedAt    time.Time
	SubjectRunID domain.RunID
}

// approvingActions are the accepted actions that advance work; a later
// returning action on the same subject run reverses one (plan §9, the planner's
// reading recorded in issue #924, veto-able on the plan comment).
var approvingActions = map[domain.Action]bool{
	domain.ActionApprove:                true,
	domain.ActionAcceptRecommendedRoute: true,
	domain.ActionChooseAlternativeRoute: true,
	domain.ActionStart:                  true,
	domain.ActionStartWithChanges:       true,
	domain.ActionFinishNow:              true,
	domain.ActionApplyThenFinish:        true,
	domain.ActionContinueUnderPolicy:    true,
}

// returningActions are the accepted actions that send work back.
var returningActions = map[domain.Action]bool{
	domain.ActionRequestChanges: true,
	domain.ActionReturnToAgent:  true,
	domain.ActionStop:           true,
}

// nonConcludingActions are the accepted actions that do not conclude the item:
// the record-only navigations (viewing a PR, mark_seen, acknowledge,
// inspect_trust_failure, run_doctor), discuss (appends to a conversation),
// snooze (defers a proposal), and convert_to_policy (pending its owning unit).
// They mirror the signet actionOutcome classifications that yield no resulting
// item status (daemon/internal/signet/service.go). Such a command is not a
// decision even when it carries a recommendation's evidence, so it is excluded
// from the override denominator entirely, neither an override nor unclassified
// (plan §9; owner-approved on issue #924). A new record-only Action must be
// added here so it cannot inflate the override rate.
var nonConcludingActions = map[domain.Action]bool{
	domain.ActionOpenPR:              true,
	domain.ActionMarkSeen:            true,
	domain.ActionAcknowledge:         true,
	domain.ActionInspectTrustFailure: true,
	domain.ActionRunDoctor:           true,
	domain.ActionDiscuss:             true,
	domain.ActionSnooze:              true,
	domain.ActionConvertToPolicy:     true,
}

// Measures is the full §9 comprehension read.
type Measures struct {
	OpenToDecision []OpenToDecisionByType `json:"open_to_decision"`
	Reversal       ReversalRate           `json:"reversal"`
	DrillDown      DrillDownRate          `json:"drill_down"`
	Overrides      []OverrideStratum      `json:"overrides"`
	DefectCount    int                    `json:"defect_count"`
}

// OpenToDecisionByType is the median open-to-decision time for one item type,
// with the sample count as its denominator. Open is the item's earliest
// card_opened event; a decided item with no card_opened event contributes no
// sample.
type OpenToDecisionByType struct {
	ItemType      domain.AttentionType `json:"item_type"`
	Samples       int                  `json:"samples"`
	MedianSeconds float64              `json:"median_seconds"`
}

// ReversalRate is reversed approvals over run-scoped approvals.
type ReversalRate struct {
	Approvals int `json:"approvals"`
	Reversed  int `json:"reversed"`
}

// DrillDownRate is decided items that drilled down before deciding over decided
// items.
type DrillDownRate struct {
	DecidedItems int `json:"decided_items"`
	DrilledDown  int `json:"drilled_down"`
}

// OverrideStratum is the override classification for one (item type,
// recommendation source). Decisions is the classified denominator (followed
// plus both override kinds); Unclassified is excluded from both rates.
type OverrideStratum struct {
	ItemType           domain.AttentionType        `json:"item_type"`
	Source             domain.RecommendationSource `json:"recommendation_source"`
	Decisions          int                         `json:"decisions"`
	VoluntaryOverrides int                         `json:"voluntary_overrides"`
	ForcedOverrides    int                         `json:"forced_overrides"`
	Unclassified       int                         `json:"unclassified"`
}

// Compute derives every §9 measure from the recorded telemetry.
func Compute(
	events []domain.ComprehensionEvent,
	surfaces []domain.DecisionActionSurface,
	decided []DecidedCommand,
	defects []domain.ComprehensionDefect,
) Measures {
	return Measures{
		OpenToDecision: openToDecision(events, decided),
		Reversal:       reversalRate(events, decided),
		DrillDown:      drillDownRate(events, decided),
		Overrides:      overrideRates(events, surfaces, decided),
		DefectCount:    len(defects),
	}
}

// earliestByKind returns, per item, the earliest OccurredAt among events of the
// given kind.
func earliestByKind(events []domain.ComprehensionEvent, kind domain.ComprehensionEventKind) map[domain.ItemID]time.Time {
	out := map[domain.ItemID]time.Time{}
	for _, e := range events {
		if e.Kind != kind {
			continue
		}
		if prev, ok := out[e.ItemID]; !ok || e.OccurredAt.Before(prev) {
			out[e.ItemID] = e.OccurredAt
		}
	}
	return out
}

func openToDecision(events []domain.ComprehensionEvent, decided []DecidedCommand) []OpenToDecisionByType {
	opened := earliestByKind(events, domain.ComprehensionCardOpened)
	// One sample per decided item (dedupe multiple commands on one item), keyed
	// by item, taking the first decided instant seen.
	type sample struct {
		itemType  domain.AttentionType
		decidedAt time.Time
	}
	perItem := map[domain.ItemID]sample{}
	for _, d := range decided {
		if d.DecidedAt.IsZero() {
			continue
		}
		if _, ok := perItem[d.Command.ItemID]; !ok {
			perItem[d.Command.ItemID] = sample{itemType: d.ItemType, decidedAt: d.DecidedAt}
		}
	}
	durations := map[domain.AttentionType][]float64{}
	for itemID, s := range perItem {
		openedAt, ok := opened[itemID]
		if !ok || !openedAt.Before(s.decidedAt) {
			continue
		}
		durations[s.itemType] = append(durations[s.itemType], s.decidedAt.Sub(openedAt).Seconds())
	}
	out := make([]OpenToDecisionByType, 0, len(durations))
	for itemType, ds := range durations {
		out = append(out, OpenToDecisionByType{
			ItemType: itemType, Samples: len(ds), MedianSeconds: median(ds),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ItemType < out[j].ItemType })
	return out
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

func reversalRate(events []domain.ComprehensionEvent, decided []DecidedCommand) ReversalRate {
	// Gate the approval denominator on instrumentation. Migration 0065
	// backfills no comprehension events, and older clients emit none, so a
	// decided item with no card_opened event predates instrumentation. Counting
	// its approval would dilute the reversal rate toward a false zero, so only
	// an instrumented approval enters the denominator (plan §9; owner-approved
	// on issue #924). Whether a later returning action reverses it is a separate
	// fact and is not itself gated.
	opened := earliestByKind(events, domain.ComprehensionCardOpened)
	byRun := map[domain.RunID][]DecidedCommand{}
	for _, d := range decided {
		if d.SubjectRunID == "" {
			continue
		}
		byRun[d.SubjectRunID] = append(byRun[d.SubjectRunID], d)
	}
	var rate ReversalRate
	for _, group := range byRun {
		sort.Slice(group, func(i, j int) bool {
			if !group[i].DecidedAt.Equal(group[j].DecidedAt) {
				return group[i].DecidedAt.Before(group[j].DecidedAt)
			}
			return group[i].Command.CommandID < group[j].Command.CommandID
		})
		for i, d := range group {
			if !approvingActions[d.Command.Action] {
				continue
			}
			if _, ok := opened[d.Command.ItemID]; !ok {
				continue
			}
			rate.Approvals++
			for _, later := range group[i+1:] {
				if later.DecidedAt.After(d.DecidedAt) && returningActions[later.Command.Action] {
					rate.Reversed++
					break
				}
			}
		}
	}
	return rate
}

func drillDownRate(events []domain.ComprehensionEvent, decided []DecidedCommand) DrillDownRate {
	opened := earliestByKind(events, domain.ComprehensionCardOpened)
	drill := earliestByKind(events, domain.ComprehensionDrillDownOpened)
	decidedAt := map[domain.ItemID]time.Time{}
	for _, d := range decided {
		if d.DecidedAt.IsZero() {
			continue
		}
		// Gate the denominator on instrumentation: an item with no card_opened
		// event is pre-instrumentation (or older-client) history that migration
		// 0065 backfills none for, so counting it would dilute the rate toward a
		// false zero (plan §9; owner-approved on issue #924).
		if _, ok := opened[d.Command.ItemID]; !ok {
			continue
		}
		if prev, ok := decidedAt[d.Command.ItemID]; !ok || d.DecidedAt.Before(prev) {
			decidedAt[d.Command.ItemID] = d.DecidedAt
		}
	}
	var rate DrillDownRate
	for itemID, decided := range decidedAt {
		rate.DecidedItems++
		if drilledAt, ok := drill[itemID]; ok && drilledAt.Before(decided) {
			rate.DrilledDown++
		}
	}
	return rate
}

func overrideRates(events []domain.ComprehensionEvent, surfaces []domain.DecisionActionSurface, decided []DecidedCommand) []OverrideStratum {
	opened := earliestByKind(events, domain.ComprehensionCardOpened)
	byDigest := map[domain.Digest]domain.DecisionActionSurface{}
	for _, s := range surfaces {
		byDigest[s.Digest] = s
	}
	type key struct {
		itemType domain.AttentionType
		source   domain.RecommendationSource
	}
	strata := map[key]*OverrideStratum{}
	stratum := func(k key) *OverrideStratum {
		if s, ok := strata[k]; ok {
			return s
		}
		s := &OverrideStratum{ItemType: k.itemType, Source: k.source}
		strata[k] = s
		return s
	}
	for _, d := range decided {
		ev := d.Command.DecisionEvidence
		// Only decisions carrying a recommendation are override candidates.
		if ev == nil || ev.RecommendedAction == nil || ev.RecommendationSource == nil {
			continue
		}
		// A record-only or otherwise non-concluding action is navigation, not a
		// decision: it leaves the item open, so it never enters the override
		// denominator even carrying a recommendation's evidence (plan §9).
		if nonConcludingActions[d.Command.Action] {
			continue
		}
		// Gate the denominator on instrumentation: an item with no card_opened
		// event is pre-instrumentation (or older-client) history that migration
		// 0065 backfills none for, so counting it would dilute the override
		// rates toward false zeros (plan §9; owner-approved on issue #924).
		if _, ok := opened[d.Command.ItemID]; !ok {
			continue
		}
		s := stratum(key{itemType: d.ItemType, source: *ev.RecommendationSource})
		surface, ok := byDigest[ev.ActionSurfaceDigest]
		if ev.ActionSurfaceDigest == "" || !ok ||
			surface.DeviceID != d.Command.DeviceID || !surface.Offers(d.Command.Action) {
			s.Unclassified++
			continue
		}
		s.Decisions++
		if d.Command.Action == *ev.RecommendedAction {
			continue // followed the recommendation
		}
		if surface.Offers(*ev.RecommendedAction) {
			s.VoluntaryOverrides++
		} else {
			s.ForcedOverrides++
		}
	}
	out := make([]OverrideStratum, 0, len(strata))
	for _, s := range strata {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ItemType != out[j].ItemType {
			return out[i].ItemType < out[j].ItemType
		}
		return out[i].Source < out[j].Source
	})
	return out
}
