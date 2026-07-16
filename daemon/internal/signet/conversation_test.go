package signet_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// convFixture is the discuss-transaction test bed (§5.14 tests 5, 6, 7, 10,
// 12): a service over a store at a fixed path (so a test can close and
// reopen it, the in-process restart), a blob store, and one open
// spec_approval item offering discuss. The fixture's paths persist across
// reopenConversationService, which is what makes "the daemon dies" a
// reconstruction instead of a fresh world.
type convFixture struct {
	service *signet.Service
	store   *store.Store
	blobs   *signet.BlobStore
	item    domain.AttentionItem
	dbPath  string
	blobDir string
	now     *time.Time
	// close shuts the store down early: "the daemon dies" in test 5. The
	// fixture's cleanup is close-once, so a test may call it explicitly.
	close func()
}

func newConversationFixture(t *testing.T) *convFixture {
	t.Helper()
	dir := t.TempDir()
	f := openConversationService(t, dir+"/signet.db", dir+"/blobs")

	runID := domain.RunID("run-1")
	start := *f.now
	expires := start.Add(24 * time.Hour)
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-d", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID},
		Type:    domain.AttentionSpecApproval, Priority: domain.PriorityNormal,
		Reason:            "the run's spec awaits approval",
		RequestedDecision: []domain.Action{domain.ActionApprove, domain.ActionDiscuss, domain.ActionStop},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionPlannedGate,
		ExpiresWhen:       &expires, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	ctx := context.Background()
	if err := f.service.PutItem(ctx, item); err != nil {
		t.Fatalf("seed item: %v", err)
	}
	device := domain.Device{
		ID: "device-1", DisplayName: "Ben's iPhone",
		Status: domain.DeviceActive, PairedAt: start,
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error { return tx.PutDevice(ctx, device) }); err != nil {
		t.Fatalf("seed device: %v", err)
	}
	f.item = item
	return f
}

// openConversationService opens (or reopens) the service over the given
// store and blob paths: the second call against the same paths is the
// in-process restart boundary §5.14 test 5 exercises.
func openConversationService(t *testing.T, dbPath, blobDir string) *convFixture {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, dbPath, store.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if closed {
			return
		}
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		closed = true
	})
	blobs, err := signet.NewBlobStore(blobDir)
	if err != nil {
		t.Fatalf("NewBlobStore: %v", err)
	}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	f := &convFixture{store: s, blobs: blobs, dbPath: dbPath, blobDir: blobDir, now: &now}
	f.service = signet.NewService(s,
		signet.WithPairingKey(testPairingKey),
		signet.WithBlobStore(blobs),
		signet.WithClock(func() time.Time { return now }),
	)
	f.close = func() {
		if closed {
			return
		}
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
		closed = true
	}
	return f
}

// discussCommand binds a discuss submission to the fixture item's state at
// the given expected entity version.
func (f *convFixture) discussCommand(commandID, message string, attachments []domain.Digest, expectedVersion int64, itemVersion int) signet.ClientCommand {
	return signet.ClientCommand{
		CommandID: commandID, DeviceID: "device-1", ExpectedEntityVersion: expectedVersion,
		Payload: signet.DecisionPayload{
			ItemID: f.item.ID, Action: domain.ActionDiscuss, ItemVersion: itemVersion,
			PRHeadSHA: f.item.PRHeadSHA, ArtifactDigests: f.item.ArtifactDigests,
			Message: message, Attachments: attachments,
		},
	}
}

func (f *convFixture) revision(t *testing.T) int64 {
	t.Helper()
	state, err := f.store.ServerState(context.Background())
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	return state.Revision
}

func (f *convFixture) conversationSnapshot(t *testing.T, id domain.ConversationID) signet.ConversationSnapshot {
	t.Helper()
	snapshot, err := f.service.GetConversation(context.Background(), id)
	if err != nil {
		t.Fatalf("GetConversation(%s): %v", id, err)
	}
	return snapshot
}

