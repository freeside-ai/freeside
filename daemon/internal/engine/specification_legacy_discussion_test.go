package engine

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// A discussion marker the pre-rename daemon queued (#986) carries the
// retired identity family. The engine must keep driving that marker instead
// of minting a second one for the same accepted command: two markers start
// two provider invocations, and the loser's completion fails the reconcile
// pass with ErrParentKeyMismatch (#1100).
const (
	legacyDiscussionCommandID = "audit-discuss"
	legacyDiscussionMarkerKey = "elaboration-discussion-" + legacyDiscussionCommandID
	legacyDiscussionKind      = "elaboration_discussion_requested"

	legacyDiscussionRunningInspects = 2
	legacyDiscussionRunID           = domain.RunID("specification-run")
)

// legacyDiscussionFixture runs a specification to its approval item, accepts
// a Discuss command, lets one pass queue the marker, then rewrites that
// marker into the pre-rename family. The result is a valid in-flight upgrade
// state: a queued legacy discussion whose Discuss invocation is still
// pending.
type legacyDiscussionFixture struct {
	specificationFixture
	driver *execfake.StageDriver
	itemID domain.ItemID
	reply  string
}

func newLegacyDiscussionFixture(t *testing.T) legacyDiscussionFixture {
	t.Helper()
	f := newSpecificationFixture(t, true, 4)
	driver := f.newDriver(t)
	initialID := specificationInvocationID("specification-run", 1)
	if err := specifyfake.Script(driver, initialID, 0, 0, specify.Output{
		Specification: &specify.Specification{
			Summary:    "The bounded implementation plan is ready.",
			Body:       "# Approved Specification\n\nImplement the bounded workflow.",
			Addressals: []specify.Addressal{},
		},
	}); err != nil {
		t.Fatal(err)
	}
	f.submit(t)
	engine := f.newEngine(t, driver)
	itemID := domain.ItemID("spec-approval-implementation-run-1")
	for pass := 1; pass <= 3; pass++ {
		if _, err := engine.Reconcile(t.Context()); err != nil {
			t.Fatalf("prepare specification pass %d: %v", pass, err)
		}
		if _, err := f.signet.GetAttentionItem(t.Context(), itemID); err == nil {
			break
		}
	}
	item, snapshot := f.item(t, itemID)
	if _, err := f.signet.Submit(t.Context(), signet.ClientCommand{
		CommandID: legacyDiscussionCommandID, DeviceID: "device-1",
		ExpectedEntityVersion: snapshot.EntityVersion,
		Payload: signet.DecisionPayload{
			ItemID: item.ID, Action: domain.ActionDiscuss, ItemVersion: item.ItemVersion,
			ArtifactDigests: item.ArtifactDigests,
			Message:         "Why does the specification keep the workflow bounded?",
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Reconcile(t.Context()); err != nil {
		t.Fatalf("enqueue discussion: %v", err)
	}
	retireDiscussionMarkerToLegacy(t, f.dbPath, legacyDiscussionCommandID)
	f = f.reopen(t)
	driver = f.newDriver(t)
	reply := "It keeps the workflow bounded by pinning the approved artifact and declared scope."
	// Two running inspects keep the started provider invocation in flight
	// across further passes, which is the window the upgrade defect needs:
	// the Discuss invocation stays pending, so every pass re-derives the
	// marker while the first one is still running.
	if err := specifyfake.Script(driver, legacyDiscussionMarkerKey, 0, legacyDiscussionRunningInspects,
		specify.Output{Reply: &reply}); err != nil {
		t.Fatal(err)
	}
	return legacyDiscussionFixture{specificationFixture: f, driver: driver, itemID: itemID, reply: reply}
}

// retireDiscussionMarkerToLegacy rewrites the queued marker and its agent
// invocation into the bytes the pre-rename daemon would have written: the
// retired idempotency key, queue kind, payload version and run-ID token, and
// the retired invocation identity in both records. The outbox payload digest
// is recomputed because the store authenticates it before canonicalizing the
// row (migration 0039).
func retireDiscussionMarkerToLegacy(t *testing.T, dbPath, commandID string) {
	t.Helper()
	current := string(specDiscussionInvocationID(commandID))
	legacy := legacyDiscussionMarkerKey
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	ctx := context.Background()
	var payload []byte
	if err := raw.QueryRowContext(ctx,
		`SELECT payload FROM outbox WHERE idempotency_key = ?`, current).Scan(&payload); err != nil {
		t.Fatalf("read queued discussion marker: %v", err)
	}
	for _, pair := range [][2]string{
		{`"specification_run_id":`, `"elaboration_run_id":`},
		{`"freeside.specification-discussion-request/v1"`, `"freeside.elaboration-discussion-request/v1"`},
		{`"invocation_id":"` + current + `"`, `"invocation_id":"` + legacy + `"`},
	} {
		if !bytes.Contains(payload, []byte(pair[0])) {
			t.Fatalf("queued marker payload lacks %s", pair[0])
		}
		payload = bytes.ReplaceAll(payload, []byte(pair[0]), []byte(pair[1]))
	}
	if _, err := raw.ExecContext(ctx,
		`UPDATE outbox SET idempotency_key = ?, kind = ?, payload = ?, payload_digest = ?
		 WHERE idempotency_key = ?`,
		legacy, legacyDiscussionKind, payload, contentaddr.Sum(payload), current); err != nil {
		t.Fatalf("retire discussion marker: %v", err)
	}
	var body []byte
	if err := raw.QueryRowContext(ctx,
		`SELECT body FROM agent_invocations WHERE id = ?`, current).Scan(&body); err != nil {
		t.Fatalf("read queued discussion invocation: %v", err)
	}
	marker := []byte(`"id":"` + current + `"`)
	if !bytes.Contains(body, marker) {
		t.Fatalf("queued invocation body lacks %s", marker)
	}
	body = bytes.ReplaceAll(body, marker, []byte(`"id":"`+legacy+`"`))
	if _, err := raw.ExecContext(ctx,
		`UPDATE agent_invocations SET id = ?, body = ? WHERE id = ?`, legacy, string(body), current); err != nil {
		t.Fatalf("retire discussion invocation: %v", err)
	}
}

func (f legacyDiscussionFixture) outbox(t *testing.T, key string) (store.QueueEntry, error) {
	t.Helper()
	var entry store.QueueEntry
	err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(t.Context(), key)
		return err
	})
	return entry, err
}

// assertNoTwinMarker is the defect's direct signature: a second marker under
// the current identity family beside the legacy one.
func (f legacyDiscussionFixture) assertNoTwinMarker(t *testing.T, when string) {
	t.Helper()
	if _, err := f.outbox(t, string(specDiscussionInvocationID(legacyDiscussionCommandID))); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("current-family discussion marker exists %s: %v", when, err)
	}
}

