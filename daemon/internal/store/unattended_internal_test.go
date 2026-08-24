package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func healthPosturePtr(posture domain.HealthPosture) *domain.HealthPosture {
	return &posture
}

// TestUnattendedOperationMigrationAppliesFromHead is the migration acceptance
// for 0017: a database at the real prior head upgrades with its existing
// attention items intact, the backfill derives item_type/status from each
// stored body, and the new transition log starts empty — the legitimate
// "never stopped" state, so nothing about the upgrade closes admission.
func TestUnattendedOperationMigrationAppliesFromHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0017_")

	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-1", ProjectID: "proj-1",
		Subject:           domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:              domain.AttentionSystemHealth,
		Priority:          domain.PriorityNormal,
		Reason:            "diagnostic finding",
		RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionExceptional,
		Posture:           healthPosturePtr(domain.HealthPostureBlocking),
		Status:            domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	body, err := encode(item)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO attention_items
		   (id, project_id, conversation_id, entity_version, as_of_revision, body)
		 VALUES ('item-1', 'proj-1', NULL, 1, 1, ?)`, body); err != nil {
		t.Fatalf("seed attention item: %v", err)
	}

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}

	var itemType, status string
	if err := db.QueryRowContext(ctx,
		`SELECT item_type, status FROM attention_items WHERE id = 'item-1'`).
		Scan(&itemType, &status); err != nil {
		t.Fatalf("read backfilled columns: %v", err)
	}
	if itemType != string(domain.AttentionSystemHealth) || status != string(domain.StatusOpen) {
		t.Fatalf("backfilled columns = (%q, %q), want (system_health, open)", itemType, status)
	}

	var transitions int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM unattended_operation_transitions`).Scan(&transitions); err != nil {
		t.Fatalf("count transitions: %v", err)
	}
	if transitions != 0 {
		t.Fatalf("transition log after upgrade holds %d rows, want 0", transitions)
	}
}

// TestAttentionHealthPostureMigrationAppliesFromHead proves the body rewrite
// preserves the historical meaning of pre-0035 system_health rows: the
// formerly implicit blocker becomes explicitly blocking and reconstructs
// through the current domain validation boundary. The body rewrite also
// advances both sync cursors, so a client holding the pre-upgrade revision
// cannot keep the posture-less body cached as current.
func TestAttentionHealthPostureMigrationAppliesFromHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0035_")
	if got := rawVersion(t, db); got != 34 {
		t.Fatalf("prior schema version = %d, want 34", got)
	}

	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "legacy-health", ProjectID: "proj-1",
		Subject:           domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:              domain.AttentionSystemHealth,
		Priority:          domain.PriorityNormal,
		Reason:            "legacy diagnostic",
		RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionExceptional,
		Posture:           healthPosturePtr(domain.HealthPostureBlocking),
		Status:            domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	body, err := encode(item)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	legacyBody := strings.Replace(body, `"posture":"blocking",`, "", 1)
	if legacyBody == body {
		t.Fatalf("legacy posture strip did not apply: %s", body)
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE server_state SET revision = 7 WHERE id = 1`); err != nil {
		t.Fatalf("seed server revision: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO attention_items
	   (id, project_id, conversation_id, item_type, status, entity_version, as_of_revision, body)
	 VALUES (?, ?, NULL, ?, ?, 3, 7, ?)`,
		item.ID, item.ProjectID, item.Type, item.Status, legacyBody); err != nil {
		t.Fatalf("seed legacy item: %v", err)
	}

	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	if got := rawVersion(t, db); got != 52 {
		t.Fatalf("schema version = %d, want 51", got)
	}
	got, snapshot, err := scanAttentionItemRecord(db.QueryRowContext(ctx,
		`SELECT id, project_id, conversation_id, item_type, status, health_posture, subject_run_id,
		        entity_version, as_of_revision, body
		 FROM attention_items WHERE id = ?`, item.ID))
	if err != nil {
		t.Fatalf("reconstruct backfilled item: %v", err)
	}
	if got.Posture == nil || *got.Posture != domain.HealthPostureBlocking {
		t.Fatalf("backfilled posture = %v, want blocking", got.Posture)
	}
	if snapshot != (Snapshot{EntityVersion: 4, AsOfRevision: 8}) {
		t.Fatalf("backfilled snapshot = %+v, want entity version 4 at revision 8", snapshot)
	}
	var serverRevision int64
	if err := db.QueryRowContext(ctx,
		`SELECT revision FROM server_state WHERE id = 1`).Scan(&serverRevision); err != nil {
		t.Fatalf("read server revision: %v", err)
	}
	if serverRevision != 8 {
		t.Fatalf("server revision = %d, want 8", serverRevision)
	}
}

