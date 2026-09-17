package signet_test

import (
	"database/sql"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestTaskHistoryOmitsVerificationWithoutProductionBinding(t *testing.T) {
	f := newCorpusFixture(t)
	f.seedAuthIdentity(t)
	runID := domain.RunID("history-unbound-readiness")
	detail := domain.ReadinessDetail{EvaluationSetDigest: "sha256:evaluation", CandidateHead: "cafebabe", Base: domain.ReadinessBoundBase{BaseRef: "main", BaseSHA: "base"}, Requirements: []domain.ReadinessRequirement{{RequirementKey: "clean", CheckClass: domain.CheckClassCleanVerification, Kind: domain.RequirementRequired, State: domain.ReadinessRequirementPassed, ProofRecipeDigest: ptr(domain.Digest("sha256:recipe"))}}}
	f.seedPublishedRun(t, runID, "history-unbound-implementation", detail)
	var taskID domain.TaskID
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		run, err := tx.GetRun(t.Context(), runID)
		taskID = run.TaskID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before, err := f.service.GetTaskTimeline(t.Context(), taskID)
	if err != nil || !slices.ContainsFunc(before.Events, func(event domain.TaskEvent) bool { return event.Kind == domain.TaskEventVerificationRecorded }) {
		t.Fatalf("bound readiness must establish verification: %+v, %v", before.Events, err)
	}
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(t.Context(), `DELETE FROM ready_item_pr_bindings WHERE item_id = ?`, domain.ProductionReadyItemID(runID)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.GetRunTimeline(t.Context(), runID); err != nil {
		t.Fatalf("unbound item remains valid legacy run history: %v", err)
	}
	after, err := f.service.GetTaskTimeline(t.Context(), taskID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Events) != len(before.Events)-1 || slices.ContainsFunc(after.Events, func(event domain.TaskEvent) bool { return event.Kind == domain.TaskEventVerificationRecorded }) {
		t.Fatalf("unbound readiness manufactured verification or lost other history: %+v", after.Events)
	}
}

func TestTaskHistoryPublishedReviewAndVerificationResponse(t *testing.T) {
	f := newCorpusFixture(t)
	f.seedAuthIdentity(t)
	ctx := t.Context()
	runID := domain.RunID("history-published")
	invocation := domain.InvocationID("history-implementation")
	detail := domain.ReadinessDetail{EvaluationSetDigest: "sha256:evaluation", CandidateHead: "cafebabe", Base: domain.ReadinessBoundBase{BaseRef: "main", BaseSHA: "base"}, Requirements: []domain.ReadinessRequirement{{RequirementKey: "clean", CheckClass: domain.CheckClassCleanVerification, Kind: domain.RequirementRequired, State: domain.ReadinessRequirementPassed, ProofRecipeDigest: ptr(domain.Digest("sha256:recipe"))}}}
	policy := f.seedPublishedRun(t, runID, invocation, detail)
	f.seedCompletionRecords(t, runID, policy)
	f.appendPublicationReady(t, runID)
	request := domain.ReviewRequestRecord{InvocationID: "history-review", RunID: runID, Round: 1, BaseSHA: "base", HeadSHA: "cafebabe", RequestedAt: f.at.Add(20 * time.Minute)}
	completed := request.RequestedAt.Add(time.Minute)
	record := domain.ReviewRecord{InvocationID: request.InvocationID, RunID: runID, Round: 1, Provider: "openai", ModelConfiguration: "codex/high", ConfigurationDigest: domain.Digest("sha256:" + strings.Repeat("c", 64)), InstructionDigest: domain.Digest("sha256:" + strings.Repeat("d", 64)), CostOwner: "owner", BaseSHA: request.BaseSHA, HeadSHA: request.HeadSHA, CompletedAt: completed, CompletionEvidence: domain.Digest("sha256:" + strings.Repeat("e", 64)), Outcome: domain.ReviewClean}
	var taskID domain.TaskID
	f.mustWrite(t, func(tx *store.WriteTx) error {
		run, err := tx.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		taskID = run.TaskID
		if err := tx.RecordTaskLifecycleFact(ctx, taskID, domain.TaskLifecycleFact{Kind: domain.TaskLifecycleStarted, RunID: runID, SourceID: "start:" + string(runID), RecordedAt: f.at}); err != nil {
			return err
		}
		if err := tx.PutReviewRequest(ctx, request); err != nil {
			return err
		}
		if err := tx.PutReviewRecord(ctx, record, nil); err != nil {
			return err
		}
		return nil
	})
	got, err := f.service.GetTaskTimeline(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[domain.TaskEventKind]int{}
	for _, event := range got.Events {
		counts[event.Kind]++
		if event.Review != nil && (*event.RunID != runID || event.Review.InvocationID != request.InvocationID || event.Review.HeadSHA != "cafebabe" || event.Review.BaseSHA != "base") {
			t.Fatalf("wrong review binding: %+v", event)
		}
		if event.Verification != nil && (event.Verification.ItemID != domain.ProductionReadyItemID(runID) || event.Verification.HeadSHA != "cafebabe" || event.Verification.Class != domain.ReadinessReadyClean) {
			t.Fatalf("wrong verification: %+v", event)
		}
	}
	for _, kind := range []domain.TaskEventKind{domain.TaskEventCreated, domain.TaskEventStarted, domain.TaskEventReviewRequested, domain.TaskEventReviewCompleted, domain.TaskEventVerificationRecorded, domain.TaskEventPROpened, domain.TaskEventRunMilestone} {
		if counts[kind] != 1 {
			t.Errorf("%s count %d, want 1", kind, counts[kind])
		}
	}
	if counts[domain.TaskEventPRMerged] != 0 || counts[domain.TaskEventCompleted] != 0 {
		t.Fatal("publication inferred completion")
	}
	again, err := f.service.GetTaskTimeline(ctx, taskID)
	if err != nil || !reflect.DeepEqual(got.Events, again.Events) {
		t.Fatal("repeated read changed history", err)
	}
	failedRequest := request
	failedRequest.InvocationID, failedRequest.Round = "history-review-failed", 2
	failedRequest.RequestedAt = completed.Add(time.Minute)
	f.mustWrite(t, func(tx *store.WriteTx) error {
		if err := tx.PutReviewRequest(ctx, failedRequest); err != nil {
			return err
		}
		return tx.PutReviewFailure(ctx, domain.ReviewFailure{InvocationID: failedRequest.InvocationID, RunID: runID, Round: 2, BaseSHA: request.BaseSHA, HeadSHA: request.HeadSHA, Class: domain.ReviewFailureConfiguration, Reason: "No diff access", ObservedAt: failedRequest.RequestedAt.Add(time.Minute)})
	})
	failed, err := f.service.GetTaskTimeline(ctx, taskID)
	if err != nil {
		t.Fatal(err)
	}
	index := slices.IndexFunc(failed.Events, func(event domain.TaskEvent) bool { return event.Kind == domain.TaskEventReviewFailed })
	if index < 0 {
		t.Fatal("missing failed review event")
	}
	if event := failed.Events[index]; event.Review.Outcome != nil || *event.Review.Failure != domain.ReviewFailureConfiguration || event.Review.InvocationID != failedRequest.InvocationID {
		t.Fatalf("failed review history: %+v", event)
	}
	// The task endpoint must retain the run endpoint's authenticated review
	// boundary instead of turning a damaged stored result into display history.
	db, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, "UPDATE review_requests SET body_digest='wrong'"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.GetTaskTimeline(ctx, taskID); !errors.Is(err, signet.ErrRunObservationIntegrity) {
		t.Fatalf("damaged review history error = %v", err)
	}
}

func TestTaskHistoryQueuedStopResponse(t *testing.T) {
	for _, state := range []domain.TaskCancellationState{domain.TaskCancellationConfirmed, domain.TaskCancellationFailed} {
		t.Run(string(state), func(t *testing.T) {
			service, s := newSubmitTaskService(t, nil, domain.DeviceActive)
			id := seedStopTask(t, s)
			result, err := service.Submit(t.Context(), stopCommand(t, s, id, "history-stop"))
			if err != nil {
				t.Fatal(err)
			}
			requested, err := service.GetTaskTimeline(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			if len(requested.Events) != 2 || requested.Events[0].Kind != domain.TaskEventStopRequested {
				t.Fatalf("request history: %+v", requested.Events)
			}
			c := result.Stop.Cancellation
			ack := domain.TaskCancellationAcknowledgement{ID: "history-stop-ack", RequestID: c.RequestID, TargetDigest: c.TargetDigest, State: state, EvidenceDigest: c.TargetDigest, RecordedAt: c.RequestedAt.Add(time.Second)}
			if err := s.Write(t.Context(), func(tx *store.WriteTx) error { _, err := tx.AcknowledgeTaskCancellation(t.Context(), ack); return err }); err != nil {
				t.Fatal(err)
			}
			got, err := service.GetTaskTimeline(t.Context(), id)
			if err != nil {
				t.Fatal(err)
			}
			want := domain.TaskEventStopped
			if state == domain.TaskCancellationFailed {
				want = domain.TaskEventStopFailed
			}
			if len(got.Events) != 3 || got.Events[0].Kind != want || got.Events[0].RunID != nil || !got.Events[0].RecordedAt.Equal(ack.RecordedAt) {
				t.Fatalf("ack history: %+v", got.Events)
			}
		})
	}
}

func TestTaskHistoryActiveStopKeepsLifecycleAndAcknowledgementDistinct(t *testing.T) {
	service, s := newSubmitTaskService(t, nil, domain.DeviceActive)
	id := seedStopTask(t, s, "implementation")
	start := time.Now().UTC()
	if err := s.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.RecordTaskStart(t.Context(), "run-implementation", start)
	}); err != nil {
		t.Fatal(err)
	}
	result, err := service.Submit(t.Context(), stopCommand(t, s, id, "active-history-stop"))
	if err != nil {
		t.Fatal(err)
	}
	requested, err := service.GetTaskTimeline(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(requested.Events) != 3 || requested.Events[0].Kind != domain.TaskEventStopRequested {
		t.Fatalf("request must not complete the active episode: %+v", requested.Events)
	}
	c := result.Stop.Cancellation
	ack := domain.TaskCancellationAcknowledgement{ID: "active-stop-ack", RequestID: c.RequestID, TargetDigest: c.TargetDigest, State: domain.TaskCancellationConfirmed, EvidenceDigest: c.TargetDigest, RecordedAt: c.RequestedAt.Add(time.Second)}
	if err := s.Write(t.Context(), func(tx *store.WriteTx) error {
		_, err := tx.AcknowledgeTaskCancellation(t.Context(), ack)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got, err := service.GetTaskTimeline(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 5 || got.Events[0].Kind != domain.TaskEventAbandoned || got.Events[1].Kind != domain.TaskEventStopped || *got.Events[0].RunID != "run-implementation" || !got.Events[0].RecordedAt.Equal(ack.RecordedAt) || !got.Events[1].RecordedAt.Equal(ack.RecordedAt) {
		t.Fatalf("confirmed stop must retain the separate episode and acknowledgement facts: %+v", got.Events)
	}
}
