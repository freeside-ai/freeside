package domain_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func validShadowReviewRecord() domain.ShadowReviewRecord {
	return domain.ShadowReviewRecord{
		InvocationID: "shadow-run-1-1", RunID: "run-1", ShadowedRound: 1,
		Source: domain.ShadowReviewClaudeLocal, Provider: "anthropic",
		ModelConfiguration:  "claude-opus/high",
		ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)),
		InstructionDigest:   domain.Digest("sha256:" + strings.Repeat("d", 64)),
		CostOwner:           "owner", BaseSHA: "base", HeadSHA: "head",
		CompletedAt:        time.Date(2026, 8, 24, 1, 0, 0, 0, time.UTC),
		CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)),
		Outcome:            domain.ReviewClean,
	}
}

func TestShadowReviewRecordCanonicalizesFindings(t *testing.T) {
	record := validShadowReviewRecord()
	record.Outcome = domain.ReviewFindings
	record.FindingIDs = []domain.FindingID{"finding-b", "finding-a", "finding-a"}
	got, err := domain.NewShadowReviewRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.FindingIDs) != 2 || got.FindingIDs[0] != "finding-a" || got.FindingIDs[1] != "finding-b" {
		t.Fatalf("canonical finding ids = %#v", got.FindingIDs)
	}
	got.Outcome = domain.ReviewClean
	if err := got.Validate(); !errors.Is(err, domain.ErrInvalidReviewOutcome) {
		t.Fatalf("clean shadow record with findings = %v", err)
	}
}

func TestShadowReviewRecordRequiresRegisteredSource(t *testing.T) {
	record := validShadowReviewRecord()
	record.Source = "decoded_shadow_flag"
	if err := record.Validate(); !errors.Is(err, domain.ErrInvalidShadowReviewSource) {
		t.Fatalf("unregistered source = %v", err)
	}
}

func TestShadowReviewFindingRequiresRegisteredSourceSchema(t *testing.T) {
	valid := domain.Finding{
		ID: "shadow-finding-1", RunID: "run-1", Source: string(domain.ShadowReviewClaudeLocal),
		Severity: domain.FindingSeverityP2,
		Location: &domain.FindingLocation{Path: "daemon/main.go", StartLine: 4, EndLine: 4},
		Message:  "unchecked error", RawText: "unchecked error",
		CreatedAt: time.Date(2026, 8, 24, 1, 30, 0, 0, time.UTC),
	}
	if err := domain.ValidateShadowReviewFinding(domain.ShadowReviewClaudeLocal, valid); err != nil {
		t.Fatalf("valid shadow finding rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*domain.Finding)
		want   error
	}{
		{name: "empty severity", mutate: func(f *domain.Finding) { f.Severity = "" }, want: domain.ErrInvalidFindingSeverity},
		{name: "missing location", mutate: func(f *domain.Finding) { f.Location = nil }, want: domain.ErrEmptyField},
		{name: "whole file location", mutate: func(f *domain.Finding) {
			f.Location = &domain.FindingLocation{Path: "daemon/main.go"}
		}, want: domain.ErrNonPositive},
		{name: "empty message", mutate: func(f *domain.Finding) { f.Message = "" }, want: domain.ErrEmptyField},
		{name: "empty raw text", mutate: func(f *domain.Finding) { f.RawText = "" }, want: domain.ErrEmptyField},
		{name: "wrong source", mutate: func(f *domain.Finding) { f.Source = "codex_local" }, want: domain.ErrParentKeyMismatch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			finding := valid
			tc.mutate(&finding)
			if err := domain.ValidateShadowReviewFinding(domain.ShadowReviewClaudeLocal, finding); !errors.Is(err, tc.want) {
				t.Fatalf("invalid shadow finding = %v, want %v", err, tc.want)
			}
		})
	}
	if err := domain.ValidateShadowReviewFinding("unregistered", valid); !errors.Is(err, domain.ErrInvalidShadowReviewSource) {
		t.Fatalf("unregistered source schema = %v", err)
	}
}

func TestClassifierAccuracySampleValidatesJoinKeysAndAssessment(t *testing.T) {
	valid := domain.ClassifierAccuracySample{
		RunID: "run-1", FindingID: "finding-1", ClassificationVersion: 1,
		ShadowInvocationID: "shadow-run-1-1",
		Assessment:         domain.ClassifierAssessmentAccurate,
		RecordedAt:         time.Date(2026, 8, 24, 2, 0, 0, 0, time.UTC),
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid sample rejected: %v", err)
	}
	bad := valid
	bad.Assessment = "probably"
	if err := bad.Validate(); !errors.Is(err, domain.ErrInvalidClassifierAssessment) {
		t.Fatalf("invalid assessment = %v", err)
	}
	bad = valid
	bad.ClassificationVersion = 0
	if err := bad.Validate(); !errors.Is(err, domain.ErrNonPositive) {
		t.Fatalf("invalid classification version = %v", err)
	}
}
