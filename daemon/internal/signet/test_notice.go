package signet

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// testNoticeTimeout bounds one test notice's publish. The operator is waiting
// on the answer, and a provider that has not replied by now is the finding.
const testNoticeTimeout = 10 * time.Second

// testNoticeMinInterval bounds how often one device may trigger a publish via
// POST /devices/{device_id}/test-notification, so a client loop cannot use
// the daemon to flood ntfy. Kept in process memory; freesided notify-test is
// not limited.
const testNoticeMinInterval = 30 * time.Second

// DeviceTestNotificationOutcome is the wire outcome of
// POST /devices/{device_id}/test-notification. The zero value is invalid.
type DeviceTestNotificationOutcome string

const (
	DeviceTestNotificationAccepted            DeviceTestNotificationOutcome = "accepted"
	DeviceTestNotificationProviderRejected    DeviceTestNotificationOutcome = "provider_rejected"
	DeviceTestNotificationTransportFailed     DeviceTestNotificationOutcome = "transport_failed"
	DeviceTestNotificationNotifierUnavailable DeviceTestNotificationOutcome = "notifier_unavailable"
)

// AllDeviceTestNotificationOutcomes is the single registration point for
// DeviceTestNotificationOutcome.
var AllDeviceTestNotificationOutcomes = []DeviceTestNotificationOutcome{
	DeviceTestNotificationAccepted,
	DeviceTestNotificationProviderRejected,
	DeviceTestNotificationTransportFailed,
	DeviceTestNotificationNotifierUnavailable,
}

func (o DeviceTestNotificationOutcome) valid() bool {
	switch o {
	case DeviceTestNotificationAccepted, DeviceTestNotificationProviderRejected,
		DeviceTestNotificationTransportFailed, DeviceTestNotificationNotifierUnavailable:
		return true
	default:
		return false
	}
}

// DeviceTestNotification is the 200 JSON body of
// POST /devices/{device_id}/test-notification. It carries only the typed
// outcome and, when applicable, ntfy's HTTP status or the fixed failure class:
// never error text, the device's topic, the publish URL, or the access token.
type DeviceTestNotification struct {
	Outcome          DeviceTestNotificationOutcome `json:"outcome"`
	ProviderStatus   *int                          `json:"provider_status"`
	TransportFailure *string                       `json:"transport_failure"`
}

const testNoticeRateLimitMessage = "test notification sent too recently"

// TestNoticeRateLimitError rejects a device test-notification request made
// less than testNoticeMinInterval after the device's previous publish, and
// carries how long remains so the HTTP handler can emit Retry-After.
type TestNoticeRateLimitError struct {
	RetryAfter time.Duration
}

func (e *TestNoticeRateLimitError) Error() string { return testNoticeRateLimitMessage }

// SendTestNotice publishes one notification to an active device's topic so
// its operator can check the phone's setup end to end (#1924). It is about no
// item: it records no delivery row, moves no timing, and its tap link opens
// the app's inbox. A nil return means the channel accepted the notice, which
// is the most the daemon can know; whether the phone showed it is for the
// operator to see.
//
// Its errors are safe to print: ErrNotifierUnavailable, ErrDeviceNotActive,
// the store's not-found, or one of publish's, which carry a status code or a
// failure class and never the device's topic.
func (s *Service) SendTestNotice(ctx context.Context, deviceID domain.DeviceID) error {
	return s.sendTestNotice(ctx, deviceID, nil)
}

