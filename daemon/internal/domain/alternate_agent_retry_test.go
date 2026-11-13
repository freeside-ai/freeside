package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestQualifyAlternateFollowsTheFailureClass(t *testing.T) {
	failed := domain.FailureFacts{Route: "route-a", UsagePool: "pool-a"}
	otherPool := domain.FailureAlternate{Route: "route-a", UsagePool: "pool-b"}
	otherRoute := domain.FailureAlternate{Route: "route-b", UsagePool: "pool-a"}
	validGeneration := domain.FailureAlternate{Route: "route-a", UsagePool: "pool-a", GenerationValid: true}
	cases := []struct {
		name  string
		cause domain.FailureCause
		facts domain.FailureFacts
		alt   domain.FailureAlternate
		want  domain.AlternateAgentQualification
		ok    bool
	}{
		{"quota, other pool", domain.FailureCauseQuota, failed, otherPool, domain.AlternateAgentDifferentPool, true},
		{"quota, same pool", domain.FailureCauseQuota, failed, otherRoute, domain.AlternateAgentDifferentPool, false},
		{"quota, failed pool unknown", domain.FailureCauseQuota, domain.FailureFacts{}, otherPool, domain.AlternateAgentDifferentPool, false},
		{"expiry, valid generation", domain.FailureCauseCredentialExpired, failed, validGeneration, domain.AlternateAgentValidGeneration, true},
		{"revocation, no valid generation", domain.FailureCauseCredentialRevoked, failed, otherPool, domain.AlternateAgentValidGeneration, false},
		{"capacity, other route", domain.FailureCauseServiceCapacity, failed, otherRoute, domain.AlternateAgentDifferentRoute, true},
		{"capacity, same route", domain.FailureCauseServiceCapacity, failed, otherPool, domain.AlternateAgentDifferentRoute, false},
		{"invalid evidence", domain.FailureCauseInvalidEvidence, failed, validGeneration, "", false},
		{"unknown cause", domain.FailureCauseUnknown, failed, validGeneration, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := domain.QualifyAlternate(tc.cause, tc.facts, tc.alt)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("QualifyAlternate = %q, %v; want %q, %v", got, ok, tc.want, tc.ok)
			}
			facts := tc.facts
			facts.Alternates = []domain.FailureAlternate{tc.alt}
			eligible := domain.DecideFailureDisposition(tc.cause, facts) == domain.FailureDispositionAlternateAgentEligible
			if eligible != tc.ok {
				t.Fatalf("DecideFailureDisposition eligible = %v, want %v", eligible, tc.ok)
			}
		})
	}
}

func testOffer(enrollment domain.ClientEnrollmentID) domain.AlternateAgentOffer {
	return domain.AlternateAgentOffer{
		Enrollment: enrollment, Pool: "pool-b", Harness: domain.HarnessClientClaudeCode,
		Model: "claude-opus-4-1", Qualification: domain.AlternateAgentDifferentPool,
	}
}

// testV1Offer is an offer as stored before #2735, naming a tree agent.
func testV1Offer(agent string) domain.AlternateAgentOffer {
	offer := testOffer("")
	offer.Agent = agent
	return offer
}

