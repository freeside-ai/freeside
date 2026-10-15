package store_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestBootstrapListsOneSnapshot is #98 acceptance 4, the permanent bootstrap
// test: ServerState plus all four collections read through one Store.Read
// callback form one transactionally consistent snapshot. Every row's
// as_of_revision is exactly the revision of the Write that last touched it,
// none exceeds the state's revision, and the bodies round-trip.
func TestBootstrapListsOneSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	f := newFixtures(t)

	revAfter := func() int64 {
		t.Helper()
		state, err := s.ServerState(ctx)
		if err != nil {
			t.Fatalf("ServerState: %v", err)
		}
		return state.Revision
	}

	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutConversation(ctx, f.conversation); err != nil {
			return err
		}
		return tx.PutRun(ctx, f.run)
	}); err != nil {
		t.Fatalf("seed run+conversation: %v", err)
	}
	revSeed := revAfter()

	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := f.putItem(ctx, tx); err != nil {
			return err
		}
		return tx.PutAttentionDelivery(ctx, f.delivery)
	}); err != nil {
		t.Fatalf("seed item+delivery: %v", err)
	}
	revItem := revAfter()

	advanced := f.item
	advanced.ItemVersion = 2
	if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionItem(ctx, advanced) }); err != nil {
		t.Fatalf("advance item: %v", err)
	}
	revAdvance := revAfter()

	var (
		state         store.ServerState
		runs          []store.Snapshotted[domain.Run]
		conversations []store.Snapshotted[domain.Conversation]
		items         []store.Snapshotted[domain.AttentionItem]
		deliveries    []store.Snapshotted[domain.AttentionDelivery]
	)
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if state, err = tx.ServerState(ctx); err != nil {
			return err
		}
		if runs, err = tx.ListRuns(ctx); err != nil {
			return err
		}
		if conversations, err = tx.ListConversations(ctx); err != nil {
			return err
		}
		if items, err = tx.ListAttentionItems(ctx); err != nil {
			return err
		}
		deliveries, err = tx.ListAttentionDeliveries(ctx)
		return err
	}); err != nil {
		t.Fatalf("bootstrap read: %v", err)
	}

	if len(runs) != 1 || len(conversations) != 1 || len(items) != 1 || len(deliveries) != 1 {
		t.Fatalf("row counts runs/conversations/items/deliveries = %d/%d/%d/%d, want 1 each",
			len(runs), len(conversations), len(items), len(deliveries))
	}
	if state.Revision != revAdvance {
		t.Fatalf("bootstrap state revision = %d, want %d", state.Revision, revAdvance)
	}
	cases := []struct {
		name          string
		snap          store.Snapshot
		want          store.Snapshot
		body, fixture any
	}{
		{"run", runs[0].Snapshot, store.Snapshot{EntityVersion: 1, AsOfRevision: revSeed}, runs[0].Value, f.run},
		{"conversation", conversations[0].Snapshot, store.Snapshot{EntityVersion: 1, AsOfRevision: revSeed}, conversations[0].Value, f.conversation},
		{"item", items[0].Snapshot, store.Snapshot{EntityVersion: 2, AsOfRevision: revAdvance}, items[0].Value, projectedAttentionItem(t, advanced)},
		{"delivery", deliveries[0].Snapshot, store.Snapshot{EntityVersion: 1, AsOfRevision: revItem}, deliveries[0].Value, f.delivery},
	}
	for _, tc := range cases {
		if tc.snap != tc.want {
			t.Errorf("%s snapshot = %+v, want %+v", tc.name, tc.snap, tc.want)
		}
		if tc.snap.AsOfRevision > state.Revision {
			t.Errorf("%s as_of_revision %d exceeds the snapshot's server revision %d",
				tc.name, tc.snap.AsOfRevision, state.Revision)
		}
		if got, want := string(marshalIndent(t, tc.body)), string(marshalIndent(t, tc.fixture)); got != want {
			t.Errorf("%s body differs from the fixture:\ngot:  %s\nwant: %s", tc.name, got, want)
		}
	}
}