// SendDeviceTestNotification serves POST /devices/{device_id}/test-notification
// (#1959). Unlike SendTestNotice, it enforces testNoticeMinInterval per device
// across publishes regardless of publish outcome, and maps publish outcomes and
// an unavailable notifier into a typed DeviceTestNotification body. Calls that
// publish nothing (a revoked device, an unknown device, or no usable notifier)
// do not start the interval.
func (s *Service) SendDeviceTestNotification(
	ctx context.Context, deviceID domain.DeviceID,
) (DeviceTestNotification, error) {
	err := s.sendTestNotice(ctx, deviceID, func() error {
		return s.admitDeviceTestNotice(deviceID)
	})
	if err == nil {
		return DeviceTestNotification{Outcome: DeviceTestNotificationAccepted}, nil
	}
	var rejected *ChannelRejectionError
	if errors.As(err, &rejected) {
		status := rejected.Status
		return DeviceTestNotification{
			Outcome:        DeviceTestNotificationProviderRejected,
			ProviderStatus: &status,
		}, nil
	}
	var transport *ChannelTransportError
	if errors.As(err, &transport) {
		kind := transport.Kind
		return DeviceTestNotification{
			Outcome:          DeviceTestNotificationTransportFailed,
			TransportFailure: &kind,
		}, nil
	}
	if errors.Is(err, ErrNotifierUnavailable) {
		return DeviceTestNotification{Outcome: DeviceTestNotificationNotifierUnavailable}, nil
	}
	return DeviceTestNotification{}, err
}

func (s *Service) sendTestNotice(ctx context.Context, deviceID domain.DeviceID, admit func() error) error {
	// Check device state before notifier availability so a revoked device is
	// refused with ErrDeviceNotActive even on a daemon without ntfy, and
	// before admit so a refused or unconfigured call never starts the interval.
	err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		device, err := tx.GetDevice(ctx, deviceID)
		if err != nil {
			return err
		}
		if device.Status != domain.DeviceActive {
			return fmt.Errorf("device %q: %w", deviceID, ErrDeviceNotActive)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if s.ntfy == nil {
		return ErrNotifierUnavailable
	}
	if err := s.ntfy.validate(); err != nil {
		return fmt.Errorf("%w: %w", err, ErrNotifierUnavailable)
	}
	if admit != nil {
		if err := admit(); err != nil {
			return err
		}
	}
	sendCtx, cancel := context.WithTimeout(ctx, testNoticeTimeout)
	defer cancel()
	return s.ntfy.publish(sendCtx, notification{
		topic:    s.ntfy.topic(deviceID),
		title:    "Freeside test notification",
		body:     "Tap to open Freeside.",
		click:    inboxLink,
		priority: domain.PriorityNormal,
	})
}

func (s *Service) admitDeviceTestNotice(deviceID domain.DeviceID) error {
	s.testNoticeMu.Lock()
	defer s.testNoticeMu.Unlock()
	now := s.now()
	if last, ok := s.testNoticeLastSent[deviceID]; ok {
		elapsed := now.Sub(last)
		if elapsed < testNoticeMinInterval {
			return &TestNoticeRateLimitError{RetryAfter: testNoticeMinInterval - elapsed}
		}
	}
	if s.testNoticeLastSent == nil {
		s.testNoticeLastSent = make(map[domain.DeviceID]time.Time)
	}
	s.testNoticeLastSent[deviceID] = now
	return nil
}

func retryAfterSeconds(remaining time.Duration) int {
	if remaining <= time.Second {
		return 1
	}
	return int((remaining + time.Second - 1) / time.Second)
}

// sendDeviceTestNotification serves POST /devices/{device_id}/test-notification.
// The path device must equal the authenticated caller (else 404). Every body is
// constructed from typed outcomes or fixed messages — never err.Error() — and
// nothing about the publish is logged, so the device's topic, publish URL, and
// ntfy token cannot leak through this handler.
func (h httpHandler) sendDeviceTestNotification(
	w http.ResponseWriter, r *http.Request, deviceID domain.DeviceID,
) {
	if domain.DeviceID(r.PathValue("device_id")) != deviceID {
		writeJSON(w, http.StatusNotFound, errorResponse{Message: "not found"})
		return
	}
	result, err := h.service.SendDeviceTestNotification(r.Context(), deviceID)
	if err != nil {
		if errors.Is(err, ErrDeviceNotActive) {
			writeJSON(w, http.StatusForbidden, errorResponse{Message: ErrDeviceNotActive.Error()})
			return
		}
		var limited *TestNoticeRateLimitError
		if errors.As(err, &limited) {
			w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds(limited.RetryAfter)))
			writeJSON(w, http.StatusTooManyRequests, errorResponse{Message: testNoticeRateLimitMessage})
			return
		}
		writeReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
