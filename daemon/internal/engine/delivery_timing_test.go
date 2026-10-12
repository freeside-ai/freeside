package engine

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// deliveryNotifier records real delivery writes on a test's store: a signet
// service composed with the ntfy channel, pointed at a local provider that
// accepts every notification, and one active device to notify. The engine
// tests that use it need the timing write a delivery makes, not a planted
// row, because that write is what must leave item_version alone (#1946).
type deliveryNotifier struct {
	service *signet.Service
	store   *store.Store
	device  domain.DeviceID
}

func newDeliveryNotifier(t *testing.T, st *store.Store) *deliveryNotifier {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	// Each receipt needs its own instant, in lifecycle order.
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time {
		now = now.Add(time.Second)
		return now
	}
	device := domain.Device{
		ID: "device-notified", DisplayName: "notified phone",
		Status: domain.DeviceActive, PairedAt: now,
	}
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		return tx.PutDevice(t.Context(), device)
	}); err != nil {
		t.Fatalf("seed notified device: %v", err)
	}
	return &deliveryNotifier{
		service: signet.NewService(st, signet.WithClock(clock), signet.WithNtfy(signet.NtfyConfig{
			BaseURL: server.URL, Client: server.Client(),
			TopicKey: []byte("engine-test-topic-key-0123456789abcdef"),
		})),
		store:  st,
		device: device.ID,
	}
}

// deliver submits a notification for an open item and has the provider accept
// it, which changes the item's timing twice.
func (n *deliveryNotifier) deliver(t *testing.T, itemID domain.ItemID) domain.AttentionDelivery {
	t.Helper()
	before := n.item(t, itemID)
	row, err := n.service.SubmitDelivery(t.Context(), itemID, n.device)
	if err != nil {
		t.Fatalf("SubmitDelivery(%s): %v", itemID, err)
	}
	if row.Status != domain.DeliveryChannelAccepted {
		t.Fatalf("delivery status = %q, want channel_accepted", row.Status)
	}
	n.assertTimingMoved(t, before, "accepted delivery")
	return row
}

// open records the opened receipt for a delivery, whatever the item's status.
func (n *deliveryNotifier) open(t *testing.T, row domain.AttentionDelivery) {
	t.Helper()
	before := n.item(t, row.ItemID)
	if _, err := n.service.RecordDeliveryOpened(
		t.Context(), row.ItemID, row.DeviceID, row.Channel, row.Attempt,
	); err != nil {
		t.Fatalf("RecordDeliveryOpened(%s): %v", row.ItemID, err)
	}
	n.assertTimingMoved(t, before, "opened receipt")
}

func (n *deliveryNotifier) item(t *testing.T, itemID domain.ItemID) domain.AttentionItem {
	t.Helper()
	var item domain.AttentionItem
	if err := n.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItemRecord(t.Context(), itemID)
		return err
	}); err != nil {
		t.Fatalf("read item %s: %v", itemID, err)
	}
	return item
}

// assertTimingMoved fails a test whose delivery event did not reach the item:
// a test that relies on the timing write would otherwise pass without it.
func (n *deliveryNotifier) assertTimingMoved(t *testing.T, before domain.AttentionItem, event string) {
	t.Helper()
	after := n.item(t, before.ID)
	if reflect.DeepEqual(after.Timing, before.Timing) {
		t.Fatalf("%s left item %s timing unchanged: %+v", event, before.ID, after.Timing)
	}
	if after.ItemVersion != before.ItemVersion {
		t.Fatalf("%s moved item %s version %d → %d", event, before.ID, before.ItemVersion, after.ItemVersion)
	}
}
