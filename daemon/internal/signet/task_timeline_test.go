package signet_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestTaskTimelineSpecificationOnly(t *testing.T) {
	f := newRunFixture(t)
	ctx := t.Context()
	implementation := domain.RunID("task-timeline-implementation")
	campaign, err := engine.ProductionCampaignIDForImplementation(implementation)
	if err != nil {
		t.Fatal(err)
	}
	specification, err := engine.SpecificationRunIDForImplementation(implementation)
	if err != nil {
		t.Fatal(err)
	}
	invocation := domain.InvocationID("inv-specify-" + specification + "-1")
	stage := domain.StageID("specify-" + specification)
	at := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	var task domain.Task
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		task, err = tx.GetOrCreateTask(ctx, "proj-1", domain.SpecificationSource{
			Kind:         domain.SpecificationSourceIssueSubject,
			IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 123, IssueNumber: 42},
		})
		if err != nil {
			return err
		}
		source, err := domain.NewArtifact(domain.ArtifactInput{
			ID: "task-timeline-source", Type: domain.ArtifactKindSpecification, Digest: "sha256:source",
			Provenance: domain.Provenance{
				ProducerClass: domain.ProducerAgent, ProducerInvocationID: "inv-source",
				HeadBinding: domain.HeadIndependent, SensitivityClass: domain.SensitivityNormal,
			},
			Metadata: domain.EvidenceMetadata{
				MediaType: domain.EvidenceMediaTextMarkdown, SizeBytes: 1, CreatedAt: at,
				Source: domain.EvidenceSourceRun, Availability: domain.EvidenceAvailable,
			},
		}, map[domain.Digest]bool{})
		if err != nil {
			return err
		}
		if err := tx.PutArtifact(ctx, source); err != nil {
			return err
		}
		if err := tx.PutProductionAttempt(ctx, domain.ProductionAttempt{
			CampaignID: campaign, AttemptNumber: 1, Kind: domain.ProductionAttemptInitial,
			SourceDigest: "sha256:source", PublicationDigest: "sha256:publication",
			SpecificationRunID: specification, ImplementationRunID: implementation,
		}); err != nil {
			return err
		}
		request, err := json.Marshal(map[string]any{
			"version": "freeside.specification-request/v1", "specification_run_id": specification,
			"implementation_run_id": implementation, "project_id": task.ProjectID,
			"invocation_id": invocation, "iteration": 1, "campaign_id": campaign,
			"attempt_number": 1, "publication_digest": "sha256:publication", "input_artifact_ids": []domain.ArtifactID{source.ID},
		})
		if err != nil {
			return err
		}
		if _, _, err := tx.EnqueueOutbox(ctx, string(invocation), string(domain.SpecificationInvocationRequestedKind), request); err != nil {
			return err
		}
		if err := tx.PutRun(ctx, domain.Run{
			ID: specification, TaskID: task.ID, ProjectID: task.ProjectID,
			SpecDigest: "sha256:source", PolicyDigest: "sha256:policy", CampaignID: campaign, AttemptNumber: 1,
			Stages: []domain.Stage{{ID: stage, RunID: specification, Name: "specification", Attempts: []domain.Attempt{{
				ID: "attempt-specification", StageID: stage, Number: 1, InvocationID: invocation,
			}}}},
		}); err != nil {
			return err
		}
		if err := tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: specification, Kind: domain.MilestoneRunSubmitted, InvocationID: &invocation, RecordedAt: at,
		}); err != nil {
			return err
		}
		if err := tx.RecordRunHold(ctx, domain.RunHoldObservation{
			RunID: specification, InvocationID: &invocation, Reason: domain.HoldAttendedModeActive,
			FirstObservedAt: at.Add(time.Minute), LastObservedAt: at.Add(2 * time.Minute),
		}); err != nil {
			return err
		}
		return tx.SetTaskName(ctx, task.ID, domain.DisplayName{Text: "Improve navigation", Source: domain.DisplayNameSourceAgent})
	}); err != nil {
		t.Fatal(err)
	}
	timeline, err := f.service.GetTaskTimeline(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := f.service.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if timeline.AsOfRevision != revision.Revision || timeline.AsOf.IsZero() || timeline.TaskID != task.ID || timeline.ProjectID != task.ProjectID {
		t.Fatalf("timeline header = %+v", timeline)
	}
	if timeline.Name.Text != "Improve navigation" || timeline.Name.Source != domain.DisplayNameSourceAgent {
		t.Fatalf("name claim = %+v", timeline.Name)
	}
	if len(timeline.Events) != 1 || timeline.Events[0].Kind != domain.TaskEventCreated || !timeline.Events[0].RecordedAt.Equal(task.CreatedAt) {
		t.Fatalf("task events = %+v", timeline.Events)
	}
	if len(timeline.Sections) != 1 || len(timeline.Sections[0].Events) != 1 || len(timeline.Sections[0].Runs) != 1 {
		t.Fatalf("specification-only sections = %+v", timeline.Sections)
	}
	section := timeline.Sections[0]
	if *section.CampaignID != campaign || section.Events[0].Kind != domain.TaskEventCampaignAllocated || !section.Events[0].RecordedAt.Equal(at) {
		t.Fatalf("campaign = %+v", section)
	}
	run := section.Runs[0]
	if run.RunID != specification || run.Role == nil || *run.Role != domain.TaskRunSpecification || *run.AttemptNumber != 1 || run.SupersededBy != nil {
		t.Fatalf("specification header = %+v", run)
	}
	if len(run.Milestones) != 1 || run.Hold == nil || run.Hold.Reason != domain.HoldAttendedModeActive || len(run.Events) != 0 {
		t.Fatalf("specification observations = %+v", run)
	}
	handler := signet.NewHTTPHandler(f.service, testAuthorizer)
	response := bearerRequest(t, handler, http.MethodGet, "/tasks/"+string(task.ID)+"/timeline", "Bearer test-device-1", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("HTTP timeline = %d: %s", response.Code, response.Body.String())
	}
	var decoded signet.TaskTimeline
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil || decoded.TaskID != task.ID || decoded.AsOfRevision != timeline.AsOfRevision {
		t.Fatalf("HTTP timeline = %+v: %v", decoded, err)
	}
}

