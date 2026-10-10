package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// notifyTestClientTimeout outlasts the daemon's own bound on each publish for
// the handful of devices one operator pairs.
const notifyTestClientTimeout = 2 * time.Minute

type notifyTestRequest struct {
	StateDir string `json:"state_dir"`
	// DeviceID limits the test to one device; empty tests every active one.
	DeviceID string `json:"device_id"`
}

type notifyTestResult struct {
	Devices []notifyTestDevice `json:"devices"`
}

// notifyTestDevice is one device's outcome. It names the device and never
// its topic, which is the capability to read that device's notifications.
type notifyTestDevice struct {
	DeviceID    string `json:"device_id"`
	DisplayName string `json:"display_name"`
	Accepted    bool   `json:"accepted"`
	// Failure is why the notice was not accepted: a provider status or a
	// failure class.
	Failure string `json:"failure"`
}

func runNotifyTestMain(args []string) {
	if err := runNotifyTestCommand(context.Background(), args, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "freesided notify-test:", err)
		os.Exit(1)
	}
}

// runNotifyTestCommand asks the running daemon to publish a test notification
// to each active paired device and prints one line per device. The line says
// whether the channel accepted the notice; whether the phone showed it, and
// whether tapping it opened Freeside, is what the operator is checking.
func runNotifyTestCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("freesided notify-test", flag.ContinueOnError)
	flags.SetOutput(stderr)
	stateDir := flags.String("state-dir", "", "existing state directory of the running daemon (required)")
	deviceID := flags.String("device", "", "test only this device ID (default: every active paired device)")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	canonical, client, closeClient, err := pairingControlClient(*stateDir, notifyTestClientTimeout)
	if err != nil {
		return err
	}
	defer closeClient()
	body, err := json.Marshal(notifyTestRequest{StateDir: canonical, DeviceID: *deviceID})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://pairing-control/notify-test", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("contact running daemon for a test notification: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("daemon refused the test notification (HTTP %d)", response.StatusCode)
	}
	var result notifyTestResult
	if err := strictjson.DecodeReader(response.Body, &result, strictjson.RejectInvalidUTF8, 1<<20); err != nil {
		return errors.New("invalid test notification response")
	}
	if len(result.Devices) == 0 {
		if *deviceID != "" {
			return fmt.Errorf("device %q is not an active paired device", *deviceID)
		}
		return errors.New("no active paired device")
	}
	failed := 0
	for _, device := range result.Devices {
		outcome := "accepted by ntfy"
		if !device.Accepted {
			failed++
			outcome = "not sent: " + device.Failure
		}
		// A display name is device-supplied text: quote it so it cannot
		// write terminal control sequences.
		if _, err := fmt.Fprintf(stdout, "%q (%s): %s\n", device.DisplayName, device.DeviceID, outcome); err != nil {
			return fmt.Errorf("write test notification result: %w", err)
		}
	}
	if failed != 0 {
		return fmt.Errorf("%d of %d test notifications were not accepted", failed, len(result.Devices))
	}
	return nil
}

// registerNotifyTest serves the notify-test command on the host control
// socket. Like pairing-code it is keyed to the state directory, which is all
// the command knows of the daemon it is addressing.
func (p *pairingControl) registerNotifyTest(mux *http.ServeMux, st *store.Store, attention *signet.Service) {
	if p.stateDir == "" {
		return
	}
	mux.HandleFunc("POST /notify-test", func(w http.ResponseWriter, r *http.Request) {
		var request notifyTestRequest
		if err := strictjson.DecodeReader(r.Body, &request, strictjson.RejectInvalidUTF8, 16<<10); err != nil ||
			request.StateDir != p.stateDir {
			http.Error(w, "notify-test state directory mismatch or invalid request", http.StatusBadRequest)
			return
		}
		devices, err := sendTestNotices(r.Context(), st, attention, domain.DeviceID(request.DeviceID))
		if err != nil {
			http.Error(w, "could not list paired devices", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(notifyTestResult{Devices: devices})
	})
}

// sendTestNotices publishes a test notice to only, or to every active device
// when only is empty, one at a time in the store's order. A device that is
// unknown or revoked is left out, so an empty result means nothing matched.
func sendTestNotices(
	ctx context.Context, st *store.Store, attention *signet.Service, only domain.DeviceID,
) ([]notifyTestDevice, error) {
	var active []domain.Device
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		devices, err := tx.ListDevices(ctx)
		if err != nil {
			return err
		}
		for _, device := range devices {
			if device.Value.Status == domain.DeviceActive && (only == "" || device.Value.ID == only) {
				active = append(active, device.Value)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	results := make([]notifyTestDevice, 0, len(active))
	for _, device := range active {
		result := notifyTestDevice{DeviceID: string(device.ID), DisplayName: device.DisplayName, Accepted: true}
		if err := attention.SendTestNotice(ctx, device.ID); err != nil {
			result.Accepted = false
			result.Failure = testNoticeFailure(err)
		}
		results = append(results, result)
	}
	return results, nil
}

// testNoticeFailure is the text the command prints for a notice that was not
// accepted. SendTestNotice's errors never carry a topic; the channel's two
// are unwrapped to their own text, which reads better than the wrapped form.
func testNoticeFailure(err error) string {
	var (
		rejected  *signet.ChannelRejectionError
		transport *signet.ChannelTransportError
	)
	switch {
	case errors.As(err, &rejected):
		return rejected.Error()
	case errors.As(err, &transport):
		return transport.Error()
	case errors.Is(err, signet.ErrNotifierUnavailable):
		return "the daemon has no usable notification channel"
	}
	return err.Error()
}
