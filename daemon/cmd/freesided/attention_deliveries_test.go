package main

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ntfySink is a loopback stand-in for the ntfy server: it records each
// publish and answers with a scripted status. No test publishes to the hosted
// server.
type ntfySink struct {
	*httptest.Server
	mu     sync.Mutex
	status int
	topics []string
	clicks []string
}

func newNtfySink(t *testing.T, status int) *ntfySink {
	t.Helper()
	sink := &ntfySink{status: status}
	sink.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sink.mu.Lock()
		defer sink.mu.Unlock()
		sink.topics = append(sink.topics, strings.TrimPrefix(r.URL.Path, "/"))
		sink.clicks = append(sink.clicks, r.Header.Get("Click"))
		w.WriteHeader(sink.status)
	}))
	t.Cleanup(sink.Close)
	return sink
}

func (s *ntfySink) published() (topics, clicks []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.topics...), append([]string(nil), s.clicks...)
}

// startNotifyingDaemon runs the walking-skeleton daemon with the notification
// sender on a short interval, publishing to ntfyURL.
func startNotifyingDaemon(t *testing.T, root, ntfyURL string, logs *lockedBuffer) *daemon {
	t.Helper()
	cfg := config{
		Environment:   environmentEphemeral,
		DBPath:        filepath.Join(root, "freeside.db"),
		FakeDriverDir: filepath.Join(root, "driver"),
		ListenAddr:    "127.0.0.1:0", ReconcileInterval: 5 * time.Millisecond,
		SeedWalkingSkeleton: true, FakeDriverEnabled: true,
		NtfyURL: ntfyURL, AttentionDeliveryInterval: 5 * time.Millisecond,
	}
	if logs != nil {
		cfg.Logger = slog.New(slog.NewTextHandler(logs, nil))
	}
	h, err := run(t.Context(), nil, cfg)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	return h
}

// pairDevice registers an active device that paired before any item existed,
// so every open item owes it a notification.
func pairDevice(t *testing.T, h *daemon, id domain.DeviceID) {
	t.Helper()
	ctx := t.Context()
	if err := h.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(ctx, domain.Device{
			ID: id, DisplayName: string(id),
			Status: domain.DeviceActive, PairedAt: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC),
		})
	}); err != nil {
		t.Fatalf("put device: %v", err)
	}
}

func deliveriesFor(t *testing.T, h *daemon, itemID domain.ItemID, deviceID domain.DeviceID) []domain.AttentionDelivery {
	t.Helper()
	var rows []domain.AttentionDelivery
	if err := h.store.Read(t.Context(), func(tx *store.ReadTx) error {
		listed, err := tx.ListAttentionDeliveries(t.Context())
		for _, row := range listed {
			if row.Value.ItemID == itemID && row.Value.DeviceID == deviceID {
				rows = append(rows, row.Value)
			}
		}
		return err
	}); err != nil {
		t.Fatalf("list deliveries: %v", err)
	}
	return rows
}

// eventually polls until ok holds. The daemon under test runs its own
// listeners and store, which a synctest bubble cannot hold, so this waits in
// real time as the package's other whole-daemon tests do; the sender's ticker
// cadence itself is pinned under synctest in the signet package.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s did not happen within 5s", what)
}

// TestDaemonNotifiesAPhoneOncePerItem is the sender's whole-daemon proof: an
// open item and a paired phone produce exactly one accepted notification
// carrying the app's link, further passes send nothing, and neither does a
// daemon restarted over the same store.
//
// A later pass is proved, not waited for: a device paired afterwards is
// notified only by a pass that also saw the phone's settled row.
func TestDaemonNotifiesAPhoneOncePerItem(t *testing.T) {
	root := t.TempDir()
	sink := newNtfySink(t, http.StatusOK)
	approval := "approval-" + domain.ItemID(defaultFakeRunID)
	link := "freeside://attention/items/" + string(approval) + "?channel=ntfy&attempt=1"
	accepted := func(h *daemon, device domain.DeviceID) func() bool {
		return func() bool {
			rows := deliveriesFor(t, h, approval, device)
			return len(rows) == 1 && rows[0].Status == domain.DeliveryChannelAccepted
		}
	}

	h := startNotifyingDaemon(t, root, sink.URL, nil)
	waitForItem(t, h.attention, approval)
	pairDevice(t, h, "phone")
	eventually(t, "the phone's accepted notification", accepted(h, "phone"))
	// Only the phone is paired, so the one publish of the item's link names
	// the phone's topic.
	var phoneTopic string
	topics, clicks := sink.published()
	for i, click := range clicks {
		if click == link {
			phoneTopic = topics[i]
		}
	}
	if phoneTopic == "" {
		t.Fatalf("published tap links = %v, want %q", clicks, link)
	}
	sentToPhone := func() int {
		topics, clicks := sink.published()
		n := 0
		for i, click := range clicks {
			if topics[i] == phoneTopic && strings.HasPrefix(click, "freeside://attention/items/"+string(approval)+"?") {
				n++
			}
		}
		return n
	}

	pairDevice(t, h, "tablet")
	eventually(t, "a later pass notifying the tablet", accepted(h, "tablet"))
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := sentToPhone(); got != 1 {
		t.Fatalf("sent the phone %d notifications for the item before the restart, want 1", got)
	}

	restarted := startNotifyingDaemon(t, root, sink.URL, nil)
	pairDevice(t, restarted, "laptop")
	eventually(t, "a pass after the restart notifying the laptop", accepted(restarted, "laptop"))
	rows := deliveriesFor(t, restarted, approval, "phone")
	if err := restarted.Close(); err != nil {
		t.Fatalf("Close after restart: %v", err)
	}
	if got := sentToPhone(); got != 1 || len(rows) != 1 {
		t.Errorf("after a restart: sent the phone %d notifications and hold %d rows for the item, want 1 and 1", got, len(rows))
	}
}

