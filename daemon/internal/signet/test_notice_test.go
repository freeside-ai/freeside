package signet_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestSendTestNoticePublishesToTheDevicesTopicAndRecordsNothing: the test
// notice reaches the same topic a real notification would, links the app's
// inbox, and leaves no trace in the store. In particular it records no
// delivery row, so it moves no item's timing or version.
func TestSendTestNoticePublishesToTheDevicesTopicAndRecordsNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newDeliveryFixture(t)
	revision := f.revision(t)

	if err := f.service.SendTestNotice(ctx, f.device.ID); err != nil {
		t.Fatalf("SendTestNotice: %v", err)
	}

	requests := f.ntfy.recorded(t)
	if len(requests) != 1 {
		t.Fatalf("published %d notifications, want 1", len(requests))
	}
	notice := requests[0]
	if notice.title != "Freeside test notification" || notice.body != "Tap to open Freeside." ||
		notice.click != "freeside://inbox" || notice.priority != "default" {
		t.Errorf("test notice = %+v", notice)
	}
	if rows := f.deliveries(t); len(rows) != 0 {
		t.Errorf("delivery rows = %+v, want none", rows)
	}
	if got := f.revision(t); got != revision {
		t.Errorf("store revision moved from %d to %d, want no write", revision, got)
	}

	// The topic is the one an item's notification goes to.
	if _, err := f.service.SubmitDelivery(ctx, f.item.ID, f.device.ID); err != nil {
		t.Fatalf("SubmitDelivery: %v", err)
	}
	if real := f.ntfy.recorded(t)[1]; real.topic != notice.topic {
		t.Errorf("test notice topic differs from the item notification's")
	}
}

// TestSendTestNoticeRefusals: an unknown or revoked device, and a service with
// no channel, publish nothing.
func TestSendTestNoticeRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("unknown device", func(t *testing.T) {
		f := newDeliveryFixture(t)
		if err := f.service.SendTestNotice(ctx, "no-such-device"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("SendTestNotice error = %v, want ErrNotFound", err)
		}
		if n := len(f.ntfy.recorded(t)); n != 0 {
			t.Errorf("published %d notifications, want 0", n)
		}
	})

	t.Run("revoked device", func(t *testing.T) {
		f := newDeliveryFixture(t)
		f.seedDevice(t, "device-2")
		if _, err := f.service.Revoke(ctx, "device-2", f.device.ID); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		if err := f.service.SendTestNotice(ctx, f.device.ID); !errors.Is(err, signet.ErrDeviceNotActive) {
			t.Errorf("SendTestNotice error = %v, want ErrDeviceNotActive", err)
		}
		if n := len(f.ntfy.recorded(t)); n != 0 {
			t.Errorf("published %d notifications, want 0", n)
		}
	})

	t.Run("no channel", func(t *testing.T) {
		f := newFixture(t)
		for name, service := range map[string]*signet.Service{
			"absent":        signet.NewService(f.store),
			"misconfigured": signet.NewService(f.store, signet.WithNtfy(signet.NtfyConfig{BaseURL: "https://ntfy.example"})),
		} {
			if err := service.SendTestNotice(ctx, f.device.ID); !errors.Is(err, signet.ErrNotifierUnavailable) {
				t.Errorf("%s channel: SendTestNotice error = %v, want ErrNotifierUnavailable", name, err)
			}
		}
	})
}

// TestSendTestNoticeFailuresNeverNameTheTopic: what the command prints for a
// failed test is this error, so it carries a status or a failure class and
// never the topic, the publish URL, or the publisher token.
func TestSendTestNoticeFailuresNeverNameTheTopic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newDeliveryFixture(t)
	if err := f.service.SendTestNotice(ctx, f.device.ID); err != nil {
		t.Fatalf("SendTestNotice: %v", err)
	}
	topic := f.ntfy.recorded(t)[0].topic
	serverURL := f.server.URL
	assertSilent := func(t *testing.T, err error) {
		t.Helper()
		for _, secret := range []string{topic, serverURL, strings.TrimPrefix(serverURL, "http://"), secretValue} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error %q carries %q", err, secret)
			}
		}
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			t.Errorf("error still wraps the transport's *url.Error %q", urlErr)
		}
	}

	f.ntfy.status = http.StatusForbidden
	err := f.service.SendTestNotice(ctx, f.device.ID)
	var rejected *signet.ChannelRejectionError
	if !errors.As(err, &rejected) || rejected.Status != http.StatusForbidden {
		t.Fatalf("SendTestNotice error = %v, want a 403 rejection", err)
	}
	assertSilent(t, err)

	f.server.Close()
	err = f.service.SendTestNotice(ctx, f.device.ID)
	if !errors.Is(err, signet.ErrChannelUnreachable) {
		t.Fatalf("SendTestNotice error = %v, want ErrChannelUnreachable", err)
	}
	assertSilent(t, err)
}

