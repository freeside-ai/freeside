package signet_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// fakeNtfy records what the pipeline actually publishes and answers with a
// scriptable status, so tests can assert both the honest receipt timestamps
// and that only the generic hint ever reaches the provider.
type fakeNtfy struct {
	mu        sync.Mutex
	status    int
	onPublish func()
	requests  []publishRequest
}

type publishRequest struct {
	topic, title, click, priority, auth, body string
}

func (f *fakeNtfy) recorded(t *testing.T) []publishRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]publishRequest(nil), f.requests...)
}

// deliveryFixture is the §5.14 fixture plus a service composed with the ntfy
// channel pointed at the fake provider.
type deliveryFixture struct {
	fixture
	ntfy *fakeNtfy
}

func newDeliveryFixture(t *testing.T) deliveryFixture {
	t.Helper()
	f := newFixture(t)
	fake := &fakeNtfy{status: http.StatusOK}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("fake ntfy read body: %v", err)
		}
		fake.requests = append(fake.requests, publishRequest{
			topic:    strings.TrimPrefix(r.URL.Path, "/"),
			title:    r.Header.Get("Title"),
			click:    r.Header.Get("Click"),
			priority: r.Header.Get("Priority"),
			auth:     r.Header.Get("Authorization"),
			body:     string(body),
		})
		if fake.onPublish != nil {
			fake.onPublish()
		}
		w.WriteHeader(fake.status)
	}))
	t.Cleanup(server.Close)
	service := signet.NewService(f.store,
		signet.WithPairingKey(testPairingKey),
		signet.WithClock(func() time.Time { return *f.now }),
		signet.WithNtfy(signet.NtfyConfig{
			BaseURL:      server.URL,
			Client:       server.Client(),
			Token:        signet.Secret(secretValue),
			TopicKey:     testTopicKey,
			ClickBaseURL: "https://daemon.example/",
		}),
	)
	return deliveryFixture{
		fixture: fixture{service: service, store: f.store, item: f.item, device: f.device, now: f.now},
		ntfy:    fake,
	}
}

// seedSubmittedDelivery plants a submitted-only delivery row directly in the
// store, bypassing the pipeline: the item's persisted timing is deliberately
// left stale so tests can observe exactly which pipeline event recomputes it.
func seedSubmittedDelivery(t *testing.T, f fixture, deviceID domain.DeviceID, attempt int, at time.Time) domain.AttentionDelivery {
	t.Helper()
	row := domain.AttentionDelivery{
		ItemID: f.item.ID, DeviceID: deviceID, Channel: "ntfy", Attempt: attempt,
		SubmittedAt: at, Status: domain.DeliverySubmitted,
	}
	if err := f.store.Write(context.Background(), func(tx *store.WriteTx) error {
		return tx.PutAttentionDelivery(context.Background(), row)
	}); err != nil {
		t.Fatalf("seed delivery: %v", err)
	}
	return row
}

// TestRecordDeliveryOpenedAdvancesTiming: the opened receipt lands on the row
// and the item's persisted timing aggregates move in the same transaction —
// FirstOpenedAt and SubmitToFirstOpen appear, and the item's entity_version
// advances so cached clients invalidate.
func TestRecordDeliveryOpenedAdvancesTiming(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	submittedAt := *f.now
	seedSubmittedDelivery(t, f, f.device.ID, 1, submittedAt)
	_, before := f.itemSnapshot(t)

	openedAt := submittedAt.Add(3 * time.Minute)
	*f.now = openedAt
	row, err := f.service.RecordDeliveryOpened(ctx, f.item.ID, f.device.ID, "ntfy", 1)
	if err != nil {
		t.Fatalf("RecordDeliveryOpened: %v", err)
	}
	if row.Status != domain.DeliveryOpened {
		t.Errorf("row status = %q, want opened", row.Status)
	}
	if row.OpenedAt == nil || !row.OpenedAt.Equal(openedAt) {
		t.Errorf("row opened_at = %v, want %v", row.OpenedAt, openedAt)
	}
	if row.ChannelAcceptedAt != nil {
		t.Errorf("row channel_accepted_at = %v, want nil (never claimed)", row.ChannelAcceptedAt)
	}

	item, after := f.itemSnapshot(t)
	if item.Timing.DeliveryCount != 1 {
		t.Errorf("timing delivery_count = %d, want 1", item.Timing.DeliveryCount)
	}
	if item.Timing.FirstOpenedAt == nil || !item.Timing.FirstOpenedAt.Equal(openedAt) {
		t.Errorf("timing first_opened_at = %v, want %v", item.Timing.FirstOpenedAt, openedAt)
	}
	if item.Timing.SubmitToFirstOpen == nil || *item.Timing.SubmitToFirstOpen != 3*time.Minute {
		t.Errorf("timing submit_to_first_open = %v, want 3m", item.Timing.SubmitToFirstOpen)
	}
	if after.EntityVersion <= before.EntityVersion {
		t.Errorf("item entity_version %d → %d, want an advance", before.EntityVersion, after.EntityVersion)
	}
	if item.ItemVersion <= f.item.ItemVersion {
		t.Errorf("item_version %d → %d, want an advance", f.item.ItemVersion, item.ItemVersion)
	}
}