// assertSettled checks the recovered discussion reached exactly one reply and
// left both the Discuss invocation and the legacy marker dispatched.
func (f legacyDiscussionFixture) assertSettled(t *testing.T) {
	t.Helper()
	conversation, err := f.signet.GetConversation(t.Context(), domain.ConversationID("conv-"+string(f.itemID)))
	if err != nil {
		t.Fatal(err)
	}
	messages := conversation.Conversation.Messages
	if len(messages) != 2 || messages[1].Author != domain.AuthorAgent || messages[1].Body != f.reply {
		t.Fatalf("recovered discussion conversation = %+v", conversation.Conversation)
	}
	var terminal store.QueueEntry
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		terminal, err = tx.GetInbox(t.Context(), legacyDiscussionMarkerKey)
		return err
	}); err != nil || terminal.Kind != kindSpecificationDiscussionTerminal {
		t.Fatalf("legacy discussion terminal = %+v, error = %v", terminal, err)
	}
	for _, key := range []string{"inv-" + legacyDiscussionCommandID, legacyDiscussionMarkerKey} {
		entry, err := f.outbox(t, key)
		if err != nil || !entry.Dispatched() {
			t.Fatalf("outbox %q = %+v, error = %v", key, entry, err)
		}
	}
}

// TestLegacyDiscussionMarkerRecoversWithoutATwin covers the plain upgrade:
// repeated passes over a queued legacy marker start one provider invocation,
// never mint the current-family twin, and keep returning a nil error.
func TestLegacyDiscussionMarkerRecoversWithoutATwin(t *testing.T) {
	f := newLegacyDiscussionFixture(t)
	engine := f.newEngine(t, f.driver)
	started := 0
	for pass := 1; pass <= legacyDiscussionRunningInspects+3; pass++ {
		result, err := engine.Reconcile(t.Context())
		if err != nil {
			t.Fatalf("upgrade reconcile pass %d: %v", pass, err)
		}
		started += result.InvocationsStarted
		f.assertNoTwinMarker(t, "after pass")
	}
	if started != 1 {
		t.Fatalf("provider invocations started across passes = %d, want 1", started)
	}
	f.assertSettled(t)
}