// TestAttentionHealthPostureReconstructionFailsClosed proves a partially
// migrated or corrupt health body cannot reach admission as an implicit
// blocker or advisory item. The body bypasses PutAttentionItem intentionally.
func TestAttentionHealthPostureReconstructionFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "missing-posture", ProjectID: "proj-1",
		Subject:           domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:              domain.AttentionSystemHealth,
		Priority:          domain.PriorityNormal,
		Reason:            "corrupt diagnostic",
		RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionExceptional,
		Posture:           healthPosturePtr(domain.HealthPostureBlocking),
		Status:            domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	body, err := encode(item)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	body = strings.Replace(body, `"posture":"blocking",`, "", 1)
	if _, err := db.ExecContext(ctx, `INSERT INTO attention_items
	   (id, project_id, conversation_id, item_type, status, entity_version, as_of_revision, body)
	 VALUES (?, ?, NULL, ?, ?, 1, 1, ?)`,
		item.ID, item.ProjectID, item.Type, item.Status, body); err != nil {
		t.Fatalf("seed malformed item: %v", err)
	}
	_, _, err = scanAttentionItemRecord(db.QueryRowContext(ctx,
		`SELECT id, project_id, conversation_id, item_type, status, health_posture, subject_run_id,
		        entity_version, as_of_revision, body
		 FROM attention_items WHERE id = ?`, item.ID))
	if !errors.Is(err, domain.ErrHealthPostureInconsistent) {
		t.Fatalf("missing posture reconstruction = %v, want %v", err, domain.ErrHealthPostureInconsistent)
	}
}

// TestTamperedTransitionFailsClosed pins the transition trust binding: the
// stored state is a decoded trust bit ("resumed" lifts a safety gate), so
// reconstruction re-derives it from the immutable accepted command the row
// names. Flipping a stopped row to the still-valid enum value "resumed"
// fails closed against the command that says stop_unattended, and a row
// stripped of its command binding is refused outright.
func TestTamperedTransitionFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	carrier, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "carrier-1", ProjectID: "proj-1",
		Subject:           domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:              domain.AttentionSystemHealth,
		Priority:          domain.PriorityNormal,
		Reason:            "operating-state decision carrier",
		RequestedDecision: []domain.Action{domain.ActionStopUnattended},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionExceptional,
		Posture:           healthPosturePtr(domain.HealthPostureBlocking),
		Status:            domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	commandID := "cmd-stop"
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := s.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutAttentionItem(ctx, carrier); err != nil {
			return err
		}
		command, err := domain.NewCommand(domain.CommandInput{
			CommandID: commandID, DeviceID: "device-1",
			ItemID: carrier.ID, ItemVersion: 1, Action: domain.ActionStopUnattended,
		})
		if err != nil {
			return err
		}
		if err := tx.PutCommand(ctx, command); err != nil {
			return err
		}
		return tx.RecordUnattendedOperationTransition(ctx, domain.UnattendedOperationTransition{
			State: domain.UnattendedStopped, CommandID: &commandID,
			Reason: "operator stop", OccurredAt: at,
		})
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	latestErr := func() error {
		return s.Read(ctx, func(tx *ReadTx) error {
			_, _, err := tx.LatestUnattendedOperationTransition(ctx)
			return err
		})
	}
	if err := latestErr(); err != nil {
		t.Fatalf("control read: %v", err)
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE unattended_operation_transitions SET state = 'resumed'`); err != nil {
		t.Fatalf("tamper state: %v", err)
	}
	if err := latestErr(); !errors.Is(err, domain.ErrTransitionCommandMismatch) {
		t.Fatalf("tampered resumed row = %v, want %v", err, domain.ErrTransitionCommandMismatch)
	}

	if _, err := s.db.ExecContext(ctx,
		`UPDATE unattended_operation_transitions SET state = 'stopped', command_id = NULL`); err != nil {
		t.Fatalf("strip command binding: %v", err)
	}
	if err := latestErr(); !errors.Is(err, domain.ErrTransitionUnbacked) {
		t.Fatalf("unbacked row = %v, want %v", err, domain.ErrTransitionUnbacked)
	}

	// The write boundary refuses the same mismatch: a transition claiming a
	// state its command's action does not authorize is never persisted.
	err = s.WriteInternal(ctx, func(tx *InternalTx) error {
		return tx.RecordUnattendedOperationTransition(ctx, domain.UnattendedOperationTransition{
			State: domain.UnattendedResumed, CommandID: &commandID,
			Reason: "forged resume", OccurredAt: at.Add(time.Minute),
		})
	})
	if !errors.Is(err, domain.ErrTransitionCommandMismatch) {
		t.Fatalf("mismatched write = %v, want %v", err, domain.ErrTransitionCommandMismatch)
	}
}

// TestForgedAdmissionColumnsOrBodyCannotLiftABlocker pins both halves of the
// admission trust rule: lookup-column tampering cannot hide a blocker, and a
// body-only posture rewrite cannot downgrade it to advisory. The whole-table
// divergence count fails either manipulation closed before admission acts.
func TestForgedAdmissionColumnsOrBodyCannotLiftABlocker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "blocker-1", ProjectID: "proj-1",
		Subject:           domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:              domain.AttentionSystemHealth,
		Priority:          domain.PriorityNormal,
		Reason:            "diagnostic finding",
		RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionExceptional,
		Posture:           healthPosturePtr(domain.HealthPostureBlocking),
		Status:            domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	body, err := encode(item)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	admission := domain.ExecutionAdmission{
		InvocationID: "inv-1", OperatingMode: domain.ModeUnattended,
	}
	requireAdmissible := func() error {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback() }()
		rtx := ReadTx{tx: tx}
		return rtx.RequireUnattendedAdmissible(ctx, admission)
	}

	for name, forged := range map[string]string{
		"status hidden as resolved": `INSERT INTO attention_items
		   (id, project_id, conversation_id, item_type, status, health_posture, entity_version, as_of_revision, body)
		 VALUES ('blocker-1', 'proj-1', NULL, 'system_health', 'resolved', 'blocking', 1, 1, ?)`,
		"type hidden as blocked": `INSERT INTO attention_items
		   (id, project_id, conversation_id, item_type, status, health_posture, entity_version, as_of_revision, body)
		 VALUES ('blocker-1', 'proj-1', NULL, 'blocked', 'open', 'blocking', 1, 1, ?)`,
		"body posture downgraded": `INSERT INTO attention_items
		   (id, project_id, conversation_id, item_type, status, health_posture, entity_version, as_of_revision, body)
		 VALUES ('blocker-1', 'proj-1', NULL, 'system_health', 'open', 'blocking', 1, 1,
		         json_set(?, '$.posture', 'advisory'))`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, `DELETE FROM attention_items`); err != nil {
				t.Fatalf("reset: %v", err)
			}
			if _, err := db.ExecContext(ctx, forged, body); err != nil {
				t.Fatalf("seed forged row: %v", err)
			}
			if err := requireAdmissible(); !errors.Is(err, errRowInconsistent) {
				t.Fatalf("admission over a hidden blocker = %v, want %v", err, errRowInconsistent)
			}
		})
	}

	// Control: the same row with truthful columns blocks through the
	// ordinary predicate, proving the forged variants above were hiding a
	// row that would have refused admission anyway.
	if _, err := db.ExecContext(ctx, `DELETE FROM attention_items`); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO attention_items
	   (id, project_id, conversation_id, item_type, status, health_posture, entity_version, as_of_revision, body)
	 VALUES ('blocker-1', 'proj-1', NULL, 'system_health', 'open', 'blocking', 1, 1, ?)`, body); err != nil {
		t.Fatalf("seed truthful row: %v", err)
	}
	if err := requireAdmissible(); !errors.Is(err, domain.ErrBlockingSystemHealth) {
		t.Fatalf("admission over an honest blocker = %v, want %v", err, domain.ErrBlockingSystemHealth)
	}
}