// TestRecordDeliveryOpenedReplayNoNewEffect: opening an already-opened attempt
// returns the recorded row and consumes no revision, mirroring the command
// path's idempotent-replay posture.
func TestRecordDeliveryOpenedReplayNoNewEffect(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	seedSubmittedDelivery(t, f, f.device.ID, 1, *f.now)
	openedAt := f.now.Add(time.Minute)
	*f.now = openedAt
	first, err := f.service.RecordDeliveryOpened(ctx, f.item.ID, f.device.ID, "ntfy", 1)
	if err != nil {
		t.Fatalf("RecordDeliveryOpened: %v", err)
	}
	before := f.revision(t)

	*f.now = openedAt.Add(time.Hour)
	replay, err := f.service.RecordDeliveryOpened(ctx, f.item.ID, f.device.ID, "ntfy", 1)
	if err != nil {
		t.Fatalf("RecordDeliveryOpened replay: %v", err)
	}
	if replay.OpenedAt == nil || !replay.OpenedAt.Equal(*first.OpenedAt) {
		t.Errorf("replay opened_at = %v, want the recorded %v", replay.OpenedAt, first.OpenedAt)
	}
	if after := f.revision(t); after != before {
		t.Errorf("replay moved the revision %d → %d", before, after)
	}
}

// TestRecordDeliveryOpenedGatesDevice: a revoked device produces no new
// server effect (the §5.14 test 15 posture applied to receipts), and an
// unknown delivery row is a loud not-found, never an implicit create.
func TestRecordDeliveryOpenedGatesDevice(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	seedSubmittedDelivery(t, f, f.device.ID, 1, *f.now)
	if _, err := f.service.Revoke(ctx, f.device.ID, f.device.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	before := f.revision(t)
	if _, err := f.service.RecordDeliveryOpened(ctx, f.item.ID, f.device.ID, "ntfy", 1); !errors.Is(err, signet.ErrDeviceNotActive) {
		t.Fatalf("RecordDeliveryOpened error = %v, want ErrDeviceNotActive", err)
	}
	if after := f.revision(t); after != before {
		t.Errorf("gated receipt moved the revision %d → %d", before, after)
	}

	f.seedDevice(t, "device-2")
	if _, err := f.service.RecordDeliveryOpened(ctx, f.item.ID, "device-2", "ntfy", 9); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("RecordDeliveryOpened(unknown attempt) error = %v, want ErrNotFound", err)
	}
}

// TestTimingReputSkippedWhenUnchanged: a receipt that moves no aggregate (a
// second device's later open, when the first open already set every first_*
// instant) writes the delivery row but leaves the item untouched — no
// entity_version churn, no staleness-invalidation of prepared commands.
func TestTimingReputSkippedWhenUnchanged(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	f.seedDevice(t, "device-2")
	submittedAt := *f.now
	seedSubmittedDelivery(t, f, f.device.ID, 1, submittedAt)
	seedSubmittedDelivery(t, f, "device-2", 1, submittedAt)

	*f.now = submittedAt.Add(time.Minute)
	if _, err := f.service.RecordDeliveryOpened(ctx, f.item.ID, f.device.ID, "ntfy", 1); err != nil {
		t.Fatalf("RecordDeliveryOpened(first): %v", err)
	}
	item, before := f.itemSnapshot(t)
	if item.Timing.DeliveryCount != 2 || item.Timing.FirstOpenedAt == nil {
		t.Fatalf("timing after first open = %+v, want count 2 with first_opened_at", item.Timing)
	}

	*f.now = submittedAt.Add(2 * time.Minute)
	if _, err := f.service.RecordDeliveryOpened(ctx, f.item.ID, "device-2", "ntfy", 1); err != nil {
		t.Fatalf("RecordDeliveryOpened(second): %v", err)
	}
	second, err := readDelivery(f, f.item.ID, "device-2", "ntfy", 1)
	if err != nil {
		t.Fatalf("GetAttentionDelivery: %v", err)
	}
	if second.Status != domain.DeliveryOpened {
		t.Errorf("second row status = %q, want opened", second.Status)
	}
	if _, after := f.itemSnapshot(t); after.EntityVersion != before.EntityVersion {
		t.Errorf("aggregate-neutral receipt churned entity_version %d → %d", before.EntityVersion, after.EntityVersion)
	}
}

