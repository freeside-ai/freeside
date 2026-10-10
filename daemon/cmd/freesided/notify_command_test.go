package main

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// notifyTestFixture is a running daemon with a state directory, which is what
// the notify-test command addresses, publishing to a loopback sink.
type notifyTestFixture struct {
	daemon   *daemon
	stateDir string
	logs     *lockedBuffer
}

func startNotifyTestFixture(t *testing.T, ntfyURL string) notifyTestFixture {
	t.Helper()
	root := t.TempDir()
	f := notifyTestFixture{stateDir: filepath.Join(root, "state"), logs: &lockedBuffer{}}
	h, err := run(t.Context(), nil, config{
		Environment: environmentEphemeral,
		DBPath:      filepath.Join(root, "freeside.db"), StateDir: f.stateDir,
		ListenAddr: "127.0.0.1:0", NtfyURL: ntfyURL,
		Logger: slog.New(slog.NewTextHandler(f.logs, nil)),
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	f.daemon = h
	return f
}

func (f notifyTestFixture) pair(t *testing.T, id domain.DeviceID, name string) {
	t.Helper()
	ctx := t.Context()
	if err := f.daemon.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(ctx, domain.Device{
			ID: id, DisplayName: name,
			Status: domain.DeviceActive, PairedAt: time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC),
		})
	}); err != nil {
		t.Fatalf("put device %s: %v", id, err)
	}
}

// notifyTest runs the command against the fixture's daemon and returns what
// it printed and its error.
func (f notifyTestFixture) notifyTest(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := runNotifyTestCommand(t.Context(), append([]string{"-state-dir", f.stateDir}, args...), &stdout, &stderr)
	if stderr.Len() != 0 {
		t.Errorf("command stderr = %q, want nothing", stderr.String())
	}
	return stdout.String(), err
}

// assertNoTopic fails when anything the operator or the log can show names a
// device's topic or where it is published. Every topic starts fs-.
func (f notifyTestFixture) assertNoTopic(t *testing.T, ntfyURL string, shown ...string) {
	t.Helper()
	for _, text := range append(shown, f.logs.String()) {
		for _, secret := range []string{"fs-", ntfyURL, strings.TrimPrefix(ntfyURL, "http://")} {
			if strings.Contains(text, secret) {
				t.Errorf("%q appears in:\n%s", secret, text)
			}
		}
	}
}

