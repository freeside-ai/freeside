package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

const killTestCommandID = "discuss-kill-recovery"

var killTestInvocationID = domain.InvocationID("inv-" + killTestCommandID)

type processFixture struct {
	cmd    *osexec.Cmd
	stderr bytes.Buffer
	ready  readiness
	done   chan struct{}

	waitErr error
}

// TestDaemonRecoversAcrossSIGKILL is #41's real-process recovery matrix. The
// in-process engine tests prove the same durable states; this fixture proves
// those states survive abrupt OS process death with the real freesided binary,
// one SQLite store, and one file-backed permanent fake.
func TestDaemonRecoversAcrossSIGKILL(t *testing.T) {
	binary := buildKillTestDaemon(t)
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ntfy.Close)

	tests := []struct {
		name            string
		checkpoint      string
		acceptedResults int
		restartFails    bool
	}{
		{"before intent dispatch", killCheckpointBeforeIntentDispatch, 1, false},
		{"after accepted intent before result", killCheckpointAfterIntentAccepted, 0, true},
		{"after committed result before local acceptance", killCheckpointAfterResultCommitted, 1, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			marker := filepath.Join(root, "kill-checkpoint")
			first := startProcessFixture(t, binary, root, ntfy.URL, tc.checkpoint, marker)
			client := pairProcessDevice(t, first.ready)
			approval := client.waitForItem(t, domain.ItemID("approval-"+string(defaultFakeRunID)))
			client.submit(t, "approve-kill-recovery", approval, domain.ActionApprove, "")
			feedback := client.waitForItem(t, domain.ItemID("feedback-"+string(defaultFakeRunID)))
			client.submit(t, killTestCommandID, feedback, domain.ActionDiscuss, "exercise restart recovery")

			waitForMarker(t, marker, tc.checkpoint)
			first.kill(t)

			restarted := startProcessFixture(t, binary, root, ntfy.URL, "", "")
			client.baseURL = restarted.ready.APIURL
			if tc.restartFails {
				restarted.waitForFailure(t, "invocation ended without an accepted result")
			} else {
				client.waitForAcceptedResult(t)
				restarted.stop(t)
			}
			assertKillRecoveryState(t, root, tc.acceptedResults)
		})
	}
}

func buildKillTestDaemon(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve kill-test source path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	binary := filepath.Join(t.TempDir(), "freesided-kill-test")
	args := []string{"build", "-tags=freeside_kill_test", "-o", binary}
	args = append(args, raceBuildFlags()...)
	args = append(args, "./cmd/freesided")
	cmd := osexec.Command("go", args...) //nolint:gosec // fixed tool and test-owned output
	cmd.Dir = moduleRoot
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build kill-test freesided: %v\n%s", err, output)
	}
	return binary
}

func startProcessFixture(
	t *testing.T,
	binary, root, ntfyURL, checkpoint, marker string,
) *processFixture {
	t.Helper()
	args := []string{
		"-db", filepath.Join(root, "freeside.db"),
		"-fake-driver-dir", filepath.Join(root, "driver"),
		"-listen", "127.0.0.1:0",
		"-ntfy-url", ntfyURL,
		"-reconcile-interval", "5ms",
	}
	p := &processFixture{
		cmd:  osexec.Command(binary, args...), //nolint:gosec // test-built binary with fixed arguments
		done: make(chan struct{}),
	}
	p.cmd.Stderr = &p.stderr
	p.cmd.Env = os.Environ()
	if checkpoint != "" {
		p.cmd.Env = append(p.cmd.Env,
			killTestCheckpointEnv+"="+checkpoint,
			killTestMarkerEnv+"="+marker,
		)
	}
	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("freesided stdout pipe: %v", err)
	}
	if err := p.cmd.Start(); err != nil {
		t.Fatalf("start freesided: %v", err)
	}
	go func() {
		p.waitErr = p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		select {
		case <-p.done:
		default:
			_ = p.cmd.Process.Kill()
			<-p.done
		}
	})

	type readyResult struct {
		value readiness
		err   error
	}
	result := make(chan readyResult, 1)
	go func() {
		var ready readiness
		err := json.NewDecoder(stdout).Decode(&ready)
		result <- readyResult{value: ready, err: err}
	}()
	select {
	case decoded := <-result:
		if decoded.err != nil {
			_ = p.cmd.Process.Kill()
			<-p.done
			t.Fatalf("decode freesided readiness: %v; stderr=%s", decoded.err, p.stderr.String())
		}
		p.ready = decoded.value
	case <-time.After(5 * time.Second):
		t.Fatal("freesided did not emit readiness within 5s")
	}
	return p
}

