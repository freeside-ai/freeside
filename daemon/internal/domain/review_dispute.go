package domain

import (
	"math"
	"slices"
	"strings"
	"time"

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

// NewHumanAdjudicationEntry records a final route, without model confidence or
// alternative proposals. The artifact records the command once as its decision.
func NewHumanAdjudicationEntry(id FindingID, route AdjudicationRoute) (FindingAdjudicationEntry, error) {
	entry := FindingAdjudicationEntry{
		FindingID: id, Producer: AdjudicationProducerHuman,
		Route: route, Rationale: "Human decision recorded.",
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
	return entry, entry.Validate()
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
