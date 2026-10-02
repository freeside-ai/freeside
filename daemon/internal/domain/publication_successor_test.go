package domain_test

import (
	"encoding/json"
	"reflect"
	"strings"
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
	reentry := reentrySuccessor()
	for _, fixture := range []struct {
		name  string
		value domain.PublicationSuccessor
	}{
		{"publication-successor-v1", legacy},
		{"publication-successor-v2", continuation},
		{"publication-successor-v3", reentry},
		{"publication-successor-v4", externalReviewSuccessor()},
	} {
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
		switch origin {
		case domain.PublicationSuccessorFeedback:
			value.FeedbackInvocationID, value.ReevaluationCommandID = "feedback-1", ""
		case domain.PublicationSuccessorReadinessInvalidation:
			value = reentry
		case domain.PublicationSuccessorExternalReview:
			value = externalReviewSuccessor()
		case domain.PublicationSuccessorRemediation:
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
		func(s *domain.PublicationSuccessor) { s.Version = domain.PublicationReentryVersion },
		func(s *domain.PublicationSuccessor) { s.Origin = domain.PublicationSuccessorReadinessInvalidation },
		func(s *domain.PublicationSuccessor) { s.Reentry = reentry.Reentry },
		func(s *domain.PublicationSuccessor) { s.CommandID = "" },
		func(s *domain.PublicationSuccessor) { s.Origin = domain.PublicationSuccessorExternalReview },
		func(s *domain.PublicationSuccessor) { s.ExternalFindingID = "external-1" },
		func(s *domain.PublicationSuccessor) { s.AdmittingProfileDigest = "sha256:profile" },
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

func reentrySuccessor() domain.PublicationSuccessor {
	return domain.PublicationSuccessor{
		Version: domain.PublicationReentryVersion, Origin: domain.PublicationSuccessorReadinessInvalidation,
		RunID: "run-1", PredecessorItemID: "production-ready-run-1", PriorReviewInvocationID: "review-1", ReviewRound: 2,
		Reentry: &domain.PublicationSuccessorReentry{
			Reason: domain.ReadinessInvalidationBaseAdvanced, BaseSHA: "base-2", HeadSHA: "head-1",
		},
	}
}

func externalReviewSuccessor() domain.PublicationSuccessor {
	return domain.PublicationSuccessor{
		Version: domain.PublicationExternalReviewVersion, Origin: domain.PublicationSuccessorExternalReview,
		RunID: "run-1", PredecessorItemID: "production-ready-run-1", PriorReviewInvocationID: "review-1", ReviewRound: 2,
		Reentry:           &domain.PublicationSuccessorReentry{BaseSHA: "base-1", HeadSHA: "head-1"},
		ExternalFindingID: "external-0123456789abcdef0123456789abcdef", AdmittingProfileDigest: "sha256:profile",
	}
}

// An external_review record carries the triggering finding and the admitting
// profile and nothing an operator command or a readiness invalidation would;
// the earlier versions carry neither of its two fields.
func TestPublicationSuccessorExternalReviewValidation(t *testing.T) {
	if err := externalReviewSuccessor().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*domain.PublicationSuccessor){
		"no finding":           func(s *domain.PublicationSuccessor) { s.ExternalFindingID = "" },
		"no profile":           func(s *domain.PublicationSuccessor) { s.AdmittingProfileDigest = "" },
		"invalidation reason":  func(s *domain.PublicationSuccessor) { s.Reentry.Reason = domain.ReadinessInvalidationBaseAdvanced },
		"no base":              func(s *domain.PublicationSuccessor) { s.Reentry.BaseSHA = "" },
		"no head":              func(s *domain.PublicationSuccessor) { s.Reentry.HeadSHA = "" },
		"no reentry":           func(s *domain.PublicationSuccessor) { s.Reentry = nil },
		"command":              func(s *domain.PublicationSuccessor) { s.CommandID = "return-1" },
		"feedback invocation":  func(s *domain.PublicationSuccessor) { s.FeedbackInvocationID = "inv-feedback-1" },
		"reevaluation command": func(s *domain.PublicationSuccessor) { s.ReevaluationCommandID = "rerun-1" },
		"invalidation origin":  func(s *domain.PublicationSuccessor) { s.Origin = domain.PublicationSuccessorReadinessInvalidation },
		"reentry version":      func(s *domain.PublicationSuccessor) { s.Version = domain.PublicationReentryVersion },
		"first round":          func(s *domain.PublicationSuccessor) { s.ReviewRound = 1 },
	} {
		invalid := externalReviewSuccessor()
		reentry := *invalid.Reentry
		invalid.Reentry = &reentry
		mutate(&invalid)
		if invalid.Validate() == nil {
			t.Errorf("%s: invalid external_review successor accepted", name)
		}
	}
	for name, mutate := range map[string]func(*domain.PublicationSuccessor){
		"finding": func(s *domain.PublicationSuccessor) { s.ExternalFindingID = "external-1" },
		"profile": func(s *domain.PublicationSuccessor) { s.AdmittingProfileDigest = "sha256:profile" },
		"origin":  func(s *domain.PublicationSuccessor) { s.Origin = domain.PublicationSuccessorExternalReview },
	} {
		invalid := reentrySuccessor()
		mutate(&invalid)
		if invalid.Validate() == nil {
			t.Errorf("readiness re-entry carrying an external review %s accepted", name)
		}
	}
	// One superseded item admits one commandless successor, whatever its
	// origin: the two origins share every predecessor-keyed identity.
	external, invalidated := externalReviewSuccessor(), reentrySuccessor()
	if external.PublicationID() != invalidated.PublicationID() || external.Key() != invalidated.Key() ||
		external.ReadyItemID() != invalidated.ReadyItemID() || external.BlockedItemID() != invalidated.BlockedItemID() ||
		external.TaskKey() != invalidated.TaskKey() {
		t.Fatal("external_review and readiness_invalidation re-entries of one item have different identities")
	}
}

// The head an external reviewer commented on is Freeside's own, so
// remediation may replace it, under the same one-base rule as a base advance.
func TestPublicationSuccessorExternalReviewRemediation(t *testing.T) {
	external := externalReviewSuccessor()
	request := domain.RemediationInvocationIntent{
		RunID: external.RunID, SuccessorPublicationID: external.PublicationID(), Round: external.ReviewRound,
		BaseSHA: external.Reentry.BaseSHA, HeadSHA: external.Reentry.HeadSHA,
	}
	if !external.AllowsRemediation(request) {
		t.Fatal("external_review re-entry refused remediation at its review round")
	}
	otherBase, otherHead := request, request
	otherBase.BaseSHA, otherHead.HeadSHA = "another-base", "another-head"
	if external.AllowsRemediation(otherBase) || external.AllowsRemediation(otherHead) {
		t.Fatal("external_review re-entry admitted a first-round request for other coordinates")
	}
	otherHead.Round, otherBase.Round = external.ReviewRound+1, external.ReviewRound+1
	if !external.AllowsRemediation(otherHead) || external.AllowsRemediation(otherBase) {
		t.Fatal("later-round remediation is not bound to the re-entry base alone")
	}
	request.Round, request.ReviewInvocationID = external.ReviewRound-1, external.PriorReviewInvocationID
	if external.AllowsRemediation(request) {
		t.Fatal("external_review re-entry admitted the prior round's findings")
	}
}

func TestPublicationSuccessorReentryValidation(t *testing.T) {
	for _, reason := range []domain.ReadinessInvalidationReason{
		domain.ReadinessInvalidationBaseAdvanced, domain.ReadinessInvalidationHeadChanged,
	} {
		valid := reentrySuccessor()
		valid.Reentry.Reason = reason
		if err := valid.Validate(); err != nil {
			t.Fatalf("%s: %v", reason, err)
		}
	}
	for name, mutate := range map[string]func(*domain.PublicationSuccessor){
		"retargeted":           func(s *domain.PublicationSuccessor) { s.Reentry.Reason = domain.ReadinessInvalidationRetargeted },
		"identity changed":     func(s *domain.PublicationSuccessor) { s.Reentry.Reason = domain.ReadinessInvalidationIdentityChanged },
		"unknown reason":       func(s *domain.PublicationSuccessor) { s.Reentry.Reason = "merged" },
		"no reason":            func(s *domain.PublicationSuccessor) { s.Reentry.Reason = "" },
		"no base":              func(s *domain.PublicationSuccessor) { s.Reentry.BaseSHA = "" },
		"no head":              func(s *domain.PublicationSuccessor) { s.Reentry.HeadSHA = "" },
		"no reentry":           func(s *domain.PublicationSuccessor) { s.Reentry = nil },
		"command":              func(s *domain.PublicationSuccessor) { s.CommandID = "return-1" },
		"feedback invocation":  func(s *domain.PublicationSuccessor) { s.FeedbackInvocationID = "inv-feedback-1" },
		"reevaluation command": func(s *domain.PublicationSuccessor) { s.ReevaluationCommandID = "rerun-1" },
		"feedback origin":      func(s *domain.PublicationSuccessor) { s.Origin = domain.PublicationSuccessorFeedback },
		"remediation origin":   func(s *domain.PublicationSuccessor) { s.Origin = domain.PublicationSuccessorRemediation },
		"no origin":            func(s *domain.PublicationSuccessor) { s.Origin = "" },
		"continuation version": func(s *domain.PublicationSuccessor) { s.Version = domain.PublicationContinuationVersion },
		"legacy version":       func(s *domain.PublicationSuccessor) { s.Version = domain.PublicationSuccessorVersion },
		"no predecessor":       func(s *domain.PublicationSuccessor) { s.PredecessorItemID = "" },
		"no prior review":      func(s *domain.PublicationSuccessor) { s.PriorReviewInvocationID = "" },
		"first round":          func(s *domain.PublicationSuccessor) { s.ReviewRound = 1 },
		"no run":               func(s *domain.PublicationSuccessor) { s.RunID = "" },
	} {
		invalid := reentrySuccessor()
		mutate(&invalid)
		if invalid.Validate() == nil {
			t.Errorf("%s: invalid re-entry successor accepted: %#v", name, invalid)
		}
	}
	// A legacy feedback record may not smuggle re-entry coordinates.
	legacy := domain.PublicationSuccessor{
		Version: domain.PublicationSuccessorVersion, RunID: "run-1", CommandID: "return-1",
		FeedbackInvocationID: "inv-feedback-1", PredecessorItemID: "production-ready-run-1", PriorReviewInvocationID: "review-1", ReviewRound: 2,
		Reentry: reentrySuccessor().Reentry,
	}
	if legacy.Validate() == nil {
		t.Fatal("legacy feedback successor accepted a re-entry object")
	}
}

// The re-entered cycle's identities depend only on the run and the superseded
// item, so a later commandless origin joins without changing them (#1622).
func TestPublicationSuccessorReentryIdentities(t *testing.T) {
	base := reentrySuccessor()
	type identities struct {
		publication  domain.InvocationID
		key, taskKey string
		ready        domain.ItemID
		blocked      domain.ItemID
	}
	of := func(s domain.PublicationSuccessor) identities {
		return identities{s.PublicationID(), s.Key(), s.TaskKey(), s.ReadyItemID(), s.BlockedItemID()}
	}
	want := of(base)
	varied := reentrySuccessor()
	varied.Reentry = &domain.PublicationSuccessorReentry{
		Reason: domain.ReadinessInvalidationHeadChanged, BaseSHA: "base-9", HeadSHA: "head-9",
	}
	varied.PriorReviewInvocationID, varied.ReviewRound = "review-7", 8
	if got := of(varied); got != want {
		t.Fatalf("identities depend on the reason or coordinates:\n got %#v\nwant %#v", got, want)
	}
	otherItem := reentrySuccessor()
	otherItem.PredecessorItemID = want.ready
	next := of(otherItem)
	otherRun := reentrySuccessor()
	otherRun.RunID = "run-2"
	if next.publication == want.publication || next.key == want.key || next.taskKey == want.taskKey ||
		next.ready == want.ready || next.blocked == want.blocked {
		t.Fatalf("a second re-entry reuses an identity: %#v", next)
	}
	if of(otherRun).key == want.key || of(otherRun).taskKey == want.taskKey {
		t.Fatal("run does not scope the outbox keys")
	}
	if want.ready == base.PredecessorItemID || want.blocked == base.PredecessorItemID {
		t.Fatal("re-entered item reuses the superseded item's ID")
	}
	for _, origin := range []domain.PublicationSuccessorOrigin{
		domain.PublicationSuccessorFeedback, domain.PublicationSuccessorRemediation,
	} {
		// The command names the superseded item, the closest a command-keyed
		// identity can come to the re-entry key.
		commanded := of(domain.PublicationSuccessor{
			Version: domain.PublicationContinuationVersion, Origin: origin, RunID: base.RunID,
			CommandID: string(base.PredecessorItemID), PredecessorItemID: base.PredecessorItemID,
		})
		if commanded.publication == want.publication || commanded.key == want.key || commanded.taskKey == want.taskKey ||
			commanded.ready == want.ready || commanded.blocked == want.blocked {
			t.Fatalf("%s identity collides with re-entry: %#v", origin, commanded)
		}
	}
	// Item IDs are free text; the outbox key takes the publication ID unescaped.
	hostile := reentrySuccessor()
	hostile.PredecessorItemID = "item/../with spaces"
	if id := string(hostile.PublicationID()); len(id) != len("publish-reentry-")+64 || strings.ContainsAny(id, "/ .") {
		t.Fatalf("publication id %q is not a path-safe digest key", id)
	}
}

func TestPublicationSuccessorReentryRemediation(t *testing.T) {
	advanced := reentrySuccessor()
	request := domain.RemediationInvocationIntent{
		RunID: advanced.RunID, SuccessorPublicationID: advanced.PublicationID(), Round: advanced.ReviewRound,
		BaseSHA: advanced.Reentry.BaseSHA, HeadSHA: advanced.Reentry.HeadSHA,
	}
	if !advanced.AllowsRemediation(request) {
		t.Fatal("base_advanced re-entry refused remediation at its review round")
	}
	// The authority is for one base and, at its first round, one head.
	otherBase := request
	otherBase.BaseSHA = "another-base"
	otherHead := request
	otherHead.HeadSHA = "another-head"
	if advanced.AllowsRemediation(otherBase) || advanced.AllowsRemediation(otherHead) {
		t.Fatal("base_advanced re-entry admitted a first-round request for other coordinates")
	}
	// A later round reviews the head remediation pushed, on the same base.
	otherHead.Round, otherBase.Round = advanced.ReviewRound+1, advanced.ReviewRound+1
	if !advanced.AllowsRemediation(otherHead) || advanced.AllowsRemediation(otherBase) {
		t.Fatal("later-round remediation is not bound to the re-entry base alone")
	}
	request.Round, request.ReviewInvocationID = advanced.ReviewRound-1, advanced.PriorReviewInvocationID
	if advanced.AllowsRemediation(request) {
		t.Fatal("base_advanced re-entry admitted the prior round's findings")
	}
	changed := reentrySuccessor()
	changed.Reentry.Reason = domain.ReadinessInvalidationHeadChanged
	for _, round := range []int{changed.ReviewRound - 1, changed.ReviewRound, changed.ReviewRound + 1} {
		request.Round = round
		if changed.AllowsRemediation(request) {
			t.Fatalf("head_changed re-entry admitted remediation at round %d", round)
		}
	}
}