func TestTaskTimelineUnknownAndEmptyTask(t *testing.T) {
	f := newRunFixture(t)
	if _, err := f.service.GetTaskTimeline(t.Context(), "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing task error = %v", err)
	}
	handler := signet.NewHTTPHandler(f.service, testAuthorizer)
	response := bearerRequest(t, handler, http.MethodGet, "/tasks/missing/timeline", "Bearer test-device-1", nil)
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown task status = %d, body = %s", response.Code, response.Body.String())
	}
	var task domain.Task
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		var err error
		task, err = tx.GetOrCreateTask(t.Context(), "proj-1", domain.SpecificationSource{
			Kind:         domain.SpecificationSourceIssueSubject,
			IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 123, IssueNumber: 77},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got, err := f.service.GetTaskTimeline(t.Context(), task.ID)
	if err != nil || got.Sections == nil || len(got.Sections) != 0 || len(got.Events) != 1 {
		t.Fatalf("empty task = %+v, %v", got, err)
	}
}

func TestTaskTimelineRejectsFutureTaskRevision(t *testing.T) {
	f := newRunFixture(t)
	ctx := t.Context()
	var task domain.Task
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		task, err = tx.GetOrCreateTask(ctx, "proj-1", domain.SpecificationSource{
			Kind:         domain.SpecificationSourceIssueSubject,
			IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 123, IssueNumber: 78},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.GetTaskTimeline(ctx, task.ID); err != nil {
		t.Fatalf("valid task timeline: %v", err)
	}
	revision, err := f.service.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A raw connection models inconsistent restore metadata that the write
	// boundary cannot produce. No run exists to catch it indirectly.
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, `UPDATE tasks SET as_of_revision = ? WHERE id = ?`, revision.Revision+1, task.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.GetTaskTimeline(ctx, task.ID); !errors.Is(err, signet.ErrInvalidSyncSnapshot) {
		t.Fatalf("future task revision error = %v", err)
	}
}

func TestTaskTimelineRejectsMissingRunIndex(t *testing.T) {
	f := newRunFixture(t)
	var task domain.Task
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		var err error
		task, err = tx.GetOrCreateTask(t.Context(), "proj-1", domain.SpecificationSource{
			Kind:         domain.SpecificationSourceIssueSubject,
			IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 123, IssueNumber: 80},
		})
		if err != nil {
			return err
		}
		for _, id := range []domain.RunID{"history-earlier", "history-current"} {
			if err := tx.PutRun(t.Context(), domain.Run{
				ID: id, TaskID: task.ID, ProjectID: task.ProjectID,
				SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
				Stages: []domain.Stage{},
			}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	timeline, err := f.service.GetTaskTimeline(t.Context(), task.ID)
	if err != nil || len(timeline.Sections) != 1 || len(timeline.Sections[0].Runs) != 2 {
		t.Fatalf("valid legacy history = %+v, %v", timeline, err)
	}
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(t.Context(), `DELETE FROM task_runs WHERE run_id = ?`, "history-earlier"); err != nil {
		t.Fatal(err)
	}
	handler := signet.NewHTTPHandler(f.service, testAuthorizer)
	response := bearerRequest(t, handler, http.MethodGet, "/tasks/"+string(task.ID)+"/timeline", "Bearer test-device-1", nil)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("missing index status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestTaskTimelineRejectsMissingHistoryRelation(t *testing.T) {
	f := newRunFixture(t)
	ctx := t.Context()
	campaign, specification, _ := seedSpecificationCampaign(t, ctx, f, "task-missing-history")
	var taskID domain.TaskID
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		run, err := tx.GetRun(ctx, specification)
		taskID = run.TaskID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.GetTaskTimeline(ctx, taskID); err != nil {
		t.Fatalf("valid specification history: %v", err)
	}
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, `DELETE FROM production_attempts WHERE campaign_id = ?`, campaign); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetTaskSnapshot(ctx, taskID)
		return err
	}); err != nil {
		t.Fatalf("task must remain readable: %v", err)
	}
	if _, err := f.service.GetTaskTimeline(ctx, taskID); !errors.Is(err, signet.ErrRunObservationIntegrity) || !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing history relation error = %v", err)
	}
	handler := signet.NewHTTPHandler(f.service, testAuthorizer)
	response := bearerRequest(t, handler, http.MethodGet, "/tasks/"+string(taskID)+"/timeline", "Bearer test-device-1", nil)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("missing history relation status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestTaskTimelineRejectsMissingApprovedImplementation(t *testing.T) {
	f := newRunFixture(t)
	ctx := t.Context()
	campaign, _, run := seedSpecificationCampaign(t, ctx, f, "task-missing-implementation")
	approveAndPersistImplementationRun(t, ctx, f, campaign, run)
	invocation := run.Stages[0].Attempts[0].InvocationID
	request, err := json.Marshal(map[string]any{
		"invocation_id": invocation, "run_id": run.ID, "stage_id": run.Stages[0].ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		if _, _, err := tx.EnqueueOutbox(ctx, string(invocation), string(domain.ProductionInvocationRequestedKind), request); err != nil {
			return err
		}
		return tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: run.ID, Kind: domain.MilestoneRunSubmitted, InvocationID: &invocation,
			RecordedAt: time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC),
		})
	}); err != nil {
		t.Fatal(err)
	}
	var taskID domain.TaskID
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		stored, err := tx.GetRun(ctx, run.ID)
		taskID = stored.TaskID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	timeline, err := f.service.GetTaskTimeline(ctx, taskID)
	if err != nil || len(timeline.Sections) != 1 || len(timeline.Sections[0].Runs) != 2 || len(timeline.Sections[0].Events) != 2 {
		t.Fatalf("approved campaign history = %+v, %v", timeline, err)
	}
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	// Both membership sources lose the implementation, but the approved
	// attempt still records that it was submitted in the approval transaction.
	for _, statement := range []string{
		`DELETE FROM task_runs WHERE run_id = ?`,
		`DELETE FROM runs WHERE id = ?`,
	} {
		if _, err := db.ExecContext(ctx, statement, run.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetProductionAttempt(ctx, campaign, 1)
		return err
	}); err != nil {
		t.Fatalf("fixture must pass the existing attempt reconstruction gate: %v", err)
	}
	handler := signet.NewHTTPHandler(f.service, testAuthorizer)
	response := bearerRequest(t, handler, http.MethodGet, "/tasks/"+string(taskID)+"/timeline", "Bearer test-device-1", nil)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("missing implementation status = %d, body = %s", response.Code, response.Body.String())
	}
}

