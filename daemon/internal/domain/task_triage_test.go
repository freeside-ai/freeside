package domain_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

var triageRecordedAt = time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

// escalatedTriage is the golden fixture: an answer that escalated, with its
// reason.
func escalatedTriage() domain.TaskTriage {
	return domain.TaskTriage{
		TaskID: "task-1", Outcome: domain.TriageOutcomeAnswered,
		Tier: new(domain.TriageTierEscalated), Reason: new("A contract change across three components."),
		Escalated: true, RecordedAt: triageRecordedAt,
	}
}

func TestTaskTriageGolden(t *testing.T) {
	record := escalatedTriage()
	if err := record.Validate(); err != nil {
		t.Fatal(err)
	}
	body, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "task_triage", append(body, '\n'))
}

// Every outcome but an answered standard escalates, and the record's
// escalated bit must say so.
func TestTaskTriageEscalation(t *testing.T) {
	for _, outcome := range domain.AllTriageOutcomes {
		tiers := []*domain.TriageTier{nil}
		if outcome == domain.TriageOutcomeAnswered {
			tiers = []*domain.TriageTier{new(domain.TriageTierStandard), new(domain.TriageTierEscalated)}
		}
		for _, tier := range tiers {
			want := outcome != domain.TriageOutcomeAnswered || *tier == domain.TriageTierEscalated
			record := domain.TaskTriage{
				TaskID: "task-1", Outcome: outcome, Tier: tier, Escalated: want, RecordedAt: triageRecordedAt,
			}
			if err := record.Validate(); err != nil {
				t.Fatalf("%s %v escalated %t = %v", outcome, tier, want, err)
			}
			record.Escalated = !want
			if err := record.Validate(); !errors.Is(err, domain.ErrTaskTriageInconsistent) {
				t.Fatalf("%s %v escalated %t = %v, want %v", outcome, tier, !want, err, domain.ErrTaskTriageInconsistent)
			}
		}
	}
}

func TestTaskTriageRefusals(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*domain.TaskTriage)
		wantErr error
	}{
		{"no task", func(r *domain.TaskTriage) { r.TaskID = "" }, domain.ErrEmptyID},
		{"unknown outcome", func(r *domain.TaskTriage) { r.Outcome = "skipped" }, domain.ErrInvalidTriageOutcome},
		{"unknown tier", func(r *domain.TaskTriage) { r.Tier = new(domain.TriageTier("huge")) }, domain.ErrInvalidTriageTier},
		{"answer with no tier", func(r *domain.TaskTriage) { r.Tier = nil }, domain.ErrTaskTriageInconsistent},
		{"failure with a tier", func(r *domain.TaskTriage) { r.Outcome = domain.TriageOutcomeCallFailed }, domain.ErrTaskTriageInconsistent},
		{"blank reason", func(r *domain.TaskTriage) { r.Reason = new(" ") }, domain.ErrTaskTriageInconsistent},
		{"long reason", func(r *domain.TaskTriage) { r.Reason = new(strings.Repeat("x", 1025)) }, domain.ErrTaskTriageInconsistent},
		{"reason not UTF-8", func(r *domain.TaskTriage) { r.Reason = new("\xff") }, domain.ErrTaskTriageInconsistent},
		{"no instant", func(r *domain.TaskTriage) { r.RecordedAt = time.Time{} }, domain.ErrMissingTimestamp},
		{"local instant", func(r *domain.TaskTriage) { r.RecordedAt = triageRecordedAt.In(time.FixedZone("x", 3600)) }, domain.ErrTimestampNotUTC},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := escalatedTriage()
			tc.mutate(&record)
			if err := record.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
