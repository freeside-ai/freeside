package signet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// seedWorkHoursRun seeds a run of a new task in project: submitted and
// admitted at f.at, which registers the project, its one invocation started after started and still
// working. It returns the run's task and its admission.
func (f corpusFixture) seedWorkHoursRun(
	t *testing.T, project domain.ProjectID, issue int, runID domain.RunID, started time.Duration,
) (domain.TaskID, domain.ExecutionAdmission) {
	t.Helper()
	ctx := context.Background()
	invocation := domain.InvocationID("inv-" + string(runID))
	var taskID domain.TaskID
	f.mustWrite(t, func(tx *store.WriteTx) error {
		task, err := tx.GetOrCreateTask(ctx, project, domain.SpecificationSource{
			Kind:         domain.SpecificationSourceIssueSubject,
			IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 1, IssueNumber: issue},
		})
		if err != nil {
			return err
		}
		taskID = task.ID
		run := corpusRun(runID, corpusAttempt(runID, invocation))
		run.ProjectID, run.TaskID = project, task.ID
		if err := tx.PutRun(ctx, run); err != nil {
			return err
		}
		if _, _, err := tx.EnqueueOutbox(ctx, string(invocation), string(domain.ProductionInvocationRequestedKind),
			productionIntentPayload(invocation, runID, corpusStageID(runID))); err != nil {
			return err
		}
		return tx.AppendRunMilestone(ctx, domain.RunMilestone{
			RunID: runID, Kind: domain.MilestoneRunSubmitted, InvocationID: &invocation, RecordedAt: f.at,
		})
	})
	admission := f.seedAdmission(t, runID, corpusStageID(runID), corpusAttempt(runID, invocation).ID, invocation)
	f.mustWriteInternal(t, func(tx *store.InternalTx) error {
		return tx.MarkOutboxDispatched(ctx, string(invocation))
	})
	f.appendMilestone(t, domain.RunMilestone{
		RunID: runID, Kind: domain.MilestoneInvocationStarted, InvocationID: &invocation, RecordedAt: f.at.Add(started),
	})
	return taskID, admission
}

// seedWorkHoursWaits gives the run a hold history and an open card: held
// for an update drain from 2 to 8 minutes, then attended mode from 8
// minutes on, and a failure card open from 10 minutes on. The failure the
// card binds to is recorded at an hour, the read's as_of.
func (f corpusFixture) seedWorkHoursWaits(t *testing.T, admission domain.ExecutionAdmission, project domain.ProjectID) {
	t.Helper()
	ctx := context.Background()
	runID := admission.RunID
	f.seedOutcome(t, admission, domain.ExecutionOutcomeFailed)
	f.mustWrite(t, func(tx *store.WriteTx) error {
		if err := tx.RecordRunHold(ctx, domain.RunHoldObservation{
			RunID: runID, Reason: domain.HoldUpdateDrain,
			FirstObservedAt: f.at.Add(2 * time.Minute), LastObservedAt: f.at.Add(5 * time.Minute),
		}); err != nil {
			return err
		}
		return tx.RecordRunHold(ctx, domain.RunHoldObservation{
			RunID: runID, InvocationID: &admission.InvocationID, Reason: domain.HoldAttendedModeActive,
			FirstObservedAt: f.at.Add(8 * time.Minute), LastObservedAt: f.at.Add(9 * time.Minute),
		})
	})
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ItemID("item-" + string(runID)), ProjectID: project,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
		Type:    domain.AttentionExecutionFailure, Priority: domain.PriorityHigh,
		Reason: "implementation failed",
		ExecutionFailure: &domain.ExecutionFailureFacts{
			Outcome: domain.ExecutionOutcomeFailed, Cause: domain.FailureCauseUnknown,
			Stage: domain.StageNameImplementation, InvocationID: admission.InvocationID,
		},
		RequestedDecision: []domain.Action{domain.ActionDiscuss, domain.ActionStop},
		ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional,
		Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	item.CreatedAt = new(f.at.Add(10 * time.Minute))
	f.seedItem(t, item)
}

func lastWeek(f corpusFixture) domain.WorkHoursPeriod {
	return domain.WorkHoursPeriod{From: f.at.AddDate(0, 0, -6), Days: 7, Location: time.UTC}
}

