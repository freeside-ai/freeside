package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

var operatorMessageAt = time.Date(2026, 11, 26, 17, 0, 0, 0, time.UTC)

func recordOperatorMessage(
	t *testing.T, s *store.Store, commandID string, invocation domain.InvocationID,
) (domain.OperatorMessage, error) {
	t.Helper()
	var m domain.OperatorMessage
	err := s.Write(context.Background(), func(tx *store.WriteTx) error {
		var err error
		m, err = tx.RecordOperatorMessage(context.Background(), domain.OperatorMessageRequest{
			CommandID: commandID, Sender: "cli:operator", RunID: "run-1", InvocationID: invocation, Text: "hello",
		}, operatorMessageAt)
		return err
	})
	return m, err
}

func settleOperatorMessage(
	t *testing.T, s *store.Store, commandID string, refusal *domain.OperatorMessageRefusal,
) (domain.OperatorMessage, error) {
	t.Helper()
	var m domain.OperatorMessage
	err := s.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		var err error
		m, err = tx.SettleOperatorMessage(context.Background(), commandID, refusal, operatorMessageAt.Add(time.Second))
		return err
	})
	return m, err
}

// TestOperatorMessagesRecordSettleAndList records messages on two
// invocations, settles some, and lists them back: ordinals count per
// invocation, a command_id records once, and a message settles once.
func TestOperatorMessagesRecordSettleAndList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStoreAt(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	for i, want := range []struct {
		command    string
		invocation domain.InvocationID
		ordinal    int64
	}{{"cmd-1", "inv-1", 1}, {"cmd-2", "inv-1", 2}, {"cmd-3", "inv-2", 1}} {
		m, err := recordOperatorMessage(t, s, want.command, want.invocation)
		if err != nil || m.Ordinal != want.ordinal || m.State != domain.OperatorMessageRecorded {
			t.Fatalf("record %d = %+v, %v; want ordinal %d recorded", i, m, err, want.ordinal)
		}
	}
	if _, err := recordOperatorMessage(t, s, "cmd-1", "inv-1"); !errors.Is(err, store.ErrImmutableConflict) {
		t.Fatalf("recording cmd-1 again = %v, want ErrImmutableConflict", err)
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.RequireCommandIDFree(ctx, "cmd-2") }); !errors.Is(err,
		store.ErrImmutableConflict) {
		t.Fatalf("another kind under a message's command_id = %v, want ErrImmutableConflict", err)
	}

	if m, err := settleOperatorMessage(t, s, "cmd-1", nil); err != nil || m.State != domain.OperatorMessageDelivered {
		t.Fatalf("settle cmd-1 = %+v, %v; want delivered", m, err)
	}
	notTaken := domain.OperatorMessageRefusalNotTaken
	if _, err := settleOperatorMessage(t, s, "cmd-1", &notTaken); !errors.Is(err, domain.ErrOperatorMessageSettled) {
		t.Fatalf("settle cmd-1 again = %v, want ErrOperatorMessageSettled", err)
	}
	if _, err := settleOperatorMessage(t, s, "cmd-none", nil); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("settle an unknown message = %v, want ErrNotFound", err)
	}
	if m, err := settleOperatorMessage(t, s, "cmd-3", &notTaken); err != nil || m.Refusal == nil || *m.Refusal != notTaken {
		t.Fatalf("settle cmd-3 refused = %+v, %v", m, err)
	}

	var listed, unsettled []domain.OperatorMessage
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if listed, err = tx.OperatorMessages(ctx, "inv-1"); err != nil {
			return err
		}
		unsettled, err = tx.UnsettledOperatorMessages(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(listed) != 2 || listed[0].CommandID != "cmd-1" || listed[0].State != domain.OperatorMessageDelivered ||
		listed[1].CommandID != "cmd-2" || listed[1].State != domain.OperatorMessageRecorded {
		t.Fatalf("inv-1's messages = %+v", listed)
	}
	if len(unsettled) != 1 || unsettled[0].CommandID != "cmd-2" {
		t.Fatalf("unsettled = %+v, want only cmd-2", unsettled)
	}
}
