package signet_test

import (
	"context"
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// currentCommandOn builds a command against an item's stored entity version,
// for a decision that follows an earlier write to the same item.
func currentCommandOn(
	t *testing.T, f fixture, itemID domain.ItemID, commandID string, action domain.Action,
) signet.ClientCommand {
	t.Helper()
	item, snapshot := f.itemSnapshotFor(t, itemID)
	command := commandOn(item, commandID, action)
	command.ExpectedEntityVersion = snapshot.EntityVersion
	return command
}

func bootstrapUnattended(t *testing.T, service *signet.Service) signet.UnattendedOperationSnapshot {
	t.Helper()
	bootstrap, err := service.Bootstrap(context.Background())
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	heartbeat, err := service.Revision(context.Background())
	if err != nil {
		t.Fatalf("Revision: %v", err)
	}
	if !reflect.DeepEqual(heartbeat.UnattendedOperation, bootstrap.UnattendedOperation) {
		t.Fatalf("heartbeat = %+v, bootstrap = %+v", heartbeat.UnattendedOperation, bootstrap.UnattendedOperation)
	}
	return bootstrap.UnattendedOperation
}

func TestRevisionTracksLiveBackupHealthWithoutRevisionChange(t *testing.T) {
	ctx := context.Background()
	health := domain.BackupHealth{
		Encryption: domain.BackupHealthHealthy, CheckpointCurrency: domain.BackupHealthHealthy,
		ArtifactClosure: domain.BackupHealthHealthy, RestoreTestAge: domain.BackupHealthHealthy,
	}
	s := storetest.Open(t, t.TempDir()+"/signet.db", store.Options{
		BackupHealthSource: store.BackupHealthSourceFunc(func(context.Context, store.BackupHealthContext) (domain.BackupHealth, error) {
			return health, nil
		}),
	})
	service := signet.NewService(s)
	posture := domain.HealthPostureBlocking
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "waiver-notice", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:    domain.AttentionSystemHealth, Priority: domain.PriorityNormal,
		Reason: "Backup encryption waiver", RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		ItemVersion: 1, InterruptionClass: domain.InterruptionExceptional,
		Posture: &posture, Status: domain.StatusOpen,
		BlockingSupersession: &domain.BlockingSupersession{
			Kind: domain.SupersessionBackupEncryptionWaiver, RepositoryID: 424242,
		},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := service.PutItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	initial, err := service.Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, encryption := range []domain.BackupHealthStatus{
		domain.BackupHealthHealthy, domain.BackupHealthUnhealthy, domain.BackupHealthHealthy,
	} {
		health.Encryption = encryption
		got := bootstrapUnattended(t, service)
		if encryption == domain.BackupHealthHealthy {
			requireOpen(t, "healthy backup", got)
		} else if got.Admission != domain.UnattendedAdmissionStopped || len(got.Stops) != 1 ||
			got.Stops[0].Kind != domain.UnattendedStopBlockingSystemHealth ||
			got.Stops[0].ItemID == nil || *got.Stops[0].ItemID != item.ID {
			t.Fatalf("unhealthy backup = %+v, want the waiver notice stop", got)
		}
		current, err := service.Revision(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if current.SyncEpoch != initial.SyncEpoch || current.Revision != initial.Revision {
			t.Fatalf("backup health moved cursor: initial=%+v, current=%+v", initial, current)
		}
	}
}

func TestRevisionFailsWhenUnattendedGateCannotBeRead(t *testing.T) {
	f := newFixture(t)
	item := seedHealthItem(t, f, "broken-health")
	db, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(t.Context(), `UPDATE attention_items SET health_posture = 'advisory' WHERE id = ?`, item.ID); err != nil {
		t.Fatal(err)
	}
	got, err := f.service.Revision(t.Context())
	if err == nil || !strings.Contains(err.Error(), "unattended operation gate:") {
		t.Fatalf("Revision error = %v, want gate read failure", err)
	}
	if !reflect.DeepEqual(got, signet.ServerRevision{}) {
		t.Fatalf("failed heartbeat returned a payload: %+v", got)
	}
}

func requireOpen(t *testing.T, when string, got signet.UnattendedOperationSnapshot) {
	t.Helper()
	if got.Admission != domain.UnattendedAdmissionOpen || got.Stops == nil || len(got.Stops) != 0 {
		t.Fatalf("%s: unattended_operation = %+v, want open with stops []", when, got)
	}
}

// TestUnattendedOperationSyncSurface is #980's projection contract at the
// service boundary: the bootstrap carries the gate's verdict and what stopped
// it, the state survives a sync rebuild and an acknowledge, and only resume
// reopens it. Each step moves the server revision, which is what makes a
// client's heartbeat re-bootstrap without polling.
func TestUnattendedOperationSyncSurface(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	requireOpen(t, "fresh daemon", bootstrapUnattended(t, f.service))

	// An open blocking finding closes the gate before any operator decision.
	health := seedHealthItem(t, f, "health-1")
	blocked := bootstrapUnattended(t, f.service)
	if blocked.Admission != domain.UnattendedAdmissionStopped || len(blocked.Stops) != 1 {
		t.Fatalf("blocking finding: unattended_operation = %+v, want one stop", blocked)
	}
	if stop := blocked.Stops[0]; stop.Kind != domain.UnattendedStopBlockingSystemHealth ||
		stop.ItemID == nil || *stop.ItemID != health.ID || stop.CommandID != nil {
		t.Fatalf("blocking finding stop = %+v, want blocking_system_health on %q with no command", stop, health.ID)
	}

	beforeStop := f.revision(t)
	if _, err := f.service.Submit(ctx, commandOn(health, "cmd-stop", domain.ActionStopUnattended)); err != nil {
		t.Fatalf("Submit(stop_unattended): %v", err)
	}
	if after := f.revision(t); after <= beforeStop {
		t.Fatalf("stop left revision at %d; a client would never re-bootstrap", after)
	}
	open := openHealthItems(t, f.store)
	if len(open) != 1 {
		t.Fatalf("open system_health items = %d, want the stopped notice", len(open))
	}
	notice := open[0]

	requireOperatorStop := func(when string, got signet.UnattendedOperationSnapshot) {
		t.Helper()
		// One record, not two: the notice is the operator stop's item, never
		// a second blocking finding.
		if got.Admission != domain.UnattendedAdmissionStopped || len(got.Stops) != 1 {
			t.Fatalf("%s: unattended_operation = %+v, want exactly the operator stop", when, got)
		}
		stop := got.Stops[0]
		if stop.Kind != domain.UnattendedStopOperator ||
			stop.ItemID == nil || *stop.ItemID != notice.ID ||
			stop.CommandID == nil || *stop.CommandID != "cmd-stop" ||
			stop.Since == nil || !stop.Since.Equal(*f.now) {
			t.Fatalf("%s: stop = %+v, want operator_stop by cmd-stop on notice %q at %v",
				when, stop, notice.ID, *f.now)
		}
	}
	requireOperatorStop("after stop", bootstrapUnattended(t, f.service))

	// Sync rebuild: a new service over the same store holds no memory of the
	// stop, so the state it reports is the durable one.
	rebuilt := signet.NewService(f.store, signet.WithClock(func() time.Time { return *f.now }))
	requireOperatorStop("after rebuild", bootstrapUnattended(t, rebuilt))

	// Acknowledge is seen, never resolved (plan §4): it must not clear the
	// stop.
	if _, err := f.service.Submit(ctx,
		currentCommandOn(t, f, notice.ID, "cmd-ack", domain.ActionAcknowledge)); err != nil {
		t.Fatalf("Submit(acknowledge): %v", err)
	}
	requireOperatorStop("after acknowledge", bootstrapUnattended(t, f.service))

	beforeResume := f.revision(t)
	if _, err := f.service.Submit(ctx,
		currentCommandOn(t, f, notice.ID, "cmd-resume", domain.ActionResumeUnattended)); err != nil {
		t.Fatalf("Submit(resume_unattended): %v", err)
	}
	if after := f.revision(t); after <= beforeResume {
		t.Fatalf("resume left revision at %d; a client would never re-bootstrap", after)
	}
	requireOpen(t, "after resume", bootstrapUnattended(t, f.service))
}

// TestUnattendedOperationSyncDurableStop: the daemon's own durable stop
// (cmd/freesided fileDurableStop) projects as a blocking-item stop, so a
// client never words it as an operator's decision, and it clears when the
// daemon resolves the item.
func TestUnattendedOperationSyncDurableStop(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	posture := domain.HealthPostureBlocking
	createdAt := f.now.Add(-time.Minute)
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "system-health-daemon-durable-stop-7", ProjectID: "proj-1",
		Subject:           domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:              domain.AttentionSystemHealth,
		Priority:          domain.PriorityHigh,
		Reason:            "The daemon stopped unattended operation after a supervised restart loop.",
		RequestedDecision: []domain.Action{domain.ActionAcknowledge, domain.ActionRunDoctor},
		HealthDiagnostic: &domain.HealthDiagnostic{
			Code: "daemon_durable_stop", Impairs: domain.ImpairedCapabilityUnattendedAdmission,
		},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionExceptional,
		CreatedAt:         &createdAt,
		Posture:           &posture,
		Status:            domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	if err := f.service.PutItem(ctx, item); err != nil {
		t.Fatalf("seed durable stop: %v", err)
	}

	got := bootstrapUnattended(t, f.service)
	if got.Admission != domain.UnattendedAdmissionStopped || len(got.Stops) != 1 {
		t.Fatalf("unattended_operation = %+v, want one stop", got)
	}
	stop := got.Stops[0]
	if stop.Kind != domain.UnattendedStopBlockingSystemHealth ||
		stop.ItemID == nil || *stop.ItemID != item.ID || stop.CommandID != nil ||
		stop.Since == nil || !stop.Since.Equal(createdAt) {
		t.Fatalf("stop = %+v, want blocking_system_health on %q since %v", stop, item.ID, createdAt)
	}

	if _, err := f.service.Submit(ctx,
		currentCommandOn(t, f, item.ID, "cmd-ack", domain.ActionAcknowledge)); err != nil {
		t.Fatalf("Submit(acknowledge): %v", err)
	}
	if got := bootstrapUnattended(t, f.service); got.Admission != domain.UnattendedAdmissionStopped {
		t.Fatalf("after acknowledge: unattended_operation = %+v, want still stopped", got)
	}

	resolved, _ := f.itemSnapshotFor(t, item.ID)
	resolved.ItemVersion++
	resolved.Status = domain.StatusResolved
	if err := f.service.PutItem(ctx, resolved); err != nil {
		t.Fatalf("resolve durable stop: %v", err)
	}
	requireOpen(t, "after the daemon resolved the item", bootstrapUnattended(t, f.service))
}