func deviceAuthorizer(tokens map[string]domain.DeviceID) signet.RequestAuthorizer {
	return func(r *http.Request) (domain.DeviceID, bool) {
		id, ok := tokens[r.Header.Get("Authorization")]
		return id, ok
	}
}

// TestHTTPDeviceTestNotificationAccepted: an active paired device gets one
// test notification on its own topic carrying freeside://inbox, with no
// delivery row written and no revision change, even while its notifications
// switch is off.
func TestHTTPDeviceTestNotificationAccepted(t *testing.T) {
	t.Parallel()
	f := newDeliveryFixture(t)
	revision := f.revision(t)
	handler := signet.NewHTTPHandler(f.service, deviceAuthorizer(map[string]domain.DeviceID{
		"Bearer dev-1": f.device.ID,
	}))

	response := bearerRequest(t, handler, http.MethodPost,
		"/devices/"+string(f.device.ID)+"/test-notification", "Bearer dev-1", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", response.Code, response.Body.String())
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(response.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw response: %v", err)
	}
	if string(raw["outcome"]) != `"accepted"` ||
		string(raw["provider_status"]) != "null" ||
		string(raw["transport_failure"]) != "null" {
		t.Fatalf("body = %s, want accepted with null provider_status and transport_failure", response.Body.String())
	}
	requests := f.ntfy.recorded(t)
	if len(requests) != 1 {
		t.Fatalf("published %d notifications, want 1", len(requests))
	}
	if requests[0].click != "freeside://inbox" {
		t.Errorf("click = %q, want freeside://inbox", requests[0].click)
	}
	if rows := f.deliveries(t); len(rows) != 0 {
		t.Errorf("delivery rows = %+v, want none", rows)
	}
	if got := f.revision(t); got != revision {
		t.Errorf("store revision moved from %d to %d, want no write", revision, got)
	}
}

// TestHTTPDeviceTestNotificationOutcomes: provider rejection, transport
// failure, and an unconfigured notifier each return 200 with their typed
// outcome and detail field.
func TestHTTPDeviceTestNotificationOutcomes(t *testing.T) {
	t.Parallel()

	t.Run("provider rejected", func(t *testing.T) {
		f := newDeliveryFixture(t)
		f.ntfy.status = http.StatusForbidden
		handler := signet.NewHTTPHandler(f.service, deviceAuthorizer(map[string]domain.DeviceID{
			"Bearer dev-1": f.device.ID,
		}))

		response := bearerRequest(t, handler, http.MethodPost,
			"/devices/"+string(f.device.ID)+"/test-notification", "Bearer dev-1", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s, want 200", response.Code, response.Body.String())
		}
		var body signet.DeviceTestNotification
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Outcome != signet.DeviceTestNotificationProviderRejected ||
			body.ProviderStatus == nil || *body.ProviderStatus != http.StatusForbidden ||
			body.TransportFailure != nil {
			t.Fatalf("body = %+v, want provider_rejected with status 403", body)
		}
	})

	t.Run("transport failed", func(t *testing.T) {
		f := newDeliveryFixture(t)
		f.server.Close()
		handler := signet.NewHTTPHandler(f.service, deviceAuthorizer(map[string]domain.DeviceID{
			"Bearer dev-1": f.device.ID,
		}))

		response := bearerRequest(t, handler, http.MethodPost,
			"/devices/"+string(f.device.ID)+"/test-notification", "Bearer dev-1", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s, want 200", response.Code, response.Body.String())
		}
		var body signet.DeviceTestNotification
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Outcome != signet.DeviceTestNotificationTransportFailed ||
			body.ProviderStatus != nil ||
			body.TransportFailure == nil || *body.TransportFailure != "connection failed" {
			t.Fatalf("body = %+v, want transport_failed with connection failed", body)
		}
	})

	t.Run("notifier unavailable", func(t *testing.T) {
		f := newFixture(t)
		service := signet.NewService(f.store)
		handler := signet.NewHTTPHandler(service, deviceAuthorizer(map[string]domain.DeviceID{
			"Bearer dev-1": f.device.ID,
		}))

		response := bearerRequest(t, handler, http.MethodPost,
			"/devices/"+string(f.device.ID)+"/test-notification", "Bearer dev-1", nil)
		if response.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s, want 200", response.Code, response.Body.String())
		}
		var body signet.DeviceTestNotification
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Outcome != signet.DeviceTestNotificationNotifierUnavailable ||
			body.ProviderStatus != nil || body.TransportFailure != nil {
			t.Fatalf("body = %+v, want notifier_unavailable with null details", body)
		}
	})
}