// TestLegacyDiscussionMarkerRecoversAcrossRestart restarts the daemon between
// the pass that starts the legacy marker and the pass that completes it, the
// window the upgrade defect was found in.
func TestLegacyDiscussionMarkerRecoversAcrossRestart(t *testing.T) {
	f := newLegacyDiscussionFixture(t)
	engine := f.newEngine(t, f.driver)
	result, err := engine.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("start legacy discussion: %v", err)
	}
	started := result.InvocationsStarted
	f.assertNoTwinMarker(t, "after the starting pass")

	f.specificationFixture = f.reopen(t)
	engine = f.newEngine(t, f.driver)
	for pass := 1; pass <= legacyDiscussionRunningInspects+3; pass++ {
		result, err := engine.Reconcile(t.Context())
		if err != nil {
			t.Fatalf("post-restart reconcile pass %d: %v", pass, err)
		}
		started += result.InvocationsStarted
		f.assertNoTwinMarker(t, "after a post-restart pass")
	}
	if started != 1 {
		t.Fatalf("provider invocations started across the restart = %d, want 1", started)
	}
	f.assertSettled(t)
}

// seedCurrentFamilyTwin writes the current-family marker a daemon built
// between the rename (#986) and #1100 minted beside the legacy row: the same
// request under the current identity family, plus its agent invocation. The
// legacy row keeps the lower rowid it was queued with, so the pair is ordered
// the way that daemon left it.
func seedCurrentFamilyTwin(t *testing.T, dbPath, commandID string) {
	t.Helper()
	current := string(specDiscussionInvocationID(commandID))
	legacy := legacyDiscussionMarkerKey
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	ctx := context.Background()
	var payload []byte
	var createdAt string
	var payloadVersion int
	if err := raw.QueryRowContext(ctx,
		`SELECT payload, created_at, payload_version FROM outbox WHERE idempotency_key = ?`,
		legacy).Scan(&payload, &createdAt, &payloadVersion); err != nil {
		t.Fatalf("read legacy marker: %v", err)
	}
	for _, pair := range [][2]string{
		{`"elaboration_run_id":`, `"specification_run_id":`},
		{`"freeside.elaboration-discussion-request/v1"`, `"freeside.specification-discussion-request/v1"`},
		{`"invocation_id":"` + legacy + `"`, `"invocation_id":"` + current + `"`},
	} {
		if !bytes.Contains(payload, []byte(pair[0])) {
			t.Fatalf("legacy marker payload lacks %s", pair[0])
		}
		payload = bytes.ReplaceAll(payload, []byte(pair[0]), []byte(pair[1]))
	}
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO outbox (idempotency_key, kind, payload, status, created_at, payload_version, payload_digest)
		 VALUES (?, ?, ?, 'pending', ?, ?, ?)`,
		current, KindSpecificationDiscussionRequested, payload, createdAt,
		payloadVersion, contentaddr.Sum(payload)); err != nil {
		t.Fatalf("insert twin marker: %v", err)
	}
	var body []byte
	var entityVersion, asOfRevision int
	if err := raw.QueryRowContext(ctx,
		`SELECT body, entity_version, as_of_revision FROM agent_invocations WHERE id = ?`,
		legacy).Scan(&body, &entityVersion, &asOfRevision); err != nil {
		t.Fatalf("read legacy invocation: %v", err)
	}
	body = bytes.ReplaceAll(body, []byte(`"id":"`+legacy+`"`), []byte(`"id":"`+current+`"`))
	if _, err := raw.ExecContext(ctx,
		`INSERT INTO agent_invocations (id, entity_version, as_of_revision, body) VALUES (?, ?, ?, ?)`,
		current, entityVersion, asOfRevision, string(body)); err != nil {
		t.Fatalf("insert twin invocation: %v", err)
	}
}

// assertTwinRetired checks the seeded twin left the start loop without ever
// running: no terminal was recorded under its key, and its queue row is out
// of the pending set.
func (f legacyDiscussionFixture) assertTwinRetired(t *testing.T) {
	t.Helper()
	twin := string(specDiscussionInvocationID(legacyDiscussionCommandID))
	entry, err := f.outbox(t, twin)
	if err != nil || !entry.Dispatched() {
		t.Fatalf("twin marker = %+v, error = %v", entry, err)
	}
	err = f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		_, err := tx.GetInbox(t.Context(), twin)
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("twin terminal error = %v, want not found", err)
	}
}

// scriptTwin lets the seeded twin run to completion too, so a twin that
// still starts shows up as a second provider invocation rather than as an
// unscripted-invocation failure.
func (f legacyDiscussionFixture) scriptTwin(t *testing.T) {
	t.Helper()
	if err := specifyfake.Script(f.driver, specDiscussionInvocationID(legacyDiscussionCommandID),
		0, legacyDiscussionRunningInspects, specify.Output{Reply: &f.reply}); err != nil {
		t.Fatal(err)
	}
}

// TestLegacyDiscussionMarkerRetiresAQueuedTwin covers the store a daemon
// built between the rename and this fix leaves behind: both marker families
// queued for one accepted command. Starting both is the #1100 failure, so
// the legacy row keeps the identity and the twin is retired unstarted.
func TestLegacyDiscussionMarkerRetiresAQueuedTwin(t *testing.T) {
	f := newLegacyDiscussionFixture(t)
	seedCurrentFamilyTwin(t, f.dbPath, legacyDiscussionCommandID)
	f.specificationFixture = f.reopen(t)
	f.scriptTwin(t)
	engine := f.newEngine(t, f.driver)
	started := 0
	for pass := 1; pass <= legacyDiscussionRunningInspects+3; pass++ {
		result, err := engine.Reconcile(t.Context())
		if err != nil {
			t.Fatalf("twin reconcile pass %d: %v", pass, err)
		}
		started += result.InvocationsStarted
	}
	if started != 1 {
		t.Fatalf("provider invocations started across passes = %d, want 1", started)
	}
	f.assertTwinRetired(t)
	f.assertSettled(t)
}

// TestLegacyDiscussionMarkerRetiresATwinPersistedMidFlight is the same store
// reached the way the older daemon actually reached it: the legacy marker is
// already running when that daemon persists the twin, and the restart lands
// on this version. Without the retire, the twin starts a second invocation
// and its completion ends a later reconcile pass with ErrParentKeyMismatch.
func TestLegacyDiscussionMarkerRetiresATwinPersistedMidFlight(t *testing.T) {
	f := newLegacyDiscussionFixture(t)
	engine := f.newEngine(t, f.driver)
	result, err := engine.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("start legacy discussion: %v", err)
	}
	started := result.InvocationsStarted
	seedCurrentFamilyTwin(t, f.dbPath, legacyDiscussionCommandID)
	f.specificationFixture = f.reopen(t)
	f.scriptTwin(t)
	engine = f.newEngine(t, f.driver)
	for pass := 1; pass <= legacyDiscussionRunningInspects+3; pass++ {
		result, err := engine.Reconcile(t.Context())
		if err != nil {
			t.Fatalf("post-restart reconcile pass %d: %v", pass, err)
		}
		started += result.InvocationsStarted
	}
	if started != 1 {
		t.Fatalf("provider invocations started across the restart = %d, want 1", started)
	}
	f.assertTwinRetired(t)
	f.assertSettled(t)
}

// TestLegacyDiscussionTwinWithARecordedAttemptIsNotRetired pins the limit of
// the retire. An older daemon that recorded the twin attempt and died before
// the dispatch mark leaves a queued row ListPendingOutbox still returns while
// the run keeps the attempt. Retiring that row would hide it from the
// recorded-attempt recovery the start loop runs next without releasing the
// attempt, so the marker is left exactly as that daemon left it.
func TestLegacyDiscussionTwinWithARecordedAttemptIsNotRetired(t *testing.T) {
	f := newLegacyDiscussionFixture(t)
	seedCurrentFamilyTwin(t, f.dbPath, legacyDiscussionCommandID)
	f.specificationFixture = f.reopen(t)
	engine := f.newEngine(t, f.driver)
	twin := specDiscussionInvocationID(legacyDiscussionCommandID)
	entry, err := f.outbox(t, string(twin))
	if err != nil {
		t.Fatal(err)
	}
	var run domain.Run
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(t.Context(), legacyDiscussionRunID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	withAttempt := run
	withAttempt.Stages = append([]domain.Stage(nil), run.Stages...)
	if len(withAttempt.Stages) == 0 {
		t.Fatal("run carries no stage to record the twin attempt on")
	}
	stage := withAttempt.Stages[0]
	stage.Attempts = append(append([]domain.Attempt(nil), stage.Attempts...),
		domain.Attempt{InvocationID: twin})
	withAttempt.Stages[0] = stage

	retired, err := engine.retireSupersededSpecDiscussionMarker(t.Context(), entry, withAttempt, twin)
	if err != nil || retired {
		t.Fatalf("retired a twin with a recorded attempt = %v, error = %v", retired, err)
	}
	if again, err := f.outbox(t, string(twin)); err != nil || again.Status != entry.Status {
		t.Fatalf("twin marker after the guarded call = %+v, error = %v", again, err)
	}

	retired, err = engine.retireSupersededSpecDiscussionMarker(t.Context(), entry, run, twin)
	if err != nil || !retired {
		t.Fatalf("retired a twin with no recorded attempt = %v, error = %v", retired, err)
	}
	f.assertTwinRetired(t)
}

// newFrozenPreRenameFixture builds an engine fixture over the store
// package's frozen dump of a database the pre-rename daemon wrote. Its bytes
// are read-only here: the test proves the renamed engine reconciles the real
// capture, where the pending-state cases above prove the in-flight upgrade
// frontier the dump does not carry.
func newFrozenPreRenameFixture(t *testing.T) specificationFixture {
	t.Helper()
	root := t.TempDir()
	dump, err := os.ReadFile(filepath.Join(
		"..", "store", "testdata", "pre_rename_specification_vocabulary.sql"))
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(root, "state.db")
	raw, err := sql.Open("sqlite", "file:"+dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(t.Context(), string(dump)); err != nil {
		t.Fatalf("load pre-rename dump: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.Context(), dbPath, store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	blobs, err := signet.NewBlobStore(filepath.Join(root, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	vendorPath := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(vendorPath, []byte("Stay within the declared work unit.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prompts := map[string]domain.Digest{}
	for _, body := range []string{"Implement the approved specification.\n", "Specify the work item using only the supplied artifacts.\n"} {
		digest := domain.Digest(contentaddr.Sum([]byte(body)))
		if _, err := blobs.Put(digest, strings.NewReader(body)); err != nil {
			t.Fatal(err)
		}
		prompts[body] = digest
	}
	return specificationFixture{
		store: st, dbPath: dbPath, blobs: blobs,
		signet:    signet.NewService(st, signet.WithBlobStore(blobs), signet.WithClock(func() time.Time { return now })),
		driverDir: filepath.Join(root, "driver"), vendorPath: vendorPath, now: &now,
		implementationPrompt: prompts["Implement the approved specification.\n"],
		specificationPrompt:  prompts["Specify the work item using only the supplied artifacts.\n"],
		fetchCalls:           &atomic.Int64{},
		validationCalls:      &atomic.Int64{},
		validationPrompts:    &deliveryValidationCapture{},
	}
}

// TestFrozenPreRenameDatabaseReconcilesWithoutATwin runs the frozen capture
// through Engine.Reconcile, not only through decode, and resolves the marker
// identity against its real pre-rename bytes. The capture's discussion is
// already complete, so its reconcile passes are quiescent (every result
// counter is zero); the identity assertion below is what ties the fix to
// bytes a pre-rename daemon actually wrote.
func TestFrozenPreRenameDatabaseReconcilesWithoutATwin(t *testing.T) {
	f := newFrozenPreRenameFixture(t)
	engine := f.newEngine(t, f.newDriver(t))
	const (
		frozenCommandID  = "explain-submitted-spec"
		frozenMarkerKey  = "elaboration-discussion-" + frozenCommandID
		frozenCurrentKey = "specification-discussion-" + frozenCommandID
	)
	var resolved, unknown domain.InvocationID
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		if resolved, err = specificationDiscussionMarkerIdentity(t.Context(), tx, frozenCommandID); err != nil {
			return err
		}
		unknown, err = specificationDiscussionMarkerIdentity(t.Context(), tx, "never-submitted")
		return err
	}); err != nil {
		t.Fatalf("resolve frozen discussion identity: %v", err)
	}
	if string(resolved) != frozenMarkerKey {
		t.Fatalf("frozen discussion identity = %q, want %q", resolved, frozenMarkerKey)
	}
	if unknown != specDiscussionInvocationID("never-submitted") {
		t.Fatalf("unqueued discussion identity = %q, want the current family", unknown)
	}
	for pass := 1; pass <= 2; pass++ {
		if _, err := engine.Reconcile(t.Context()); err != nil {
			t.Fatalf("frozen capture reconcile pass %d: %v", pass, err)
		}
		var marker store.QueueEntry
		twin := errors.New("unread")
		if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
			var err error
			if marker, err = tx.GetOutbox(t.Context(), frozenMarkerKey); err != nil {
				return err
			}
			_, twin = tx.GetOutbox(t.Context(), frozenCurrentKey)
			return nil
		}); err != nil {
			t.Fatalf("frozen discussion marker after pass %d: %v", pass, err)
		}
		if !marker.Dispatched() || marker.Kind != KindSpecificationDiscussionRequested {
			t.Fatalf("frozen discussion marker after pass %d = %+v", pass, marker)
		}
		if !errors.Is(twin, store.ErrNotFound) {
			t.Fatalf("current-family twin after pass %d: %v", pass, twin)
		}
	}
}
