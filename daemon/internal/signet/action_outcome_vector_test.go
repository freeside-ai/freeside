package signet

// The app's ActionOutcomeTests reads testdata/action-outcomes.golden and
// checks its hand-written ActionOutcome table against it
// (app/Tests/FreesideAPITests/ActionOutcomeTests.swift). A change to this
// file's output needs that test's mapping checked in the same unit.

import (
	"encoding/json"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// actionOutcomeVector is one action's row of the outcome table. Status is
// the status half of actionOutcome's result, "" for no status change; it is
// the table's value, not always what a handler writes (the revise handler
// supersedes the item whatever the table says). ByItemType lists only the
// item types whose decisionOutcome differs from the action-only one.
type actionOutcomeVector struct {
	Action     string                        `json:"action"`
	Status     string                        `json:"status"`
	Kind       string                        `json:"kind"`
	ByItemType []actionOutcomeItemTypeVector `json:"by_item_type"`
}

type actionOutcomeItemTypeVector struct {
	ItemType string `json:"item_type"`
	Status   string `json:"status"`
	Kind     string `json:"kind"`
}

func TestActionOutcomeVector(t *testing.T) {
	t.Parallel()
	vector := make([]actionOutcomeVector, 0, len(domain.AllActions))
	for _, action := range domain.AllActions {
		status, kind := actionOutcome(action)
		entry := actionOutcomeVector{
			Action:     string(action),
			Status:     string(status),
			Kind:       outcomeKindName(t, kind),
			ByItemType: []actionOutcomeItemTypeVector{},
		}
		for _, itemType := range domain.AllAttentionTypes {
			typedStatus, typedKind := decisionOutcome(itemType, action)
			if typedStatus == status && typedKind == kind {
				continue
			}
			entry.ByItemType = append(entry.ByItemType, actionOutcomeItemTypeVector{
				ItemType: string(itemType),
				Status:   string(typedStatus),
				Kind:     outcomeKindName(t, typedKind),
			})
		}
		vector = append(vector, entry)
	}
	b, err := json.MarshalIndent(vector, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "action-outcomes", b)
}

// outcomeKindName names a kind for the vector. A behaviour switch with no
// default, so the exhaustive linter fails when a kind is added without a
// name.
func outcomeKindName(t *testing.T, kind outcomeKind) string {
	t.Helper()
	switch kind {
	case outcomeConcludes:
		return "concludes"
	case outcomeRecords:
		return "records"
	case outcomeDiscusses:
		return "discusses"
	case outcomePending:
		return "pending"
	case outcomeStopsUnattended:
		return "stops_unattended"
	case outcomeResumesUnattended:
		return "resumes_unattended"
	case outcomeRecoversReview:
		return "recovers_review"
	case outcomeRerunsTrustEvaluation:
		return "reruns_trust_evaluation"
	case outcomeAdoptsReviewConfiguration:
		return "adopts_review_configuration"
	case outcomeResolvesReenrollment:
		return "resolves_reenrollment"
	case outcomeAcceptsRecommendedRoute:
		return "accepts_recommended_route"
	case outcomeChoosesAlternativeRoute:
		return "chooses_alternative_route"
	case outcomeAdjudicatesFindings:
		return "adjudicates_findings"
	case outcomeStartsProposal:
		return "starts_proposal"
	case outcomeRevisesAndStartsProposal:
		return "revises_and_starts_proposal"
	case outcomeDeclinesProposal:
		return "declines_proposal"
	case outcomeSnoozesProposal:
		return "snoozes_proposal"
	case outcomeAnswersAndRetries:
		return "answers_and_retries"
	case outcomeReturnsToAgent:
		return "returns_to_agent"
	case outcomeRetriesWithCapabilities:
		return "retries_with_capabilities"
	case outcomeRetriesWithAlternateAgent:
		return "retries_with_alternate_agent"
	case outcomeRunsDoctor:
		return "runs_doctor"
	}
	t.Fatalf("outcome kind %d has no vector name", kind)
	return ""
}