// TestWorkHoursLimitedToAProject pins the response for two projects with
// the filter set: only the named project's run counts, and the response
// names the project.
func TestWorkHoursLimitedToAProject(t *testing.T) {
	t.Parallel()
	f := newCorpusFixture(t)
	f.seedAuthIdentity(t)
	taskID, admission := f.seedWorkHoursRun(t, "proj-1", 1, "run-proj-1", 30*time.Minute)
	f.seedWorkHoursWaits(t, admission, "proj-1")
	f.seedWorkHoursRun(t, "proj-2", 2, "run-proj-2", 15*time.Minute)

	project := domain.ProjectID("proj-1")
	hours, err := f.service.WorkHours(t.Context(), signet.WorkHoursRequest{
		Period: domain.WorkHoursPeriod{From: f.at, Days: 2, Location: time.UTC}, ProjectID: &project,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The hold history starts when the test store migrated, a wall-clock
	// instant, and the store names tasks at random; check both, then pin
	// them.
	var since time.Time
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		since, err = tx.HoldsRecordedSince(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !hours.HoldHistorySince.Equal(since) {
		t.Errorf("hold_history_since = %v, want the store's %v", hours.HoldHistorySince, since)
	}
	hours.HoldHistorySince = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	got, err := json.MarshalIndent(hours, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(got, []byte(taskID)) {
		t.Fatalf("response names no task %q:\n%s", taskID, got)
	}
	got = bytes.ReplaceAll(got, []byte(taskID), []byte("task-proj-1"))
	golden.Assert(t, "work-hours-project-filter", append(got, '\n'))

	all, err := f.service.WorkHours(t.Context(), signet.WorkHoursRequest{Period: lastWeek(f)})
	if err != nil {
		t.Fatal(err)
	}
	if all.ProjectID != nil || all.Totals.TaskCount != 2 || all.Totals.ElapsedSeconds != 2*3600 {
		t.Errorf("every project = %+v, want both runs' hours and no project", all.Totals)
	}
}

// TestWorkHoursAgreesWithTheRunTimeline is the proof that the chart and the
// strips agree: for one run inside the period, the totals' elapsed and agent
// working seconds equal the elapsed and active seconds its timeline serves
// at the same as_of.
func TestWorkHoursAgreesWithTheRunTimeline(t *testing.T) {
	t.Parallel()
	f := newCorpusFixture(t)
	ctx := t.Context()
	f.seedAuthIdentity(t)
	taskID, admission := f.seedWorkHoursRun(t, "proj-1", 1, "run-agree", 30*time.Minute)
	f.seedWorkHoursWaits(t, admission, "proj-1")

	hours, err := f.service.WorkHours(ctx, signet.WorkHoursRequest{Period: lastWeek(f)})
	if err != nil {
		t.Fatal(err)
	}
	var timeline signet.TaskTimeline
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		timeline, err = signet.ReadTaskTimeline(ctx, tx, taskID, hours.AsOf)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	clocks := timeline.Sections[0].Runs[0].Clocks
	if clocks == nil || len(timeline.Sections[0].Runs[0].HoldHistory) != 2 {
		t.Fatalf("timeline run = %+v, want clocks and two holds", timeline.Sections[0].Runs[0])
	}
	if hours.Totals.ElapsedSeconds != clocks.ElapsedSeconds || hours.Totals.AgentWorkingSeconds != clocks.ActiveSeconds {
		t.Errorf("totals elapsed %d working %d, want the timeline's elapsed %d active %d",
			hours.Totals.ElapsedSeconds, hours.Totals.AgentWorkingSeconds, clocks.ElapsedSeconds, clocks.ActiveSeconds)
	}
	// Other 0–2 minutes, held 2–10 (update drain to 8, attended mode
	// after), waiting on you 10–30, agent working 30–60.
	want := domain.WorkHoursSeconds{
		ElapsedSeconds: 3600, AgentWorkingSeconds: 1800, WaitingOnYouSeconds: 1200, HeldSeconds: 480,
		HeldByReason: []domain.WorkHoursHeld{
			{Reason: domain.HoldUpdateDrain, Seconds: 360}, {Reason: domain.HoldAttendedModeActive, Seconds: 120},
		},
		OtherSeconds: 120,
	}
	gotJSON, _ := json.Marshal(hours.Totals.WorkHoursSeconds)
	wantJSON, _ := json.Marshal(want)
	if !bytes.Equal(gotJSON, wantJSON) {
		t.Errorf("totals = %s, want %s", gotJSON, wantJSON)
	}
}

// TestWorkHoursSkipsADamagedRun pins that a run whose record fails the
// integrity check is skipped, logged, and counted, and the rest still
// serve.
func TestWorkHoursSkipsADamagedRun(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	f := newCorpusFixture(t, signet.WithLogger(logger))
	f.seedAuthIdentity(t)
	f.seedWorkHoursRun(t, "proj-1", 1, "run-healthy", 30*time.Minute)
	f.seedObservationBindingFailureRun(t, "run-damaged")

	hours, err := f.service.WorkHours(t.Context(), signet.WorkHoursRequest{Period: lastWeek(f)})
	if err != nil {
		t.Fatalf("WorkHours over a damaged run = %v, want the healthy run served", err)
	}
	if hours.SkippedRunCount != 1 || hours.Totals.TaskCount != 1 || hours.Totals.ElapsedSeconds != 3600 {
		t.Errorf("response = skipped %d totals %+v, want one skipped and the healthy run's hour",
			hours.SkippedRunCount, hours.Totals)
	}
	if out := logs.String(); !strings.Contains(out, "excluding run from listing") || !strings.Contains(out, "run-damaged") {
		t.Errorf("logs = %q, want a warn naming the damaged run", out)
	}
}

func TestWorkHoursRefusesAnUnknownProject(t *testing.T) {
	t.Parallel()
	f := newCorpusFixture(t)
	project := domain.ProjectID("proj-missing")
	_, err := f.service.WorkHours(t.Context(), signet.WorkHoursRequest{Period: lastWeek(f), ProjectID: &project})
	if !errors.Is(err, signet.ErrWorkHoursProjectNotFound) {
		t.Fatalf("WorkHours(unknown project) = %v, want ErrWorkHoursProjectNotFound", err)
	}
}

// TestHTTPWorkHours pins the route: a paired device reads the service's
// response, an unknown project is a 404, and each malformed request is a
// 400 that names its parameter.
func TestHTTPWorkHours(t *testing.T) {
	t.Parallel()
	f := newCorpusFixture(t)
	f.seedAuthIdentity(t)
	f.seedWorkHoursRun(t, "proj-1", 1, "run-http", 30*time.Minute)
	handler := signet.NewHTTPHandler(f.service, testAuthorizer)

	const target = "/work-hours?from=2026-08-12&days=2&time_zone=America/New_York&project_id=proj-1"
	response := authenticatedRequest(t, handler, http.MethodGet, target, nil)
	if response.Code != http.StatusOK {
		t.Fatalf("GET %s status = %d body=%s", target, response.Code, response.Body)
	}
	newYork, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	project := domain.ProjectID("proj-1")
	want, err := f.service.WorkHours(t.Context(), signet.WorkHoursRequest{
		Period:    domain.WorkHoursPeriod{From: time.Date(2026, 8, 12, 0, 0, 0, 0, time.UTC), Days: 2, Location: newYork},
		ProjectID: &project,
	})
	if err != nil {
		t.Fatal(err)
	}
	wantJSON, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.TrimSpace(response.Body.Bytes()); !bytes.Equal(got, wantJSON) {
		t.Fatalf("body = %s, want the service's %s", got, wantJSON)
	}
	if want.Totals.ElapsedSeconds != 3600 || want.TimeZone != "America/New_York" {
		t.Fatalf("response = %+v, want the run's hour in New York days", want)
	}

	missing := authenticatedRequest(t, handler, http.MethodGet,
		"/work-hours?from=2026-08-12&days=2&time_zone=UTC&project_id=proj-missing", nil)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown project status = %d body=%s, want 404", missing.Code, missing.Body)
	}

	for query, wantMessage := range map[string]string{
		"days=2&time_zone=UTC":                                           "from is required",
		"from=2026-08-12&from=2026-08-13&days=2&time_zone=UTC":           "from must be given once",
		"from=2026-8-12&days=2&time_zone=UTC":                            "from must be a calendar date",
		"from=2026-02-30&days=2&time_zone=UTC":                           "from must be a calendar date",
		"from=2026-08-12&time_zone=UTC":                                  "days is required",
		"from=0000-12-31&days=1&time_zone=UTC":                           "from must be a calendar date",
		"from=9999-12-01&days=31&time_zone=UTC":                          "the period must end by 9999-12-30",
		"from=2026-08-12&days=two&time_zone=UTC":                         "days must be an integer from 1 to 92",
		"from=2026-08-12&days=0&time_zone=UTC":                           "days must be an integer from 1 to 92",
		"from=2026-08-12&days=93&time_zone=UTC":                          "days must be an integer from 1 to 92",
		"from=2026-08-12&days=2":                                         "time_zone is required",
		"from=2026-08-12&days=2&time_zone=":                              "time_zone must not be empty",
		"from=2026-08-12&days=2&time_zone=Local":                         "time_zone \"Local\" is not a known",
		"from=2026-08-12&days=2&time_zone=Mars/Olympus":                  "time_zone \"Mars/Olympus\" is not a known",
		"from=2026-08-12&days=2&time_zone=GMT%2B0500":                    "time_zone \"GMT+0500\" is not a known",
		"from=2026-08-12&days=2&time_zone=../../etc/passwd":              "is not a known IANA time zone",
		"from=2026-08-12&days=2&time_zone=UTC&project_id=":               "project_id must not be empty",
		"from=2026-08-12&days=2&time_zone=UTC&project_id=a&project_id=b": "project_id must be given once",
	} {
		t.Run(query, func(t *testing.T) {
			response := authenticatedRequest(t, handler, http.MethodGet, "/work-hours?"+query, nil)
			var body struct{ Message string }
			if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if response.Code != http.StatusBadRequest || !strings.Contains(body.Message, wantMessage) {
				t.Fatalf("status = %d message %q, want 400 naming %q", response.Code, body.Message, wantMessage)
			}
		})
	}
}
