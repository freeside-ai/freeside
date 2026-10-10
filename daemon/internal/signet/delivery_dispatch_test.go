package signet_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// deliveries returns every recorded delivery row, in the store's order.
func (f fixture) deliveries(t *testing.T) []domain.AttentionDelivery {
	t.Helper()
	var rows []domain.AttentionDelivery
	if err := f.store.Read(context.Background(), func(tx *store.ReadTx) error {
		listed, err := tx.ListAttentionDeliveries(context.Background())
		for _, row := range listed {
			rows = append(rows, row.Value)
		}
		return err
	}); err != nil {
		t.Fatalf("ListAttentionDeliveries: %v", err)
	}
	return rows
}

// pass runs one sender pass and returns what it reported.
func (f deliveryFixture) pass(t *testing.T, service *signet.Service) []error {
	t.Helper()
	var reported []error
	if err := service.DeliverOwed(context.Background(), func(err error) { reported = append(reported, err) }); err != nil {
		t.Fatalf("DeliverOwed: %v", err)
	}
	return reported
}

// TestDeliverOwedNotifiesEachDeviceOnce: an open item and an active device
// produce exactly one accepted notification. A second pass sends nothing, and
// neither does a daemon restarted over the same store, because the delivery
// rows are the sender's only memory.
func TestDeliverOwedNotifiesEachDeviceOnce(t *testing.T) {
	f := newDeliveryFixture(t)

	if reported := f.pass(t, f.service); len(reported) != 0 {
		t.Fatalf("first pass reported %v, want nothing", reported)
	}
	rows := f.deliveries(t)
	if len(rows) != 1 || rows[0].Status != domain.DeliveryChannelAccepted || rows[0].ChannelAcceptedAt == nil ||
		rows[0].ItemID != f.item.ID || rows[0].DeviceID != f.device.ID || rows[0].Attempt != 1 {
		t.Fatalf("delivery rows = %+v, want one channel_accepted first attempt", rows)
	}

	f.pass(t, f.service)
	restarted := signet.NewService(f.store,
		signet.WithClock(func() time.Time { return *f.now }),
		signet.WithNtfy(signet.NtfyConfig{BaseURL: f.server.URL, Client: f.server.Client(), TopicKey: testTopicKey}))
	*f.now = (*f.now).Add(48 * time.Hour)
	f.pass(t, restarted)

	if published := f.ntfy.recorded(t); len(published) != 1 {
		t.Errorf("published %d notifications across three passes and a restart, want 1", len(published))
	}
	if rows := f.deliveries(t); len(rows) != 1 {
		t.Errorf("delivery rows = %+v, want the one accepted attempt and no other", rows)
	}
}

// TestDeliverOwedPublishesTheGenericHint pins what a notification says and
// where its tap leads: the generic title, the item's type and nothing of its
// content, and the freeside:// link the iPhone app parses.
func TestDeliverOwedPublishesTheGenericHint(t *testing.T) {
	f := newDeliveryFixture(t)
	f.pass(t, f.service)

	published := f.ntfy.recorded(t)
	if len(published) != 1 {
		t.Fatalf("published %d notifications, want 1", len(published))
	}
	got := published[0]
	if got.title != "Attention needed" {
		t.Errorf("title = %q, want %q", got.title, "Attention needed")
	}
	if got.body != "ready for final review" {
		t.Errorf("body = %q, want the item's type in words", got.body)
	}
	if want := "freeside://attention/items/item-1?channel=ntfy&attempt=1"; got.click != want {
		t.Errorf("tap link = %q, want %q", got.click, want)
	}
}