// TestDaemonSurvivesANotificationOutage: a provider that rejects every send,
// or cannot be reached at all, costs a logged warning per attempt and nothing
// else. The daemon keeps serving, the failed attempt is recorded as submitted
// with no acceptance, and the log names the status or the failure class but
// never the device's topic or the publish URL.
func TestDaemonSurvivesANotificationOutage(t *testing.T) {
	approval := "approval-" + domain.ItemID(defaultFakeRunID)

	t.Run("rejected", func(t *testing.T) {
		sink := newNtfySink(t, http.StatusServiceUnavailable)
		var logs lockedBuffer
		h := startNotifyingDaemon(t, t.TempDir(), sink.URL, &logs)
		t.Cleanup(func() { _ = h.Close() })
		waitForItem(t, h.attention, approval)
		pairDevice(t, h, "phone")

		eventually(t, "the logged rejection", func() bool {
			return strings.Contains(logs.String(), "phone notification not sent")
		})
		rows := deliveriesFor(t, h, approval, "phone")
		if len(rows) != 1 || rows[0].Status != domain.DeliverySubmitted || rows[0].ChannelAcceptedAt != nil {
			t.Errorf("delivery rows = %+v, want one submitted attempt with no acceptance", rows)
		}
		assertDaemonHealthy(t, h)
		log := logs.String()
		if !strings.Contains(log, "ntfy returned status 503") || !strings.Contains(log, "item_id="+string(approval)) ||
			!strings.Contains(log, "device_id=phone") || !strings.Contains(log, "attempt=1") {
			t.Errorf("log = %s\nwant the item, device, attempt, and the provider's status", log)
		}
		topics, _ := sink.published()
		for _, secret := range append(topics, sink.URL, strings.TrimPrefix(sink.URL, "http://")) {
			if strings.Contains(log, secret) {
				t.Errorf("log carries %q:\n%s", secret, log)
			}
		}
	})

	t.Run("unreachable", func(t *testing.T) {
		// A server that is gone: its address refuses every connection.
		gone := httptest.NewServer(http.NotFoundHandler())
		goneURL := gone.URL
		gone.Close()
		var logs lockedBuffer
		h := startNotifyingDaemon(t, t.TempDir(), goneURL, &logs)
		t.Cleanup(func() { _ = h.Close() })
		waitForItem(t, h.attention, approval)
		pairDevice(t, h, "phone")

		eventually(t, "the logged transport failure", func() bool {
			return strings.Contains(logs.String(), "phone notification not sent")
		})
		assertDaemonHealthy(t, h)
		log := logs.String()
		if !strings.Contains(log, "ntfy connection failed") {
			t.Errorf("log = %s\nwant the failure's class", log)
		}
		// Every topic starts fs-, and the publish URL is the server's
		// address followed by the topic.
		for _, secret := range []string{"fs-", goneURL, strings.TrimPrefix(goneURL, "http://")} {
			if strings.Contains(log, secret) {
				t.Errorf("log carries %q:\n%s", secret, log)
			}
		}
	})
}

// TestDaemonWithoutTheSenderSendsNothing: a composition that does not ask for
// the sender never notifies. That keeps every other whole-daemon test from
// publishing to the default hosted server, and it is how the freesided
// command runs until #1946.
func TestDaemonWithoutTheSenderSendsNothing(t *testing.T) {
	sink := newNtfySink(t, http.StatusOK)
	root := t.TempDir()
	h, err := run(t.Context(), nil, config{
		Environment:   environmentEphemeral,
		DBPath:        filepath.Join(root, "freeside.db"),
		FakeDriverDir: filepath.Join(root, "driver"),
		ListenAddr:    "127.0.0.1:0", ReconcileInterval: 5 * time.Millisecond,
		SeedWalkingSkeleton: true, FakeDriverEnabled: true, NtfyURL: sink.URL,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	t.Cleanup(func() { _ = h.Close() })
	approval := "approval-" + domain.ItemID(defaultFakeRunID)
	waitForItem(t, h.attention, approval)
	pairDevice(t, h, "phone")

	time.Sleep(100 * time.Millisecond)
	if topics, _ := sink.published(); len(topics) != 0 {
		t.Errorf("published %d notifications with the sender off, want 0", len(topics))
	}
}

func assertDaemonHealthy(t *testing.T, h *daemon) {
	t.Helper()
	response, err := http.Get(h.readiness().APIURL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("GET /health = %d, want 200", response.StatusCode)
	}
	select {
	case err := <-h.errs:
		t.Errorf("daemon reported a component exit: %v", err)
	default:
	}
	// A durable stop leaves the listener up and h.errs empty; its item is
	// the only sign of it.
	if err := h.store.Read(context.Background(), func(tx *store.ReadTx) error {
		items, err := tx.ListOpenAttentionItems(context.Background(), domain.AttentionSystemHealth)
		for _, item := range items {
			if strings.HasPrefix(string(item.ID), durableStopItemPrefix) {
				t.Errorf("daemon recorded a durable stop: %+v", item)
			}
		}
		return err
	}); err != nil {
		t.Fatalf("list system health items: %v", err)
	}
}