// completeInvocation scripts, dispatches, and drives the fake driver until
// the invocation's result is collectable, returning it. The driver
// progression is call-step-counted (fake doc), so a bounded Inspect loop is
// deterministic, not a wait.
func completeInvocation(t *testing.T, f *convFixture, driver *fake.StageDriver, id domain.InvocationID, summary string, artifacts []domain.Digest) exec.StageResult {
	t.Helper()
	ctx := context.Background()
	driver.Script(id, fake.StageScript{
		RunningInspects: 1,
		Outcome:         fake.OutcomeComplete,
		Result:          exec.StageResult{Summary: summary, Artifacts: artifacts},
	})
	dispatched, err := f.service.DispatchPendingInvocations(ctx, driver)
	if err != nil {
		t.Fatalf("DispatchPendingInvocations: %v", err)
	}
	if dispatched != 1 {
		t.Fatalf("dispatched = %d, want 1", dispatched)
	}
	for range 4 {
		status, err := driver.Inspect(ctx, id)
		if err != nil {
			t.Fatalf("Inspect: %v", err)
		}
		if status == exec.StatusCompleted {
			break
		}
	}
	result, err := driver.Collect(ctx, id)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return result
}

// TestDiscussRecoverySingleAcceptedResult is §5.14 test 5: discuss commits
// and the daemon dies pre-invocation; recovery produces exactly one accepted
// invocation result and the workflow does not advance twice.
func TestDiscussRecoverySingleAcceptedResult(t *testing.T) {
	ctx := context.Background()
	f := newConversationFixture(t)
	driverDir := t.TempDir()

	result, err := f.service.Submit(ctx, f.discussCommand("cmd-d1", "why does the retry back off twice?", nil, 1, 1))
	if err != nil {
		t.Fatalf("Submit(discuss): %v", err)
	}
	if result.Record.Action != domain.ActionDiscuss {
		t.Fatalf("recorded action = %q", result.Record.Action)
	}

	// The daemon dies before any dispatch: close the store with the
	// committed intent still pending, then reconstruct service and driver
	// from the same paths.
	f.close()
	f2 := openConversationService(t, f.dbPath, f.blobDir)
	f2.item = f.item
	driver, err := fake.NewStageDriverAt(driverDir)
	if err != nil {
		t.Fatalf("NewStageDriverAt: %v", err)
	}

	invocationID := domain.InvocationID("inv-cmd-d1")
	collected := completeInvocation(t, f2, driver, invocationID, "the backoff doubles by design", nil)
	if collected.Status != exec.StatusCompleted {
		t.Fatalf("collected status = %q", collected.Status)
	}

	// A second recovery scan finds nothing pending, and the driver's durable
	// intent rejects a direct duplicate start: one committed invocation.
	dispatched, err := f2.service.DispatchPendingInvocations(ctx, driver)
	if err != nil {
		t.Fatalf("second dispatch: %v", err)
	}
	if dispatched != 0 {
		t.Fatalf("second dispatch = %d intents, want 0", dispatched)
	}
	if err := driver.Start(ctx, invocationID, exec.StartSpec{}); !errors.Is(err, exec.ErrDuplicateStart) {
		t.Fatalf("duplicate Start = %v, want ErrDuplicateStart", err)
	}

	if err := f2.service.AcceptAgentCompletion(ctx, invocationID, signet.AgentReply{Body: collected.Summary}); err != nil {
		t.Fatalf("AcceptAgentCompletion: %v", err)
	}
	afterAccept := f2.revision(t)
	// The workflow must not advance twice: a duplicate acceptance is a
	// converging no-op, appending nothing and bumping no revision.
	if err := f2.service.AcceptAgentCompletion(ctx, invocationID, signet.AgentReply{Body: collected.Summary}); err != nil {
		t.Fatalf("duplicate AcceptAgentCompletion: %v", err)
	}
	if again := f2.revision(t); again != afterAccept {
		t.Fatalf("duplicate acceptance moved the revision %d → %d", afterAccept, again)
	}

	snapshot := f2.conversationSnapshot(t, "conv-item-d")
	if len(snapshot.Conversation.Messages) != 2 {
		t.Fatalf("messages = %d, want 2 (one user turn, one accepted agent result)", len(snapshot.Conversation.Messages))
	}
	if snapshot.Conversation.Status != domain.ConversationIdle {
		t.Fatalf("conversation status = %q, want idle", snapshot.Conversation.Status)
	}
	if err := f2.store.Read(ctx, func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(ctx, f.item.ID)
		if err != nil {
			return err
		}
		if item.ItemVersion != 3 {
			t.Errorf("item version = %d, want 3 (discuss supersede + completion replacement)", item.ItemVersion)
		}
		if item.Status != domain.StatusOpen {
			t.Errorf("item status = %q, want open", item.Status)
		}
		return nil
	}); err != nil {
		t.Fatalf("read item: %v", err)
	}
}

