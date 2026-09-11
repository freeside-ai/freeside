package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestPairingCodeRenewsRunningDaemon(t *testing.T) {
	root := t.TempDir()
	// This valid state path cannot itself hold a macOS Unix socket address.
	stateDir := filepath.Join(root, strings.Repeat("long-state-", 12))
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	var clockMu sync.Mutex
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	var logs lockedBuffer
	logger, err := newLogger(&logs, defaultLogLevel)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	h, err := run(ctx, nil, config{
		DBPath: filepath.Join(root, "freeside.db"), StateDir: stateDir,
		ListenAddr: "127.0.0.1:0", Logger: logger,
		FakeDriverEnabled: true, SeedWalkingSkeleton: true,
		now: func() time.Time {
			clockMu.Lock()
			defer clockMu.Unlock()
			return now
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := h.Close(); err != nil {
			t.Error(err)
		}
	})
	// The production control and Signet handlers are real. Only the parked
	// workflow is a fixture; this test is not live campaign acceptance.
	if _, err := h.workflow.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	beforeHealth := pairingTestHealth(t, h.readiness().APIURL)
	workflow := h.workflow
	original := pairingTestRedeem(t, h.readiness().APIURL, h.readiness().PairingCode)
	mint := func() pairingCodeResult {
		t.Helper()
		before := pairingTestWorkflow(t, h.store)
		var stdout, stderr bytes.Buffer
		if err := runPairingCodeCommand(ctx, []string{"-state-dir", stateDir}, &stdout, &stderr); err != nil {
			t.Fatal(err)
		}
		var result pairingCodeResult
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatal("invalid command JSON")
		}
		if result.APIURL != h.readiness().APIURL || len(result.PairingCode) != 8 ||
			!result.ExpiresAt.Equal(now.Add(10*time.Minute)) {
			t.Fatal("mint did not return the current endpoint, canonical code and ten-minute expiry")
		}
		if stderr.Len() != 0 || strings.Contains(logs.String(), result.PairingCode) {
			t.Fatal("pairing code leaked outside command stdout")
		}
		if after := pairingTestWorkflow(t, h.store); !reflect.DeepEqual(before, after) {
			t.Fatal("mint changed workflow state, commands or client revision")
		}
		if h.workflow != workflow || pairingTestHealth(t, result.APIURL) != beforeHealth {
			t.Fatal("mint replaced the workflow or daemon")
		}
		return result
	}
	expired := mint()
	clockMu.Lock()
	now = now.Add(10 * time.Minute)
	clockMu.Unlock()
	pairingTestRejected(t, expired.APIURL, expired.PairingCode)
	fresh := mint()
	pairingTestRejected(t, expired.APIURL, expired.PairingCode)
	pairingTestRejected(t, fresh.APIURL, h.readiness().PairingCode)
	preview := pairingTestPost(t, fresh.APIURL+"/pairing/preview", fresh.PairingCode)
	if preview.StatusCode != http.StatusOK {
		t.Fatalf("preview status = %d", preview.StatusCode)
	}
	var facts signet.PairingFacts
	if err := json.NewDecoder(preview.Body).Decode(&facts); err != nil {
		t.Fatal(err)
	}
	if facts.CodeExpiresAt != fresh.ExpiresAt {
		t.Fatal("preview expiry differs from mint result")
	}
	second := pairingTestRedeem(t, fresh.APIURL, fresh.PairingCode)
	pairingTestRejected(t, fresh.APIURL, fresh.PairingCode)
	pairingTestAuth(t, fresh.APIURL, original.DeviceToken, http.StatusOK)
	pairingTestAuth(t, fresh.APIURL, second.DeviceToken, http.StatusOK)
	if _, err := h.attention.Revoke(ctx, second.Device.Device.ID, original.Device.Device.ID); err != nil {
		t.Fatal(err)
	}
	_ = mint()
	pairingTestAuth(t, fresh.APIURL, original.DeviceToken, http.StatusUnauthorized)
	pairingTestAuth(t, fresh.APIURL, second.DeviceToken, http.StatusOK)

	response := pairingTestPost(t, fresh.APIURL+"/pairing-code", "unused")
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("network API exposed host mint route: HTTP %d", response.StatusCode)
	}
	for _, resource := range h.pairing.owned {
		info, err := os.Stat(resource.path)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o600)
		if info.IsDir() {
			want = 0o700
		}
		if info.Mode().Perm() != want {
			t.Fatalf("control resource mode = %o, want %o", info.Mode().Perm(), want)
		}
	}
}