func TestTaskTimelineRejectsNonUTCTaskCreation(t *testing.T) {
	f := newRunFixture(t)
	var task domain.Task
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		var err error
		task, err = tx.GetOrCreateTask(t.Context(), "proj-1", domain.SpecificationSource{
			Kind:         domain.SpecificationSourceIssueSubject,
			IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 123, IssueNumber: 79},
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.GetTaskTimeline(t.Context(), task.ID); err != nil {
		t.Fatalf("valid task timeline: %v", err)
	}
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(t.Context(), `UPDATE tasks SET body = json_set(body, '$.created_at', ?) WHERE id = ?`,
		"2026-09-12T08:00:00-04:00", task.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		_, err := tx.GetTaskSnapshot(t.Context(), task.ID)
		return err
	}); err != nil {
		t.Fatalf("fixture must pass the existing task reconstruction gate: %v", err)
	}
	if _, err := f.service.GetTaskTimeline(t.Context(), task.ID); !errors.Is(err, domain.ErrTimestampNotUTC) ||
		!errors.Is(err, signet.ErrRunObservationIntegrity) {
		t.Fatalf("non-UTC task creation error = %v", err)
	}
}

func TestTaskTimelineRejectsCorruptPRHistoryRows(t *testing.T) {
	for name, statement := range map[string]string{
		"unsupported declaration": `UPDATE work_unit_declarations SET body = json_set(body, '$.declared_paths', json('["different"]')) WHERE run_id = ?`,
		"inconsistent PR binding": `UPDATE work_unit_pr_bindings SET pr_number = pr_number + 1 WHERE unit_id = (SELECT unit_id FROM work_unit_declarations WHERE run_id = ?)`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newCorpusFixture(t)
			f.seedAuthIdentity(t)
			runID := domain.RunID("task-corrupt-pr-history")
			policy := f.seedPublishedRun(t, runID, "inv-task-corrupt-pr-history")
			f.seedCompletionRecordsForPR(t, runID, policy, corpusReadyPRNumber)
			var taskID domain.TaskID
			if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
				run, err := tx.GetRun(t.Context(), runID)
				taskID = run.TaskID
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.GetTaskTimeline(t.Context(), taskID); err != nil {
				t.Fatalf("valid PR history: %v", err)
			}
			db, err := sql.Open("sqlite", f.path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if _, err := db.ExecContext(t.Context(), statement, runID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.GetRunTimeline(t.Context(), runID); err != nil {
				t.Fatalf("fixture must pass existing run observation gates: %v", err)
			}
			if _, err := f.service.GetTaskTimeline(t.Context(), taskID); !errors.Is(err, signet.ErrRunObservationIntegrity) {
				t.Fatalf("corrupt PR history error = %v", err)
			}
		})
	}
}