// TestListSnapshotIsolatedFromConcurrentWrite: a Write issued while a
// bootstrap Read is open cannot move the revision or surface in the lists
// before the Read commits; the single pooled connection serializes it behind
// the open transaction, deterministically (no sleeps). A Write awaited
// synchronously inside a Read callback would deadlock by design; the
// concurrent goroutine below is the only shape that can exist.
func TestListSnapshotIsolatedFromConcurrentWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	f := newFixtures(t)
	seedItem(t, s, &f)

	before, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}

	// The concurrent write advances the very item the list returns, so a
	// torn read would be visible in the listed row itself, not only in the
	// revision counter.
	advanced := f.item
	advanced.ItemVersion = 2

	writeIssued := make(chan struct{})
	writeDone := make(chan error, 1)
	err = s.Read(ctx, func(tx *store.ReadTx) error {
		go func() {
			// Proves only that the goroutine is running before the reads
			// below; determinism comes from connection exclusivity (the write
			// cannot commit while this transaction holds the store's only
			// connection), not from scheduling.
			close(writeIssued)
			writeDone <- s.Write(ctx, func(wtx *store.WriteTx) error { return wtx.PutAttentionItem(ctx, advanced) })
		}()
		<-writeIssued
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		if len(items) != 1 || items[0].Value.ItemVersion != 1 ||
			(items[0].Snapshot != store.Snapshot{EntityVersion: 1, AsOfRevision: before.Revision}) {
			return fmt.Errorf("list observed a torn state: %d items, %+v", len(items), items[0])
		}
		state, err := tx.ServerState(ctx)
		if err != nil {
			return err
		}
		if state.Revision != before.Revision {
			return fmt.Errorf("revision moved inside an open read: %d then %d", before.Revision, state.Revision)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := <-writeDone; err != nil {
		t.Fatalf("concurrent write: %v", err)
	}
	after, err := s.ServerState(ctx)
	if err != nil {
		t.Fatalf("ServerState: %v", err)
	}
	if after.Revision != before.Revision+1 {
		t.Fatalf("revision after the read = %d, want %d", after.Revision, before.Revision+1)
	}
	// The write the read never saw is durable and visible now.
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		if len(items) != 1 || items[0].Value.ItemVersion != 2 ||
			(items[0].Snapshot != store.Snapshot{EntityVersion: 2, AsOfRevision: after.Revision}) {
			return fmt.Errorf("advanced item not visible after the read: %+v", items)
		}
		return nil
	}); err != nil {
		t.Fatalf("post-read list: %v", err)
	}
}

// TestListDeterministicOrder is #98 acceptance 3: enumeration is ordered by
// ascending primary key, never by insertion order. Runs cover the single-key
// shape, deliveries the composite key with every column permuted.
func TestListDeterministicOrder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	f := newFixtures(t)
	seedItem(t, s, &f)

	for _, id := range []domain.RunID{"run-b", "run-a", "run-c"} {
		run := domain.Run{ID: id, ProjectID: "proj-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}
		if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutRun(ctx, run) }); err != nil {
			t.Fatalf("put run %q: %v", id, err)
		}
	}

	ts := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	permuted := []struct {
		device  domain.DeviceID
		channel string
		attempt int
	}{ // inserted in no key order; every composite-key column varies
		{"device-2", "ntfy", 1},
		{"device-1", "push", 2},
		{"device-1", "ntfy", 2},
		{"device-1", "ntfy", 1},
	}
	for _, p := range permuted {
		delivery := domain.AttentionDelivery{
			ItemID: f.item.ID, DeviceID: p.device, Channel: p.channel, Attempt: p.attempt,
			SubmittedAt: ts, Status: domain.DeliverySubmitted,
		}
		if err := s.Write(ctx, func(tx *store.WriteTx) error { return tx.PutAttentionDelivery(ctx, delivery) }); err != nil {
			t.Fatalf("put delivery %+v: %v", p, err)
		}
	}

	var (
		runs       []store.Snapshotted[domain.Run]
		deliveries []store.Snapshotted[domain.AttentionDelivery]
	)
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if runs, err = tx.ListRuns(ctx); err != nil {
			return err
		}
		deliveries, err = tx.ListAttentionDeliveries(ctx)
		return err
	}); err != nil {
		t.Fatalf("read: %v", err)
	}

	var gotRuns []domain.RunID
	for _, r := range runs {
		gotRuns = append(gotRuns, r.Value.ID)
	}
	wantRuns := []domain.RunID{"run-1", "run-a", "run-b", "run-c"}
	if fmt.Sprint(gotRuns) != fmt.Sprint(wantRuns) {
		t.Errorf("run order = %v, want %v", gotRuns, wantRuns)
	}

	// Only the permuted rows exist: seedItem embeds the fixture delivery in
	// the item's timing, never in the attention_deliveries table.
	var gotDeliveries []string
	for _, d := range deliveries {
		gotDeliveries = append(gotDeliveries,
			fmt.Sprintf("%s/%s/%d", d.Value.DeviceID, d.Value.Channel, d.Value.Attempt))
	}
	wantDeliveries := []string{"device-1/ntfy/1", "device-1/ntfy/2", "device-1/push/2", "device-2/ntfy/1"}
	if fmt.Sprint(gotDeliveries) != fmt.Sprint(wantDeliveries) {
		t.Errorf("delivery order = %v, want %v", gotDeliveries, wantDeliveries)
	}
}