// TestAgentResponseRetrievedByBothClients is §5.14 test 6: an agent response
// arriving with both clients closed is later retrieved by both as the same
// ordered thread.
func TestAgentResponseRetrievedByBothClients(t *testing.T) {
	ctx := context.Background()
	f := newConversationFixture(t)

	blob := []byte("agent-diagram-bytes")
	digest := domain.Digest("sha256:" + hex.EncodeToString(sha256sum(blob)))
	if _, err := f.blobs.Put(digest, bytes.NewReader(blob)); err != nil {
		t.Fatalf("Put blob: %v", err)
	}

	if _, err := f.service.Submit(ctx, f.discussCommand("cmd-d1", "please explain the failure", nil, 1, 1)); err != nil {
		t.Fatalf("Submit(discuss): %v", err)
	}
	driver := fake.NewStageDriver()
	collected := completeInvocation(t, f, driver, "inv-cmd-d1", "the fixture clock was frozen", []domain.Digest{digest})
	// No client is connected while the completion lands; acceptance is
	// purely daemon-side.
	if err := f.service.AcceptAgentCompletion(ctx, "inv-cmd-d1", signet.AgentReply{Body: collected.Summary, Attachments: collected.Artifacts}); err != nil {
		t.Fatalf("AcceptAgentCompletion: %v", err)
	}

	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(ctx, domain.Device{
			ID: "device-2", DisplayName: "Ben's Mac",
			Status: domain.DeviceActive, PairedAt: *f.now,
		})
	}); err != nil {
		t.Fatalf("seed device-2: %v", err)
	}
	handler := signet.NewHTTPHandler(f.service, headerDeviceAuthorizer)

	read := func(device string) []byte {
		req := httptest.NewRequest(http.MethodGet, "/conversations/conv-item-d", nil)
		req.Header.Set("X-Test-Device", device)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET conversation as %s = %d: %s", device, rec.Code, rec.Body.String())
		}
		return rec.Body.Bytes()
	}
	first := read("device-1")
	second := read("device-2")
	if !bytes.Equal(first, second) {
		t.Fatalf("devices read different threads:\n%s\n%s", first, second)
	}
	snapshot := f.conversationSnapshot(t, "conv-item-d")
	if got := len(snapshot.Conversation.Messages); got != 2 {
		t.Fatalf("messages = %d, want 2", got)
	}
	for want, m := range snapshot.Conversation.Messages {
		if m.Sequence != want+1 {
			t.Fatalf("message %d sequence = %d, want %d", want, m.Sequence, want+1)
		}
	}
	if got := snapshot.Conversation.Messages[1].Attachments; len(got) != 1 || got[0] != digest {
		t.Fatalf("agent message attachments = %v, want [%s]", got, digest)
	}
}

// headerDeviceAuthorizer authenticates whatever device the X-Test-Device
// header names: the two-device read path of §5.14 test 6.
func headerDeviceAuthorizer(r *http.Request) (domain.DeviceID, bool) {
	device := r.Header.Get("X-Test-Device")
	if device == "" {
		return "", false
	}
	return domain.DeviceID(device), true
}

