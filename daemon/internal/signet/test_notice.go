package signet

import (
	"context"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// testNoticeTimeout bounds one test notice's publish. The operator is waiting
// on the answer, and a provider that has not replied by now is the finding.
const testNoticeTimeout = 10 * time.Second

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
	if s.ntfy == nil {
		return ErrNotifierUnavailable
	}
	if err := s.ntfy.validate(); err != nil {
		return fmt.Errorf("%w: %w", err, ErrNotifierUnavailable)
	}
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
