package domain

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

// TriageOutcome is what the triager's one call for a task came to (plan
// §5.13, Specifier Triage): an answer, or the failure or skip that stood in
// for one.
type TriageOutcome string

const (
	// TriageOutcomeAnswered is a schema-valid answer naming a tier.
	TriageOutcomeAnswered TriageOutcome = "answered"
	// TriageOutcomeCallFailed is a call that returned no answer.
	TriageOutcomeCallFailed TriageOutcome = "call_failed"
	// TriageOutcomeInvalidAnswer is an answer the site's schema refused.
	TriageOutcomeInvalidAnswer TriageOutcome = "invalid_answer"
	// TriageOutcomeBudgetSpent is a call skipped because the site's budget
	// was spent.
	TriageOutcomeBudgetSpent TriageOutcome = "budget_spent"
	// TriageOutcomeNoTriagerLine is a call skipped because the lineup has no
	// triager line.
	TriageOutcomeNoTriagerLine TriageOutcome = "no_triager_line"
)

// AllTriageOutcomes lists every valid TriageOutcome; it is the single
// registration point.
var AllTriageOutcomes = []TriageOutcome{
	TriageOutcomeAnswered, TriageOutcomeCallFailed, TriageOutcomeInvalidAnswer,
	TriageOutcomeBudgetSpent, TriageOutcomeNoTriagerLine,
}

func (o TriageOutcome) valid() bool {
	switch o {
	case TriageOutcomeAnswered, TriageOutcomeCallFailed, TriageOutcomeInvalidAnswer,
		TriageOutcomeBudgetSpent, TriageOutcomeNoTriagerLine:
		return true
	default:
		return false
	}
}

// TriageTier is the triager's bounded choice: the specifier's lineup agent
// (standard) or its escalated counterpart.
type TriageTier string

const (
	TriageTierStandard  TriageTier = "standard"
	TriageTierEscalated TriageTier = "escalated"
)

// AllTriageTiers lists every valid TriageTier; it is the single registration
// point.
var AllTriageTiers = []TriageTier{TriageTierStandard, TriageTierEscalated}

func (t TriageTier) valid() bool {
	switch t {
	case TriageTierStandard, TriageTierEscalated:
		return true
	default:
		return false
	}
}

// maxTriageReasonBytes bounds the triager's reason; it is advisory prose for
// display, so a sentence or two fits with room to spare.
const maxTriageReasonBytes = 1024

// TaskTriage is a task's one triage record (plan §5.13, Specifier Triage).
// Every outcome but an answered standard escalates: a failure or skip must
// not quietly put a complex specification on the smaller model.
//
// The record is Tier B state, like a task line: it decodes with plain
// validation and grants nothing by itself. The Tier A gate is admission's
// anchor of a triage-sourced task line, which re-reads this record and the
// current lineup rather than trusting the line.
type TaskTriage struct {
	TaskID  TaskID        `json:"task_id"`
	Outcome TriageOutcome `json:"outcome"`
	// Tier is the answered tier, set exactly when Outcome is answered.
	Tier *TriageTier `json:"tier"`
	// Reason is the triager's reason or the failure's, nil when there is
	// none. It is advisory and never read as a decision.
	Reason     *string   `json:"reason"`
	Escalated  bool      `json:"escalated"`
	RecordedAt time.Time `json:"recorded_at"`
}

// TriageEscalates reports whether an outcome and its tier escalate the
// task: every outcome but an answered standard does.
func TriageEscalates(outcome TriageOutcome, tier *TriageTier) bool {
	return outcome != TriageOutcomeAnswered || tier == nil || *tier != TriageTierStandard
}

// Validate reports whether the record is well-formed and its escalated bit
// follows from its outcome and tier.
func (t TaskTriage) Validate() error {
	if t.TaskID == "" {
		return fmt.Errorf("task triage task_id: %w", ErrEmptyID)
	}
	if err := validateTriageOutcome(t.Outcome, t.Tier, t.Reason, t.Escalated); err != nil {
		return fmt.Errorf("task triage %s %w", t.TaskID, err)
	}
	if t.RecordedAt.IsZero() {
		return fmt.Errorf("task triage %s recorded_at: %w", t.TaskID, ErrMissingTimestamp)
	}
	if t.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("task triage %s recorded_at: %w", t.TaskID, ErrTimestampNotUTC)
	}
	return nil
}

// validateTriageOutcome holds a triage's outcome, tier, reason, and escalated
// bit to one another. The record and the timeline event that shows it share
// it, so the event can never show a combination the record refuses.
func validateTriageOutcome(outcome TriageOutcome, tier *TriageTier, reason *string, escalated bool) error {
	if !outcome.valid() {
		return fmt.Errorf("outcome %q: %w", outcome, ErrInvalidTriageOutcome)
	}
	if (tier != nil) != (outcome == TriageOutcomeAnswered) {
		return fmt.Errorf("outcome %s tier: %w", outcome, ErrTaskTriageInconsistent)
	}
	if tier != nil && !tier.valid() {
		return fmt.Errorf("tier %q: %w", *tier, ErrInvalidTriageTier)
	}
	if escalated != TriageEscalates(outcome, tier) {
		return fmt.Errorf("outcome %s escalated %t: %w", outcome, escalated, ErrTaskTriageInconsistent)
	}
	if reason != nil {
		if strings.TrimSpace(*reason) == "" || !utf8.ValidString(*reason) || len(*reason) > maxTriageReasonBytes {
			return fmt.Errorf("reason: %w", ErrTaskTriageInconsistent)
		}
	}
	return nil
}
