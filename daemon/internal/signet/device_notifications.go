package signet

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ErrDeviceRevoked rejects setting the notifications switch of a revoked
// device: a revoked device is never notified, so it has no switch. The route
// answers it as it answers an unknown device.
var ErrDeviceRevoked = errors.New("device is revoked")

// errNotificationsReplay abandons the Write of a switch that already holds
// the requested value, after the entry has been captured, so a replay moves
// no revision and never moves the instant the switch was turned on (the
// errRevokeReplay pattern).
var errNotificationsReplay = errors.New("notifications switch replay: entry captured")

// SetDeviceNotifications turns the target device's notifications switch on
// or off on behalf of caller and returns the device's list entry after the
// write (api/openapi.yaml PUT /devices/{device_id}/notifications). Any active
// device may set any active device's switch, as any may revoke any. The
// caller's active-device gate runs inside the writing transaction, as Revoke
// does, so a device revoked after the HTTP authorizer passed it is refused.
// The switch grants nothing: it decides only what owedDeliveries owes the
// device and what the device list shows.
func (s *Service) SetDeviceNotifications(
	ctx context.Context, caller, id domain.DeviceID, enabled bool,
) (DeviceListEntry, error) {
	var out DeviceListEntry
	err := s.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := gateActiveDevice(ctx, tx, caller); err != nil {
			return fmt.Errorf("set notifications of device %q: caller %w", id, err)
		}
		device, snap, err := tx.GetDeviceSnapshot(ctx, id)
		if err != nil {
			return fmt.Errorf("set notifications of device %q: %w", id, err)
		}
		if device.Status != domain.DeviceActive {
			return fmt.Errorf("set notifications of device %q: %w", id, ErrDeviceRevoked)
		}
		pref, err := tx.GetDeviceNotificationPreference(ctx, id)
		switch {
		case errors.Is(err, store.ErrNotFound):
			pref = domain.DeviceNotificationPreference{DeviceID: id}
		case err != nil:
			return fmt.Errorf("set notifications of device %q: %w", id, err)
		}
		replay := pref.Enabled == enabled
		if !replay {
			pref = domain.DeviceNotificationPreference{DeviceID: id, Enabled: enabled, UpdatedAt: s.now().UTC()}
			if err := tx.SetDeviceNotificationPreference(ctx, pref); err != nil {
				return fmt.Errorf("set notifications of device %q: %w", id, err)
			}
		}
		activity, err := tx.ListDeviceActivity(ctx)
		if err != nil {
			return fmt.Errorf("set notifications of device %q: %w", id, err)
		}
		out = deviceListEntry(device, snap, activity, pref.Enabled)
		if replay {
			return errNotificationsReplay
		}
		return nil
	})
	if err != nil && !errors.Is(err, errNotificationsReplay) {
		return DeviceListEntry{}, err
	}
	return out, nil
}

// deviceListEntry builds one device-list row from the device and the
// bookkeeping kept beside it.
//
// A revoked device lists off whatever its stored switch says: revocation
// leaves the row, and the sender never notifies a revoked device.
func deviceListEntry(
	device domain.Device, snap store.Snapshot, activity map[domain.DeviceID]time.Time, notificationsEnabled bool,
) DeviceListEntry {
	entry := DeviceListEntry{
		AsOfRevision: snap.AsOfRevision, EntityVersion: snap.EntityVersion,
		Device: device, NotificationsEnabled: notificationsEnabled && device.Status == domain.DeviceActive,
	}
	if lastSeenAt, ok := activity[device.ID]; ok {
		entry.LastSeenAt = &lastSeenAt
	}
	return entry
}

type deviceNotificationsRequest struct {
	Enabled *bool `json:"enabled"`
}

// setDeviceNotifications sets the path device's switch for the authenticated
// caller. The target comes from the path and the caller from the credential,
// as revokeDevice takes them.
func (h httpHandler) setDeviceNotifications(w http.ResponseWriter, r *http.Request, caller domain.DeviceID) {
	var request deviceNotificationsRequest
	if err := decodeRequest(w, r, &request); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeJSON(w, http.StatusRequestEntityTooLarge, errorResponse{Message: "request body is too large"})
			return
		}
		writeJSON(w, http.StatusBadRequest, errorResponse{Message: err.Error()})
		return
	}
	if request.Enabled == nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Message: "enabled is required"})
		return
	}
	entry, err := h.service.SetDeviceNotifications(
		r.Context(), caller, domain.DeviceID(r.PathValue("device_id")), *request.Enabled)
	if err != nil {
		// The caller's in-tx gate can reject a device revoked after the
		// middleware authorized it; that is a credential failure, not a
		// missing target.
		if errors.Is(err, ErrDeviceNotActive) {
			writeJSON(w, http.StatusForbidden, errorResponse{Message: err.Error()})
			return
		}
		if errors.Is(err, ErrDeviceRevoked) {
			writeJSON(w, http.StatusNotFound, errorResponse{Message: "not found"})
			return
		}
		writeReadError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entry)
}