func sha256sum(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// TestConcurrentDiscussSingleWinner is §5.14 test 7: concurrent discuss on
// one item version — one wins, no second accepted result.
func TestConcurrentDiscussSingleWinner(t *testing.T) {
	ctx := context.Background()
	f := newConversationFixture(t)

	if _, err := f.service.Submit(ctx, f.discussCommand("cmd-c1", "first question", nil, 1, 1)); err != nil {
		t.Fatalf("Submit(first discuss): %v", err)
	}

	// The second device prepared against the same item version; the
	// discuss supersede already advanced it, so the loser is rejected with
	// the replacement and no side effect.
	loser := f.discussCommand("cmd-c2", "second question", nil, 1, 1)
	loser.DeviceID = "device-2"
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutDevice(ctx, domain.Device{
			ID: "device-2", DisplayName: "Ben's Mac",
			Status: domain.DeviceActive, PairedAt: *f.now,
		})
	}); err != nil {
		t.Fatalf("seed device-2: %v", err)
	}
	beforeLoser := f.revision(t)
	_, err := f.service.Submit(ctx, loser)
	var stale *signet.StaleVersionError
	if !errors.As(err, &stale) {
		t.Fatalf("second discuss error = %v, want StaleVersionError", err)
	}
	if stale.Replacement.ItemVersion != 2 {
		t.Fatalf("replacement version = %d, want 2", stale.Replacement.ItemVersion)
	}
	if stale.Replacement.ConversationID == nil {
		t.Fatal("replacement carries no conversation binding")
	}
	if after := f.revision(t); after != beforeLoser {
		t.Fatalf("losing discuss moved the revision %d → %d", beforeLoser, after)
	}

	snapshot := f.conversationSnapshot(t, "conv-item-d")
	if got := len(snapshot.Conversation.Messages); got != 1 {
		t.Fatalf("messages = %d, want 1 (single winner)", got)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetAgentInvocation(ctx, "inv-cmd-c2"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("loser invocation persisted: %v", err)
		}
		if _, err := tx.GetCommand(ctx, "cmd-c2"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("loser command persisted: %v", err)
		}
		pending, err := tx.ListPendingOutbox(ctx, "agent_invocation_requested")
		if err != nil {
			return err
		}
		if len(pending) != 1 {
			t.Errorf("pending intents = %d, want 1", len(pending))
		}
		return nil
	}); err != nil {
		t.Fatalf("read: %v", err)
	}
}

// TestRetriedAttachmentAndMessageConverge is §5.14 test 10: a retried
// attachment or message yields one artifact and one message.
func TestRetriedAttachmentAndMessageConverge(t *testing.T) {
	ctx := context.Background()
	f := newConversationFixture(t)
	handler := signet.NewHTTPHandler(f.service, headerDeviceAuthorizer)

	blob := []byte("screenshot-bytes")
	digest := "sha256:" + hex.EncodeToString(sha256sum(blob))

	put := func(path string, body []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, path, bytes.NewReader(body))
		req.Header.Set("X-Test-Device", "device-1")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	blobCount := func() int {
		entries, err := os.ReadDir(f.blobDir)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		count := 0
		for _, e := range entries {
			if !strings.HasPrefix(e.Name(), "tmp-") {
				count++
			}
		}
		return count
	}

	if rec := put("/attachments/"+digest, blob); rec.Code != http.StatusCreated {
		t.Fatalf("first PUT = %d: %s", rec.Code, rec.Body.String())
	}
	retried := put("/attachments/"+digest, blob)
	if retried.Code != http.StatusOK {
		t.Fatalf("retried PUT = %d: %s", retried.Code, retried.Body.String())
	}
	if want := fmt.Sprintf("{\"digest\":%q}\n", digest); retried.Body.String() != want {
		t.Fatalf("retried receipt = %q, want %q", retried.Body.String(), want)
	}
	if got := blobCount(); got != 1 {
		t.Fatalf("stored blobs = %d, want 1 (retry converged)", got)
	}
	if rec := put("/attachments/"+digest, []byte("different bytes")); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatched PUT = %d, want 422", rec.Code)
	}
	if got := blobCount(); got != 1 {
		t.Fatalf("stored blobs after mismatch = %d, want 1", got)
	}

	// The message half: a retried discuss under the same command_id returns
	// the original committed result, appends nothing, and moves no revision.
	command := f.discussCommand("cmd-a1", "see the attached screenshot", []domain.Digest{domain.Digest(digest)}, 1, 1)
	first, err := f.service.Submit(ctx, command)
	if err != nil {
		t.Fatalf("Submit(discuss): %v", err)
	}
	afterFirst := f.revision(t)
	replayed, err := f.service.Submit(ctx, command)
	if err != nil {
		t.Fatalf("Submit(replay): %v", err)
	}
	if marshal(t, replayed) != marshal(t, first) {
		t.Fatalf("replay result differs:\n%s\n%s", marshal(t, replayed), marshal(t, first))
	}
	if after := f.revision(t); after != afterFirst {
		t.Fatalf("replay moved the revision %d → %d", afterFirst, after)
	}
	snapshot := f.conversationSnapshot(t, "conv-item-d")
	if got := len(snapshot.Conversation.Messages); got != 1 {
		t.Fatalf("messages = %d, want 1 (retry converged)", got)
	}
	if got := snapshot.Conversation.Messages[0].Attachments; len(got) != 1 || got[0] != domain.Digest(digest) {
		t.Fatalf("message attachments = %v, want [%s]", got, digest)
	}
}