// TestListEmptyStore: an empty collection is an empty, non-nil list, not an
// error; a fresh daemon's first bootstrap serves zero rows, and a nil slice
// would marshal as JSON null where the sync schemas require arrays.
func TestListEmptyStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		runs, err := tx.ListRuns(ctx)
		if err != nil {
			return err
		}
		conversations, err := tx.ListConversations(ctx)
		if err != nil {
			return err
		}
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		deliveries, err := tx.ListAttentionDeliveries(ctx)
		if err != nil {
			return err
		}
		if len(runs)+len(conversations)+len(items)+len(deliveries) != 0 {
			return fmt.Errorf("empty store listed %d/%d/%d/%d rows",
				len(runs), len(conversations), len(items), len(deliveries))
		}
		if runs == nil || conversations == nil || items == nil || deliveries == nil {
			return fmt.Errorf("empty store returned a nil list (runs=%v conversations=%v items=%v deliveries=%v)",
				runs == nil, conversations == nil, items == nil, deliveries == nil)
		}
		return nil
	}); err != nil {
		t.Fatalf("read: %v", err)
	}
}

// TestListAttentionItemsRegatesEvidence is #98 acceptance 2 (the policy
// half): the list reconstructs through the same evidence gate as the single
// Get, so an item persisted under an approving policy fails the whole list
// closed when enumerated under a policy that no longer approves its recipe.
func TestListAttentionItemsRegatesEvidence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := tempDBPath(t)
	f := newFixtures(t)

	approving := openStoreAt(t, path, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	err := approving.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutConversation(ctx, f.conversation); err != nil {
			return err
		}
		return f.putItem(ctx, tx)
	})
	if err != nil {
		t.Fatalf("seed item with evidence: %v", err)
	}
	if err := approving.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	closed := openStoreAt(t, path, store.Options{})
	err = closed.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.ListAttentionItems(ctx)
		return err
	})
	if !errors.Is(err, domain.ErrUnapprovedRecipe) {
		t.Fatalf("ListAttentionItems under empty policy error = %v, want ErrUnapprovedRecipe", err)
	}
}

