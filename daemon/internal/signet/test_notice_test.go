package signet_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestSendTestNoticePublishesToTheDevicesTopicAndRecordsNothing: the test
// notice reaches the same topic a real notification would, links the app's
// inbox, and leaves no trace in the store. In particular it records no
// delivery row, so it moves no item's timing or version.
func TestSendTestNoticePublishesToTheDevicesTopicAndRecordsNothing(t *testing.T) {
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