// TestConversationStatusChangeReachesCaughtUpClient is §5.14 test 12: a
// conversation status change reaches a client that had already fetched past
// that sequence. Status rides entity_version and the server revision, not
// the message sequence, so a client holding every message still learns of
// the flip through the ordinary revision heartbeat and snapshot refetch.
func TestConversationStatusChangeReachesCaughtUpClient(t *testing.T) {
	ctx := context.Background()
	f := newConversationFixture(t)

	if _, err := f.service.Submit(ctx, f.discussCommand("cmd-d1", "thinking out loud", nil, 1, 1)); err != nil {
		t.Fatalf("Submit(discuss): %v", err)
	}

	// The client fetches the whole conversation: its message cursor is past
	// every stored sequence, and it saw awaiting_agent.
	cached := f.conversationSnapshot(t, "conv-item-d")
	if cached.Conversation.Status != domain.ConversationAwaitingAgent {
		t.Fatalf("cached status = %q, want awaiting_agent", cached.Conversation.Status)
	}
	cachedRevision := f.revision(t)

	driver := fake.NewStageDriver()
	collected := completeInvocation(t, f, driver, "inv-cmd-d1", "resolved", nil)
	if err := f.service.AcceptAgentCompletion(ctx, "inv-cmd-d1", signet.AgentReply{Body: collected.Summary}); err != nil {
		t.Fatalf("AcceptAgentCompletion: %v", err)
	}

	// The heartbeat the client polls (GET /sync/revision) reports a higher
	// revision even though the client's cursor covered every sequence it
	// knew, and the refetched snapshot carries the flip.
	if after := f.revision(t); after <= cachedRevision {
		t.Fatalf("revision did not advance past %d", cachedRevision)
	}
	refetched := f.conversationSnapshot(t, "conv-item-d")
	if refetched.Conversation.Status != domain.ConversationIdle {
		t.Fatalf("refetched status = %q, want idle", refetched.Conversation.Status)
	}
	if refetched.EntityVersion <= cached.EntityVersion {
		t.Fatalf("entity_version did not advance (%d → %d)", cached.EntityVersion, refetched.EntityVersion)
	}
}

