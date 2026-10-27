package domain

import (
	"math"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/pathfold"
)

// ReviewDisputeRoutes derives executable routes from the immutable run scope.
// A missing or out-of-scope location cannot authorize remediation.
func ReviewDisputeRoutes(finding Finding, declaredPaths []string) []AdjudicationRoute {
	routes := []AdjudicationRoute{RouteDecline, RouteDefer}
	if finding.Location != nil && canonicalRepoPath(finding.Location.Path) && pathfold.MatchAny(declaredPaths, finding.Location.Path, false) {
		routes = append([]AdjudicationRoute{RouteRemediate}, routes...)
	}
	return routes
}

// DisputedFindingAdjudicationFor derives one dispute row's adjudication block
// from the finding's entry in the round's latest model-authored artifact
// revision. The engine and the store gate share it, so a stored block is
// always re-derived rather than trusted. A nil entry, or one no model judged,
// has no block. A demoted entry's lean is the route it proposed before the
// demotion; an `unknown` park with none has no lean.
func DisputedFindingAdjudicationFor(
	entry *FindingAdjudicationEntry, threshold DispatchThreshold,
) (*DisputedFindingAdjudication, error) {
	if entry == nil || (entry.Producer != AdjudicationProducerModel &&
		entry.Producer != AdjudicationProducerEngineModel) {
		return nil, nil
	}
	accepted, err := entry.Accepted(threshold)
	if err != nil {
		return nil, err
	}
	var lean *AdjudicationRoute
	switch {
	case entry.ProposedRoute != nil:
		value := *entry.ProposedRoute
		lean = &value
	case entry.Route != RouteParkUnknown:
		value := entry.Route
		lean = &value
	}
	return &DisputedFindingAdjudication{
		Lean: lean, Confidence: *entry.Confidence, Rationale: entry.Rationale,
		BelowThreshold: !accepted,
	}, nil
}

// DisputedFindingAdjudicationIn derives the block for findingID from the
// round's latest model-authored artifact, nil when there is none or it has no
// entry for the finding (an external finding the batch never judged).
func DisputedFindingAdjudicationIn(
	artifact *FindingAdjudication, findingID FindingID, threshold DispatchThreshold,
) (*DisputedFindingAdjudication, error) {
	if artifact == nil {
		return nil, nil
	}
	for index := range artifact.Entries {
		if artifact.Entries[index].FindingID == findingID {
			return DisputedFindingAdjudicationFor(&artifact.Entries[index], threshold)
		}
	}
	return nil, nil
}

// ReviewDisputeBindingMatches reports whether a stored binding agrees with the
// one derived from current state. A stored row with no adjudication block
// agrees with any derived block: such a row predates the block (#2059), the
// block carries no authority, and an item written before the upgrade keeps
// its version rather than being rebuilt to add it. Every other field, and any
// block the stored row does carry, must match exactly.
func ReviewDisputeBindingMatches(stored, derived *ReviewDisputeBinding) bool {
	if stored == nil || derived == nil {
		return stored == derived
	}
	comparable := *derived
	comparable.Findings = slices.Clone(derived.Findings)
	for index := range comparable.Findings {
		if index < len(stored.Findings) && stored.Findings[index].Adjudication == nil {
			comparable.Findings[index].Adjudication = nil
		}
	}
	return reflect.DeepEqual(stored, &comparable)
}

// HumanAdjudicationRationale is the rationale of a person's entry that carries
// no reason: an internal finding's route, a remediation, or a decision made
// before a reason could be given.
const HumanAdjudicationRationale = "Human decision recorded."

// MaxHumanAdjudicationReasonBytes bounds the reason a person gives for
// declining or deferring an external finding, which the reply posts as written.
const MaxHumanAdjudicationReasonBytes = 2048

