package engine

import (
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestProductionPublicationRouteEnum(t *testing.T) {
	t.Parallel()
	seen := map[productionPublicationRoute]bool{}
	for _, route := range allProductionPublicationRoutes {
		if !route.valid() {
			t.Errorf("route %q is listed but not valid", route)
		}
		if seen[route] {
			t.Errorf("route %q is listed twice", route)
		}
		seen[route] = true
	}
	if productionPublicationRoute("").valid() {
		t.Error("zero route is valid")
	}
}

// TestProductionPublicationTaskRoute pins every task-field combination that
// selects a path to its route. The overlapping rows pin the first-match
// order: in-place re-entry before retirement, and both before continuation.
func TestProductionPublicationTaskRoute(t *testing.T) {
	t.Parallel()
	const producer = domain.InvocationID("inv-producer")
	reentry := &domain.PublicationSuccessorReentry{BaseSHA: "base", HeadSHA: "head"}
	successor := func(version string, origin domain.PublicationSuccessorOrigin, reentry *domain.PublicationSuccessorReentry) *domain.PublicationSuccessor {
		return &domain.PublicationSuccessor{Version: version, Origin: origin, Reentry: reentry}
	}
	unpublishedReentry := func() *domain.PublicationSuccessor {
		return successor(domain.PublicationUnpublishedReentryVersion,
			domain.PublicationSuccessorUnpublishedBaseAdvance, reentry)
	}
	successorReentry := func() *domain.PublicationSuccessor {
		return successor(domain.PublicationSuccessorReentryVersion, domain.PublicationSuccessorBaseAdvance, reentry)
	}
	readinessInvalidation := func() *domain.PublicationSuccessor {
		return successor(domain.PublicationReentryVersion, domain.PublicationSuccessorReadinessInvalidation, reentry)
	}
	continuation := &domain.PublicationContinuationIntent{}

	for _, tc := range []struct {
		name string
		task productionPublicationTask
		want productionPublicationRoute
	}{
		{
			name: "no successor or continuation",
			task: productionPublicationTask{ProducingInvocationID: producer},
			want: productionPublicationRouteForward,
		},
		{
			name: "feedback successor",
			task: productionPublicationTask{
				ProducingInvocationID: producer,
				Successor:             successor(domain.PublicationSuccessorVersion, "", nil),
			},
			want: productionPublicationRouteForward,
		},
		{
			name: "remediation continuation successor (v2) without continuation intent",
			task: productionPublicationTask{
				ProducingInvocationID: producer,
				Successor: successor(domain.PublicationContinuationVersion,
					domain.PublicationSuccessorRemediation, nil),
			},
			want: productionPublicationRouteForward,
		},
		{
			name: "handoff conflict successor",
			task: productionPublicationTask{
				ProducingInvocationID: producer,
				Successor: successor(domain.PublicationHandoffConflictVersion,
					domain.PublicationSuccessorHandoffConflict, reentry),
			},
			want: productionPublicationRouteForward,
		},
		{
			name: "readiness invalidation successor without producer",
			task: productionPublicationTask{Successor: readinessInvalidation()},
			want: productionPublicationRouteReentryInPlace,
		},
		{
			name: "external review successor without producer",
			task: productionPublicationTask{
				Successor: successor(domain.PublicationExternalReviewVersion,
					domain.PublicationSuccessorExternalReview, reentry),
			},
			want: productionPublicationRouteReentryInPlace,
		},
		{
			name: "unpublished re-entry (v5) with producer",
			task: productionPublicationTask{ProducingInvocationID: producer, Successor: unpublishedReentry()},
			want: productionPublicationRouteRetireUnpublishedReentry,
		},
		{
			name: "successor re-entry (v6) with producer",
			task: productionPublicationTask{ProducingInvocationID: producer, Successor: successorReentry()},
			want: productionPublicationRouteRetireUnpublishedReentry,
		},
		// No builder makes a v5 or v6 task without a producer today, but
		// validateReentry doesn't check the version, so a stored row can.
		{
			name: "unpublished re-entry (v5) without producer",
			task: productionPublicationTask{Successor: unpublishedReentry()},
			want: productionPublicationRouteReentryInPlace,
		},
		{
			name: "successor re-entry (v6) without producer",
			task: productionPublicationTask{Successor: successorReentry()},
			want: productionPublicationRouteReentryInPlace,
		},
		{
			name: "continuation without successor",
			task: productionPublicationTask{ProducingInvocationID: producer, continuation: continuation},
			want: productionPublicationRouteContinuation,
		},
		{
			name: "continuation on in-place re-entry",
			task: productionPublicationTask{Successor: readinessInvalidation(), continuation: continuation},
			want: productionPublicationRouteReentryInPlace,
		},
		{
			name: "continuation on successor re-entry (v6)",
			task: productionPublicationTask{
				ProducingInvocationID: producer, Successor: successorReentry(), continuation: continuation,
			},
			want: productionPublicationRouteRetireUnpublishedReentry,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := tc.task.route()
			if got != tc.want {
				t.Errorf("route() = %q, want %q", got, tc.want)
			}
			if !slices.Contains(allProductionPublicationRoutes, got) {
				t.Errorf("route() = %q, not in allProductionPublicationRoutes", got)
			}
		})
	}
}
