package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// seedDecisionSurface writes item's epoch-1 decision surface and its item-body
// projection next to a raw-seeded attention_items row: the state the 0059
// backfill leaves every
// pre-existing item in, which the reconstruction re-gate requires. A tamper
// test that rewrites an item's structural fields in place to exercise a later
// gate uses it to forge the record consistently, since the re-gate refuses
// the rewritten row first otherwise.
func seedDecisionSurface(t *testing.T, ctx context.Context, db execer, item domain.AttentionItem) {
	t.Helper()
	surface := insertDecisionSurface(t, ctx, db, item)
	item.DecisionSurface = domain.DecisionSurfaceRef{Epoch: surface.Epoch, Digest: surface.Digest}
	item.Recommendation = nil
	itemBody, err := encode(item)
	if err != nil {
		t.Fatalf("encode attention item decision surface: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE attention_items SET body = ? WHERE id = ?`, itemBody, item.ID); err != nil {
		t.Fatalf("seed attention item decision surface: %v", err)
	}
}

func insertDecisionSurface(t *testing.T, ctx context.Context, db execer, item domain.AttentionItem) domain.DecisionSurface {
	t.Helper()
	surface, err := domain.NewDecisionSurface(item)
	if err != nil {
		t.Fatalf("NewDecisionSurface: %v", err)
	}
	body, err := encode(surface)
	if err != nil {
		t.Fatalf("encode decision surface: %v", err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO attention_decision_surfaces (item_id, epoch, digest, body)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (item_id) DO UPDATE SET epoch = excluded.epoch, digest = excluded.digest, body = excluded.body`,
		surface.ItemID, surface.Epoch, surface.Digest, body); err != nil {
		t.Fatalf("seed decision surface: %v", err)
	}
	return surface
}

func decisionSurfaceItem(t *testing.T, id domain.ItemID) domain.AttentionItem {
	t.Helper()
	item := attentionItemForRun(t, id, "run-1")
	item.RequestedDecision = []domain.Action{domain.ActionStop, domain.ActionDiscuss}
	item.PRHeadSHA = "cafebabe"
	return item
}

// evidenceMetaTime is the fixed timestamp every internal test's evidence
// metadata is stamped with, matching the fixtures' other UTC-fixed times.
var evidenceMetaTime = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// runMeta is valid run-source evidence metadata for an Artifact fixture.
func runMeta() domain.EvidenceMetadata {
	return domain.EvidenceMetadata{
		MediaType: domain.EvidenceMediaApplicationJSON, SizeBytes: 1, CreatedAt: evidenceMetaTime,
		Source: domain.EvidenceSourceRun, Availability: domain.EvidenceAvailable,
	}
}

// claimMeta is valid claim-source evidence metadata for an AgentClaim fixture;
// mt matches the claim's Text media type when it carries text.
func claimMeta(mt domain.EvidenceMediaType) domain.EvidenceMetadata {
	return domain.EvidenceMetadata{
		MediaType: mt, SizeBytes: 1, CreatedAt: evidenceMetaTime,
		Source: domain.EvidenceSourceClaim, Availability: domain.EvidenceAvailable,
	}
}

// claimTextMeta is valid claim-source metadata for an inline text claim: it
// derives both the media type and size_bytes from the content, so the fixture
// satisfies AgentClaim.Validate's text/metadata bindings by construction.
func claimTextMeta(text domain.ClaimText) domain.EvidenceMetadata {
	m := claimMeta(domain.EvidenceMediaType(text.MediaType))
	m.SizeBytes = int64(len(text.Content))
	return m
}

// surfaceClaim is a text claim that fills the agent_claims presentation slot.
func surfaceClaim() domain.AgentClaim {
	text := domain.ClaimText{MediaType: domain.MediaTypeTextMarkdown, Content: "the diff touches only docs"}
	return domain.AgentClaim{
		Label: "summary", Artifact: "art-1", Digest: text.ComputeDigest(), Text: &text,
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerAgent, ProducerInvocationID: "inv-1",
			HeadBinding: domain.HeadBound, SourceHeadSHA: "cafebabe",
			SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: claimTextMeta(text),
	}
}

func readSurface(t *testing.T, ctx context.Context, st *Store, id domain.ItemID) domain.DecisionSurface {
	t.Helper()
	var surface domain.DecisionSurface
	if err := st.Read(ctx, func(tx *ReadTx) error {
		var err error
		surface, err = tx.DecisionSurface(ctx, id)
		return err
	}); err != nil {
		t.Fatalf("DecisionSurface: %v", err)
	}
	return surface
}

func rawSurfaceRow(t *testing.T, ctx context.Context, db *sql.DB, id domain.ItemID) (int, string, string) {
	t.Helper()
	var (
		epoch  int
		digest string
		body   string
	)
	if err := db.QueryRowContext(ctx,
		`SELECT epoch, digest, body FROM attention_decision_surfaces WHERE item_id = ?`, id,
	).Scan(&epoch, &digest, &body); err != nil {
		t.Fatalf("read surface row: %v", err)
	}
	return epoch, digest, body
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "store.db"), Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func putItem(t *testing.T, ctx context.Context, st *Store, item domain.AttentionItem) {
	t.Helper()
	if err := st.Write(ctx, func(tx *WriteTx) error { return tx.PutAttentionItem(ctx, item) }); err != nil {
		t.Fatalf("PutAttentionItem: %v", err)
	}
}

// TestPutAttentionItemMaintainsDecisionSurface covers the write path: creation
// stores epoch 1; a telemetry transition (status, decided_at, item_version)
// leaves the row byte-identical; a structural change advances the epoch by
// exactly one; and the identity a producer computes for the prospective item
// before the write equals the one the store derives (the non-cyclic
// prepare-then-put case, plan §4).
func TestPutAttentionItemMaintainsDecisionSurface(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTestStore(t)
	item := decisionSurfaceItem(t, "item-1")
	putItem(t, ctx, st, item)

	want, err := domain.NewDecisionSurface(item)
	if err != nil {
		t.Fatal(err)
	}
	created := readSurface(t, ctx, st, item.ID)
	if created.Epoch != 1 || created.Digest != want.Digest {
		t.Fatalf("created surface = %d/%s, want 1/%s", created.Epoch, created.Digest, want.Digest)
	}
	_, _, createdBody := rawSurfaceRow(t, ctx, st.db, item.ID)

	decided, err := item.WithDecidedAt(time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	decided.Status = domain.StatusResolved
	decided.ItemVersion = 2
	putItem(t, ctx, st, decided)
	if epoch, digest, body := rawSurfaceRow(t, ctx, st.db, item.ID); epoch != 1 || digest != string(want.Digest) || body != createdBody {
		t.Fatalf("telemetry transition rewrote the surface row: epoch %d digest %s", epoch, digest)
	}

	prospective := decided
	prospective.ItemVersion = 3
	prospective.RequestedDecision = []domain.Action{domain.ActionStop}
	precommitted, advanced, err := domain.NextDecisionSurface(created, prospective)
	if err != nil || !advanced {
		t.Fatalf("NextDecisionSurface = %v, advanced %v", err, advanced)
	}
	putItem(t, ctx, st, prospective)
	admitted := readSurface(t, ctx, st, item.ID)
	if admitted.Epoch != 2 || admitted.Digest != precommitted.Digest {
		t.Fatalf("admitted surface = %d/%s, want 2/%s", admitted.Epoch, admitted.Digest, precommitted.Digest)
	}
	if err := domain.VerifyDecisionSurfaceCommitment(admitted, precommitted.Digest); err != nil {
		t.Fatalf("pre-committed digest rejected after admission: %v", err)
	}
	if err := domain.VerifyDecisionSurfaceCommitment(admitted, created.Digest); !errors.Is(err, domain.ErrDecisionSurfaceMismatch) {
		t.Fatalf("epoch-1 commitment against epoch 2 = %v, want ErrDecisionSurfaceMismatch", err)
	}

	// A replay of the current body converges without touching the row.
	_, _, before := rawSurfaceRow(t, ctx, st.db, item.ID)
	putItem(t, ctx, st, prospective)
	if _, _, after := rawSurfaceRow(t, ctx, st.db, item.ID); after != before {
		t.Fatal("idempotent replay rewrote the surface row")
	}

	// A presented-set-only change (a claim joins) opens the next epoch too.
	claimed := prospective
	claimed.ItemVersion = 4
	claimed.AgentClaims = []domain.AgentClaim{surfaceClaim()}
	claimed.ArtifactDigests = domain.PresentedArtifactDigests(claimed)
	putItem(t, ctx, st, claimed)
	third := readSurface(t, ctx, st, item.ID)
	if third.Epoch != 3 || third.Digest == admitted.Digest ||
		!slices.Contains(third.PresentedArtifactDigests, claimed.AgentClaims[0].Digest) {
		t.Fatalf("claim admission = %d/%s presented %v, want epoch 3 with the claim digest",
			third.Epoch, third.Digest, third.PresentedArtifactDigests)
	}
}

// TestDecisionSurfaceReconstructionFailsClosed is the returned-object trust
// boundary: the persisted record is authority, so a missing row, a forged
// digest, a column diverging from the body, and a record that disagrees with
// the item's structural fields or presented set each refuse the item.
func TestDecisionSurfaceReconstructionFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTestStore(t)
	item := decisionSurfaceItem(t, "item-1")
	putItem(t, ctx, st, item)
	_, _, honest := rawSurfaceRow(t, ctx, st.db, item.ID)

	// Both the single-entity and the list reconstruction paths share the gate.
	readItem := func() error {
		if err := st.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.GetAttentionItem(ctx, item.ID)
			return err
		}); err != nil {
			return err
		}
		return st.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.ListAttentionItems(ctx)
			return err
		})
	}
	listItems := func() error {
		return st.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.ListAttentionItems(ctx)
			return err
		})
	}
	// The record tier reconstructs the same row for terminal recovery and
	// historical authentication, so it shares the identity gate: a corrupt
	// surface must not reach validateReadyItemPRBinding, effect-proposal
	// revision validation, or the production observer either.
	readRecords := func() error {
		if err := st.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.GetAttentionItemRecord(ctx, item.ID)
			return err
		}); err != nil {
			return err
		}
		return st.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.ListOpenAttentionItemRecordsForRun(ctx, "run-1")
			return err
		})
	}
	restore := func() {
		t.Helper()
		if _, err := st.db.ExecContext(ctx, `INSERT INTO attention_decision_surfaces (item_id, epoch, digest, body)
			VALUES (?, 1, json_extract(?, '$.digest'), ?)
			ON CONFLICT (item_id) DO UPDATE SET epoch = 1, digest = excluded.digest, body = excluded.body`,
			item.ID, honest, honest); err != nil {
			t.Fatalf("restore: %v", err)
		}
		if err := readItem(); err != nil {
			t.Fatalf("control read after restore: %v", err)
		}
		if err := readRecords(); err != nil {
			t.Fatalf("control record read after restore: %v", err)
		}
	}

	foreignHead := item
	foreignHead.PRHeadSHA = "deadbeef"
	foreignSurface, err := domain.NewDecisionSurface(foreignHead)
	if err != nil {
		t.Fatal(err)
	}
	foreignBody, err := encode(foreignSurface)
	if err != nil {
		t.Fatal(err)
	}
	for name, tamper := range map[string]struct {
		sql  string
		want error
	}{
		"missing row": {`DELETE FROM attention_decision_surfaces WHERE item_id = ?`, errRowInconsistent},
		"forged digest": {
			`UPDATE attention_decision_surfaces
			SET digest = 'sha256:forged', body = json_set(body, '$.digest', 'sha256:forged') WHERE item_id = ?`,
			domain.ErrDecisionSurfaceMismatch,
		},
		"epoch column diverges": {`UPDATE attention_decision_surfaces SET epoch = 2 WHERE item_id = ?`, errRowInconsistent},
		"structural field diverges": {`UPDATE attention_decision_surfaces SET body = '` + foreignBody + `',
			digest = '` + string(foreignSurface.Digest) + `' WHERE item_id = ?`, errRowInconsistent},
	} {
		t.Run(name, func(t *testing.T) {
			restore()
			if _, err := st.db.ExecContext(ctx, tamper.sql, item.ID); err != nil {
				t.Fatalf("tamper: %v", err)
			}
			if err := readItem(); !errors.Is(err, tamper.want) {
				t.Fatalf("read after tamper = %v, want %v", err, tamper.want)
			}
			if err := listItems(); !errors.Is(err, tamper.want) {
				t.Fatalf("list after tamper = %v, want %v", err, tamper.want)
			}
			if err := readRecords(); !errors.Is(err, tamper.want) {
				t.Fatalf("record read after tamper = %v, want %v", err, tamper.want)
			}
		})
	}

	// The presented set is checked too: a claim added to the item body without
	// a matching epoch is a surface the record never described.
	restore()
	if _, err := st.db.ExecContext(ctx, `UPDATE attention_decision_surfaces
		SET body = json_set(body, '$.presented_artifact_digests', json_array('sha256:phantom')) WHERE item_id = ?`,
		item.ID); err != nil {
		t.Fatal(err)
	}
	if err := readItem(); !errors.Is(err, errRowInconsistent) {
		t.Fatalf("read with a diverging presented set = %v, want %v", err, errRowInconsistent)
	}
	if err := readRecords(); !errors.Is(err, errRowInconsistent) {
		t.Fatalf("record read with a diverging presented set = %v, want %v", err, errRowInconsistent)
	}

	// A write against an existing item whose record vanished is refused, not
	// repaired: the invariant item-row ⇔ surface-row belongs to the migration
	// and the writer, never to a self-healing read.
	restore()
	if _, err := st.db.ExecContext(ctx, `DELETE FROM attention_decision_surfaces WHERE item_id = ?`, item.ID); err != nil {
		t.Fatal(err)
	}
	next := item
	next.ItemVersion = 2
	next.Status = domain.StatusDismissed
	err = st.Write(ctx, func(tx *WriteTx) error { return tx.PutAttentionItem(ctx, next) })
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("put over a missing surface = %v, want %v", err, errRowInconsistent)
	}

	// A self-consistent record planted on some other surface is refused by the
	// read-modify-write too, not laundered. Reading the row proves only that it
	// validates against itself; without the re-gate against the stored item the
	// next put would bless the planted lineage with a fresh epoch and re-open
	// every read the gate had failed closed.
	restore()
	if _, err := st.db.ExecContext(ctx, `UPDATE attention_decision_surfaces
		SET body = ?, digest = ? WHERE item_id = ?`,
		foreignBody, string(foreignSurface.Digest), item.ID); err != nil {
		t.Fatal(err)
	}
	err = st.Write(ctx, func(tx *WriteTx) error { return tx.PutAttentionItem(ctx, next) })
	if !errors.Is(err, errRowInconsistent) {
		t.Fatalf("put over a planted surface = %v, want %v", err, errRowInconsistent)
	}
	if _, _, planted := rawSurfaceRow(t, ctx, st.db, item.ID); planted != foreignBody {
		t.Fatalf("refused put rewrote the surface row: %s", planted)
	}

	if err := st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.DecisionSurface(ctx, "no-such-item")
		return err
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DecisionSurface(unknown) = %v, want ErrNotFound", err)
	}
}

// TestAttentionDecisionSurfacesMigrationAppliesFromHead is the migration
// acceptance: on a database at the real prior head, every derivable legacy
// item receives its epoch-1 record and reads back through the re-gate, while
// a row no identity can be derived from is left without one and stays
// refused, exactly as it was before the migration.
func TestAttentionDecisionSurfacesMigrationAppliesFromHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := openDB(path, Options{})
	if err != nil {
		t.Fatalf("openDB: %v", err)
	}
	migrateThrough(t, ctx, db, "0058_")
	if got := rawVersion(t, db); got != 57 {
		t.Fatalf("prior schema version = %d, want 57", got)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatal(err)
	}
	legacy := decisionSurfaceItem(t, "item-legacy")
	body, err := encode(legacy)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ id, body string }{
		{string(legacy.ID), body},
		{"item-malformed", "{"},
		{"item-foreign-body", body},
	} {
		if _, err := db.ExecContext(ctx, `INSERT INTO attention_items
			(id, project_id, conversation_id, item_type, status, subject_run_id, entity_version, as_of_revision, body)
			VALUES (?, 'proj-1', NULL, 'blocked', 'open', 'run-1', 1, 1, ?)`, row.id, row.body); err != nil {
			t.Fatalf("seed %s: %v", row.id, err)
		}
	}
	// Decodes with matching ids but fails validation: no identity is derivable.
	if _, err := db.ExecContext(ctx, `INSERT INTO attention_items
		(id, project_id, conversation_id, item_type, status, subject_run_id, entity_version, as_of_revision, body)
		VALUES ('item-bad-subject', 'proj-1', NULL, 'blocked', 'open', 'run-1', 1, 1,
		        json_set(?, '$.id', 'item-bad-subject', '$.subject.subject_id', ''))`, body); err != nil {
		t.Fatalf("seed item-bad-subject: %v", err)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	if got := rawVersion(t, db); got != 62 {
		t.Fatalf("schema version = %d, want 62", got)
	}
	want, err := domain.NewDecisionSurface(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if epoch, digest, _ := rawSurfaceRow(t, ctx, db, legacy.ID); epoch != 1 || digest != string(want.Digest) {
		t.Fatalf("backfilled surface = %d/%s, want 1/%s", epoch, digest, want.Digest)
	}
	var count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM attention_decision_surfaces`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("backfilled %d surfaces, want 1 (underivable rows are skipped)", count)
	}

	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetAttentionItem(ctx, legacy.ID)
		return err
	}); err != nil {
		t.Fatalf("read backfilled item: %v", err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetAttentionItem(ctx, "item-foreign-body")
		return err
	}); !errors.Is(err, errRowInconsistent) {
		t.Fatalf("read foreign-body item = %v, want %v", err, errRowInconsistent)
	}
	for _, id := range []domain.ItemID{"item-malformed", "item-bad-subject"} {
		if err := st.Read(ctx, func(tx *ReadTx) error {
			_, err := tx.GetAttentionItem(ctx, id)
			return err
		}); err == nil {
			t.Fatalf("read %s succeeded, want the pre-migration refusal", id)
		}
	}
}

