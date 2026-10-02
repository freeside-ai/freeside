package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// DriftRouteGate is the route gate's answer for one audited round (plan §7
// Review Drift, Routing). A reversal list is model output, so nothing acts on
// it until the gate has validated it against stored records.
//
// ListValid says the list may be acted on at all. RoundRemains says the
// simplification round's re-review fits under the resolved hard round limit.
// Auto says the round routes to a simplification round without a human, and
// implies the other two.
//
// Superseded holds, when ListValid, the fixed disposition each reversal would
// supersede, in the audit's reversal order.
type DriftRouteGate struct {
	Audit        domain.DriftAudit
	ListValid    bool
	RoundRemains bool
	Auto         bool
	Superseded   []domain.ReviewDispositionRecord
}

// DriftRouteInput is the gate's whole input. Every field is a stored record or
// derived from one; none is a caller's claim.
type DriftRouteInput struct {
	Audit domain.DriftAudit
	// Record is the audited round's review record.
	Record domain.ReviewRecord
	Policy ReviewConvergencePolicy
	// EarlierOverHardened says an earlier round of the run already has an
	// over_hardened audit, whatever became of it.
	EarlierOverHardened bool
	// Findings holds each cited finding that exists.
	Findings map[domain.FindingID]domain.Finding
	// Effective is each finding's effective latest disposition as of the round
	// before the audited one.
	Effective []EffectiveFindingDisposition
}

// EvaluateDriftAuditRoute is the pure route gate.
//
// The list is valid when the verdict is over_hardened and every reversal names
// a finding of this run, outside the audited round's own batch, whose effective
// latest disposition before that round is fixed. An unknown identity, a finding
// the round itself reported, and a finding already declined, deferred, or
// reversed all fail it.
//
// The round routes automatically when the list is valid, the policy routes
// automatically, this is the run's first over_hardened verdict, the confidence
// meets the adjudication threshold, no reversal would undo the fix of a
// critical or high finding, and a review round remains. A missing severity is
// treated as high.
func EvaluateDriftAuditRoute(input DriftRouteInput) DriftRouteGate {
	gate := DriftRouteGate{
		Audit:        input.Audit,
		RoundRemains: input.Audit.Round < input.Policy.HardRoundLimit,
	}
	if input.Audit.Verdict != domain.DriftVerdictOverHardened || len(input.Audit.Reversals) == 0 ||
		input.Record.RunID != input.Audit.RunID || input.Record.Round != input.Audit.Round {
		return gate
	}
	batch := make(map[domain.FindingID]struct{}, len(input.Record.FindingIDs))
	for _, findingID := range input.Record.FindingIDs {
		batch[findingID] = struct{}{}
	}
	effective := make(map[domain.FindingID]EffectiveFindingDisposition, len(input.Effective))
	for _, disposition := range input.Effective {
		effective[disposition.Stored.FindingID] = disposition
	}
	superseded := make([]domain.ReviewDispositionRecord, 0, len(input.Audit.Reversals))
	blocker := false
	for _, reversal := range input.Audit.Reversals {
		finding, known := input.Findings[reversal.FindingID]
		if !known || finding.ID != reversal.FindingID || finding.RunID != input.Audit.RunID {
			return gate
		}
		if _, own := batch[reversal.FindingID]; own {
			return gate
		}
		disposition, dispositioned := effective[reversal.FindingID]
		if !dispositioned || disposition.Effective != domain.ReviewDispositionFixed ||
			disposition.Stored.RunID != input.Audit.RunID ||
			disposition.Stored.Round >= input.Audit.Round {
			return gate
		}
		// Anything but a recorded medium or low severity blocks the automatic
		// route: critical, high, and the unset value plan §7 treats as high.
		if finding.Severity != domain.FindingSeverityP2 && finding.Severity != domain.FindingSeverityP3 {
			blocker = true
		}
		superseded = append(superseded, disposition.Stored)
	}
	gate.ListValid = true
	gate.Superseded = superseded
	gate.Auto = gate.RoundRemains && !blocker && !input.EarlierOverHardened &&
		input.Policy.DriftAuditRoute == domain.DriftAuditRouteAuto &&
		driftConfidenceMeets(input.Audit.Confidence, input.Policy.AdjudicationConfidenceThreshold)
	return gate
}

// driftConfidenceMeets reports whether a verdict's confidence reaches the
// threshold. The threshold is never below medium, so low never meets it, and
// an unknown value on either side meets nothing.
func driftConfidenceMeets(
	confidence domain.AdjudicationConfidence, threshold domain.DispatchThreshold,
) bool {
	// The switch dispatches on the threshold, so it omits default.
	switch threshold {
	case domain.DispatchThresholdMedium:
		return confidence == domain.ConfidenceMedium || confidence == domain.ConfidenceHigh
	case domain.DispatchThresholdHigh:
		return confidence == domain.ConfidenceHigh
	}
	return false
}

// DriftAuditRouteGate re-derives the route gate for one round from stored
// records alone. ErrNotFound means the round has no audit.
//
// Dispositions and supersession records are read as of the round before the
// audited one. The supersession records a routed round writes carry the
// audited round as their reversing round, so the answer does not change once
// they exist, and a supersession read can re-derive the gate that authorized
// it. The reads are the decision-time ones, which do not follow the audited
// round's own item: the gate runs inside that item's reconstruction.
func (tx *ReadTx) DriftAuditRouteGate(
	ctx context.Context, runID domain.RunID, round int,
) (DriftRouteGate, error) {
	fail := func(err error) (DriftRouteGate, error) {
		return DriftRouteGate{}, fmt.Errorf("drift route gate %q round %d: %w", runID, round, err)
	}
	audit, err := tx.GetDriftAuditForRound(ctx, runID, round)
	if err != nil {
		return fail(err)
	}
	// From here a missing parent is a broken binding, not a missing audit.
	bound := func(what string, err error) (DriftRouteGate, error) {
		return fail(supersessionBindingFailure(what, err))
	}
	record, err := tx.reviewRecordForRound(ctx, runID, round)
	if err != nil {
		return bound("review record", err)
	}
	policy, err := tx.ReviewConvergencePolicy(ctx, runID)
	if err != nil {
		return bound("policy", err)
	}
	audits, err := tx.ListDriftAudits(ctx, runID)
	if err != nil {
		return fail(err)
	}
	input := DriftRouteInput{
		Audit: audit, Record: record, Policy: policy,
		Findings: make(map[domain.FindingID]domain.Finding, len(audit.Reversals)),
	}
	for _, earlier := range audits {
		if earlier.Round < round && earlier.Verdict == domain.DriftVerdictOverHardened {
			input.EarlierOverHardened = true
		}
	}
	for _, reversal := range audit.Reversals {
		finding, err := tx.GetFinding(ctx, reversal.FindingID)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return fail(err)
		}
		input.Findings[reversal.FindingID] = finding
	}
	dispositions, err := tx.loadFindingDispositionsAtDecision(ctx, runID, round)
	if err != nil {
		return fail(err)
	}
	supersessions, err := tx.loadDispositionSupersessionsAtDecision(ctx, runID, round, dispositions)
	if err != nil {
		return fail(err)
	}
	input.Effective = resolveEffectiveDispositions(dispositions, supersessions, round-1)
	return EvaluateDriftAuditRoute(input), nil
}