// TestDiscussRejectedWhileAgentReplyPending: one outstanding agent turn per
// conversation. A second discuss prepared against the superseding entity
// version, while the reply is in flight, is rejected with the current item
// and no side effect; once the reply is accepted the conversation is idle
// and discuss works again.
func TestDiscussRejectedWhileAgentReplyPending(t *testing.T) {
	ctx := context.Background()
	f := newConversationFixture(t)

	if _, err := f.service.Submit(ctx, f.discussCommand("cmd-d1", "first question", nil, 1, 1)); err != nil {
		t.Fatalf("Submit(first discuss): %v", err)
	}

	// The client refetched: entity version 2, item version 2, reply pending.
	followUp := f.discussCommand("cmd-d2", "second thought", nil, 2, 2)
	before := f.revision(t)
	_, err := f.service.Submit(ctx, followUp)
	var pending *signet.AgentPendingError
	if !errors.As(err, &pending) {
		t.Fatalf("mid-turn discuss error = %v, want AgentPendingError", err)
	}
	if !errors.Is(err, signet.ErrAgentReplyPending) {
		t.Fatalf("errors.Is(ErrAgentReplyPending) = false for %v", err)
	}
	if after := f.revision(t); after != before {
		t.Fatalf("mid-turn discuss moved the revision %d → %d", before, after)
	}
	snapshot := f.conversationSnapshot(t, "conv-item-d")
	if got := len(snapshot.Conversation.Messages); got != 1 {
		t.Fatalf("messages = %d, want 1 (no mid-turn append)", got)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetCommand(ctx, "cmd-d2"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("mid-turn command persisted: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("read: %v", err)
	}

	// After the reply lands the conversation is idle again and the follow-up
	// commits (entity version 3: discuss supersede + completion replacement).
	driver := fake.NewStageDriver()
	collected := completeInvocation(t, f, driver, "inv-cmd-d1", "answered", nil)
	if err := f.service.AcceptAgentCompletion(ctx, "inv-cmd-d1", signet.AgentReply{Body: collected.Summary}); err != nil {
		t.Fatalf("AcceptAgentCompletion: %v", err)
	}
	retried := f.discussCommand("cmd-d3", "second thought", nil, 3, 3)
	if _, err := f.service.Submit(ctx, retried); err != nil {
		t.Fatalf("post-reply discuss rejected: %v", err)
	}
}

// TestCompletionRedeliveryConvergesBeforeAttachmentChecks: idempotency is
// judged first, as at the command boundary. A redelivery of an accepted
// completion converges as a no-op even when its payload now carries a
// malformed or missing attachment; a genuinely new completion with a missing
// attachment still fails loudly.
func TestCompletionRedeliveryConvergesBeforeAttachmentChecks(t *testing.T) {
	ctx := context.Background()
	f := newConversationFixture(t)

	if _, err := f.service.Submit(ctx, f.discussCommand("cmd-d1", "question", nil, 1, 1)); err != nil {
		t.Fatalf("Submit(discuss): %v", err)
	}
	driver := fake.NewStageDriver()
	collected := completeInvocation(t, f, driver, "inv-cmd-d1", "answer", nil)
	if err := f.service.AcceptAgentCompletion(ctx, "inv-cmd-d1", signet.AgentReply{Body: collected.Summary}); err != nil {
		t.Fatalf("AcceptAgentCompletion: %v", err)
	}
	before := f.revision(t)

	// Redelivery with a never-stored attachment: already accepted, so it
	// converges instead of surfacing the attachment error.
	redelivered := signet.AgentReply{Body: collected.Summary, Attachments: []domain.Digest{domain.Digest("sha256:" + strings.Repeat("11", 32))}}
	if err := f.service.AcceptAgentCompletion(ctx, "inv-cmd-d1", redelivered); err != nil {
		t.Fatalf("redelivered completion = %v, want converging no-op", err)
	}
	if after := f.revision(t); after != before {
		t.Fatalf("redelivery moved the revision %d → %d", before, after)
	}

	// A genuinely new completion referencing a missing attachment fails
	// loudly and leaves no partial state.
	if _, err := f.service.Submit(ctx, f.discussCommand("cmd-d2", "again", nil, 3, 3)); err != nil {
		t.Fatalf("Submit(second discuss): %v", err)
	}
	fresh := signet.AgentReply{Body: "x", Attachments: []domain.Digest{domain.Digest("sha256:" + strings.Repeat("22", 32))}}
	if err := f.service.AcceptAgentCompletion(ctx, "inv-cmd-d2", fresh); !errors.Is(err, signet.ErrAttachmentNotStored) {
		t.Fatalf("new completion with missing attachment = %v, want ErrAttachmentNotStored", err)
	}
	snapshot := f.conversationSnapshot(t, "conv-item-d")
	if got := len(snapshot.Conversation.Messages); got != 3 {
		t.Fatalf("messages = %d, want 3 (failed acceptance appended nothing)", got)
	}
}

// TestCompletionAfterTerminalDecisionAppendsWithoutReplacement: the user may
// conclude the superseding item (stop/approve stay offered) while the reply
// is in flight. The late completion still lands in the thread (an inbound
// fact, durable per test 6) and idles the conversation, but a terminal
// decision is final: no replacement version is written past it.
func TestCompletionAfterTerminalDecisionAppendsWithoutReplacement(t *testing.T) {
	ctx := context.Background()
	f := newConversationFixture(t)

	if _, err := f.service.Submit(ctx, f.discussCommand("cmd-d1", "question", nil, 1, 1)); err != nil {
		t.Fatalf("Submit(discuss): %v", err)
	}
	// The user concludes the superseding version before the reply arrives.
	stop := f.discussCommand("cmd-stop", "", nil, 2, 2)
	stop.Payload.Action = domain.ActionStop
	stop.Payload.Message = ""
	if _, err := f.service.Submit(ctx, stop); err != nil {
		t.Fatalf("Submit(stop): %v", err)
	}

	driver := fake.NewStageDriver()
	collected := completeInvocation(t, f, driver, "inv-cmd-d1", "late answer", nil)
	if err := f.service.AcceptAgentCompletion(ctx, "inv-cmd-d1", signet.AgentReply{Body: collected.Summary}); err != nil {
		t.Fatalf("AcceptAgentCompletion: %v", err)
	}

	snapshot := f.conversationSnapshot(t, "conv-item-d")
	if got := len(snapshot.Conversation.Messages); got != 2 {
		t.Fatalf("messages = %d, want 2 (late reply still recorded)", got)
	}
	if snapshot.Conversation.Status != domain.ConversationIdle {
		t.Fatalf("conversation status = %q, want idle", snapshot.Conversation.Status)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		item, err := tx.GetAttentionItem(ctx, f.item.ID)
		if err != nil {
			return err
		}
		if item.Status != domain.StatusResolved {
			t.Errorf("item status = %q, want resolved (terminal decision is final)", item.Status)
		}
		if item.ItemVersion != 3 {
			t.Errorf("item version = %d, want 3 (no replacement past the terminal decision)", item.ItemVersion)
		}
		return nil
	}); err != nil {
		t.Fatalf("read item: %v", err)
	}
}

// TestMessageNamespacesDisjoint: message identity derives from a
// client-chosen command_id for user turns but from the daemon's invocation
// id for agent turns; without distinct role prefixes a crafted command_id
// ("inv-X") would collide with the agent reply's id and wedge the thread on
// the duplicate-ID gate.
func TestMessageNamespacesDisjoint(t *testing.T) {
	ctx := context.Background()
	f := newConversationFixture(t)

	if _, err := f.service.Submit(ctx, f.discussCommand("X", "first", nil, 1, 1)); err != nil {
		t.Fatalf("Submit(discuss X): %v", err)
	}
	driver := fake.NewStageDriver()
	collected := completeInvocation(t, f, driver, "inv-X", "reply", nil)
	if err := f.service.AcceptAgentCompletion(ctx, "inv-X", signet.AgentReply{Body: collected.Summary}); err != nil {
		t.Fatalf("AcceptAgentCompletion: %v", err)
	}

	// The adversarial follow-up: a command_id equal to the prior turn's
	// invocation id must still append cleanly.
	if _, err := f.service.Submit(ctx, f.discussCommand("inv-X", "follow-up", nil, 3, 3)); err != nil {
		t.Fatalf("Submit(discuss inv-X): %v", err)
	}
	snapshot := f.conversationSnapshot(t, "conv-item-d")
	if got := len(snapshot.Conversation.Messages); got != 3 {
		t.Fatalf("messages = %d, want 3", got)
	}
	seen := map[domain.MessageID]bool{}
	for _, m := range snapshot.Conversation.Messages {
		if seen[m.ID] {
			t.Fatalf("duplicate message id %q", m.ID)
		}
		seen[m.ID] = true
	}
}

// TestDispatchRejectsCorruptIntent: the decoded outbox payload is a
// reconstruction boundary. An intent whose payload does not name its own
// idempotency key is never started and never marked dispatched; the row
// stays pending as loud evidence.
func TestDispatchRejectsCorruptIntent(t *testing.T) {
	ctx := context.Background()
	f := newConversationFixture(t)

	if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.EnqueueOutbox(ctx, "inv-corrupt", "agent_invocation_requested",
			[]byte(`{"invocation_id":"inv-other","conversation_id":"conv-x","item_id":"item-d","item_version":2}`))
		return err
	}); err != nil {
		t.Fatalf("seed corrupt intent: %v", err)
	}

	driver := fake.NewStageDriver()
	if _, err := f.service.DispatchPendingInvocations(ctx, driver); err == nil {
		t.Fatal("corrupt intent dispatched, want error")
	}
	if _, err := driver.Inspect(ctx, "inv-other"); !errors.Is(err, exec.ErrUnknownInvocation) {
		t.Fatalf("foreign id reached the driver: %v", err)
	}
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		pending, err := tx.ListPendingOutbox(ctx, "agent_invocation_requested")
		if err != nil {
			return err
		}
		if len(pending) != 1 || pending[0].IdempotencyKey != "inv-corrupt" {
			t.Errorf("pending = %v, want the corrupt row still pending", pending)
		}
		return nil
	}); err != nil {
		t.Fatalf("read: %v", err)
	}
}