// TestHTTPDeviceTestNotificationRefusals: another device's ID earns 404, a
// device revoked after authentication earns 403, and neither publishes or
// starts the rate-limit interval.
func TestHTTPDeviceTestNotificationRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("another device is 404 and does not start interval", func(t *testing.T) {
		f := newDeliveryFixture(t)
		f.seedDevice(t, "device-2")
		handler := signet.NewHTTPHandler(f.service, deviceAuthorizer(map[string]domain.DeviceID{
			"Bearer dev-1": f.device.ID,
		}))

		response := bearerRequest(t, handler, http.MethodPost,
			"/devices/device-2/test-notification", "Bearer dev-1", nil)
		if response.Code != http.StatusNotFound {
			t.Fatalf("status = %d body=%s, want 404", response.Code, response.Body.String())
		}
		if n := len(f.ntfy.recorded(t)); n != 0 {
			t.Fatalf("published %d notifications, want 0", n)
		}
		// Own call right after still publishes.
		own := bearerRequest(t, handler, http.MethodPost,
			"/devices/"+string(f.device.ID)+"/test-notification", "Bearer dev-1", nil)
		if own.Code != http.StatusOK || len(f.ntfy.recorded(t)) != 1 {
			t.Fatalf("own call = %d published=%d, want 200 and 1", own.Code, len(f.ntfy.recorded(t)))
		}
	})

	t.Run("device revoked after authentication is 403", func(t *testing.T) {
		f := newDeliveryFixture(t)
		f.seedDevice(t, "device-2")
		if _, err := f.service.Revoke(ctx, "device-2", f.device.ID); err != nil {
			t.Fatalf("Revoke: %v", err)
		}
		handler := signet.NewHTTPHandler(f.service, deviceAuthorizer(map[string]domain.DeviceID{
			"Bearer dev-1": f.device.ID,
		}))

		response := bearerRequest(t, handler, http.MethodPost,
			"/devices/"+string(f.device.ID)+"/test-notification", "Bearer dev-1", nil)
		if response.Code != http.StatusForbidden {
			t.Fatalf("status = %d body=%s, want 403", response.Code, response.Body.String())
		}
		if n := len(f.ntfy.recorded(t)); n != 0 {
			t.Fatalf("published %d notifications, want 0", n)
		}
	})
}

// TestHTTPDeviceTestNotificationRateLimit: a second call inside 30 seconds
// returns 429 with Retry-After and publishes nothing; another device and the
// host SendTestNotice command are unaffected; once the interval passes the
// next publish succeeds and a rejected publish also starts the interval.
func TestHTTPDeviceTestNotificationRateLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newDeliveryFixture(t)
	f.seedDevice(t, "device-2")
	handler := signet.NewHTTPHandler(f.service, deviceAuthorizer(map[string]domain.DeviceID{
		"Bearer dev-1": f.device.ID,
		"Bearer dev-2": "device-2",
	}))

	first := bearerRequest(t, handler, http.MethodPost,
		"/devices/"+string(f.device.ID)+"/test-notification", "Bearer dev-1", nil)
	if first.Code != http.StatusOK || len(f.ntfy.recorded(t)) != 1 {
		t.Fatalf("first = %d published=%d, want 200 and 1", first.Code, len(f.ntfy.recorded(t)))
	}

	// Inside the 30-second window: 429 with Retry-After, nothing published.
	*f.now = f.now.Add(10 * time.Second)
	limited := bearerRequest(t, handler, http.MethodPost,
		"/devices/"+string(f.device.ID)+"/test-notification", "Bearer dev-1", nil)
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("second = %d body=%s, want 429", limited.Code, limited.Body.String())
	}
	if retryAfter := limited.Header().Get("Retry-After"); retryAfter != "20" {
		t.Errorf("Retry-After = %q, want 20", retryAfter)
	}
	if n := len(f.ntfy.recorded(t)); n != 1 {
		t.Fatalf("published %d notifications after 429, want 1", n)
	}

	// Another device is not delayed by the first device's interval.
	other := bearerRequest(t, handler, http.MethodPost,
		"/devices/device-2/test-notification", "Bearer dev-2", nil)
	if other.Code != http.StatusOK || len(f.ntfy.recorded(t)) != 2 {
		t.Fatalf("other device = %d published=%d, want 200 and 2", other.Code, len(f.ntfy.recorded(t)))
	}

	// Host command SendTestNotice is not limited.
	if err := f.service.SendTestNotice(ctx, f.device.ID); err != nil {
		t.Fatalf("host SendTestNotice during interval: %v", err)
	}
	if n := len(f.ntfy.recorded(t)); n != 3 {
		t.Fatalf("published %d notifications after host SendTestNotice, want 3", n)
	}

	// Once 30s have passed since the first HTTP publish, another publish is
	// admitted, and even a provider rejection starts a fresh interval.
	*f.now = f.now.Add(20 * time.Second)
	f.ntfy.status = http.StatusBadGateway
	afterWindow := bearerRequest(t, handler, http.MethodPost,
		"/devices/"+string(f.device.ID)+"/test-notification", "Bearer dev-1", nil)
	if afterWindow.Code != http.StatusOK || len(f.ntfy.recorded(t)) != 4 {
		t.Fatalf("after window = %d published=%d, want 200 and 4", afterWindow.Code, len(f.ntfy.recorded(t)))
	}
	again := bearerRequest(t, handler, http.MethodPost,
		"/devices/"+string(f.device.ID)+"/test-notification", "Bearer dev-1", nil)
	if again.Code != http.StatusTooManyRequests {
		t.Fatalf("call after rejected publish = %d, want 429", again.Code)
	}
	if n := len(f.ntfy.recorded(t)); n != 4 {
		t.Fatalf("published %d notifications after second 429, want 4", n)
	}
}

