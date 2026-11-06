package domain_test

import (
	"encoding/json"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func handoffConflictSuccessor() domain.PublicationSuccessor {
	return domain.PublicationSuccessor{
		Version: domain.PublicationHandoffConflictVersion, Origin: domain.PublicationSuccessorHandoffConflict,
		RunID: "run-1", PredecessorItemID: "production-ready-run-1",
		PriorReviewInvocationID: "review-2", ReviewRound: 3,
		Reentry:              &domain.PublicationSuccessorReentry{BaseSHA: "tip-sha", HeadSHA: "head-sha"},
		HandoffConflictCause: domain.HandoffConflictTextual,
		FirstPublication:     true,
	}
}

func TestPublicationSuccessorHandoffConflictValidation(t *testing.T) {
	for _, cause := range domain.AllHandoffConflictCauses {
		valid := handoffConflictSuccessor()
		valid.HandoffConflictCause = cause
		if err := valid.Validate(); err != nil {
			t.Fatalf("%s: %v", cause, err)
		}
	}
	updates := handoffConflictSuccessor()
	updates.FirstPublication = false
	if err := updates.Validate(); err != nil {
		t.Fatalf("a successor's handoff conflict: %v", err)
	}
	for name, mutate := range map[string]func(*domain.PublicationSuccessor){
		"no cause":             func(s *domain.PublicationSuccessor) { s.HandoffConflictCause = "" },
		"unknown cause":        func(s *domain.PublicationSuccessor) { s.HandoffConflictCause = "merge_conflict" },
		"invalidation reason":  func(s *domain.PublicationSuccessor) { s.Reentry.Reason = domain.ReadinessInvalidationBaseAdvanced },
		"no tip":               func(s *domain.PublicationSuccessor) { s.Reentry.BaseSHA = "" },
		"no head":              func(s *domain.PublicationSuccessor) { s.Reentry.HeadSHA = "" },
		"head is the tip":      func(s *domain.PublicationSuccessor) { s.Reentry.HeadSHA = s.Reentry.BaseSHA },
		"no reentry":           func(s *domain.PublicationSuccessor) { s.Reentry = nil },
		"command":              func(s *domain.PublicationSuccessor) { s.CommandID = "return-1" },
		"feedback invocation":  func(s *domain.PublicationSuccessor) { s.FeedbackInvocationID = "inv-feedback-1" },
		"reevaluation command": func(s *domain.PublicationSuccessor) { s.ReevaluationCommandID = "rerun-1" },
		"external finding":     func(s *domain.PublicationSuccessor) { s.ExternalFindingID = "external-1" },
		"profile":              func(s *domain.PublicationSuccessor) { s.AdmittingProfileDigest = "sha256:profile" },
		"other origin":         func(s *domain.PublicationSuccessor) { s.Origin = domain.PublicationSuccessorBaseAdvance },
		"other version":        func(s *domain.PublicationSuccessor) { s.Version = domain.PublicationReentryVersion },
		"first round":          func(s *domain.PublicationSuccessor) { s.ReviewRound = 1 },
	} {
		invalid := handoffConflictSuccessor()
		reentry := *invalid.Reentry
		invalid.Reentry = &reentry
		mutate(&invalid)
		if invalid.Validate() == nil {
			t.Errorf("%s: invalid handoff_conflict successor accepted", name)
		}
	}
	// No other version carries the v7 fields, and the v2 shape refuses the
	// origin.
	for name, mutate := range map[string]func(*domain.PublicationSuccessor){
		"cause":             func(s *domain.PublicationSuccessor) { s.HandoffConflictCause = domain.HandoffConflictTextual },
		"first publication": func(s *domain.PublicationSuccessor) { s.FirstPublication = true },
	} {
		for _, other := range []domain.PublicationSuccessor{
			reentrySuccessor(), externalReviewSuccessor(), unpublishedReentrySuccessor(),
		} {
			mutate(&other)
			if other.Validate() == nil {
				t.Errorf("%s successor carrying a handoff conflict %s accepted", other.EffectiveOrigin(), name)
			}
		}
	}
	continuation := domain.PublicationSuccessor{
		Version: domain.PublicationContinuationVersion, Origin: domain.PublicationSuccessorHandoffConflict,
		RunID: "run-1", CommandID: "approve-1", PredecessorItemID: "production-ready-run-1",
		PriorReviewInvocationID: "review-2", ReviewRound: 3,
	}
	if continuation.Validate() == nil {
		t.Error("a v2 record with the handoff_conflict origin was accepted")
	}
}

// The v7 fields are omitempty, so the record round-trips through the
// canonical-bytes decoder and every older version keeps its bytes.
func TestPublicationSuccessorHandoffConflictCanonical(t *testing.T) {
	successor := handoffConflictSuccessor()
	body, err := json.Marshal(successor)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := domain.DecodePublicationSuccessor(body)
	if err != nil || *decoded.Reentry != *successor.Reentry ||
		decoded.HandoffConflictCause != successor.HandoffConflictCause || !decoded.FirstPublication {
		t.Fatalf("decoded = %#v, %v", decoded, err)
	}
	older, err := json.Marshal(reentrySuccessor())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(older, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"handoff_conflict_cause", "first_publication"} {
		if _, present := fields[name]; present {
			t.Errorf("a v3 record encodes %s", name)
		}
	}
}

func TestPublicationSuccessorHandoffConflictPredicates(t *testing.T) {
	first := handoffConflictSuccessor()
	if !first.RemediatesHandoffConflict() || !first.MakesFirstPublication() ||
		first.ReentersUnpublishedHead() || first.RepublishesUnpublished() || first.AdmitsSuccessorReentry() {
		t.Fatalf("first-publication handoff conflict predicates are wrong: %#v", first)
	}
	update := first
	update.FirstPublication = false
	if update.MakesFirstPublication() {
		t.Fatal("a handoff conflict that updates a pull request makes a first publication")
	}
	if !unpublishedReentrySuccessor().MakesFirstPublication() || reentrySuccessor().MakesFirstPublication() {
		t.Fatal("MakesFirstPublication disagrees with the v5 and v3 shapes")
	}
	merge := domain.ProspectiveMergeIdentity{
		BaseSHA: first.Reentry.BaseSHA, HeadSHA: first.Reentry.HeadSHA,
		MergeSHA: "0123456789012345678901234567890123456789",
	}
	if first.AllowsProspectiveMerge(merge) {
		t.Fatal("a handoff conflict evaluates a prospective merge in place of its head")
	}
}

// A head_changed re-entry adopted a head someone else pushed, so a handoff
// conflict on it gets no remediation; every other authority's head is one a
// Freeside invocation produced.
func TestPublicationSuccessorCarriesForeignHead(t *testing.T) {
	changed := reentrySuccessor()
	reentry := *changed.Reentry
	reentry.Reason = domain.ReadinessInvalidationHeadChanged
	changed.Reentry = &reentry
	if !changed.CarriesForeignHead() {
		t.Fatal("a head_changed re-entry does not carry a foreign head")
	}
	for name, successor := range map[string]domain.PublicationSuccessor{
		"base_advanced re-entry": reentrySuccessor(),
		"handoff conflict":       handoffConflictSuccessor(),
		"unpublished re-entry":   unpublishedReentrySuccessor(),
		"no re-entry":            {},
	} {
		if successor.CarriesForeignHead() {
			t.Errorf("%s carries a foreign head", name)
		}
	}
}

// Every remediation of the cycle is at the sealed tip. The first answers the
// conflict on the prior review's round with no findings; later ones answer
// the cycle's own review rounds.
func TestPublicationSuccessorHandoffConflictRemediation(t *testing.T) {
	successor := handoffConflictSuccessor()
	first := domain.RemediationInvocationIntent{
		RunID: successor.RunID, SuccessorPublicationID: successor.PublicationID(),
		Round: successor.ReviewRound - 1, ReviewInvocationID: successor.PriorReviewInvocationID,
		BaseSHA: successor.Reentry.BaseSHA, HeadSHA: successor.Reentry.HeadSHA,
	}
	if !successor.AllowsRemediation(first) || !successor.AdmitsHandoffConflictRemediation(first) {
		t.Fatal("handoff conflict refused its first remediation")
	}
	for name, mutate := range map[string]func(*domain.RemediationInvocationIntent){
		"evidence base":     func(r *domain.RemediationInvocationIntent) { r.BaseSHA = "evidence-base" },
		"other head":        func(r *domain.RemediationInvocationIntent) { r.HeadSHA = "other-head" },
		"other review":      func(r *domain.RemediationInvocationIntent) { r.ReviewInvocationID = "review-1" },
		"earlier round":     func(r *domain.RemediationInvocationIntent) { r.Round = successor.ReviewRound - 2 },
		"a finding":         func(r *domain.RemediationInvocationIntent) { r.FindingIDs = []domain.FindingID{"finding-1"} },
		"an adjudication":   func(r *domain.RemediationInvocationIntent) { r.AdjudicationDigest = "sha256:adjudication" },
		"other publication": func(r *domain.RemediationInvocationIntent) { r.SuccessorPublicationID = "publish-feedback-1" },
		"other run":         func(r *domain.RemediationInvocationIntent) { r.RunID = "run-2" },
	} {
		request := first
		mutate(&request)
		if successor.AllowsRemediation(request) {
			t.Errorf("%s: handoff conflict admitted the first remediation", name)
		}
	}
	later := first
	later.Round, later.ReviewInvocationID, later.HeadSHA = successor.ReviewRound, "review-3", "remediated-head"
	later.FindingIDs, later.AdjudicationDigest = []domain.FindingID{"finding-3"}, "sha256:adjudication"
	if !successor.AllowsRemediation(later) || successor.AdmitsHandoffConflictRemediation(later) {
		t.Fatal("handoff conflict refused a remediation of its own review round")
	}
	later.BaseSHA = "evidence-base"
	if successor.AllowsRemediation(later) {
		t.Fatal("handoff conflict admitted a later remediation off the sealed tip")
	}
}