// TestOpenToDecisionDerivableFromDeliveries is issue #69's acceptance 3: the
// §8 product metric, open-to-decision time, is computable from the delivery
// rows' opened_at and the decision that concluded the item — no dashboard,
// just the honest endpoints.
func TestOpenToDecisionDerivableFromDeliveries(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t)
	seedSubmittedDelivery(t, f, f.device.ID, 1, *f.now)

	openedAt := f.now.Add(2 * time.Minute)
	*f.now = openedAt
	if _, err := f.service.RecordDeliveryOpened(ctx, f.item.ID, f.device.ID, "ntfy", 1); err != nil {
		t.Fatalf("RecordDeliveryOpened: %v", err)
	}

	decidedAt := openedAt.Add(5 * time.Minute)
	*f.now = decidedAt
	item, snap := f.itemSnapshot(t)
	cmd := f.command("cmd-decide", domain.ActionStop)
	cmd.Payload.ItemVersion = item.ItemVersion
	cmd.ExpectedEntityVersion = snap.EntityVersion
	if _, err := f.service.Submit(ctx, cmd); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	rows, err := f.service.ListAttentionItemDeliveries(ctx, f.item.ID)
	if err != nil {
		t.Fatalf("ListAttentionItemDeliveries: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("deliveries = %d rows, want 1", len(rows))
	}
	got := rows[0].Delivery.OpenedAt
	if got == nil || !got.Equal(openedAt) {
		t.Fatalf("opened_at = %v, want %v", got, openedAt)
	}
	if openToDecision := decidedAt.Sub(*got); openToDecision != 5*time.Minute {
		t.Errorf("open-to-decision = %v, want 5m", openToDecision)
	}
}