func TestValidateAlternateAgentOffers(t *testing.T) {
	cases := []struct {
		name   string
		offers []domain.AlternateAgentOffer
		want   error
	}{
		{"none", []domain.AlternateAgentOffer{}, domain.ErrEmptyField},
		{"unsorted", []domain.AlternateAgentOffer{testOffer("b"), testOffer("a")}, domain.ErrCardFactInconsistent},
		{"repeated", []domain.AlternateAgentOffer{testOffer("a"), testOffer("a")}, domain.ErrCardFactInconsistent},
		{"no pool", []domain.AlternateAgentOffer{func() domain.AlternateAgentOffer {
			o := testOffer("a")
			o.Pool = ""
			return o
		}()}, domain.ErrEmptyID},
		{"unknown rule", []domain.AlternateAgentOffer{func() domain.AlternateAgentOffer {
			o := testOffer("a")
			o.Qualification = "nearest"
			return o
		}()}, domain.ErrInvalidAlternateAgentQualification},
		{"neither enrollment nor agent", []domain.AlternateAgentOffer{testOffer("")}, domain.ErrEmptyID},
		{"enrollment and agent", []domain.AlternateAgentOffer{func() domain.AlternateAgentOffer {
			o := testOffer("a")
			o.Agent = "claude-a"
			return o
		}()}, domain.ErrCardFactInconsistent},
		{"mixed shapes", []domain.AlternateAgentOffer{testOffer("a"), testV1Offer("b")}, domain.ErrCardFactInconsistent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := domain.ValidateAlternateAgentOffers(tc.offers); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if err := domain.ValidateAlternateAgentOffers([]domain.AlternateAgentOffer{testOffer("a"), testOffer("b")}); err != nil {
		t.Fatalf("sorted offers: %v", err)
	}
	if err := domain.ValidateAlternateAgentOffers([]domain.AlternateAgentOffer{testV1Offer("a"), testV1Offer("b")}); err != nil {
		t.Fatalf("sorted v1 offers: %v", err)
	}
	offers := []domain.AlternateAgentOffer{testOffer("b"), testOffer("a")}
	domain.SortAlternateAgentOffers(offers)
	if offers[0].Enrollment != "a" {
		t.Fatalf("sorted = %+v, want by enrollment", offers)
	}
	if _, ok := domain.OfferedEnrollment(offers, "b"); !ok {
		t.Fatal("an offered enrollment was not found")
	}
	if _, ok := domain.OfferedEnrollment([]domain.AlternateAgentOffer{testV1Offer("b")}, "b"); ok {
		t.Fatal("a v1 offer matched an enrollment")
	}
}

// TestAlternateAgentChoiceMessage: a choice's message is its canonical JSON,
// which parses back; another form of the same choice, or a malformed one,
// is refused, and a model or effort is optional.
func TestAlternateAgentChoiceMessage(t *testing.T) {
	choice := domain.AlternateAgentChoice{EnrollmentID: "claude-b/claude_code", ModelID: "claude-opus-4-5", Effort: "high"}
	message := choice.Message()
	if message != `{"enrollment_id":"claude-b/claude_code","model_id":"claude-opus-4-5","effort":"high"}` {
		t.Fatalf("message = %s", message)
	}
	if got, err := domain.ParseAlternateAgentChoice(message); err != nil || got != choice {
		t.Fatalf("parse = %+v, %v; want %+v", got, err, choice)
	}
	bare := domain.AlternateAgentChoice{EnrollmentID: "claude-b/claude_code"}
	if got, err := domain.ParseAlternateAgentChoice(bare.Message()); err != nil || got != bare {
		t.Fatalf("parse bare = %+v, %v", got, err)
	}
	for name, message := range map[string]string{
		"reordered":      `{"model_id":"claude-opus-4-5","enrollment_id":"claude-b/claude_code","effort":"high"}`,
		"spaced":         `{"enrollment_id": "claude-b/claude_code","model_id":"claude-opus-4-5","effort":"high"}`,
		"an agent name":  "claude-b",
		"no enrollment":  `{"enrollment_id":"","model_id":"claude-opus-4-5","effort":"high"}`,
		"a bad effort":   `{"enrollment_id":"claude-b/claude_code","model_id":"claude-opus-4-5","effort":"very high"}`,
		"an extra field": `{"enrollment_id":"claude-b/claude_code","model_id":"","effort":"","agent":"x"}`,
	} {
		if _, err := domain.ParseAlternateAgentChoice(message); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	selection := domain.TaskLineSelection(choice)
	if !choice.Chose(selection) || !bare.Chose(selection) {
		t.Fatal("a choice did not choose its own selection")
	}
	other := selection
	other.Effort = "max"
	if choice.Chose(other) || bare.Chose(domain.TaskLineSelection{EnrollmentID: "claude-c/claude_code"}) {
		t.Fatal("a choice chose another selection")
	}
}

var testRetrySelection = domain.TaskLineSelection{
	EnrollmentID: "claude-b/claude_code", ModelID: "claude-opus-4-5", Effort: "high",
}

func testRetry(t *testing.T) domain.AlternateAgentRetry {
	t.Helper()
	selection := testRetrySelection
	record, err := domain.NewAlternateAgentRetry(domain.AlternateAgentRetry{
		TaskID: "task-1", Role: domain.RoleImplementer, FailedInvocationID: "inv-1",
		CommandID: "cmd-1", Selection: &selection, Pool: "pool-b",
		Qualification: domain.AlternateAgentDifferentPool,
		RecordedAt:    time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestAlternateAgentRetryIdentity(t *testing.T) {
	record := testRetry(t)
	if record.RecordedAt.Location() != time.UTC {
		t.Fatal("recorded_at was not normalized to UTC")
	}
	if !record.ParkedMove() {
		t.Fatal("a record with no item is a parked move")
	}
	tampered := record
	tampered.Selection = &domain.TaskLineSelection{EnrollmentID: "claude-c/claude_code", ModelID: "m", Effort: "high"}
	if err := tampered.Validate(); !errors.Is(err, domain.ErrAlternateAgentRetryInconsistent) {
		t.Fatalf("an edited selection: err = %v", err)
	}
	both := record
	both.Agent = "claude-b"
	if err := both.Validate(); err == nil {
		t.Fatal("a record with an agent and a selection validated")
	}
	if _, err := domain.NewAlternateAgentRetry(both); err == nil {
		t.Fatal("a record with an agent was built")
	}
	// A v1 record, naming its agent, keeps a different id and validates.
	v1 := record
	v1.Selection, v1.Agent = nil, "claude-b"
	var err error
	if v1.ID, err = v1.ComputeID(); err != nil {
		t.Fatal(err)
	}
	if v1.ID == record.ID || v1.Validate() != nil {
		t.Fatalf("v1 record %s validates %v, want its own id", v1.ID, v1.Validate())
	}
	ineligible := record
	ineligible.Role = domain.RoleShadowReviewer
	if err := ineligible.Validate(); !errors.Is(err, domain.ErrTaskLineRoleIneligible) {
		t.Fatalf("a role with no line: err = %v", err)
	}
	noCommand := record
	noCommand.CommandID = ""
	if err := noCommand.Validate(); !errors.Is(err, domain.ErrEmptyID) {
		t.Fatalf("no command: err = %v", err)
	}
}

func TestAttentionItemAlternateAgentsFollowTheOffer(t *testing.T) {
	runID := domain.RunID("run-1")
	base := func() domain.AttentionItemInput {
		return domain.AttentionItemInput{
			ID: "item-1", ProjectID: "proj-1",
			Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID},
			Type:    domain.AttentionExecutionFailure, Priority: domain.PriorityHigh, Reason: "failed",
			RequestedDecision: []domain.Action{domain.ActionRetryWithAlternateAgent, domain.ActionStop},
			AlternateAgents:   []domain.AlternateAgentOffer{testOffer("a")},
			ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
		}
	}
	if _, err := domain.NewAttentionItem(base(), nil); err != nil {
		t.Fatalf("offer with agents: %v", err)
	}
	noAgents := base()
	noAgents.AlternateAgents = nil
	if _, err := domain.NewAttentionItem(noAgents, nil); !errors.Is(err, domain.ErrCardFactInconsistent) {
		t.Fatalf("action without agents: err = %v", err)
	}
	noAction := base()
	noAction.RequestedDecision = []domain.Action{domain.ActionStop}
	if _, err := domain.NewAttentionItem(noAction, nil); !errors.Is(err, domain.ErrCardFactInconsistent) {
		t.Fatalf("agents without action: err = %v", err)
	}
	wrongType := base()
	wrongType.Type = domain.AttentionPublishBlocked
	if _, err := domain.NewAttentionItem(wrongType, nil); err == nil {
		t.Fatal("agents on a publish_blocked item were accepted")
	}
}

func TestProductionAttemptAlternateAgentBindings(t *testing.T) {
	command, invocation := "cmd-1", domain.InvocationID("inv-1")
	record := domain.Digest("sha256:" + strings.Repeat("0", 64))
	manifest := record
	attempt := domain.ProductionAttempt{
		CampaignID: "campaign-1", AttemptNumber: 2, Kind: domain.ProductionAttemptRetry,
		Reason: "operator alternate agent retry cmd-1", ParentRunID: "run-1",
		SourceDigest: "source", ApprovedSpecDigest: "spec",
		SpecificationRunID: "spec-2", ImplementationRunID: "run-2",
		OperatorCommandID: &command, RetryOfInvocationID: &invocation, AlternateAgentRetryID: &record,
	}
	if err := attempt.Validate(); err != nil {
		t.Fatalf("alternate agent triple: %v", err)
	}
	both := attempt
	both.CapabilityManifestDigest = &manifest
	if err := both.Validate(); !errors.Is(err, domain.ErrProductionAttemptInconsistent) {
		t.Fatalf("both bindings: err = %v", err)
	}
	partial := attempt
	partial.RetryOfInvocationID = nil
	if err := partial.Validate(); !errors.Is(err, domain.ErrProductionAttemptInconsistent) {
		t.Fatalf("partial bindings: err = %v", err)
	}
}

func TestParkedTaskMoveReceiptBindsItsRecord(t *testing.T) {
	record := testRetry(t)
	provenance := &domain.TaskLineProvenance{
		PolicyRevision: 1, Tier: domain.TaskLineSelectionPolicyOrder, Pool: "pool-b",
		RankedBy: []domain.TaskLineRankingReading{},
	}
	moved := func(selection domain.TaskLineSelection) domain.TaskLine {
		line, err := domain.NewTaskLine(domain.TaskLineInput{
			TaskID: "task-1", Role: domain.RoleImplementer, Selection: &selection,
			Source: domain.TaskLineSourceAlternateAgentMove, SetBy: string(record.ID), Provenance: provenance,
		}, 1, nil, record.RecordedAt)
		if err != nil {
			t.Fatal(err)
		}
		return line
	}
	selection := testRetrySelection
	receipt := domain.ParkedTaskMoveReceipt{
		ParkedTaskMoveRequest: domain.ParkedTaskMoveRequest{
			CommandIdentity: domain.CommandIdentity{CommandID: "cmd-1", DeviceID: "device-1"},
			TaskID:          "task-1", Agent: "claude-b", Selection: &selection,
		},
		Retry: record, Lines: []domain.TaskLine{moved(testRetrySelection)},
	}
	if err := (domain.CommandReceipt{ParkedTaskMove: &receipt}).Validate(); err != nil {
		t.Fatalf("receipt: %v", err)
	}
	unmoved := receipt
	unmoved.Lines = nil
	if err := unmoved.Validate(); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("no moved line: err = %v", err)
	}
	other := testRetrySelection
	other.Effort = "max"
	elsewhere := receipt
	elsewhere.Lines = []domain.TaskLine{moved(other)}
	if err := elsewhere.Validate(); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("a line on another selection: err = %v", err)
	}
	foreign := receipt
	foreign.Selection = &other
	if err := foreign.Validate(); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("another selection: err = %v", err)
	}
	if !receipt.Same(receipt.ParkedTaskMoveRequest) ||
		receipt.Same(foreign.ParkedTaskMoveRequest) {
		t.Fatal("Same did not compare the selection")
	}
	// Since #2808 the request stores the choice the operator sent, which
	// the record's selection must satisfy.
	chosen := receipt
	chosen.Agent, chosen.Selection = "", nil
	chosen.Choice = &domain.AlternateAgentChoice{EnrollmentID: testRetrySelection.EnrollmentID, Effort: "high"}
	if err := chosen.Validate(); err != nil {
		t.Fatalf("a choice receipt: %v", err)
	}
	unchosen := chosen
	unchosen.Choice = &domain.AlternateAgentChoice{EnrollmentID: testRetrySelection.EnrollmentID, Effort: "max"}
	if err := unchosen.Validate(); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("a choice the record does not satisfy: err = %v", err)
	}
	both := chosen
	both.Agent = "claude-b"
	if err := both.Validate(); !errors.Is(err, domain.ErrInvalidTaskLineSelection) {
		t.Fatalf("a choice beside an agent: err = %v", err)
	}
	agentOnly := receipt.ParkedTaskMoveRequest
	agentOnly.Selection = nil
	if !chosen.Same(chosen.ParkedTaskMoveRequest) || chosen.Same(unchosen.ParkedTaskMoveRequest) ||
		chosen.Same(receipt.ParkedTaskMoveRequest) || chosen.Same(agentOnly) {
		t.Fatal("Same did not compare the choice")
	}
	if _, err := domain.NewTaskLine(domain.TaskLineInput{
		TaskID: "task-1", Role: domain.RoleImplementer, Selection: &selection,
		Source: domain.TaskLineSourceAlternateAgentMove, SetBy: string(record.ID),
	}, 1, nil, record.RecordedAt); !errors.Is(err, domain.ErrTaskLineProvenanceInconsistent) {
		t.Fatalf("a move line without provenance: err = %v", err)
	}
}
