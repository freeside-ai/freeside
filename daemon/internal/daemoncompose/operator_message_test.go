package daemoncompose

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/stage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// fakeMessageDriver answers each offer by invocation and records the
// offers it saw.
type fakeMessageDriver struct {
	answers map[domain.InvocationID]error
	offers  []string
}

func (d *fakeMessageDriver) SendMessage(
	_ context.Context, id domain.InvocationID, ordinal int64, commandID, _ string,
) error {
	d.offers = append(d.offers, fmt.Sprintf("%s#%d %s", id, ordinal, commandID))
	return d.answers[id]
}

// TestSettleOperatorMessagesAfterRestart: a restart offers each recorded
// message again under its own ordinal and command ID, settles what the run
// answers, reads a run whose directory is gone as delivery_unknown, and
// leaves a message whose offer errors recorded for the next start. A
// settled message isn't offered again.
func TestSettleOperatorMessagesAfterRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	at := time.Date(2026, 11, 26, 17, 0, 0, 0, time.UTC)
	record := func(commandID string, id domain.InvocationID) {
		t.Helper()
		if err := st.Write(ctx, func(tx *store.WriteTx) error {
			_, err := tx.RecordOperatorMessage(ctx, domain.OperatorMessageRequest{
				CommandID: commandID, Sender: "cli:operator", RunID: "run-1", InvocationID: id, Text: "steer",
			}, at)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	record("taken", "writer-taken")
	record("released", "writer-released")
	record("unstarted", "writer-unstarted")
	record("unreachable", "writer-unreachable")
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		_, err := tx.RecordOperatorMessage(ctx, domain.OperatorMessageRequest{
			CommandID: "settled", Sender: "cli:operator", RunID: "run-1", InvocationID: "writer-taken", Text: "steer",
		}, at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.SettleOperatorMessage(ctx, "settled", nil, at)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	driver := &fakeMessageDriver{answers: map[domain.InvocationID]error{
		"writer-released":    fmt.Errorf("terminal: %w", stage.ErrCordonNoRun),
		"writer-unstarted":   fmt.Errorf("no intent: %w", exec.ErrUnknownInvocation),
		"writer-unreachable": errors.New("cordon port unavailable"),
	}}
	if err := settleOperatorMessages(ctx, st, driver, func() time.Time { return at.Add(time.Minute) }, nil); err != nil {
		t.Fatal(err)
	}

	wantOffers := []string{
		"writer-released#1 released", "writer-taken#1 taken",
		"writer-unreachable#1 unreachable", "writer-unstarted#1 unstarted",
	}
	if fmt.Sprint(driver.offers) != fmt.Sprint(wantOffers) {
		t.Fatalf("offers = %v, want %v", driver.offers, wantOffers)
	}
	want := map[string]string{
		"taken": "delivered <nil>", "released": "refused delivery_unknown",
		"unstarted": "refused not_started", "unreachable": "recorded <nil>",
	}
	for commandID, state := range want {
		var m domain.OperatorMessage
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			m, err = tx.GetOperatorMessage(ctx, commandID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		got := fmt.Sprintf("%s %v", m.State, m.Refusal)
		if m.Refusal != nil {
			got = fmt.Sprintf("%s %s", m.State, *m.Refusal)
		}
		if got != state {
			t.Errorf("%s = %s, want %s", commandID, got, state)
		}
	}
}

// TestWriterMessageSenderReadsAGoneRunAsEnded: on the command path the
// message was never offered before, so a run whose directory is gone had
// ended, and a driver error is returned unsettled.
func TestWriterMessageSenderReadsAGoneRunAsEnded(t *testing.T) {
	t.Parallel()
	m := domain.OperatorMessage{OperatorMessageRequest: domain.OperatorMessageRequest{InvocationID: "writer-1"}, Ordinal: 1}
	driver := &fakeMessageDriver{answers: map[domain.InvocationID]error{"writer-1": stage.ErrCordonNoRun}}
	sender := writerMessageSender{driver: driver, noRun: domain.OperatorMessageRefusalRunEnded}
	refusal, err := sender.OfferWriterMessage(context.Background(), m)
	if err != nil || refusal == nil || *refusal != domain.OperatorMessageRefusalRunEnded {
		t.Fatalf("refusal, err = %v, %v; want run_ended", refusal, err)
	}
	driver.answers["writer-1"] = errors.New("port unavailable")
	if refusal, err := sender.OfferWriterMessage(context.Background(), m); err == nil || refusal != nil {
		t.Fatalf("refusal, err = %v, %v; want the error", refusal, err)
	}
}