// TestHTTPDeviceTestNotificationNeverLeaksSecrets: no response body, response
// header, or daemon log line carries the device's topic, the publish URL, or
// the ntfy access token in the accepted, rejected, or transport-failure cases.
func TestHTTPDeviceTestNotificationNeverLeaksSecrets(t *testing.T) {
	t.Parallel()
	f := newDeliveryFixture(t)
	var logs bytes.Buffer
	service := signet.NewService(f.store,
		signet.WithClock(func() time.Time { return *f.now }),
		signet.WithLogger(slog.New(slog.NewTextHandler(&logs, nil))),
		signet.WithNtfy(signet.NtfyConfig{
			BaseURL:  f.server.URL,
			Client:   f.server.Client(),
			Token:    signet.Secret(secretValue),
			TopicKey: testTopicKey,
		}),
	)
	handler := signet.NewHTTPHandler(service, deviceAuthorizer(map[string]domain.DeviceID{
		"Bearer dev-1": f.device.ID,
	}))
	serverURL := f.server.URL
	host := strings.TrimPrefix(serverURL, "http://")

	assertSilent := func(t *testing.T, label string, resp *httptest.ResponseRecorder, topic string) {
		t.Helper()
		secrets := []string{topic, serverURL, host, secretValue}
		body := resp.Body.String()
		for _, secret := range secrets {
			if strings.Contains(body, secret) {
				t.Errorf("%s: response body %q carries %q", label, body, secret)
			}
			if strings.Contains(logs.String(), secret) {
				t.Errorf("%s: daemon log %q carries %q", label, logs.String(), secret)
			}
			for key, values := range resp.Header() {
				for _, v := range values {
					if strings.Contains(v, secret) {
						t.Errorf("%s: header %s=%q carries %q", label, key, v, secret)
					}
				}
			}
		}
	}

	accepted := bearerRequest(t, handler, http.MethodPost,
		"/devices/"+string(f.device.ID)+"/test-notification", "Bearer dev-1", nil)
	if accepted.Code != http.StatusOK || !strings.Contains(accepted.Body.String(), `"accepted"`) {
		t.Fatalf("accepted = %d body=%s", accepted.Code, accepted.Body.String())
	}
	requests := f.ntfy.recorded(t)
	if len(requests) != 1 {
		t.Fatalf("published %d notifications, want 1", len(requests))
	}
	topic := requests[0].topic
	assertSilent(t, "accepted", accepted, topic)

	*f.now = f.now.Add(31 * time.Second)
	f.ntfy.status = http.StatusForbidden
	rejected := bearerRequest(t, handler, http.MethodPost,
		"/devices/"+string(f.device.ID)+"/test-notification", "Bearer dev-1", nil)
	if rejected.Code != http.StatusOK || !strings.Contains(rejected.Body.String(), `"provider_rejected"`) {
		t.Fatalf("rejected = %d body=%s", rejected.Code, rejected.Body.String())
	}
	assertSilent(t, "provider_rejected", rejected, topic)

	*f.now = f.now.Add(31 * time.Second)
	f.server.Close()
	failed := bearerRequest(t, handler, http.MethodPost,
		"/devices/"+string(f.device.ID)+"/test-notification", "Bearer dev-1", nil)
	if failed.Code != http.StatusOK || !strings.Contains(failed.Body.String(), `"transport_failed"`) {
		t.Fatalf("transport_failed = %d body=%s", failed.Code, failed.Body.String())
	}
	assertSilent(t, "transport_failed", failed, topic)
}