// TestSubmitDeliveryRecordsHonestReceipts is issue #69's acceptance 2 for the
// happy path: the submitted and channel-accepted instants are recorded
// distinctly (the provider's acceptance populates channel_accepted_at only,
// never anything stronger), opened_at stays null, and the item's timing
// aggregates move with the row. The published payload is the generic
// read-only hint: topic derived from the device, deep link to canonical
// state, and no item subject or reason text.
func TestSubmitDeliveryRecordsHonestReceipts(t *testing.T) {
	ctx := context.Background()
	f := newDeliveryFixture(t)
	submittedAt := *f.now
	acceptedAt := submittedAt.Add(30 * time.Second)
	f.ntfy.onPublish = func() { *f.now = acceptedAt }

	row, err := f.service.SubmitDelivery(ctx, f.item.ID, f.device.ID)
	if err != nil {
		t.Fatalf("SubmitDelivery: %v", err)
	}
	if row.Status != domain.DeliveryChannelAccepted {
		t.Errorf("row status = %q, want channel_accepted", row.Status)
	}
	if row.Channel != "ntfy" || row.Attempt != 1 {
		t.Errorf("row channel/attempt = %s/%d, want ntfy/1", row.Channel, row.Attempt)
	}
	if !row.SubmittedAt.Equal(submittedAt) {
		t.Errorf("submitted_at = %v, want %v", row.SubmittedAt, submittedAt)
	}
	if row.ChannelAcceptedAt == nil || !row.ChannelAcceptedAt.Equal(acceptedAt) {
		t.Errorf("channel_accepted_at = %v, want %v", row.ChannelAcceptedAt, acceptedAt)
	}
	if row.OpenedAt != nil {
		t.Errorf("opened_at = %v, want nil: acceptance is never an open", row.OpenedAt)
	}

	item, _ := f.itemSnapshot(t)
	if item.Timing.DeliveryCount != 1 {
		t.Errorf("timing delivery_count = %d, want 1", item.Timing.DeliveryCount)
	}
	if item.Timing.FirstSubmittedAt == nil || !item.Timing.FirstSubmittedAt.Equal(submittedAt) {
		t.Errorf("timing first_submitted_at = %v, want %v", item.Timing.FirstSubmittedAt, submittedAt)
	}
	if item.Timing.FirstAcceptedAt == nil || !item.Timing.FirstAcceptedAt.Equal(acceptedAt) {
		t.Errorf("timing first_accepted_at = %v, want %v", item.Timing.FirstAcceptedAt, acceptedAt)
	}
	if item.Timing.FirstOpenedAt != nil {
		t.Errorf("timing first_opened_at = %v, want nil", item.Timing.FirstOpenedAt)
	}

	requests := f.ntfy.recorded(t)
	if len(requests) != 1 {
		t.Fatalf("published %d notifications, want 1", len(requests))
	}
	got := requests[0]
	if !strings.HasPrefix(got.topic, "fs-") || len(got.topic) != len("fs-")+32 {
		t.Errorf("topic = %q, want fs- prefix and 32 hex chars", got.topic)
	}
	if got.title != "Attention needed" {
		t.Errorf("title = %q, want the generic hint", got.title)
	}
	if got.click != "https://daemon.example/attention/items/"+string(f.item.ID) {
		t.Errorf("click = %q, want the canonical deep link", got.click)
	}
	if got.priority != "default" {
		t.Errorf("priority = %q, want default for a normal item", got.priority)
	}
	if got.auth != "Bearer "+secretValue {
		t.Errorf("authorization = %q, want the bearer token", got.auth)
	}
	for _, leak := range []string{f.item.Reason, string(f.item.Subject.ID), f.item.PRHeadSHA} {
		if strings.Contains(got.title+got.body+got.click, leak) {
			t.Errorf("published hint leaks item content %q", leak)
		}
	}
}

// TestSubmitDeliveryTopicIsDeterministicPerDevice: one device always maps to
// one topic (the client must be able to subscribe once), and two devices
// never share one (a topic is a capability URL).
func TestSubmitDeliveryTopicIsDeterministicPerDevice(t *testing.T) {
	ctx := context.Background()
	f := newDeliveryFixture(t)
	f.seedDevice(t, "device-2")
	for _, device := range []domain.DeviceID{f.device.ID, f.device.ID, "device-2"} {
		if _, err := f.service.SubmitDelivery(ctx, f.item.ID, device); err != nil {
			t.Fatalf("SubmitDelivery(%s): %v", device, err)
		}
	}
	requests := f.ntfy.recorded(t)
	if len(requests) != 3 {
		t.Fatalf("published %d notifications, want 3", len(requests))
	}
	if requests[0].topic != requests[1].topic {
		t.Errorf("one device produced two topics: %q, %q", requests[0].topic, requests[1].topic)
	}
	if requests[0].topic == requests[2].topic {
		t.Errorf("two devices share topic %q", requests[0].topic)
	}
}

// TestPairingTopicMatchesDeliveryTopic is issue #131's round trip: the topic
// handed to a newly paired device is exactly the topic the delivery pipeline
// later publishes to for that device, not a duplicated client-facing
// derivation that can drift.
func TestPairingTopicMatchesDeliveryTopic(t *testing.T) {
	ctx := context.Background()
	f := newDeliveryFixture(t)
	code, _, err := f.service.MintPairingCode(ctx)
	if err != nil {
		t.Fatalf("MintPairingCode: %v", err)
	}
	grant, err := f.service.Pair(ctx, code, "Notification subscriber")
	if err != nil {
		t.Fatalf("Pair: %v", err)
	}
	if _, err := f.service.SubmitDelivery(ctx, f.item.ID, grant.Device.Device.ID); err != nil {
		t.Fatalf("SubmitDelivery: %v", err)
	}
	requests := f.ntfy.recorded(t)
	if len(requests) != 1 {
		t.Fatalf("published %d notifications, want 1", len(requests))
	}
	if got, want := requests[0].topic, grant.NtfySubscription.Topic; got != want {
		t.Errorf("published topic = %q, pairing grant topic = %q", got, want)
	}
}

