package engine

import (
	"errors"
	"slices"
	"strings"
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
// order: in-place re-entry before continuation.
func TestProductionPublicationTaskRoute(t *testing.T) {
	t.Parallel()
	const producer = domain.InvocationID("inv-producer")
	reentry := &domain.PublicationSuccessorReentry{BaseSHA: "base", HeadSHA: "head"}
	successor := func(version string, origin domain.PublicationSuccessorOrigin, reentry *domain.PublicationSuccessorReentry) *domain.PublicationSuccessor {
		return &domain.PublicationSuccessor{Version: version, Origin: origin, Reentry: reentry}
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
			name: "continuation without successor",
			task: productionPublicationTask{ProducingInvocationID: producer, continuation: continuation},
			want: productionPublicationRouteContinuation,
		},
		{
			name: "continuation on in-place re-entry",
			task: productionPublicationTask{Successor: readinessInvalidation(), continuation: continuation},
			want: productionPublicationRouteReentryInPlace,
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

// TestProductionPublicationTaskUnderRetiredReentryIsRefused: a task an
// earlier daemon queued under a v5 or v6 successor no longer decodes, so it
// never runs down the forward route or as an in-place re-entry (#3323). The
// successors are valid, so the refusal is the version's.
func TestProductionPublicationTaskUnderRetiredReentryIsRefused(t *testing.T) {
	t.Parallel()
	const runID = domain.RunID("run-retired-reentry")
	reentry := &domain.PublicationSuccessorReentry{
		Reason:  domain.ReadinessInvalidationBaseAdvanced,
		BaseSHA: strings.Repeat("b", 40), HeadSHA: strings.Repeat("a", 40),
	}
	for _, successor := range []domain.PublicationSuccessor{
		{
			Version: domain.PublicationUnpublishedReentryVersion,
			Origin:  domain.PublicationSuccessorUnpublishedBaseAdvance,
			RunID:   runID, PredecessorItemID: productionReadyItemID(runID),
			PriorReviewInvocationID: ProductionReviewInvocationID(runID, 1), ReviewRound: 2,
			Reentry: reentry,
		},
		{
			Version: domain.PublicationSuccessorReentryVersion,
			Origin:  domain.PublicationSuccessorBaseAdvance,
			RunID:   runID, PredecessorItemID: "ready-feedback",
			PriorReviewInvocationID: ProductionReviewInvocationID(runID, 2), ReviewRound: 3,
			Reentry: reentry,
		},
	} {
		if err := successor.Validate(); err != nil {
			t.Fatalf("%s fixture: %v", successor.Version, err)
		}
		for name, task := range map[string]productionPublicationTask{
			"with producer":    validRetiredReentryTask(t, runID, successor, remediationInvocationID(runID, 1)),
			"without producer": newReentryTask("project-retired-reentry", successor),
		} {
			err := task.validate()
			if !errors.Is(err, domain.ErrParentKeyMismatch) || !strings.Contains(err.Error(), "retired re-entry") {
				t.Errorf("%s %s: validate = %v, want the retired re-entry refusal", successor.Version, name, err)
			}
		}
	}
}

func validRetiredReentryTask(
	t *testing.T, runID domain.RunID, successor domain.PublicationSuccessor, producer domain.InvocationID,
) productionPublicationTask {
	t.Helper()
	task := validPublicationTask(t, runID, "project-retired-reentry")
	task.ProducingInvocationID = producer
	task.VerificationID = productionVerificationInvocationIDForProducer(runID, producer)
	task.Replay.InvocationID = producer
	task.HeadSHA, task.Replay.HeadSHA = successor.Reentry.HeadSHA, successor.Reentry.HeadSHA
	task.PublicationID = successor.PublicationID()
	task.Successor = &successor
	return task
}
