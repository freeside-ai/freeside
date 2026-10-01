package domain_test

import (
	"errors"
	"slices"
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
	adjudicationDigest := domain.Digest("sha256:" + strings.Repeat("a", 64))
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
	deferred.AdjudicationDigest = adjudicationDigest
	deferred.RemediationInvocationID = ""
	if err := deferred.Validate(); err != nil {
		t.Fatalf("valid deferred disposition rejected: %v", err)
	}
	deferred.RemediationInvocationID = "review-run-1-2"
	if err := deferred.Validate(); !errors.Is(err, domain.ErrInvalidHeadBinding) {
		t.Fatalf("deferred disposition with remediation head = %v, want invalid binding", err)
	}
	deferred.RemediationInvocationID = ""
	deferred.AdjudicationDigest = ""
	if err := deferred.Validate(); !errors.Is(err, domain.ErrEmptyField) {
		t.Fatalf("deferred disposition without adjudication = %v, want empty field", err)
	}
	deferred.AdjudicationDigest = "sha256:not-a-digest"
	if err := deferred.Validate(); !errors.Is(err, domain.ErrInvalidDispositionAdjudication) {
		t.Fatalf("deferred disposition with malformed adjudication = %v, want invalid binding", err)
	}
	fixedWithAdjudication := valid
	fixedWithAdjudication.AdjudicationDigest = adjudicationDigest
	if err := fixedWithAdjudication.Validate(); !errors.Is(err, domain.ErrInvalidDispositionAdjudication) {
		t.Fatalf("fixed disposition with adjudication = %v, want invalid binding", err)
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

func validDispositionSupersession() domain.FindingDispositionSupersession {
	return domain.FindingDispositionSupersession{
		RunID: "run-1", ReversingRound: 3, FindingID: "finding-1", SupersededRound: 1,
		RemediationInvocationID: "review-run-1-2",
		DriftAuditDigest:        domain.Digest("sha256:" + strings.Repeat("b", 64)),
		Authority: domain.DispositionSupersessionAuthority{
			Kind: domain.DispositionSupersessionHumanCommand,
			Command: &domain.DispositionSupersessionCommand{
				ItemID: "item-1", ItemVersion: 1, CommandID: "command-1",
			},
		},
		CreatedAt: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC),
	}
}

func TestFindingDispositionSupersessionValidate(t *testing.T) {
	t.Parallel()
	valid := validDispositionSupersession()
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid human supersession rejected: %v", err)
	}
	if !slices.Equal(domain.AllDispositionSupersessionAuthorityKinds,
		[]domain.DispositionSupersessionAuthorityKind{"auto_route", "human_command"}) {
		t.Fatalf("authority kinds = %v, want the plan §7 pair", domain.AllDispositionSupersessionAuthorityKinds)
	}
	// Every registered kind validates with exactly the command reference its
	// kind requires.
	for _, kind := range domain.AllDispositionSupersessionAuthorityKinds {
		record := validDispositionSupersession()
		record.Authority.Kind = kind
		if kind != domain.DispositionSupersessionHumanCommand {
			record.Authority.Command = nil
		}
		if err := record.Validate(); err != nil {
			t.Fatalf("valid %s supersession rejected: %v", kind, err)
		}
	}

	type record = domain.FindingDispositionSupersession
	command := func(mutate func(*domain.DispositionSupersessionCommand)) func(*record) {
		return func(r *record) {
			changed := *r.Authority.Command
			mutate(&changed)
			r.Authority.Command = &changed
		}
	}
	for name, tc := range map[string]struct {
		mutate func(*record)
		want   error
	}{
		"run":         {func(r *record) { r.RunID = "" }, domain.ErrEmptyID},
		"finding":     {func(r *record) { r.FindingID = "" }, domain.ErrEmptyID},
		"remediation": {func(r *record) { r.RemediationInvocationID = "" }, domain.ErrEmptyID},
		"superseded round": {
			func(r *record) { r.SupersededRound = 0 }, domain.ErrNonPositive,
		},
		"reversing round equal": {
			func(r *record) { r.ReversingRound = r.SupersededRound }, domain.ErrDispositionSupersessionInvalid,
		},
		"reversing round earlier": {
			func(r *record) { r.SupersededRound = 4 }, domain.ErrDispositionSupersessionInvalid,
		},
		"audit digest empty": {
			func(r *record) { r.DriftAuditDigest = "" }, domain.ErrDispositionSupersessionInvalid,
		},
		"audit digest malformed": {
			func(r *record) { r.DriftAuditDigest = "sha256:short" }, domain.ErrDispositionSupersessionInvalid,
		},
		"authority kind zero": {
			func(r *record) { r.Authority.Kind = "" }, domain.ErrInvalidSupersessionAuthorityKind,
		},
		"authority kind unknown": {
			func(r *record) { r.Authority.Kind = "operator" }, domain.ErrInvalidSupersessionAuthorityKind,
		},
		"human without command": {
			func(r *record) { r.Authority.Command = nil }, domain.ErrDispositionSupersessionInvalid,
		},
		"automatic with command": {
			func(r *record) { r.Authority.Kind = domain.DispositionSupersessionAutoRoute },
			domain.ErrDispositionSupersessionInvalid,
		},
		"command item": {
			command(func(c *domain.DispositionSupersessionCommand) { c.ItemID = "" }), domain.ErrEmptyID,
		},
		"command id": {
			command(func(c *domain.DispositionSupersessionCommand) { c.CommandID = "" }), domain.ErrEmptyID,
		},
		"command item version": {
			command(func(c *domain.DispositionSupersessionCommand) { c.ItemVersion = 0 }), domain.ErrNonPositive,
		},
		"time zero": {func(r *record) { r.CreatedAt = time.Time{} }, domain.ErrMissingTimestamp},
		"time not UTC": {
			func(r *record) { r.CreatedAt = r.CreatedAt.In(time.FixedZone("offset", 3600)) },
			domain.ErrTimestampNotUTC,
		},
		"run invalid UTF-8": {
			func(r *record) { r.RunID = "run-\xff" }, domain.ErrDispositionSupersessionInvalid,
		},
		"finding invalid UTF-8": {
			func(r *record) { r.FindingID = "finding-\xff" }, domain.ErrDispositionSupersessionInvalid,
		},
		"remediation invalid UTF-8": {
			func(r *record) { r.RemediationInvocationID = "review-\xff" },
			domain.ErrDispositionSupersessionInvalid,
		},
		"command item invalid UTF-8": {
			command(func(c *domain.DispositionSupersessionCommand) { c.ItemID = "item-\xff" }),
			domain.ErrDispositionSupersessionInvalid,
		},
		"command id invalid UTF-8": {
			command(func(c *domain.DispositionSupersessionCommand) { c.CommandID = "command-\xff" }),
			domain.ErrDispositionSupersessionInvalid,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			changed := validDispositionSupersession()
			tc.mutate(&changed)
			if err := changed.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}
