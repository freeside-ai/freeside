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

func testOffer(agent string) domain.AlternateAgentOffer {
	return domain.AlternateAgentOffer{
		Agent: agent, Pool: "pool-b", Harness: domain.HarnessClientClaudeCode,
		Model: "claude-opus-4-1", Qualification: domain.AlternateAgentDifferentPool,
	}
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
}

func testRetry(t *testing.T) domain.AlternateAgentRetry {
	t.Helper()
	record, err := domain.NewAlternateAgentRetry(domain.AlternateAgentRetry{
		TaskID: "task-1", Role: domain.RoleImplementer, FailedInvocationID: "inv-1",
		CommandID: "cmd-1", Agent: "claude-b", Pool: "pool-b",
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
	tampered.Agent = "claude-c"
	if err := tampered.Validate(); !errors.Is(err, domain.ErrAlternateAgentRetryInconsistent) {
		t.Fatalf("an edited agent: err = %v", err)
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
	line, err := domain.NewTaskLine(domain.TaskLineInput{
		TaskID: "task-1", Role: domain.RoleImplementer, Agent: "claude-b",
		Source: domain.TaskLineSourceAlternateAgentMove, SetBy: string(record.ID),
		Provenance: &domain.TaskLineProvenance{
			PolicyRevision: 1, Tier: domain.TaskLineSelectionPolicyOrder, Pool: "pool-b",
			RankedBy: []domain.TaskLineRankingReading{},
		},
	}, 1, nil, record.RecordedAt)
	if err != nil {
		t.Fatal(err)
	}
	receipt := domain.ParkedTaskMoveReceipt{
		ParkedTaskMoveRequest: domain.ParkedTaskMoveRequest{
			CommandIdentity: domain.CommandIdentity{CommandID: "cmd-1", DeviceID: "device-1"},
			TaskID:          "task-1", Agent: "claude-b",
		},
		Retry: record, Lines: []domain.TaskLine{line},
	}
	if err := (domain.CommandReceipt{ParkedTaskMove: &receipt}).Validate(); err != nil {
		t.Fatalf("receipt: %v", err)
	}
	unmoved := receipt
	unmoved.Lines = nil
	if err := unmoved.Validate(); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("no moved line: err = %v", err)
	}
	foreign := receipt
	foreign.Agent = "claude-c"
	if err := foreign.Validate(); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("another agent: err = %v", err)
	}
	if _, err := domain.NewTaskLine(domain.TaskLineInput{
		TaskID: "task-1", Role: domain.RoleImplementer, Agent: "claude-b",
		Source: domain.TaskLineSourceAlternateAgentMove, SetBy: string(record.ID),
	}, 1, nil, record.RecordedAt); !errors.Is(err, domain.ErrTaskLineProvenanceInconsistent) {
		t.Fatalf("a move line without provenance: err = %v", err)
	}
}