// TestListAttentionItemRowsReportsRefusedRows: where ListAttentionItems
// fails whole on one refused row, the row-by-row read keeps every row in
// the same order and reports the refused one in place, with its id, its
// subject columns, and an error naming the recipe the read did not approve.
func TestListAttentionItemRowsReportsRefusedRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := tempDBPath(t)
	approved := approvedFixtureRecipes()
	recipe := fixtureRecipe
	at := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: "rows-evidence", Type: domain.ArtifactKindVerifyLog, Digest: "sha256:rows-evidence",
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerVerifier, ProducerInvocationID: "rows-invocation",
			HeadBinding: domain.HeadBound, SourceHeadSHA: "cafebabe",
			VerificationRecipeDigest: &recipe, SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: domain.EvidenceMetadata{
			MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: 1, CreatedAt: at,
			Source: domain.EvidenceSourceRun, Availability: domain.EvidenceAvailable,
		},
	}, approved)
	if err != nil {
		t.Fatal(err)
	}
	runID := domain.RunID("rows-run")
	approving := openStoreAt(t, path, store.Options{ApprovedRecipes: approved})
	if err := approving.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{ID: runID, ProjectID: "proj-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy"}); err != nil {
			return err
		}
		// The evidence-bearing item sorts first, so a refused row does not
		// stop the rows after it.
		for _, id := range []domain.ItemID{"rows-item-a", "rows-item-b"} {
			input := domain.AttentionItemInput{
				ID: id, ProjectID: "proj-1",
				Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID},
				Type:    domain.AttentionExecutionFailure, Priority: domain.PriorityHigh, Reason: "Recovery needed.",
				RequestedDecision: []domain.Action{domain.ActionStop}, ItemVersion: 1,
				Status: domain.StatusOpen, InterruptionClass: domain.InterruptionExceptional,
			}
			if id == "rows-item-a" {
				input.EvidenceSnapshot = []domain.Artifact{artifact}
			}
			item, err := domain.NewAttentionItem(input, approved)
			if err != nil {
				return err
			}
			if err := tx.PutAttentionItem(ctx, item); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed items: %v", err)
	}
	if err := approving.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	readRows := func(options store.Options) (rows []store.AttentionItemRow, listed []store.Snapshotted[domain.AttentionItem], listErr error) {
		t.Helper()
		s := openStoreAt(t, path, options)
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			if rows, err = tx.ListAttentionItemRows(ctx); err != nil {
				return err
			}
			listed, listErr = tx.ListAttentionItems(ctx)
			return nil
		}); err != nil {
			t.Fatalf("ListAttentionItemRows: %v", err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
		return rows, listed, listErr
	}

	rows, _, listErr := readRows(store.Options{})
	if !errors.Is(listErr, domain.ErrUnapprovedRecipe) {
		t.Fatalf("ListAttentionItems error = %v, want ErrUnapprovedRecipe", listErr)
	}
	if len(rows) != 2 || rows[0].ID != "rows-item-a" || rows[1].ID != "rows-item-b" {
		t.Fatalf("rows = %+v, want rows-item-a then rows-item-b", rows)
	}
	refused, read := rows[0], rows[1]
	if !errors.Is(refused.Err, domain.ErrUnapprovedRecipe) || refused.Item != nil {
		t.Fatalf("refused row = %+v, want ErrUnapprovedRecipe and no item", refused)
	}
	if !strings.Contains(refused.Err.Error(), string(fixtureRecipe)) {
		t.Errorf("refused row error %q does not name recipe %s", refused.Err, fixtureRecipe)
	}
	if refused.SubjectRunID == nil || *refused.SubjectRunID != runID || refused.Snapshot != (store.Snapshot{}) {
		t.Errorf("refused row subject run = %v, snapshot = %+v", refused.SubjectRunID, refused.Snapshot)
	}
	if read.Err != nil || read.Item == nil || read.Item.ID != "rows-item-b" {
		t.Fatalf("read row = %+v, want rows-item-b", read)
	}
	if !equalTaskIDs(refused.SubjectTaskID, read.Item.Subject.TaskID) {
		t.Errorf("refused row subject task = %v, want the column the read row carries (%v)",
			refused.SubjectTaskID, read.Item.Subject.TaskID)
	}

	// Under the recipe that produced the evidence every row reads, and the
	// rows equal the whole-list read.
	rows, listed, listErr := readRows(store.Options{ApprovedRecipes: approved})
	if listErr != nil || len(rows) != len(listed) {
		t.Fatalf("approved read: %d rows, %d listed, list error %v", len(rows), len(listed), listErr)
	}
	for index, row := range rows {
		if row.Err != nil || row.Item == nil {
			t.Fatalf("approved row %d = %+v", index, row)
		}
		if !reflect.DeepEqual(*row.Item, listed[index].Value) || row.Snapshot != listed[index].Snapshot {
			t.Errorf("approved row %d differs from ListAttentionItems", index)
		}
	}
}

func equalTaskIDs(a, b *domain.TaskID) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
