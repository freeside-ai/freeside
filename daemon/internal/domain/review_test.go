package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func validReviewRecord() domain.ReviewRecord {
	return domain.ReviewRecord{
		InvocationID: "review-run-1-1", RunID: "run-1", Round: 1,
		Provider: "openai", ModelConfiguration: "gpt-codex/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)), CostOwner: "owner",
		BaseSHA: "base", HeadSHA: "head", CompletedAt: time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC),
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)), Outcome: domain.ReviewClean,
	}
}

func TestReviewDispositionRecordValidate(t *testing.T) {
	t.Parallel()
	valid := domain.ReviewDispositionRecord{
		FindingID: "finding-1", RunID: "run-1", Round: 1,
		Disposition: domain.ReviewDispositionFixed, Reason: "fixed in abc123",
		RemediationInvocationID: "review-run-1-2",
		CreatedAt:               time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid disposition rejected: %v", err)
	}
	for name, mutate := range map[string]func(*domain.ReviewDispositionRecord){
		"finding":      func(r *domain.ReviewDispositionRecord) { r.FindingID = "" },
		"run":          func(r *domain.ReviewDispositionRecord) { r.RunID = "" },
		"round":        func(r *domain.ReviewDispositionRecord) { r.Round = 0 },
		"disposition":  func(r *domain.ReviewDispositionRecord) { r.Disposition = "ignored" },
		"reason":       func(r *domain.ReviewDispositionRecord) { r.Reason = "" },
		"fixed review": func(r *domain.ReviewDispositionRecord) { r.RemediationInvocationID = "" },
		"time":         func(r *domain.ReviewDispositionRecord) { r.CreatedAt = time.Time{} },
	} {
		t.Run(name, func(t *testing.T) {
			record := valid
			mutate(&record)
			if err := record.Validate(); err == nil {
				t.Fatal("malformed disposition validated")
			}
		})
	}
	deferred := valid
	deferred.Disposition = domain.ReviewDispositionDeferred
	deferred.RemediationInvocationID = ""
	if err := deferred.Validate(); err != nil {
		t.Fatalf("valid deferred disposition rejected: %v", err)
	}
	deferred.RemediationInvocationID = "review-run-1-2"
	if err := deferred.Validate(); !errors.Is(err, domain.ErrInvalidHeadBinding) {
		t.Fatalf("deferred disposition with remediation head = %v, want invalid binding", err)
	}
}

func TestReviewRecordBindsOutcomeToCanonicalFindings(t *testing.T) {
	record := validReviewRecord()
	record.Outcome = domain.ReviewFindings
	record.FindingIDs = []domain.FindingID{"finding-b", "finding-a", "finding-a"}
	got, err := domain.NewReviewRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.FindingIDs) != 2 || got.FindingIDs[0] != "finding-a" || got.FindingIDs[1] != "finding-b" {
		t.Fatalf("canonical finding ids = %#v", got.FindingIDs)
	}
	got.Outcome = domain.ReviewClean
	if err := got.Validate(); !errors.Is(err, domain.ErrInvalidReviewOutcome) {
		t.Fatalf("clean record with findings = %v", err)
	}
}

func TestReviewRecordRejectsMissingInstructionDigest(t *testing.T) {
	record := validReviewRecord()
	record.InstructionDigest = ""
	if err := record.Validate(); err == nil {
		t.Fatal("new review record accepted a missing instruction digest")
	}
}

func TestReviewFailureRequiresTypedTerminalAccount(t *testing.T) {
	failure := domain.ReviewFailure{
		InvocationID: "review-run-1-1", RunID: "run-1", Round: 1,
		BaseSHA: "base", HeadSHA: "head", Class: domain.ReviewFailureQuota,
		Reason: "quota exhausted", ObservedAt: time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC),
	}
	if err := failure.Validate(); err != nil {
		t.Fatal(err)
	}
	failure.Class = "retry_maybe"
	if err := failure.Validate(); !errors.Is(err, domain.ErrInvalidReviewFailureClass) {
		t.Fatalf("unknown failure class = %v", err)
	}
}

func TestReviewRetryValidatesIdentityAndTimestamp(t *testing.T) {
	retry := domain.ReviewRetry{
		RunID: "run-1", InvocationID: "review-run-1-1", Round: 1,
		BaseSHA: "base", HeadSHA: "head",
		ObservedAt: time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC), Reason: "transient poll failure",
	}
	if err := retry.Validate(); err != nil {
		t.Fatal(err)
	}
	zeroRound := retry
	zeroRound.Round = 0
	if err := zeroRound.Validate(); !errors.Is(err, domain.ErrNonPositive) {
		t.Fatalf("zero round = %v", err)
	}
	local := retry
	local.ObservedAt = time.Date(2026, 8, 3, 12, 0, 0, 0, time.Local)
	if err := local.Validate(); !errors.Is(err, domain.ErrTimestampNotUTC) {
		t.Fatalf("non-UTC observed_at = %v", err)
	}
}
