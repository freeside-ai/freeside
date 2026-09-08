package signet_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestRetainedOperatorFeedbackStartRemainsVisible(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "started", true: "failed"}[failed], func(t *testing.T) {
			f := newCorpusFixture(t)
			ctx := t.Context()
			invocation := domain.InvocationID("inv-operator-feedback-command")
			runID := domain.RunID("run-feedback")
			stageID := domain.StageID("operator-feedback-" + string(invocation))
			run := corpusRun(runID, corpusAttempt(runID, invocation))
			run.Stages[0].ID = stageID
			run.Stages[0].Attempts[0].StageID = stageID
			payload, err := json.Marshal(domain.OperatorFeedbackInvocationIntent{
				Version:      domain.OperatorFeedbackInvocationIntentVersion,
				InvocationID: invocation, RunID: runID, StageID: stageID,
				CommandID: "command", ItemID: "ready", SourceInvocationID: "inv-implement-run-feedback",
				InputArtifactID:     "operator-feedback-command",
				InputArtifactDigest: domain.Digest("sha256:" + strings.Repeat("a", 64)),
			})
			if err != nil {
				t.Fatal(err)
			}
			f.mustWrite(t, func(tx *store.WriteTx) error { return tx.PutRun(ctx, run) })
			f.mustWriteInternal(t, func(tx *store.InternalTx) error {
				if _, _, err := tx.EnqueueOutbox(ctx, string(invocation), string(domain.OperatorFeedbackInvocationRequestedKind), payload); err != nil {
					return err
				}
				return tx.MarkOutboxDispatched(ctx, string(invocation))
			})
			f.appendMilestone(t, domain.RunMilestone{
				RunID: runID, Kind: domain.MilestoneInvocationStarted, InvocationID: &invocation, RecordedAt: f.at,
			})
			if failed {
				f.observe(t, domain.InvocationObservation{
					InvocationID: invocation, RunID: runID, Status: domain.ObservedStatusFailed,
					ObservedAt: f.at.Add(time.Minute),
				})
			}
			if err := f.read(ctx, runID); err != nil {
				t.Fatalf("retained feedback projection: %v", err)
			}
			timeline, err := f.service.GetRunTimeline(ctx, runID)
			if err != nil || len(timeline.Milestones) != 1 || timeline.Milestones[0].Kind != domain.MilestoneInvocationStarted {
				t.Fatalf("started milestone missing: %#v, %v", timeline, err)
			}
			if failed && (len(timeline.Invocations) != 1 || timeline.Invocations[0].Status != domain.ObservedStatusFailed) {
				t.Fatalf("failed observation missing: %#v", timeline.Invocations)
			}
		})
	}
}

func TestOperatorFeedbackReservedIntentProjection(t *testing.T) {
	for _, submitted := range []bool{false, true} {
		t.Run(map[bool]string{false: "held_before_attempt", true: "forged_submission"}[submitted], func(t *testing.T) {
			f := newCorpusFixture(t)
			ctx := t.Context()
			invocation := domain.InvocationID("inv-operator-feedback-command")
			runID := domain.RunID("run-feedback")
			run := corpusRun(runID)
			run.Stages[0].ID = domain.StageID("operator-feedback-" + string(invocation))
			payload, err := json.Marshal(domain.OperatorFeedbackInvocationIntent{
				Version:      domain.OperatorFeedbackInvocationIntentVersion,
				InvocationID: invocation, RunID: runID, StageID: run.Stages[0].ID,
				CommandID: "command", ItemID: "ready", SourceInvocationID: "inv-implement-run-feedback",
				InputArtifactID:     "operator-feedback-command",
				InputArtifactDigest: domain.Digest("sha256:" + strings.Repeat("a", 64)),
			})
			if err != nil {
				t.Fatal(err)
			}
			f.mustWrite(t, func(tx *store.WriteTx) error { return tx.PutRun(ctx, run) })
			f.mustWriteInternal(t, func(tx *store.InternalTx) error {
				_, _, err := tx.EnqueueOutbox(ctx, string(invocation), string(domain.OperatorFeedbackInvocationRequestedKind), payload)
				return err
			})
			if submitted {
				f.appendMilestone(t, domain.RunMilestone{
					RunID: runID, Kind: domain.MilestoneRunSubmitted, InvocationID: &invocation, RecordedAt: f.at,
				})
				if err := f.read(ctx, runID); !errors.Is(err, domain.ErrParentKeyMismatch) {
					t.Fatalf("feedback continuation accepted as run submission: %v", err)
				}
				return
			}
			f.mustWrite(t, func(tx *store.WriteTx) error {
				return tx.RecordRunHold(ctx, domain.RunHoldObservation{
					RunID: runID, InvocationID: &invocation, Reason: domain.HoldIdentityParallelism,
					FirstObservedAt: f.at, LastObservedAt: f.at.Add(time.Minute),
				})
			})
			if err := f.read(ctx, runID); err != nil {
				t.Fatalf("valid held feedback rejected before admission: %v", err)
			}
			timeline, err := f.service.GetRunTimeline(ctx, runID)
			if err != nil || timeline.Hold == nil || timeline.Hold.InvocationID == nil || *timeline.Hold.InvocationID != invocation {
				t.Fatalf("held feedback missing from timeline: %#v, %v", timeline, err)
			}
		})
	}
}