func pairingTestWorkflow(t *testing.T, st *store.Store) any {
	t.Helper()
	var snapshot struct {
		State    store.ServerState
		Runs     []store.Snapshotted[domain.Run]
		Items    []store.Snapshotted[domain.AttentionItem]
		Commands []domain.Command
	}
	if err := st.Read(t.Context(), func(tx *store.ReadTx) (err error) {
		if snapshot.State, err = tx.ServerState(t.Context()); err != nil {
			return err
		}
		if snapshot.Runs, err = tx.ListRuns(t.Context()); err != nil {
			return err
		}
		if snapshot.Items, err = tx.ListAttentionItems(t.Context()); err != nil {
			return err
		}
		for _, item := range snapshot.Items {
			commands, err := tx.ListCommandsForItem(t.Context(), item.Value.ID)
			if err != nil {
				return err
			}
			snapshot.Commands = append(snapshot.Commands, commands...)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Runs) != 1 || len(snapshot.Items) != 1 || len(snapshot.Commands) != 0 {
		t.Fatal("fixture is not one unapproved workflow with no commands")
	}
	return snapshot
}

func pairingTestPost(t *testing.T, url, code string) *http.Response {
	t.Helper()
	payload := map[string]string{"pairing_code": code}
	if strings.HasSuffix(url, "/pairing") {
		payload["display_name"] = "Pairing renewal fixture"
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.Post(url, "application/json", bytes.NewReader(body)) //nolint:gosec // fixture-owned loopback endpoint
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

func pairingTestRedeem(t *testing.T, endpoint, code string) signet.PairingGrant {
	t.Helper()
	response := pairingTestPost(t, endpoint+"/pairing", code)
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("pairing status = %d", response.StatusCode)
	}
	var grant signet.PairingGrant
	if err := json.NewDecoder(response.Body).Decode(&grant); err != nil {
		t.Fatal(err)
	}
	return grant
}

func pairingTestRejected(t *testing.T, endpoint, code string) {
	t.Helper()
	for _, path := range []string{"/pairing/preview", "/pairing"} {
		response := pairingTestPost(t, endpoint+path, code)
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("%s accepted an expired or consumed code: HTTP %d", path, response.StatusCode)
		}
	}
}

func pairingTestAuth(t *testing.T, endpoint, token string, want int) {
	t.Helper()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, endpoint+"/sync/revision", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != want {
		t.Fatalf("existing device authentication = HTTP %d, want %d", response.StatusCode, want)
	}
}

func pairingTestHealth(t *testing.T, endpoint string) signet.HealthResponse {
	t.Helper()
	response, err := http.Get(endpoint + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var health signet.HealthResponse
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	return health
}

func startPairingControlTest(t *testing.T, peerUID func(*net.UnixConn) (uint32, error)) (*pairingControl, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	p, err := newPairingControl(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p.configure("http://127.0.0.1:7331", func(context.Context) (string, domain.PairingCode, error) {
		calls.Add(1)
		return "TESTCODE", domain.PairingCode{ExpiresAt: time.Now().Add(10 * time.Minute)}, nil
	})
	if peerUID != nil {
		p.peerUID = peerUID
	}
	served := make(chan error, 1)
	go func() { served <- p.Serve() }()
	t.Cleanup(func() {
		_ = p.Close()
		if err := <-served; !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	})
	if err := p.publish(); err != nil {
		t.Fatal(err)
	}
	return p, &calls
}

func TestPairingControlRejectsUnauthorizedPeers(t *testing.T) {
	for name, peerUID := range map[string]func(*net.UnixConn) (uint32, error){
		"different user": func(conn *net.UnixConn) (uint32, error) {
			uid, err := unixPeerUID(conn)
			return uid ^ 1, err
		},
		"unverifiable user": func(*net.UnixConn) (uint32, error) {
			return 0, errors.New("credential lookup failed")
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, calls := startPairingControlTest(t, peerUID)
			var stdout bytes.Buffer
			err := runPairingCodeCommand(t.Context(), []string{"-state-dir", p.stateDir}, &stdout, io.Discard)
			if err == nil || stdout.Len() != 0 || calls.Load() != 0 {
				t.Fatal("unauthorized peer reached mint or received a code")
			}
		})
	}
}

func TestPairingControlRejectsWrongDaemonAndConflictingOwner(t *testing.T) {
	p, calls := startPairingControlTest(t, nil)
	wrongState := t.TempDir()
	address, err := os.ReadFile(filepath.Join(p.stateDir, pairingControlFileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wrongState, pairingControlFileName), address, 0o600); err != nil { //nolint:gosec // copy an advertisement between test-owned state directories
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	err = runPairingCodeCommand(t.Context(), []string{"-state-dir", wrongState}, &stdout, io.Discard)
	if err == nil || stdout.Len() != 0 || calls.Load() != 0 {
		t.Fatal("wrong state directory minted on another daemon")
	}
	other, err := newPairingControl(p.stateDir)
	if err == nil {
		_ = other.Close()
		t.Fatal("second daemon acquired the same control directory")
	}
	if err := runPairingCodeCommand(t.Context(), []string{"-state-dir", p.stateDir}, io.Discard, io.Discard); err != nil {
		t.Fatalf("conflict damaged original endpoint: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("unexpected mint count after conflict")
	}
}

func TestRunRejectsPairingConflictBeforeOpeningDatabase(t *testing.T) {
	p, _ := startPairingControlTest(t, nil)
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	h, err := run(t.Context(), nil, config{DBPath: dbPath, StateDir: p.stateDir, ListenAddr: "127.0.0.1:0"})
	if err == nil {
		_ = h.Close()
		t.Fatal("second daemon acquired a shared state directory")
	}
	if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("conflicting daemon reached database startup before ownership refusal")
	}
	if err := runPairingCodeCommand(t.Context(), []string{"-state-dir", p.stateDir}, io.Discard, io.Discard); err != nil {
		t.Fatalf("startup conflict damaged original control: %v", err)
	}
}

func TestPairingCodeRefusesUnavailableDaemon(t *testing.T) {
	for _, args := range [][]string{nil, {"-state-dir", t.TempDir()}, {"-state-dir", filepath.Join(t.TempDir(), "missing")}, {"unexpected"}} {
		var stdout bytes.Buffer
		if err := runPairingCodeCommand(t.Context(), args, &stdout, io.Discard); err == nil || stdout.Len() != 0 {
			t.Fatal("unavailable daemon or invalid invocation returned success")
		}
	}
	p, _ := startPairingControlTest(t, nil)
	addressPath := filepath.Join(p.stateDir, pairingControlFileName)
	address, err := os.ReadFile(addressPath) //nolint:gosec // test-owned state directory
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	for _, resource := range p.owned {
		if _, err := os.Lstat(resource.path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("normal shutdown retained a control resource: %v", err)
		}
	}
	// A crash may leave an advertisement. It cannot mint and a replacement
	// daemon may publish its fresh socket after acquiring the released lock.
	if err := os.WriteFile(addressPath, address, 0o600); err != nil { //nolint:gosec // restore the test-owned stale advertisement
		t.Fatal(err)
	}
	if err := runPairingCodeCommand(t.Context(), []string{"-state-dir", p.stateDir}, io.Discard, io.Discard); err == nil {
		t.Fatal("stale endpoint returned success")
	}
	replacement, err := newPairingControl(p.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.publish(); err != nil {
		t.Fatal(err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPairingControlCreatesStateOnlyOnDaemonStartup(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "fresh", "state")
	if err := runPairingCodeCommand(t.Context(), []string{"-state-dir", stateDir}, io.Discard, io.Discard); err == nil {
		t.Fatal("command accepted an absent daemon state directory")
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("command created state")
	}
	p, err := newPairingControl(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(stateDir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatal("daemon did not create and preserve a private state directory")
	}
}

func TestPairingControlCleanupPreservesReplacements(t *testing.T) {
	p, _ := startPairingControlTest(t, nil)
	socketPath := p.listener.Addr().String()
	movedSocket := socketPath + ".retained"
	if err := os.Rename(socketPath, movedSocket); err != nil {
		t.Fatal(err)
	}
	replacement, err := net.ListenUnix("unix", &net.UnixAddr{Name: socketPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = replacement.Close()
		_ = os.Remove(movedSocket)
		_ = os.Remove(filepath.Dir(socketPath))
	})
	addressPath := filepath.Join(p.stateDir, pairingControlFileName)
	if err := os.Rename(addressPath, addressPath+".retained"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(addressPath, []byte("replacement advertisement"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The owned directory remains nonempty. No recursive cleanup is allowed.
	if err := p.Close(); err == nil {
		t.Fatal("expected refusal to remove a nonempty runtime directory")
	}
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("cleanup removed another listener: %v", err)
	}
	_ = conn.Close()
	body, err := os.ReadFile(addressPath) //nolint:gosec // test-owned replacement advertisement
	if err != nil || string(body) != "replacement advertisement" {
		t.Fatal("cleanup removed or changed the replacement advertisement")
	}
}
