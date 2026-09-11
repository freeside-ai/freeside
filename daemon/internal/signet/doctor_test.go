package signet_test

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/scheduler"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

const testDoctorScheduleID domain.ScheduleID = "test-doctor"

func doctorCommandJSON(commandID string) *bytes.Reader {
	return bytes.NewReader([]byte(`{
		"command_id":"` + commandID + `", "device_id":"device-1", "expected_entity_version":1,
		"expected_bindings":{}, "payload":{
			"item_id":"health-doctor", "action":"run_doctor", "item_version":1,
			"pr_head_sha":"", "artifact_digests":[]
		}
	}`))
}

func doctorFixture(t *testing.T) fixture {
	t.Helper()
	f := newFixture(t)
	posture := domain.HealthPostureBlocking
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "health-doctor", ProjectID: "proj-1", Subject: f.item.Subject,
		Type: domain.AttentionSystemHealth, Priority: domain.PriorityHigh,
		Reason:            "A diagnostic needs another pass.",
		RequestedDecision: []domain.Action{domain.ActionRunDoctor},
		ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional,
		CreatedAt: f.now, Posture: &posture, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.PutItem(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	f.item = item
	f.service = signet.NewService(f.store, signet.WithClock(func() time.Time { return *f.now }),
		signet.WithDoctorSchedule(testDoctorScheduleID, func() bool { return true }))
	return f
}

func doctorScheduler(t *testing.T, f fixture, calls *int) *scheduler.Scheduler {
	t.Helper()
	sched, err := scheduler.New(f.store, domain.ModeAttendedDev,
		func() time.Time { return *f.now }, map[domain.ScheduleKind]scheduler.Registration{
			domain.ScheduleDoctor: {Handle: func(context.Context, domain.ScheduleEvent, domain.Schedule) (scheduler.Consumption, error) {
				*calls++
				return scheduler.Consumption{Outcome: domain.OutcomeHandled}, nil
			}},
		})
	if err != nil {
		t.Fatal(err)
	}
	seconds := int64(3600)
	schedule, err := domain.NewSchedule(domain.ScheduleInput{
		ID: testDoctorScheduleID, ProjectID: "project-system", Kind: domain.ScheduleDoctor,
		Subject:   domain.ScheduleSubject{Type: domain.ScheduleSubjectTrustedConfig},
		CreatedAt: *f.now, IntervalSeconds: &seconds,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sched.Arm(context.Background(), schedule, f.now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	return sched
}

func TestHTTPRunDoctorSurvivesRestartAndReplayDoesNotRunAgain(t *testing.T) {
	ctx := context.Background()
	f := doctorFixture(t)
	calls := 0
	sched := doctorScheduler(t, f, &calls)
	if err := sched.RunOnce(ctx); err != nil || calls != 0 {
		t.Fatalf("before request: calls=%d err=%v", calls, err)
	}
	command := f.command("doctor-command", domain.ActionRunDoctor)
	handler := signet.NewHTTPHandler(f.service, testAuthorizer)
	response := authenticatedRequest(t, handler, http.MethodPost, "/commands", doctorCommandJSON(command.CommandID))
	if response.Code != http.StatusOK {
		t.Fatalf("POST /commands = %d: %s", response.Code, response.Body.String())
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store = storetest.Open(t, f.dbPath, store.Options{})
	t.Cleanup(func() { _ = f.store.Close() })
	f.service = signet.NewService(f.store, signet.WithClock(func() time.Time { return *f.now }),
		signet.WithDoctorSchedule(testDoctorScheduleID, func() bool { return true }))
	sched = doctorScheduler(t, f, &calls) // Startup preserves the already-due clock.
	if err := sched.RunOnce(ctx); err != nil || calls != 1 {
		t.Fatalf("after restart: calls=%d err=%v", calls, err)
	}
	if _, err := f.service.Submit(ctx, command); err != nil {
		t.Fatal(err)
	}
	if err := sched.RunOnce(ctx); err != nil || calls != 1 {
		t.Fatalf("after replay: calls=%d err=%v", calls, err)
	}
	if item, _ := f.itemSnapshot(t); item.Status != domain.StatusOpen {
		t.Fatalf("doctor request cleared the health item: %s", item.Status)
	}
	*f.now = f.now.Add(time.Second)
	if _, err := f.service.Submit(ctx, f.command("another-doctor-command", domain.ActionRunDoctor)); err != nil {
		t.Fatal(err)
	}
	if err := sched.RunOnce(ctx); err != nil || calls != 2 {
		t.Fatalf("new request: calls=%d err=%v", calls, err)
	}
}

func TestRunDoctorWithoutAnArmedJobRejectsTheCommandAtomically(t *testing.T) {
	ctx := context.Background()
	f := doctorFixture(t)
	before := f.revision(t)
	if _, err := f.service.Submit(ctx, f.command("unavailable-doctor", domain.ActionRunDoctor)); err == nil {
		t.Fatal("accepted a doctor request without an executable job")
	}
	if f.revision(t) != before {
		t.Fatal("rejected request changed the revision")
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetCommand(ctx, "unavailable-doctor")
		return err
	}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("rejected command persisted: %v", err)
	}
}

func TestHTTPRunDoctorRejectsAnArmedJobWithoutARunningConsumer(t *testing.T) {
	ctx := context.Background()
	f := doctorFixture(t)
	calls := 0
	sched := doctorScheduler(t, f, &calls)
	available := false // A retained schedule in a mode without diagnostics.
	f.service = signet.NewService(f.store, signet.WithClock(func() time.Time { return *f.now }),
		signet.WithDoctorSchedule(testDoctorScheduleID, func() bool { return available }))
	handler := signet.NewHTTPHandler(f.service, testAuthorizer)
	for _, id := range []string{"unsupported-mode", "stopped-consumer"} {
		before := f.revision(t)
		response := authenticatedRequest(t, handler, http.MethodPost, "/commands", doctorCommandJSON(id))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("unavailable doctor = %d: %s", response.Code, response.Body.String())
		}
		if f.revision(t) != before {
			t.Fatal("unavailable consumer changed the revision")
		}
		if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
			_, err := tx.GetCommand(ctx, id)
			return err
		}); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("rejected command persisted: %v", err)
		}
		available = true
		if _, err := f.service.Submit(ctx, f.command("before-"+id, domain.ActionRunDoctor)); err != nil {
			t.Fatal(err)
		}
		if err := sched.RunOnce(ctx); err != nil {
			t.Fatal(err)
		}
		available = false // The consumer stopped while HTTP remained available.
	}
}