func TestTaskTimelineAuthenticatesOpenedPRWithoutCompletionMilestone(t *testing.T) {
	for _, test := range []struct {
		name      string
		prNumber  int
		milestone bool
		wantError bool
	}{
		{"published", corpusReadyPRNumber, true, false},
		{"binding precedes milestone", corpusReadyPRNumber, false, false},
		{"unrelated PR", corpusReadyPRNumber + 1, true, true},
		{"unrelated PR before milestone", corpusReadyPRNumber + 1, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newCorpusFixture(t)
			f.seedAuthIdentity(t)
			runID := domain.RunID("task-opened-pr")
			policy := f.seedPublishedRun(t, runID, "inv-task-opened-pr")
			// This fixture can persist internally consistent PR records that
			// disagree with the independently authenticated ready resource.
			f.seedCompletionRecordsForPR(t, runID, policy, test.prNumber)
			if test.milestone {
				f.appendPublicationReady(t, runID)
			}
			if _, err := f.service.GetRunTimeline(t.Context(), runID); err != nil {
				t.Fatalf("fixture must pass existing observation gates: %v", err)
			}
			var taskID domain.TaskID
			if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
				run, err := tx.GetRun(t.Context(), runID)
				taskID = run.TaskID
				return err
			}); err != nil {
				t.Fatal(err)
			}
			timeline, err := f.service.GetTaskTimeline(t.Context(), taskID)
			if test.wantError {
				if !errors.Is(err, signet.ErrRunObservationIntegrity) {
					t.Fatalf("unrelated PR error = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			events := timeline.Sections[0].Runs[0].Events
			if len(events) != 1 || events[0].Kind != domain.TaskEventPROpened ||
				*events[0].PRNumber != corpusReadyPRNumber || !events[0].RecordedAt.Equal(f.at.Add(time.Hour)) {
				t.Fatalf("opened events = %+v", events)
			}
		})
	}
}