// TestSubmitDeliverySurvivesCallerCancellation: once the submitted row is
// durable, the provider call and the acceptance record run under a
// daemon-owned context, so a caller abandoning its request mid-publish (an
// HTTP client dropping the control route's connection) cannot strand the
// committed attempt half-advanced or lose the real acceptance instant.
func TestSubmitDeliverySurvivesCallerCancellation(t *testing.T) {
	f := newDeliveryFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	f.ntfy.onPublish = cancel

	row, err := f.service.SubmitDelivery(ctx, f.item.ID, f.device.ID)
	if err != nil {
		t.Fatalf("SubmitDelivery under canceled caller: %v", err)
	}
	if row.Status != domain.DeliveryChannelAccepted || row.ChannelAcceptedAt == nil {
		t.Errorf("row = status %q accepted_at %v, want channel_accepted with a receipt",
			row.Status, row.ChannelAcceptedAt)
	}
	stored, err := readDelivery(f.fixture, f.item.ID, f.device.ID, "ntfy", 1)
	if err != nil {
		t.Fatalf("GetAttentionDelivery: %v", err)
	}
	if stored.Status != domain.DeliveryChannelAccepted {
		t.Errorf("stored row status = %q, want the acceptance recorded despite the canceled caller", stored.Status)
	}
}

// TestSubmitDeliveryChannelFailureStaysSubmitted: a provider rejection leaves
// the honest submitted-only row (only submitted_at is claimed, which is
// true), surfaces a typed error carrying the status and never the response
// body, and the retry is the next attempt number.
func TestSubmitDeliveryChannelFailureStaysSubmitted(t *testing.T) {
	ctx := context.Background()
	f := newDeliveryFixture(t)
	f.ntfy.status = http.StatusInternalServerError

	row, err := f.service.SubmitDelivery(ctx, f.item.ID, f.device.ID)
	if !errors.Is(err, signet.ErrChannelRejected) {
		t.Fatalf("SubmitDelivery error = %v, want ErrChannelRejected", err)
	}
	var rejection *signet.ChannelRejectionError
	if !errors.As(err, &rejection) || rejection.Status != http.StatusInternalServerError {
		t.Errorf("rejection = %+v, want status 500", rejection)
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Errorf("error leaks the channel token: %v", err)
	}
	if row.Status != domain.DeliverySubmitted {
		t.Errorf("returned row status = %q, want submitted", row.Status)
	}
	stored, err := readDelivery(f.fixture, f.item.ID, f.device.ID, "ntfy", 1)
	if err != nil {
		t.Fatalf("GetAttentionDelivery: %v", err)
	}
	if stored.Status != domain.DeliverySubmitted || stored.ChannelAcceptedAt != nil {
		t.Errorf("stored row = %+v, want submitted with no acceptance", stored)
	}
	item, _ := f.itemSnapshot(t)
	if item.Timing.DeliveryCount != 1 {
		t.Errorf("timing delivery_count = %d, want the failed attempt counted", item.Timing.DeliveryCount)
	}

	f.ntfy.status = http.StatusOK
	retry, err := f.service.SubmitDelivery(ctx, f.item.ID, f.device.ID)
	if err != nil {
		t.Fatalf("SubmitDelivery retry: %v", err)
	}
	if retry.Attempt != 2 || retry.Status != domain.DeliveryChannelAccepted {
		t.Errorf("retry = attempt %d status %q, want attempt 2 accepted", retry.Attempt, retry.Status)
	}
	if item, _ := f.itemSnapshot(t); item.Timing.DeliveryCount != 2 {
		t.Errorf("timing delivery_count = %d, want 2 after the retry", item.Timing.DeliveryCount)
	}
}