// NewHumanAdjudicationEntry records a final route, without model confidence or
// alternative proposals. The artifact records the command once as its decision.
// A non-empty reason, the person's own words for a decline or defer, becomes
// the rationale; it must already be trimmed, so the command and the entry
// carry the same text. An empty reason keeps HumanAdjudicationRationale, which
// rebuilds an entry decided before reasons existed byte for byte.
func NewHumanAdjudicationEntry(id FindingID, route AdjudicationRoute, reason string) (FindingAdjudicationEntry, error) {
	entry := FindingAdjudicationEntry{
		FindingID: id, Producer: AdjudicationProducerHuman,
		Route: route, Rationale: HumanAdjudicationRationale,
	}
	switch route {
	case RouteRemediate:
		compatibility := CompatibilityAllowed
		entry.GoalRelationship, entry.Compatibility = GoalRequired, &compatibility
	case RouteDecline:
		entry.GoalRelationship = GoalContradictory
	case RouteDefer:
		entry.GoalRelationship = GoalAdjacent
	default:
		return FindingAdjudicationEntry{}, ErrInvalidAdjudicationRoute
	}
	if reason != "" {
		if route == RouteRemediate || !ValidHumanAdjudicationReason(reason) {
			return FindingAdjudicationEntry{}, ErrInvalidHumanAdjudicationReason
		}
		entry.Rationale = reason
	}
	return entry, entry.Validate()
}

// ValidHumanAdjudicationReason reports whether reason is a trimmed, non-blank,
// valid UTF-8 text within MaxHumanAdjudicationReasonBytes.
func ValidHumanAdjudicationReason(reason string) bool {
	return reason != "" && strings.TrimSpace(reason) == reason && utf8.ValidString(reason) &&
		len(reason) <= MaxHumanAdjudicationReasonBytes
}

// HumanReason returns the reason a person gave for this entry's route, or ""
// when the entry is not a person's or carries none.
func (e FindingAdjudicationEntry) HumanReason() string {
	if e.Producer != AdjudicationProducerHuman || e.Rationale == HumanAdjudicationRationale {
		return ""
	}
	return e.Rationale
}

// NewHumanFindingAdjudication creates the first accepted artifact for a parked
// round. createdAt is the item's decision time, so crash recovery is stable.
func NewHumanFindingAdjudication(runID RunID, round int, spec, instruction, policy Digest,
	entries []FindingAdjudicationEntry, decision AdjudicationDecision, createdAt time.Time,
) (FindingAdjudication, error) {
	return newFindingAdjudication(runID, round, spec, instruction, policy, entries, "", &decision, createdAt)
}

// NewHumanSuccessorFindingAdjudication replaces disputed routes while retaining
// the predecessor's batch and immutable review bindings.
func NewHumanSuccessorFindingAdjudication(prior FindingAdjudication,
	entries []FindingAdjudicationEntry, decision AdjudicationDecision, createdAt time.Time,
) (FindingAdjudication, error) {
	if err := prior.Validate(); err != nil {
		return FindingAdjudication{}, err
	}
	if prior.Revision == math.MaxInt {
		return FindingAdjudication{}, ErrFindingAdjudicationInconsistent
	}
	artifact := prior
	artifact.Revision++
	predecessor := prior.Digest
	artifact.PredecessorDigest, artifact.Feedback, artifact.Decision = &predecessor, nil, &decision
	artifact.CreatedAt, artifact.DecisionSurfaceDigest = createdAt, ""
	artifact.Entries = slices.Clone(entries)
	slices.SortFunc(artifact.Entries, func(a, b FindingAdjudicationEntry) int {
		return strings.Compare(string(a.FindingID), string(b.FindingID))
	})
	batch, err := computeFindingBatchDigest(artifact.Entries)
	if err != nil {
		return FindingAdjudication{}, err
	}
	if batch != prior.FindingBatchDigest {
		return FindingAdjudication{}, ErrFindingAdjudicationInconsistent
	}
	artifact.Digest, err = artifact.ComputeDigest()
	if err != nil {
		return FindingAdjudication{}, err
	}
	return artifact, artifact.Validate()
}
