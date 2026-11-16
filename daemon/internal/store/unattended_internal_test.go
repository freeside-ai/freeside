package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func healthPosturePtr(posture domain.HealthPosture) *domain.HealthPosture {
	return &posture
}

// TestAttentionHealthPostureReconstructionFailsClosed proves a partially
// migrated or corrupt health body cannot reach admission as an implicit
// blocker or advisory item. The body bypasses PutAttentionItem intentionally.
func TestAttentionHealthPostureReconstructionFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRawTemplate(t)
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
		`SELECT id, project_id, conversation_id, item_type, status, health_posture, subject_run_id, subject_task_id,
		        readiness_summary, readiness_detail, yield_history, entity_version, as_of_revision, body
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
	s := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})

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
	db := openRawTemplate(t)
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
	seedDecisionSurface(t, ctx, db, item)
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
	db := openRawTemplate(t)
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