// TestSubmitDeliveryFailsClosed: no channel configured, a revoked device, and
// a concluded item are each refused before any effect — no delivery row, no
// revision movement, and nothing published.
func TestSubmitDeliveryFailsClosed(t *testing.T) {
	ctx := context.Background()

	t.Run("unconfigured channel", func(t *testing.T) {
		f := newFixture(t)
		service := signet.NewService(f.store, signet.WithClock(func() time.Time { return *f.now }))
		before := f.revision(t)
		if _, err := service.SubmitDelivery(ctx, f.item.ID, f.device.ID); !errors.Is(err, signet.ErrNotifierUnavailable) {
			t.Fatalf("SubmitDelivery error = %v, want ErrNotifierUnavailable", err)
		}
		if after := f.revision(t); after != before {
			t.Errorf("refusal moved the revision %d → %d", before, after)
		}
	})

	t.Run("misconfigured channel", func(t *testing.T) {
		for name, cfg := range map[string]signet.NtfyConfig{
			"malformed base URL": {
				BaseURL: "not a url", TopicKey: testTopicKey, ClickBaseURL: "https://daemon.example",
			},
			"relative base URL": {
				BaseURL: "ntfy.example/path", TopicKey: testTopicKey, ClickBaseURL: "https://daemon.example",
			},
			"userinfo in base URL": {
				BaseURL: "https://publisher-value@ntfy.example", TopicKey: testTopicKey,
				ClickBaseURL: "https://daemon.example",
			},
			"cleartext non-loopback": {
				BaseURL: "http://ntfy.internal", TopicKey: testTopicKey, ClickBaseURL: "https://daemon.example",
			},
			"query in base URL": {
				BaseURL: "https://ntfy.example/base?route=shared", TopicKey: testTopicKey,
				ClickBaseURL: "https://daemon.example",
			},
			"fragment in base URL": {
				BaseURL: "https://ntfy.example/base#shared", TopicKey: testTopicKey,
				ClickBaseURL: "https://daemon.example",
			},
			"out-of-range base port": {
				BaseURL: "https://ntfy.example:99999", TopicKey: testTopicKey,
				ClickBaseURL: "https://daemon.example",
			},
			"zero click port": {
				BaseURL: "https://ntfy.example", TopicKey: testTopicKey,
				ClickBaseURL: "https://daemon.example:0",
			},
			"weak topic key": {
				BaseURL: "https://ntfy.example", TopicKey: []byte("weak"),
				ClickBaseURL: "https://daemon.example",
			},
		} {
			t.Run(name, func(t *testing.T) {
				f := newFixture(t)
				service := signet.NewService(f.store,
					signet.WithClock(func() time.Time { return *f.now }), signet.WithNtfy(cfg))
				before := f.revision(t)
				_, err := service.SubmitDelivery(ctx, f.item.ID, f.device.ID)
				if !errors.Is(err, signet.ErrNotifierUnavailable) {
					t.Fatalf("SubmitDelivery error = %v, want ErrNotifierUnavailable", err)
				}
				if strings.Contains(err.Error(), secretValue) {
					t.Errorf("refusal leaks the channel token: %v", err)
				}
				if after := f.revision(t); after != before {
					t.Errorf("refusal moved the revision %d → %d", before, after)
				}
			})
		}
	})

	t.Run("revoked device", func(t *testing.T) {
		f := newDeliveryFixture(t)
		if _, err := f.service.Revoke(ctx, f.device.ID, f.device.ID); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		before := f.revision(t)
		if _, err := f.service.SubmitDelivery(ctx, f.item.ID, f.device.ID); !errors.Is(err, signet.ErrDeviceNotActive) {
			t.Fatalf("SubmitDelivery error = %v, want ErrDeviceNotActive", err)
		}
		if after := f.revision(t); after != before {
			t.Errorf("refusal moved the revision %d → %d", before, after)
		}
		if requests := f.ntfy.recorded(t); len(requests) != 0 {
			t.Errorf("refused submission still published %d notifications", len(requests))
		}
	})

	t.Run("concluded item", func(t *testing.T) {
		f := newDeliveryFixture(t)
		if _, err := f.service.Submit(ctx, f.command("cmd-conclude", domain.ActionStop)); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		before := f.revision(t)
		if _, err := f.service.SubmitDelivery(ctx, f.item.ID, f.device.ID); !errors.Is(err, signet.ErrItemNotOpenForDelivery) {
			t.Fatalf("SubmitDelivery error = %v, want ErrItemNotOpenForDelivery", err)
		}
		if after := f.revision(t); after != before {
			t.Errorf("refusal moved the revision %d → %d", before, after)
		}
		if requests := f.ntfy.recorded(t); len(requests) != 0 {
			t.Errorf("refused submission still published %d notifications", len(requests))
		}
	})
}

func readDelivery(f fixture, itemID domain.ItemID, deviceID domain.DeviceID, channel string, attempt int) (domain.AttentionDelivery, error) {
	var row domain.AttentionDelivery
	err := f.store.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		row, err = tx.GetAttentionDelivery(context.Background(), itemID, deviceID, channel, attempt)
		return err
	})
	return row, err
}