// TestHTTPDiscussCommand covers the wire path of the widened payload: the
// optional message/attachments fields decode into the acceptance policy, a
// discuss submission commits over POST /commands, and the content-policy
// rejections map to 400.
func TestHTTPDiscussCommand(t *testing.T) {
	f := newConversationFixture(t)
	handler := signet.NewHTTPHandler(f.service, headerDeviceAuthorizer)

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/commands", strings.NewReader(body))
		req.Header.Set("X-Test-Device", "device-1")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	payload := func(commandID, action, extra string) string {
		return fmt.Sprintf(`{
			"command_id": %q, "device_id": "device-1",
			"expected_entity_version": 1, "expected_bindings": {},
			"payload": {"item_id": "item-d", "action": %q, "item_version": 1,
				"pr_head_sha": "", "artifact_digests": []%s}
		}`, commandID, action, extra)
	}

	if rec := post(payload("cmd-h1", "discuss", `, "message": "over the wire"`)); rec.Code != http.StatusOK {
		t.Fatalf("discuss over HTTP = %d: %s", rec.Code, rec.Body.String())
	}
	var result struct {
		Record struct {
			Message     string          `json:"message"`
			Attachments []domain.Digest `json:"attachments"`
		} `json:"record"`
	}
	rec := post(payload("cmd-h1", "discuss", `, "message": "over the wire"`))
	if rec.Code != http.StatusOK {
		t.Fatalf("replayed discuss over HTTP = %d: %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.Record.Message != "over the wire" {
		t.Fatalf("recorded message = %q", result.Record.Message)
	}
	if result.Record.Attachments == nil || len(result.Record.Attachments) != 0 {
		t.Fatalf("recorded attachments = %v, want []", result.Record.Attachments)
	}

	// Content-policy rejections are request errors: 400, never 500.
	if rec := post(payload("cmd-h2", "discuss", "")); rec.Code != http.StatusBadRequest {
		t.Fatalf("messageless discuss = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if rec := post(payload("cmd-h3", "stop", `, "message": "stray"`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("stop with message = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if rec := post(payload("cmd-h4", "discuss", `, "message": "x", "attachments": ["sha256:`+strings.Repeat("00", 32)+`"]`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("discuss with unstored attachment = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	// A malformed digest is the client's error, never a 500: the blob-store
	// gate's rejection maps to a request error like the unstored case.
	if rec := post(payload("cmd-h5", "discuss", `, "message": "x", "attachments": ["sha256:NOT-HEX"]`)); rec.Code != http.StatusBadRequest {
		t.Fatalf("discuss with malformed digest = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// TestBlobStoreRejectsMalformedDigests enumerates the digest input space
// (case, length, prefix, separators, traversal) at the path-construction
// trust boundary: only sha256:<64 lowercase hex> ever reaches the
// filesystem.
func TestBlobStoreRejectsMalformedDigests(t *testing.T) {
	f := newConversationFixture(t)
	valid := strings.Repeat("ab", 32)
	cases := []string{
		"",
		"sha256:",
		"sha256:" + valid[:62],
		"sha256:" + valid + "ff",
		"sha256:" + strings.ToUpper(valid),
		"SHA256:" + valid,
		"sha512:" + valid,
		valid,
		"sha256:" + valid[:60] + "/../x",
		"sha256:" + valid[:63] + "É",
		"sha256: " + valid[:63],
		"sha256:" + valid[:63] + " ",
		"sha256:" + valid[:63] + "\n",
	}
	for _, digest := range cases {
		if _, err := f.blobs.Put(domain.Digest(digest), strings.NewReader("x")); !errors.Is(err, signet.ErrInvalidDigest) {
			t.Errorf("Put(%q) = %v, want ErrInvalidDigest", digest, err)
		}
		if _, err := f.blobs.Has(domain.Digest(digest)); !errors.Is(err, signet.ErrInvalidDigest) {
			t.Errorf("Has(%q) = %v, want ErrInvalidDigest", digest, err)
		}
		if _, err := f.blobs.Open(domain.Digest(digest)); !errors.Is(err, signet.ErrInvalidDigest) {
			t.Errorf("Open(%q) = %v, want ErrInvalidDigest", digest, err)
		}
	}
	entries, err := os.ReadDir(f.blobDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "tmp-") {
			t.Errorf("malformed digest created %q", e.Name())
		}
	}
}
