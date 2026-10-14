package engine

import (
	"bytes"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

func TestAdjudicationParkCauses(t *testing.T) {
	const marker = "UNTRUSTED-MODEL-ERROR-AND-RATIONALE"
	covered := map[adjudicationParkCause]bool{}
	for _, tc := range []struct {
		cause adjudicationParkCause
		err   error
	}{
		{parkAdjudicatorUnconfigured, nil},
		{parkJudgmentFallback, &inference.AdjudicationUnavailable{Cause: inference.AdjudicationJudgmentFallback, Fallback: inference.FallbackDriverFailed, Err: errors.New(marker)}},
		{parkInputRefused, &inference.AdjudicationUnavailable{Cause: inference.AdjudicationInputRefused, Err: errors.New(marker)}},
		{parkOutputRefused, &inference.AdjudicationUnavailable{Cause: inference.AdjudicationOutputRefused, Err: errors.New(marker)}},
		{parkBindingMismatch, errors.Join(domain.ErrParentKeyMismatch, errors.New(marker))},
		{parkInternalError, errors.New(marker)},
		{parkBatchMismatch, nil},
		{parkEntryNotAccepted, nil},
	} {
		t.Run(string(tc.cause), func(t *testing.T) {
			var logs bytes.Buffer
			f := newFindingAdjudicationFixture(t, domain.FindingSeverityP2, nil, "low", "high")
			f.workflow.logger = slog.New(slog.NewTextHandler(&logs, nil))
			if tc.cause != parkAdjudicatorUnconfigured {
				adjudicator := &scriptedFindingAdjudicator{err: tc.err}
				if tc.cause == parkEntryNotAccepted {
					entry, err := domain.NewModelAdjudicationEntry(f.finding.ID, domain.GoalAdjacent, nil,
						domain.RouteDefer, "low", marker, nil, nil, nil, nil, nil)
					if err != nil {
						t.Fatal(err)
					}
					// A valid low-confidence answer now survives in the batch.
					// An invalid entry still refuses the whole answer set.
					entry.Confidence = nil
					adjudicator.entries = []domain.FindingAdjudicationEntry{entry}
				}
				f.workflow.findingAdjudicator = adjudicator
			} else {
				f.workflow.findingAdjudicator = nil
			}
			state, err := f.workflow.reconcileFindingAdjudication(f.ctx, f.task, f.binding, f.record, f.baseRoot, f.headRoot)
			if err != nil || state != productionReviewPending {
				t.Fatalf("park: %v, %v", state, err)
			}
			for _, want := range []string{"run=" + string(f.record.RunID), "round=1", "external_first_round=false", "cause=" + string(tc.cause)} {
				if !strings.Contains(logs.String(), want) {
					t.Errorf("log missing %q: %s", want, logs.String())
				}
			}
			if tc.cause == parkJudgmentFallback && !strings.Contains(logs.String(), "fallback=driver_failed") {
				t.Errorf("missing fallback: %s", logs.String())
			}
			if strings.Contains(logs.String(), marker) {
				t.Fatal("untrusted content logged")
			}
			if !tc.cause.valid() || !slices.Contains(AllAdjudicationParkCauses, tc.cause) {
				t.Fatalf("unregistered cause %q", tc.cause)
			}
			covered[tc.cause] = true
		})
	}
	for _, cause := range AllAdjudicationParkCauses {
		if !covered[cause] {
			t.Errorf("untested cause %q", cause)
		}
	}
	if adjudicationParkCause("").valid() || adjudicationParkCause(marker).valid() {
		t.Fatal("open park cause")
	}
}

func TestAdjudicationErrorCauseRejectsUntrustedCauseText(t *testing.T) {
	for _, err := range []error{
		&inference.AdjudicationUnavailable{Cause: inference.AdjudicationRefusal("untrusted")},
		&inference.AdjudicationUnavailable{Cause: inference.AdjudicationJudgmentFallback, Fallback: inference.FallbackReason("untrusted")},
	} {
		cause, fallback := adjudicationErrorCause(err)
		if cause != parkInternalError || fallback != "" {
			t.Fatalf("cause = %q/%q", cause, fallback)
		}
	}
}
