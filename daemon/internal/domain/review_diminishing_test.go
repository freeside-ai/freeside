package domain_test

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// reviewDiminishingDriftFacts is a valid drift_audit card payload for the
// verdict: over_hardened carries the reversal list, the others an empty one.
func reviewDiminishingDriftFacts(verdict domain.DriftVerdict) domain.ReviewDiminishingFacts {
	reversals := []domain.DriftReversal{}
	if verdict == domain.DriftVerdictOverHardened {
		reversals = []domain.DriftReversal{
			{
				FindingID: "finding-0001",
				Undo:      "remove the nil guard on the injected clock",
				Rationale: "the specification makes the clock a required argument",
			},
			{
				FindingID: "finding-0002",
				Undo:      "drop the retry wrapper around the config read",
				Rationale: "the specification reads the config once at start",
			},
		}
	}
	return domain.ReviewDiminishingFacts{
		Cause: domain.ReviewDiminishingDriftAudit,
		DriftAudit: &domain.DriftAuditFacts{
			AuditDigest: "sha256:4444444444444444444444444444444444444444444444444444444444444444",
			Verdict:     verdict,
			Confidence:  domain.ConfidenceHigh,
			Explanation: "the audit compared the change with the approved specification",
			Reversals:   reversals,
		},
	}
}

func reviewDiminishingFactsInput(facts *domain.ReviewDiminishingFacts) domain.AttentionItemInput {
	runID := domain.RunID("run-1")
	return domain.AttentionItemInput{
		ID: "item-diminishing", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID},
		Type:    domain.AttentionReviewDiminishing, Priority: domain.PriorityNormal,
		Reason:            "review rounds are surfacing only marginal findings",
		RequestedDecision: []domain.Action{domain.ActionFinishNow},
		ReviewDiminishing: facts,
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionPlannedGate,
		Status:            domain.StatusOpen,
	}
}

func TestReviewDiminishingFactsValidate(t *testing.T) {
	t.Parallel()
	for _, cause := range domain.AllReviewDiminishingCauses {
		facts := domain.ReviewDiminishingFacts{Cause: cause}
		if cause == domain.ReviewDiminishingDriftAudit {
			facts = reviewDiminishingDriftFacts(domain.DriftVerdictStuck)
		}
		if err := facts.Validate(); err != nil {
			t.Errorf("cause %q = %v", cause, err)
		}
	}
	for _, verdict := range domain.AllDriftVerdicts {
		if err := reviewDiminishingDriftFacts(verdict).Validate(); err != nil {
			t.Errorf("verdict %q = %v", verdict, err)
		}
	}
	promised := reviewDiminishingDriftFacts(domain.DriftVerdictOverHardened)
	promised.DriftAudit.SimplificationOnContinue = true
	if err := promised.Validate(); err != nil {
		t.Errorf("over_hardened promising simplification = %v", err)
	}

	drift := func(mutate func(*domain.DriftAuditFacts)) domain.ReviewDiminishingFacts {
		facts := reviewDiminishingDriftFacts(domain.DriftVerdictOverHardened)
		mutate(facts.DriftAudit)
		return facts
	}
	for _, tc := range []struct {
		name  string
		facts domain.ReviewDiminishingFacts
		want  error
	}{
		{"zero cause", domain.ReviewDiminishingFacts{}, domain.ErrInvalidReviewDiminishingCause},
		{"unknown cause", domain.ReviewDiminishingFacts{Cause: "hard_round_limit"}, domain.ErrInvalidReviewDiminishingCause},
		{
			"drift cause without drift facts",
			domain.ReviewDiminishingFacts{Cause: domain.ReviewDiminishingDriftAudit},
			domain.ErrCardFactInconsistent,
		},
		{
			"drift facts under another cause",
			func() domain.ReviewDiminishingFacts {
				facts := reviewDiminishingDriftFacts(domain.DriftVerdictStuck)
				facts.Cause = domain.ReviewDiminishingLowValue
				return facts
			}(),
			domain.ErrCardFactInconsistent,
		},
		{"audit digest not a content address", drift(func(f *domain.DriftAuditFacts) {
			f.AuditDigest = "sha256:short"
		}), domain.ErrCardFactInconsistent},
		{"zero verdict", drift(func(f *domain.DriftAuditFacts) { f.Verdict = "" }), domain.ErrInvalidDriftVerdict},
		{"zero confidence", drift(func(f *domain.DriftAuditFacts) {
			f.Confidence = ""
		}), domain.ErrInvalidAdjudicationConfidence},
		{"blank explanation", drift(func(f *domain.DriftAuditFacts) { f.Explanation = " " }), domain.ErrEmptyField},
		{"invalid UTF-8 explanation", drift(func(f *domain.DriftAuditFacts) {
			f.Explanation = "bad \xff"
		}), domain.ErrCardFactInconsistent},
		{"nil reversals", drift(func(f *domain.DriftAuditFacts) {
			f.Reversals = nil
		}), domain.ErrCardFactInconsistent},
		{"over_hardened without reversals", drift(func(f *domain.DriftAuditFacts) {
			f.Reversals = []domain.DriftReversal{}
		}), domain.ErrCardFactInconsistent},
		{"stuck with reversals", drift(func(f *domain.DriftAuditFacts) {
			f.Verdict = domain.DriftVerdictStuck
		}), domain.ErrCardFactInconsistent},
		{"converged with reversals", drift(func(f *domain.DriftAuditFacts) {
			f.Verdict = domain.DriftVerdictConverged
		}), domain.ErrCardFactInconsistent},
		{"stuck promising simplification", drift(func(f *domain.DriftAuditFacts) {
			f.Verdict = domain.DriftVerdictStuck
			f.Reversals = []domain.DriftReversal{}
			f.SimplificationOnContinue = true
		}), domain.ErrCardFactInconsistent},
		{"reversals out of order", drift(func(f *domain.DriftAuditFacts) {
			f.Reversals[0], f.Reversals[1] = f.Reversals[1], f.Reversals[0]
		}), domain.ErrFindingsNotCanonical},
		{"duplicate reversal", drift(func(f *domain.DriftAuditFacts) {
			f.Reversals[1].FindingID = f.Reversals[0].FindingID
		}), domain.ErrFindingsNotCanonical},
		{"reversal without rationale", drift(func(f *domain.DriftAuditFacts) {
			f.Reversals[0].Rationale = ""
		}), domain.ErrEmptyField},
	} {
		if err := tc.facts.Validate(); !errors.Is(err, tc.want) {
			t.Errorf("%s = %v, want %v", tc.name, err, tc.want)
		}
	}
}