// TestDeliverOwedSkipsWhatIsNotOwed: no notification goes out for an item
// that is no longer open, to a revoked device, or for an item created before
// the device paired, and none of those is reported as a failure.
func TestDeliverOwedSkipsWhatIsNotOwed(t *testing.T) {
	ctx := context.Background()

	t.Run("resolved item", func(t *testing.T) {
		f := newDeliveryFixture(t)
		if _, err := f.service.Submit(ctx, f.command("cmd-resolve", domain.ActionStop)); err != nil {
			t.Fatalf("Submit(stop): %v", err)
		}
		if reported := f.pass(t, f.service); len(reported) != 0 {
			t.Errorf("reported %v, want nothing", reported)
		}
		if published := f.ntfy.recorded(t); len(published) != 0 {
			t.Errorf("published %d notifications for a resolved item, want 0", len(published))
		}
	})

	t.Run("revoked device", func(t *testing.T) {
		f := newDeliveryFixture(t)
		f.seedDevice(t, "device-2")
		if _, err := f.service.Revoke(ctx, "device-2", f.device.ID); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		if reported := f.pass(t, f.service); len(reported) != 0 {
			t.Errorf("reported %v, want nothing", reported)
		}
		rows := f.deliveries(t)
		if len(rows) != 1 || rows[0].DeviceID != "device-2" {
			t.Errorf("delivery rows = %+v, want one, to the device still active", rows)
		}
	})

	t.Run("item created before the device paired", func(t *testing.T) {
		f := newDeliveryFixture(t)
		// The fixture item carries no creation time; this one does, and a
		// device that pairs later was never asked about it.
		createdAt := *f.now
		runID := domain.RunID("run-2")
		dated, err := domain.NewAttentionItem(domain.AttentionItemInput{
			ID: "item-dated", ProjectID: "proj-1",
			Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-2", RunID: &runID},
			Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
			Reason:            "checks are green and the diff is ready",
			RequestedDecision: []domain.Action{domain.ActionOpenPR, domain.ActionStop, domain.ActionDismiss},
			PRHeadSHA:         "cafebabe",
			PRReference:       &domain.PRReference{Repo: "owner/repo", Number: 124},
			ItemVersion:       1,
			InterruptionClass: domain.InterruptionPlannedGate,
			CreatedAt:         &createdAt, Status: domain.StatusOpen,
		}, nil)
		if err != nil {
			t.Fatalf("NewAttentionItem: %v", err)
		}
		if err := f.store.Write(ctx, func(tx *store.WriteTx) error { return storetest.BindSubject(ctx, tx, &dated) }); err != nil {
			t.Fatal(err)
		}
		if err := f.service.PutItem(ctx, dated); err != nil {
			t.Fatalf("PutItem: %v", err)
		}
		late := domain.Device{
			ID: "device-late", DisplayName: "Paired later",
			Status: domain.DeviceActive, PairedAt: createdAt.Add(time.Hour),
		}
		if err := f.store.Write(ctx, func(tx *store.WriteTx) error { return tx.PutDevice(ctx, late) }); err != nil {
			t.Fatalf("seed device: %v", err)
		}
		*f.now = createdAt.Add(2 * time.Hour)

		if reported := f.pass(t, f.service); len(reported) != 0 {
			t.Errorf("reported %v, want nothing", reported)
		}
		for _, row := range f.deliveries(t) {
			if row.ItemID == dated.ID && row.DeviceID == late.ID {
				t.Errorf("row %+v notifies a device of an item older than its pairing", row)
			}
		}
		// The device that was already paired is still told.
		var told bool
		for _, row := range f.deliveries(t) {
			told = told || (row.ItemID == dated.ID && row.DeviceID == f.device.ID)
		}
		if !told {
			t.Errorf("delivery rows = %+v, want the earlier-paired device notified of the dated item", f.deliveries(t))
		}
	})
}

// TestDeliverOwedSkipsASnoozedProposal: a proposal the operator snoozed asks
// for nothing until the snooze ends, so it sends no notification and records
// no attempt meanwhile.
func TestDeliverOwedSkipsASnoozedProposal(t *testing.T) {
	ctx := context.Background()
	f := newProposalDecisionFixture(t)
	sink := newDeliveryFixture(t)
	service := signet.NewService(f.store,
		signet.WithClock(func() time.Time { return *f.now }),
		signet.WithNtfy(signet.NtfyConfig{BaseURL: sink.server.URL, Client: sink.server.Client(), TopicKey: testTopicKey}))

	snooze := f.proposalCommand("command-snooze", domain.ActionSnooze)
	until := (*f.now).Add(time.Hour)
	snooze.Payload.SnoozeUntil = &until
	if _, err := f.service.Submit(ctx, snooze); err != nil {
		t.Fatalf("Submit(snooze): %v", err)
	}

	var reported []error
	if err := service.DeliverOwed(ctx, func(err error) { reported = append(reported, err) }); err != nil {
		t.Fatalf("DeliverOwed: %v", err)
	}
	if len(reported) != 0 {
		t.Errorf("reported %v, want nothing", reported)
	}
	for _, row := range f.deliveries(t) {
		if row.ItemID == f.item.ID {
			t.Errorf("row %+v notifies of a snoozed proposal", row)
		}
	}

	// Once the snooze ends the proposal is owed like any other open item.
	*f.now = until.Add(time.Minute)
	if err := service.DeliverOwed(ctx, func(err error) { reported = append(reported, err) }); err != nil {
		t.Fatalf("DeliverOwed after the snooze: %v", err)
	}
	var notified bool
	for _, row := range f.deliveries(t) {
		notified = notified || (row.ItemID == f.item.ID && row.Status == domain.DeliveryChannelAccepted)
	}
	if !notified || len(reported) != 0 {
		t.Errorf("after the snooze: notified = %v, reported = %v, want the proposal notified", notified, reported)
	}
}