func (p *processFixture) kill(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatalf("SIGKILL freesided: %v", err)
	}
	<-p.done
	if p.waitErr == nil {
		t.Fatal("SIGKILLed freesided exited successfully")
	}
}

func (p *processFixture) stop(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("interrupt freesided: %v", err)
	}
	<-p.done
	if p.waitErr != nil {
		t.Fatalf("stop freesided: %v; stderr=%s", p.waitErr, p.stderr.String())
	}
}

func (p *processFixture) waitForFailure(t *testing.T, want string) {
	t.Helper()
	select {
	case <-p.done:
		if p.waitErr == nil {
			t.Fatal("restarted freesided unexpectedly exited successfully")
		}
		if !strings.Contains(p.stderr.String(), want) {
			t.Fatalf("restarted freesided stderr = %q, want %q", p.stderr.String(), want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("restarted freesided did not report the lost invocation within 5s")
	}
}

type processClient struct {
	baseURL  string
	token    string
	deviceID domain.DeviceID
	client   *http.Client
}

func pairProcessDevice(t *testing.T, ready readiness) *processClient {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"pairing_code": ready.PairingCode,
		"display_name": "Kill recovery fixture",
	})
	if err != nil {
		t.Fatalf("marshal pairing request: %v", err)
	}
	client := &http.Client{Timeout: time.Second}
	response, err := client.Post(ready.APIURL+"/pairing", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("pair process device: %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusCreated {
		payload, _ := io.ReadAll(response.Body)
		t.Fatalf("pair process device status = %d body=%s", response.StatusCode, payload)
	}
	var grant signet.PairingGrant
	if err := json.NewDecoder(response.Body).Decode(&grant); err != nil {
		t.Fatalf("decode process pairing grant: %v", err)
	}
	return &processClient{
		baseURL: ready.APIURL, token: grant.DeviceToken,
		deviceID: grant.Device.Device.ID, client: client,
	}
}

func (c *processClient) waitForItem(t *testing.T, id domain.ItemID) signet.AttentionItemSnapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var item signet.AttentionItemSnapshot
		status := c.getJSON(t, "/attention/items/"+string(id), &item)
		if status == http.StatusOK {
			return item
		}
		if status != http.StatusNotFound {
			t.Fatalf("get item %q status = %d, want 200 or 404", id, status)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("item %q did not appear within 5s", id)
	return signet.AttentionItemSnapshot{}
}

func (c *processClient) submit(
	t *testing.T,
	commandID string,
	item signet.AttentionItemSnapshot,
	action domain.Action,
	message string,
) {
	t.Helper()
	digests := item.Item.ArtifactDigests
	if digests == nil {
		digests = []domain.Digest{}
	}
	payload := map[string]any{
		"item_id": item.Item.ID, "action": action, "item_version": item.Item.ItemVersion,
		"pr_head_sha": item.Item.PRHeadSHA, "artifact_digests": digests,
	}
	if message != "" {
		payload["message"] = message
	}
	body, err := json.Marshal(map[string]any{
		"command_id": commandID, "device_id": c.deviceID,
		"expected_entity_version": item.EntityVersion,
		"expected_bindings":       map[string]domain.Digest{},
		"payload":                 payload,
	})
	if err != nil {
		t.Fatalf("marshal %s command: %v", action, err)
	}
	request, err := http.NewRequest(http.MethodPost, c.baseURL+"/commands", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new %s command request: %v", action, err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		t.Fatalf("submit %s command: %v", action, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		responseBody, _ := io.ReadAll(response.Body)
		t.Fatalf("submit %s status = %d body=%s", action, response.StatusCode, responseBody)
	}
}

func (c *processClient) waitForAcceptedResult(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var item signet.AttentionItemSnapshot
		if c.getJSON(t, "/attention/items/feedback-"+string(defaultFakeRunID), &item) == http.StatusOK &&
			item.Item.ConversationID != nil {
			var conversation signet.ConversationSnapshot
			if c.getJSON(t, "/conversations/"+string(*item.Item.ConversationID), &conversation) == http.StatusOK &&
				len(conversation.Conversation.Messages) == 2 &&
				conversation.Conversation.Status == domain.ConversationIdle {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("restarted freesided did not accept the committed result within 5s")
}

func (c *processClient) getJSON(t *testing.T, path string, target any) int {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		t.Fatalf("new GET %s: %v", path, err)
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	response, err := c.client.Do(request)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(target); err != nil {
			t.Fatalf("decode GET %s: %v", path, err)
		}
	}
	return response.StatusCode
}

func waitForMarker(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		body, err := os.ReadFile(path) //nolint:gosec // test-owned marker path
		if err == nil {
			if string(body) != want {
				t.Fatalf("kill checkpoint = %q, want %q", body, want)
			}
			return
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read kill checkpoint: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("kill checkpoint %q did not appear within 5s", want)
}

func assertKillRecoveryState(t *testing.T, root string, acceptedResults int) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(root, "freeside.db")
	st, err := store.Open(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatalf("open recovered store: %v", err)
	}
	defer func() {
		if st != nil {
			if err := st.Close(); err != nil {
				t.Errorf("close recovered store: %v", err)
			}
		}
	}()

	var (
		run          domain.Run
		item         domain.AttentionItem
		conversation domain.Conversation
		invocation   domain.AgentInvocation
	)
	err = st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(ctx, defaultFakeRunID)
		if err != nil {
			return err
		}
		item, err = tx.GetAttentionItem(ctx, domain.ItemID("feedback-"+string(defaultFakeRunID)))
		if err != nil {
			return err
		}
		if item.ConversationID == nil {
			return errors.New("feedback item has no conversation")
		}
		conversation, err = tx.GetConversation(ctx, *item.ConversationID)
		if err != nil {
			return err
		}
		invocation, err = tx.GetAgentInvocation(ctx, killTestInvocationID)
		return err
	})
	if err != nil {
		t.Fatalf("read recovered workflow: %v", err)
	}
	if invocation.ID != killTestInvocationID {
		t.Fatalf("invocation id = %q, want %q", invocation.ID, killTestInvocationID)
	}
	if len(run.Stages) != 1 || len(run.Stages[0].Attempts) != 1 ||
		run.Stages[0].Attempts[0].InvocationID != killTestInvocationID {
		t.Fatalf("run attempts = %#v, want one attempt for %q", run.Stages, killTestInvocationID)
	}
	if item.ItemVersion != 2+acceptedResults {
		t.Fatalf("feedback item version = %d, want %d", item.ItemVersion, 2+acceptedResults)
	}
	wantMessages := 1 + acceptedResults
	if len(conversation.Messages) != wantMessages {
		t.Fatalf("conversation messages = %#v, want %d", conversation.Messages, wantMessages)
	}
	wantStatus := domain.ConversationAwaitingAgent
	if acceptedResults == 1 {
		wantStatus = domain.ConversationIdle
	}
	if conversation.Status != wantStatus {
		t.Fatalf("conversation status = %q, want %q", conversation.Status, wantStatus)
	}

	driver, err := fake.NewStageDriverAt(filepath.Join(root, "driver"))
	if err != nil {
		t.Fatalf("reopen permanent fake: %v", err)
	}
	err = driver.Start(ctx, killTestInvocationID, exec.StartSpec{
		RunID: defaultFakeRunID, StageID: domain.StageID("feedback-" + string(defaultFakeRunID)),
	})
	if !errors.Is(err, exec.ErrDuplicateStart) {
		t.Fatalf("re-start committed fake intent error = %v, want ErrDuplicateStart", err)
	}

	if err := st.Close(); err != nil {
		t.Fatalf("close store before ledger query: %v", err)
	}
	st = nil
	assertLedgerCount(t, dbPath, "outbox", "agent_invocation_requested", 1)
	assertLedgerCount(t, dbPath, "inbox", "agent_completion", acceptedResults)
}

func assertLedgerCount(t *testing.T, dbPath, table, kind string, want int) {
	t.Helper()
	if table != "inbox" && table != "outbox" {
		t.Fatalf("unsupported ledger table %q", table)
	}
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatalf("open %s ledger: %v", table, err)
	}
	defer func() { _ = db.Close() }()
	query := fmt.Sprintf("SELECT count(*) FROM %s WHERE idempotency_key = ? AND kind = ?", table) //nolint:gosec // table is allowlisted above
	var count int
	if err := db.QueryRow(query, string(killTestInvocationID), kind).Scan(&count); err != nil {
		t.Fatalf("count %s ledger: %v", table, err)
	}
	if count != want {
		t.Fatalf("%s ledger count = %d, want %d", table, count, want)
	}
}