// TestReviewDiminishingFactsBelongToTheirItem pins the type gate: only a
// review_diminishing_returns item carries the facts, and the item runs their
// validation.
func TestReviewDiminishingFactsBelongToTheirItem(t *testing.T) {
	t.Parallel()
	facts := domain.ReviewDiminishingFacts{Cause: domain.ReviewDiminishingLowValue}
	if _, err := domain.NewAttentionItem(reviewDiminishingFactsInput(&facts), nil); err != nil {
		t.Fatalf("diminishing item with facts = %v", err)
	}
	if _, err := domain.NewAttentionItem(reviewDiminishingFactsInput(nil), nil); err != nil {
		t.Fatalf("diminishing item without facts = %v", err)
	}
	for _, itemType := range domain.AllAttentionTypes {
		if itemType == domain.AttentionReviewDiminishing {
			continue
		}
		item := mustItem(t, reviewDiminishingFactsInput(nil))
		item.Type = itemType
		// A type that needs a binding of its own is refused for that first;
		// every type this bare item is otherwise valid as must name the gate.
		bare := item.Validate()
		item.ReviewDiminishing = &facts
		err := item.Validate()
		if err == nil || (bare == nil && !errors.Is(err, domain.ErrCardFactOutsideItem)) {
			t.Errorf("facts on %q = %v, want ErrCardFactOutsideItem", itemType, err)
		}
	}
	invalid := domain.ReviewDiminishingFacts{Cause: domain.ReviewDiminishingDriftAudit}
	if _, err := domain.NewAttentionItem(reviewDiminishingFactsInput(&invalid), nil); !errors.Is(err, domain.ErrCardFactInconsistent) {
		t.Fatalf("item with inconsistent facts = %v, want ErrCardFactInconsistent", err)
	}
}

// TestNewAttentionItemDetachesReviewDiminishingFacts shows a caller cannot
// rewrite validated facts through the input it still holds.
func TestNewAttentionItemDetachesReviewDiminishingFacts(t *testing.T) {
	t.Parallel()
	facts := reviewDiminishingDriftFacts(domain.DriftVerdictOverHardened)
	item := mustItem(t, reviewDiminishingFactsInput(&facts))
	facts.Cause = domain.ReviewDiminishingLowValue
	facts.DriftAudit.Verdict = domain.DriftVerdictStuck
	facts.DriftAudit.Reversals[0].Undo = "rewritten"
	got := item.ReviewDiminishing
	if got.Cause != domain.ReviewDiminishingDriftAudit ||
		got.DriftAudit.Verdict != domain.DriftVerdictOverHardened ||
		got.DriftAudit.Reversals[0].Undo == "rewritten" {
		t.Fatalf("item facts follow the caller's input: %+v", got)
	}
}

// TestReviewDiminishingFactsAreFixedOnceAttached pins the transition rule
// shared with the other card facts: a successor version may add the facts to
// an item that had none, never change them.
func TestReviewDiminishingFactsAreFixedOnceAttached(t *testing.T) {
	t.Parallel()
	facts := domain.ReviewDiminishingFacts{Cause: domain.ReviewDiminishingLowValue}
	old := mustItem(t, reviewDiminishingFactsInput(&facts))
	next := old
	next.ItemVersion = 2
	if err := domain.ValidateAttentionItemTransition(old, next); err != nil {
		t.Fatalf("unchanged facts = %v", err)
	}
	changed := next
	changed.ReviewDiminishing = &domain.ReviewDiminishingFacts{Cause: domain.ReviewDiminishingFixedRecurrence}
	if err := domain.ValidateAttentionItemTransition(old, changed); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("changed facts = %v, want ErrImmutableTransition", err)
	}
	removed := next
	removed.ReviewDiminishing = nil
	if err := domain.ValidateAttentionItemTransition(old, removed); !errors.Is(err, domain.ErrImmutableTransition) {
		t.Fatalf("removed facts = %v, want ErrImmutableTransition", err)
	}
}