// TestDeliverOwedRetriesAFailedSend: a rejected send leaves a submitted row
// with no acceptance, is reported with the provider's status, and is retried
// as the next attempt number after 1, 2, 4, 8, then 16 minutes. The sixth
// failure is the last: nothing more is sent, even once the provider recovers.
func TestDeliverOwedRetriesAFailedSend(t *testing.T) {
	f := newDeliveryFixture(t)
	f.ntfy.status = http.StatusServiceUnavailable

	waits := []time.Duration{0, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute}
	for i, wait := range waits {
		attempt := i + 1
		if wait > 0 {
			// One second short of the wait, nothing is sent.
			*f.now = (*f.now).Add(wait - time.Second)
			if reported := f.pass(t, f.service); len(reported) != 0 || len(f.deliveries(t)) != attempt-1 {
				t.Fatalf("before attempt %d was due: reported %v, rows %d", attempt, reported, len(f.deliveries(t)))
			}
			*f.now = (*f.now).Add(time.Second)
		}
		reported := f.pass(t, f.service)
		if len(reported) != 1 {
			t.Fatalf("attempt %d reported %v, want one failure", attempt, reported)
		}
		var failure *signet.DeliveryFailure
		if !errors.As(reported[0], &failure) {
			t.Fatalf("attempt %d reported %T, want *DeliveryFailure", attempt, reported[0])
		}
		if failure.ItemID != f.item.ID || failure.DeviceID != f.device.ID || failure.Attempt != attempt ||
			failure.Final != (attempt == len(waits)) {
			t.Errorf("failure = %+v, want attempt %d of item and device, final only on the last", failure, attempt)
		}
		var rejection *signet.ChannelRejectionError
		if !errors.As(reported[0], &rejection) || rejection.Status != http.StatusServiceUnavailable {
			t.Errorf("attempt %d failure = %v, want the provider's 503", attempt, reported[0])
		}
		rows := f.deliveries(t)
		if len(rows) != attempt {
			t.Fatalf("after attempt %d: %d rows, want %d", attempt, len(rows), attempt)
		}
		if last := rows[len(rows)-1]; last.Attempt != attempt || last.Status != domain.DeliverySubmitted ||
			last.ChannelAcceptedAt != nil {
			t.Errorf("attempt %d row = %+v, want submitted with no acceptance", attempt, last)
		}
	}

	f.ntfy.status = http.StatusOK
	*f.now = (*f.now).Add(72 * time.Hour)
	if reported := f.pass(t, f.service); len(reported) != 0 {
		t.Errorf("after the last attempt reported %v, want nothing", reported)
	}
	if rows := f.deliveries(t); len(rows) != len(waits) {
		t.Errorf("%d rows after the attempts ran out, want %d", len(rows), len(waits))
	}
}

// TestDeliverOwedStopsRetryingOnceAccepted: a retry the provider accepts ends
// the attempts for that item and device.
func TestDeliverOwedStopsRetryingOnceAccepted(t *testing.T) {
	f := newDeliveryFixture(t)
	f.ntfy.status = http.StatusBadGateway
	if reported := f.pass(t, f.service); len(reported) != 1 {
		t.Fatalf("first pass reported %v, want one failure", reported)
	}

	f.ntfy.status = http.StatusOK
	*f.now = (*f.now).Add(time.Minute)
	if reported := f.pass(t, f.service); len(reported) != 0 {
		t.Fatalf("retry reported %v, want nothing", reported)
	}
	*f.now = (*f.now).Add(time.Hour)
	f.pass(t, f.service)

	rows := f.deliveries(t)
	if len(rows) != 2 || rows[0].Status != domain.DeliverySubmitted || rows[1].Status != domain.DeliveryChannelAccepted ||
		rows[1].Attempt != 2 {
		t.Errorf("delivery rows = %+v, want a failed first attempt and an accepted second", rows)
	}
}

// TestDeliverOwedOneFailureDoesNotHoldBackAnother: a send that fails for one
// device is reported and the pass goes on, so the next device is still told.
func TestDeliverOwedOneFailureDoesNotHoldBackAnother(t *testing.T) {
	f := newDeliveryFixture(t)
	f.seedDevice(t, "device-2")
	f.ntfy.status = http.StatusInternalServerError
	// onPublish runs under the fake's lock, before it answers: the first
	// request is rejected and every later one accepted.
	f.ntfy.onPublish = func() {
		if len(f.ntfy.requests) > 1 {
			f.ntfy.status = http.StatusOK
		}
	}

	reported := f.pass(t, f.service)
	if len(reported) != 1 {
		t.Fatalf("reported %v, want the one failed send", reported)
	}
	accepted, failed := 0, 0
	for _, row := range f.deliveries(t) {
		switch row.Status {
		case domain.DeliveryChannelAccepted:
			accepted++
		case domain.DeliverySubmitted:
			failed++
		case domain.DeliveryOpened:
		}
	}
	if accepted != 1 || failed != 1 {
		t.Errorf("accepted %d and failed %d, want one device told and one failure", accepted, failed)
	}
}