// TestForgedItemTypeColumnFailsClosed pins the lookup-key trust rule: the
// extracted columns select candidates, but a row whose columns diverge from
// its canonical body is refused at reconstruction, so a tampered column
// cannot hide an open blocking item from the admission query while the row
// still reads as valid elsewhere.
func TestForgedItemTypeColumnFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-1", ProjectID: "proj-1",
		Subject:           domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:              domain.AttentionSystemHealth,
		Priority:          domain.PriorityNormal,
		Reason:            "diagnostic finding",
		RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionExceptional,
		Posture:           healthPosturePtr(domain.HealthPostureBlocking),
		Status:            domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	body, err := encode(item)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for name, forged := range map[string]string{
		"item_type": `INSERT INTO attention_items
		   (id, project_id, conversation_id, item_type, status, health_posture, entity_version, as_of_revision, body)
		 VALUES ('item-1', 'proj-1', NULL, 'blocked', 'open', 'blocking', 1, 1, ?)`,
		"status": `INSERT INTO attention_items
		   (id, project_id, conversation_id, item_type, status, health_posture, entity_version, as_of_revision, body)
		 VALUES ('item-1', 'proj-1', NULL, 'system_health', 'resolved', 'blocking', 1, 1, ?)`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, `DELETE FROM attention_items`); err != nil {
				t.Fatalf("reset: %v", err)
			}
			if _, err := db.ExecContext(ctx, forged, body); err != nil {
				t.Fatalf("seed forged row: %v", err)
			}
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback() }()
			rtx := ReadTx{tx: tx}
			if _, err := rtx.GetAttentionItem(ctx, "item-1"); err == nil {
				t.Fatal("forged column reconstructed as valid")
			}
		})
	}
}