// TestAttentionDecisionSurfacesMigrationRetiresLegacyAdjudicate proves an
// item that was valid at schema 0057 remains readable through both get and
// list after the decorative action leaves the current enum.
func TestAttentionDecisionSurfacesMigrationRetiresLegacyAdjudicate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := openDB(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	migrateThrough(t, ctx, db, "0058_")
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatal(err)
	}
	legacy := decisionSurfaceItem(t, "item-legacy-adjudicate")
	legacy.Type = domain.AttentionReviewDispute
	body, err := encode(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO attention_items
		(id, project_id, conversation_id, item_type, status, subject_run_id,
		 entity_version, as_of_revision, body)
		VALUES (?, ?, NULL, ?, ?, ?, 1, 1,
		        json_set(?, '$.requested_decision',
		                 json_array('adjudicate', 'discuss', 'stop')))`,
		legacy.ID, legacy.ProjectID, legacy.Type, legacy.Status,
		legacy.Subject.RunID, body); err != nil {
		t.Fatalf("seed legacy adjudicate item: %v", err)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Read(ctx, func(tx *ReadTx) error {
		item, err := tx.GetAttentionItem(ctx, legacy.ID)
		if err != nil {
			return err
		}
		if !slices.Equal(item.RequestedDecision,
			[]domain.Action{domain.ActionDiscuss, domain.ActionStop}) {
			t.Fatalf("migrated requested decision = %v", item.RequestedDecision)
		}
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		if len(items) != 1 || items[0].Value.ID != legacy.ID {
			t.Fatalf("listed migrated items = %#v", items)
		}
		return nil
	}); err != nil {
		t.Fatalf("read migrated legacy adjudicate item: %v", err)
	}
}

func TestRetireLegacyAdjudicate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		body        string
		want        []domain.Action
		wantChanged bool
		wantNested  bool
	}{
		{
			name: "spacing indentation and order",
			body: "{\n  \"requested_decision\": [ \"discuss\", \"adjudicate\", \"stop\" ]\n}",
			want: []domain.Action{domain.ActionDiscuss, domain.ActionStop}, wantChanged: true,
		},
		{
			name: "duplicates",
			body: `{"requested_decision":["adjudicate","discuss","adjudicate"]}`,
			want: []domain.Action{domain.ActionDiscuss}, wantChanged: true,
		},
		{
			name: "case prefix and suffix remain distinct",
			body: `{"requested_decision":["Adjudicate","adjudicate_now","readjudicate"]}`,
			want: []domain.Action{"Adjudicate", "adjudicate_now", "readjudicate"},
		},
		{
			name: "nested field is not an action surface",
			body: `{"requested_decision":["adjudicate"],"metadata":{"requested_decision":["adjudicate"]}}`,
			want: []domain.Action{}, wantChanged: true, wantNested: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rewritten, changed, err := retireLegacyAdjudicate([]byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			if changed != tc.wantChanged {
				t.Fatalf("changed = %v, want %v", changed, tc.wantChanged)
			}
			var got struct {
				RequestedDecision []domain.Action `json:"requested_decision"`
				Metadata          struct {
					RequestedDecision []domain.Action `json:"requested_decision"`
				} `json:"metadata"`
			}
			if err := json.Unmarshal(rewritten, &got); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got.RequestedDecision, tc.want) {
				t.Fatalf("requested_decision = %v, want %v", got.RequestedDecision, tc.want)
			}
			if tc.wantNested && !slices.Equal(got.Metadata.RequestedDecision,
				[]domain.Action{retiredAdjudicateAction}) {
				t.Fatalf("nested requested_decision = %v", got.Metadata.RequestedDecision)
			}
		})
	}
}

// TestAttentionDecisionSurfaceBodiesMigrationAppliesFromHead starts at the
// pre-unit schema head (0058) and proves the item-side identity projection is
// backfilled without advancing entity_version, after which the ordinary gated
// read accepts the item. Migration 0060 then joins the same upgrade to head.
func TestAttentionDecisionSurfaceBodiesMigrationAppliesFromHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	db, err := openDB(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	migrateThrough(t, ctx, db, "0059_")
	if got := rawVersion(t, db); got != 58 {
		t.Fatalf("prior schema version = %d, want 58", got)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatal(err)
	}
	item := decisionSurfaceItem(t, "item-body-backfill")
	body, err := encode(item)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO attention_items
        (id, project_id, conversation_id, item_type, status, subject_run_id,
         entity_version, as_of_revision, body)
        VALUES (?, ?, NULL, ?, ?, ?, 7, 1, ?)`,
		item.ID, item.ProjectID, item.Type, item.Status, item.Subject.RunID, body); err != nil {
		t.Fatal(err)
	}
	surface, err := domain.NewDecisionSurface(item)
	if err != nil {
		t.Fatal(err)
	}
	surfaceBody, err := encode(surface)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, insertDecisionSurfaceSQL,
		surface.ItemID, surface.Epoch, surface.Digest, surfaceBody); err != nil {
		t.Fatal(err)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate to head: %v", err)
	}
	if got := rawVersion(t, db); got != 62 {
		t.Fatalf("schema version = %d, want 62", got)
	}
	var (
		storedBody    []byte
		entityVersion int
	)
	if err := db.QueryRowContext(ctx, `SELECT body, entity_version FROM attention_items WHERE id = ?`, item.ID).
		Scan(&storedBody, &entityVersion); err != nil {
		t.Fatal(err)
	}
	if entityVersion != 7 {
		t.Fatalf("entity_version = %d, want unchanged 7", entityVersion)
	}
	stored, err := decode[domain.AttentionItem](storedBody)
	if err != nil {
		t.Fatal(err)
	}
	if stored.DecisionSurface != (domain.DecisionSurfaceRef{Epoch: surface.Epoch, Digest: surface.Digest}) ||
		stored.Recommendation != nil {
		t.Fatalf("backfilled derived fields = %#v / %#v", stored.DecisionSurface, stored.Recommendation)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	st, err := Open(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetAttentionItem(ctx, item.ID)
		return err
	}); err != nil {
		t.Fatalf("read backfilled item: %v", err)
	}
}
