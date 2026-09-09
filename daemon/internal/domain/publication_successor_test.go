package domain_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func TestPublicationSuccessorVersions(t *testing.T) {
	legacy := domain.PublicationSuccessor{
		Version: domain.PublicationSuccessorVersion, RunID: "run-1", CommandID: "return-1",
		FeedbackInvocationID: "inv-feedback-1", PredecessorItemID: "production-ready-run-1", PriorReviewInvocationID: "review-1", ReviewRound: 2,
	}
	continuation := domain.PublicationSuccessor{
		Version: domain.PublicationContinuationVersion, Origin: domain.PublicationSuccessorRemediation,
		RunID: "run-1", CommandID: "approve-1", ReevaluationCommandID: "rerun-1", PredecessorItemID: legacy.ReadyItemID(), PriorReviewInvocationID: "review-2", ReviewRound: 3,
	}
	for _, fixture := range []struct {
		name  string
		value domain.PublicationSuccessor
	}{{"publication-successor-v1", legacy}, {"publication-successor-v2", continuation}} {
		t.Run(fixture.name, func(t *testing.T) {
			if err := fixture.value.Validate(); err != nil {
				t.Fatal(err)
			}
			body, err := json.MarshalIndent(fixture.value, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden.Assert(t, fixture.name, body)
			canonical, err := json.Marshal(fixture.value)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := domain.DecodePublicationSuccessor(canonical)
			if err != nil || !reflect.DeepEqual(decoded, fixture.value) {
				t.Fatalf("round trip: %#v, %v", decoded, err)
			}
			if _, err := domain.DecodePublicationSuccessor(append(canonical, '\n')); err == nil {
				t.Fatal("noncanonical successor accepted")
			}
		})
	}
	if legacy.EffectiveOrigin() != domain.PublicationSuccessorFeedback {
		t.Fatal("legacy origin changed")
	}
	for _, origin := range domain.AllPublicationSuccessorOrigins {
		value := continuation
		value.Origin = origin
		if origin == domain.PublicationSuccessorFeedback {
			value.FeedbackInvocationID, value.ReevaluationCommandID = "feedback-1", ""
		}
		if err := value.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, outcome := range domain.AllPublicationReevaluationOutcomes {
		value := domain.PublicationReevaluationCompletion{
			RunID: "run-1", CommandID: "rerun-1", IntentKey: "intent", Outcome: outcome,
			PRHeadSHA: "head", EvidenceItemID: "item", EvidenceItemVersion: 1, TerminalInvocationID: "production-reevaluation-terminal/rerun-1",
		}
		if err := value.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	for _, mutate := range []func(*domain.PublicationSuccessor){
		func(s *domain.PublicationSuccessor) { s.Origin = "" },
		func(s *domain.PublicationSuccessor) { s.ReevaluationCommandID = "" },
		func(s *domain.PublicationSuccessor) { s.FeedbackInvocationID = "feedback" },
		func(s *domain.PublicationSuccessor) { s.Version = domain.PublicationSuccessorVersion },
	} {
		invalid := continuation
		mutate(&invalid)
		if invalid.Validate() == nil {
			t.Fatalf("invalid successor accepted: %#v", invalid)
		}
	}
	request := domain.RemediationInvocationIntent{RunID: continuation.RunID, SuccessorPublicationID: continuation.PublicationID(), Round: 2, ReviewInvocationID: "review-2"}
	if !continuation.AllowsRemediation(request) {
		t.Fatal("first continuation producer refused")
	}
	request.ReviewInvocationID = "unrelated-review"
	if continuation.AllowsRemediation(request) {
		t.Fatal("unrelated first producer accepted")
	}
	request.Round = 3
	if !continuation.AllowsRemediation(request) {
		t.Fatal("ordinary later remediation refused")
	}
	request.Round = 1
	if continuation.AllowsRemediation(request) {
		t.Fatal("old producer accepted")
	}
}