// TestNotifyTestSendsToEachActiveDevice: one line per active device, each
// notice on that device's own topic with the inbox link, nothing for a
// revoked device, and no delivery row.
func TestNotifyTestSendsToEachActiveDevice(t *testing.T) {
	sink := newNtfySink(t, http.StatusOK)
	f := startNotifyTestFixture(t, sink.URL)
	f.pair(t, "phone", "Ben's iPhone")
	f.pair(t, "tablet", "Tablet")
	f.pair(t, "old-phone", "Old Phone")
	if _, err := f.daemon.attention.Revoke(t.Context(), "phone", "old-phone"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	out, err := f.notifyTest(t)
	if err != nil {
		t.Fatalf("notify-test: %v", err)
	}
	want := "\"Ben's iPhone\" (phone): accepted by ntfy\n\"Tablet\" (tablet): accepted by ntfy\n"
	if out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	topics, clicks := sink.published()
	if len(topics) != 2 || topics[0] == topics[1] {
		t.Fatalf("published to topics %d times (distinct: %t), want one each for two devices",
			len(topics), len(topics) == 2 && topics[0] != topics[1])
	}
	for _, click := range clicks {
		if click != "freeside://inbox" {
			t.Errorf("tap link = %q, want the inbox link", click)
		}
	}
	for _, topic := range topics {
		if strings.Contains(out, topic) || strings.Contains(f.logs.String(), topic) {
			t.Errorf("a topic appears in the output or the log")
		}
	}
	f.assertNoTopic(t, sink.URL, out)
	if err := f.daemon.store.Read(t.Context(), func(tx *store.ReadTx) error {
		rows, err := tx.ListAttentionDeliveries(t.Context())
		if len(rows) != 0 {
			t.Errorf("delivery rows = %d, want none", len(rows))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// TestNotifyTestOneDevice: -device narrows the test to that device, and a
// device that is revoked or unknown is an error that publishes nothing.
func TestNotifyTestOneDevice(t *testing.T) {
	sink := newNtfySink(t, http.StatusOK)
	f := startNotifyTestFixture(t, sink.URL)
	f.pair(t, "phone", "Phone")
	f.pair(t, "tablet", "Tablet")
	f.pair(t, "old-phone", "Old Phone")
	if _, err := f.daemon.attention.Revoke(t.Context(), "phone", "old-phone"); err != nil {
		t.Fatalf("Revoke: %v", err)
	}

	out, err := f.notifyTest(t, "-device", "tablet")
	if err != nil || out != "\"Tablet\" (tablet): accepted by ntfy\n" {
		t.Errorf("notify-test -device tablet = %q, %v", out, err)
	}
	for _, id := range []string{"old-phone", "no-such-device"} {
		out, err := f.notifyTest(t, "-device", id)
		if err == nil || out != "" || !strings.Contains(err.Error(), "is not an active paired device") {
			t.Errorf("notify-test -device %s = %q, %v; want a refusal", id, out, err)
		}
	}
	if topics, _ := sink.published(); len(topics) != 1 {
		t.Errorf("published %d notifications, want 1", len(topics))
	}
}

// TestNotifyTestReportsAFailureWithoutTheTopic: a provider that rejects the
// notice, or cannot be reached, is a per-device line naming the status or
// the failure class and a failing command. Neither the output nor the daemon
// log names the topic or the publish URL.
func TestNotifyTestReportsAFailureWithoutTheTopic(t *testing.T) {
	t.Run("rejected", func(t *testing.T) {
		sink := newNtfySink(t, http.StatusForbidden)
		f := startNotifyTestFixture(t, sink.URL)
		f.pair(t, "phone", "Phone")

		out, err := f.notifyTest(t)
		if out != "\"Phone\" (phone): not sent: ntfy returned status 403\n" {
			t.Errorf("output = %q", out)
		}
		if err == nil || err.Error() != "1 of 1 test notifications were not accepted" {
			t.Errorf("error = %v", err)
		}
		f.assertNoTopic(t, sink.URL, out, err.Error())
	})

	t.Run("unreachable", func(t *testing.T) {
		gone := httptest.NewServer(http.NotFoundHandler())
		goneURL := gone.URL
		gone.Close()
		f := startNotifyTestFixture(t, goneURL)
		f.pair(t, "phone", "Phone")

		out, err := f.notifyTest(t)
		if out != "\"Phone\" (phone): not sent: ntfy connection failed\n" {
			t.Errorf("output = %q", out)
		}
		if err == nil {
			t.Fatal("notify-test succeeded against an unreachable provider")
		}
		f.assertNoTopic(t, goneURL, out, err.Error())
	})
}

// TestNotifyTestRefusals: no paired device, a daemon that is not running, and
// a state directory that advertises another daemon's socket all fail without
// publishing.
func TestNotifyTestRefusals(t *testing.T) {
	sink := newNtfySink(t, http.StatusOK)
	f := startNotifyTestFixture(t, sink.URL)

	if out, err := f.notifyTest(t); err == nil || out != "" || err.Error() != "no active paired device" {
		t.Errorf("with no device: %q, %v", out, err)
	}
	f.pair(t, "phone", "Phone")

	for name, args := range map[string][]string{
		"no state directory":      nil,
		"daemon not running":      {"-state-dir", t.TempDir()},
		"missing state directory": {"-state-dir", filepath.Join(t.TempDir(), "missing")},
		"positional argument":     {"-state-dir", f.stateDir, "phone"},
	} {
		var stdout bytes.Buffer
		if err := runNotifyTestCommand(t.Context(), args, &stdout, &bytes.Buffer{}); err == nil || stdout.Len() != 0 {
			t.Errorf("%s: output %q, error %v; want a refusal", name, stdout.String(), err)
		}
	}

	// Another state directory that points at this daemon's socket: the
	// daemon refuses a request that names a state directory it does not own.
	wrongState := t.TempDir()
	address, err := os.ReadFile(filepath.Join(f.stateDir, pairingControlFileName))
	if err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // G703: a test-owned temporary directory and a fixed file name.
	if err := os.WriteFile(filepath.Join(wrongState, pairingControlFileName), address, 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	if err := runNotifyTestCommand(t.Context(), []string{"-state-dir", wrongState}, &stdout, &bytes.Buffer{}); err == nil || stdout.Len() != 0 {
		t.Errorf("wrong state directory: output %q, error %v; want a refusal", stdout.String(), err)
	}

	if topics, _ := sink.published(); len(topics) != 0 {
		t.Errorf("published %d notifications across the refusals, want 0", len(topics))
	}
}