// TestDeliverOwedEndsWithItsContext: a pass cut short by shutdown returns
// the context's error and reports no failed send. The clock ends the context
// on its second read, which is SubmitDelivery's, after the pass has decided
// the notification is owed.
func TestDeliverOwedEndsWithItsContext(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reads := 0
	service := signet.NewService(f.store,
		signet.WithClock(func() time.Time {
			if reads++; reads == 2 {
				cancel()
			}
			return *f.now
		}),
		signet.WithNtfy(signet.NtfyConfig{BaseURL: f.server.URL, Client: f.server.Client(), TopicKey: testTopicKey}))

	var reported []error
	err := service.DeliverOwed(ctx, func(err error) { reported = append(reported, err) })
	if !errors.Is(err, context.Canceled) {
		t.Errorf("DeliverOwed = %v, want context.Canceled", err)
	}
	if len(reported) != 0 {
		t.Errorf("reported %v, want nothing: shutdown is not a failed send", reported)
	}
	if rows := f.deliveries(t); len(rows) != 0 {
		t.Errorf("delivery rows = %+v, want none", rows)
	}
}

// TestDeliverOwedReportsNoTopic: what the sender reports is what the daemon
// logs, so a provider that cannot be reached must be reported without the
// device's topic or the publish URL (#1924).
func TestDeliverOwedReportsNoTopic(t *testing.T) {
	f := newDeliveryFixture(t)
	// A rejected first send shows the fake the device's topic, and leaves the
	// pair owed a retry.
	f.ntfy.status = http.StatusInternalServerError
	rejected := f.pass(t, f.service)
	topic := f.ntfy.recorded(t)[0].topic
	serverURL := f.server.URL
	f.server.Close()
	*f.now = (*f.now).Add(time.Minute)

	unreachable := f.pass(t, f.service)
	if len(rejected) != 1 || len(unreachable) != 1 || !errors.Is(unreachable[0], signet.ErrChannelUnreachable) {
		t.Fatalf("reported %v then %v, want a rejection then an unreachable provider", rejected, unreachable)
	}
	for _, reported := range []error{rejected[0], unreachable[0]} {
		for _, secret := range []string{topic, serverURL, strings.TrimPrefix(serverURL, "http://")} {
			if strings.Contains(reported.Error(), secret) {
				t.Errorf("reported error %q carries %q", reported, secret)
			}
		}
	}
}

// TestRunDeliveriesWithoutAChannel: a composition with no usable channel has
// nothing to send, and says so in a way the caller can tell from a failure.
func TestRunDeliveriesWithoutAChannel(t *testing.T) {
	f := newFixture(t)
	for name, service := range map[string]*signet.Service{
		"no channel":            signet.NewService(f.store),
		"misconfigured channel": signet.NewService(f.store, signet.WithNtfy(signet.NtfyConfig{BaseURL: "https://ntfy.example"})),
	} {
		t.Run(name, func(t *testing.T) {
			err := service.RunDeliveries(context.Background(), time.Second, func(err error) {
				t.Errorf("reported %v, want nothing", err)
			})
			if !errors.Is(err, signet.ErrNotifierUnavailable) {
				t.Errorf("RunDeliveries = %v, want ErrNotifierUnavailable", err)
			}
		})
	}
}

// TestRunDeliveriesTickerCadence pins RunDeliveries's real time.NewTicker
// cadence: one pass at once, one more per interval, and a nil return when ctx
// ends. The store holds nothing owed, so a pass reads the clock exactly once
// and publishes nothing; the bubble's fake clock makes the ticker
// deterministic (the timer-dependent-tests convention, daemon/README.md).
func TestRunDeliveriesTickerCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := storetest.Open(t, t.TempDir()+"/signet.db", store.Options{})
		t.Cleanup(func() {
			if err := st.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		})
		var passes atomic.Int64
		service := signet.NewService(st,
			signet.WithClock(func() time.Time {
				passes.Add(1)
				return time.Now().UTC()
			}),
			signet.WithNtfy(signet.NtfyConfig{BaseURL: "https://ntfy.example", TopicKey: testTopicKey}))

		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			done <- service.RunDeliveries(ctx, 5*time.Second, func(err error) {
				t.Errorf("reported %v, want nothing", err)
			})
		}()

		time.Sleep(3*5*time.Second + time.Second)
		synctest.Wait()
		if got := passes.Load(); got != 4 {
			t.Fatalf("passes after the immediate one and 3 ticks = %d, want 4", got)
		}

		cancel()
		if err := <-done; err != nil {
			t.Errorf("RunDeliveries after cancel = %v, want nil", err)
		}
	})
}
