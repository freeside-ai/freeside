package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestGetRejectsInconsistentRow: a row whose JSON body disagrees with its
// extracted key columns must fail the read, whether the mismatch is on the
// queried key or on a join column the lookup did not filter by; the columns
// are what foreign keys and lookups enforce, so a divergent body is corrupt,
// not trusted data. Internal test: writing the corrupt row requires raw SQL
// past the Put boundary.
func TestGetRejectsInconsistentRow(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatalf("seedEpoch: %v", err)
	}
	s := &Store{db: db}

	body, err := encode(domain.Run{
		ID: "run-1", ProjectID: "proj-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	cases := []struct {
		name   string
		id     string // the id column (and Get key); the body always says run-1
		proj   string // the project_id column; the body always says proj-1
		policy string // the policy_digest column; the body always says sha256:policy
		getID  domain.RunID
	}{
		{"queried key differs from body identity", "run-2", "proj-1", "sha256:policy", "run-2"},
		{"join column differs from body", "run-1", "proj-other", "sha256:policy", "run-1"},
		{"digest column differs from body", "run-1", "proj-1", "sha256:other", "run-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, `DELETE FROM runs`); err != nil {
				t.Fatalf("reset: %v", err)
			}
			if _, err := db.ExecContext(ctx,
				`INSERT INTO runs (id, project_id, policy_digest, entity_version, as_of_revision, body) VALUES (?, ?, ?, 1, 1, ?)`,
				tc.id, tc.proj, tc.policy, body); err != nil {
				t.Fatalf("insert corrupt row: %v", err)
			}
			err := s.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetRun(ctx, tc.getID)
				return err
			})
			if !errors.Is(err, errRowInconsistent) {
				t.Fatalf("GetRun error = %v, want errRowInconsistent", err)
			}
		})
	}
}

// TestGetCommandRejectsInconsistentRow: a commands row whose JSON body disagrees
// with an extracted binding column (the bound version, action, and the rest are
// the authoritative decision record) must fail the read. The columns are what
// the store and its foreign keys act on, so a divergent body is a forged
// decision record, not trusted data (#32 acceptance 4). Internal test: the
// corrupt row is written past the Put boundary as raw SQL.
func TestGetCommandRejectsInconsistentRow(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatalf("seedEpoch: %v", err)
	}
	s := &Store{db: db}

	// A parent attention item to satisfy the commands foreign key; GetCommand
	// never reads it, so a minimal raw row suffices.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO attention_items (id, project_id, conversation_id, entity_version, as_of_revision, body) VALUES ('item-1', 'proj-1', NULL, 1, 1, '{}')`); err != nil {
		t.Fatalf("insert parent item: %v", err)
	}

	// A well-formed command body (passes domain Validate): the columns below
	// diverge from it one at a time.
	const body = `{"command_id":"cmd-1","device_id":"device-1","item_id":"item-1","item_version":1,"pr_head_sha":"cafebabe","artifact_digests":["sha256:img","sha256:log"],"action":"open_pr"}`

	cases := []struct {
		name    string
		version int
		action  string
	}{
		{"version column differs from body", 2, "open_pr"},
		{"action column differs from body", 1, "stop"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := db.ExecContext(ctx, `DELETE FROM commands`); err != nil {
				t.Fatalf("reset: %v", err)
			}
			if _, err := db.ExecContext(ctx,
				`INSERT INTO commands (command_id, item_id, item_version, pr_head_sha, device_id, action, entity_version, as_of_revision, body) VALUES ('cmd-1', 'item-1', ?, 'cafebabe', 'device-1', ?, 1, 1, ?)`,
				tc.version, tc.action, body); err != nil {
				t.Fatalf("insert corrupt row: %v", err)
			}
			err := s.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetCommand(ctx, "cmd-1")
				return err
			})
			if !errors.Is(err, errRowInconsistent) {
				t.Fatalf("GetCommand error = %v, want errRowInconsistent", err)
			}
		})
	}
}

// TestGetResolvedPolicyRejectsForgedDigest: a stored resolved_policies body
// whose digest is internally consistent with its digest column (so the
// row-consistency check passes) but does not address the keys it carries must
// fail the read. decode re-validates every body, and Validate recomputes the
// content digest, so a forged digest is refused on read as well as on write
// (#33 acceptance 2, the "stored digest" half). Internal test: encode would
// reject the body, so it is written past the Put boundary as raw JSON.
func TestGetResolvedPolicyRejectsForgedDigest(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatalf("seedEpoch: %v", err)
	}
	s := &Store{db: db}

	// A run to satisfy the resolved_policies foreign key.
	runBody, err := encode(domain.Run{
		ID: "run-1", ProjectID: "proj-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:forged",
	})
	if err != nil {
		t.Fatalf("encode run: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		`INSERT INTO runs (id, project_id, policy_digest, entity_version, as_of_revision, body) VALUES ('run-1', 'proj-1', 'sha256:forged', 1, 1, ?)`,
		runBody); err != nil {
		t.Fatalf("insert run: %v", err)
	}

	// digest column == body.digest (row-consistent) but neither addresses the
	// keys: their authentic content digest is something else entirely.
	const forgedBody = `{"run_id":"run-1","digest":"sha256:forged","keys":[{"key":"rein","value":"tight","provenance":{"source":"preset","digest":"sha256:preset"}}]}`
	if _, err := db.ExecContext(ctx,
		`INSERT INTO resolved_policies (run_id, digest, entity_version, as_of_revision, body) VALUES ('run-1', 'sha256:forged', 1, 1, ?)`,
		forgedBody); err != nil {
		t.Fatalf("insert forged policy: %v", err)
	}

	err = s.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetResolvedPolicy(ctx, "run-1")
		return err
	})
	if !errors.Is(err, domain.ErrPolicyDigestMismatch) {
		t.Fatalf("GetResolvedPolicy error = %v, want ErrPolicyDigestMismatch", err)
	}
}

// TestGetRejectsForgedMetadata: runs, conversations, and deliveries share the
// items' reconstruction convention (#91, extended by #98's shared scan
// functions), so a store-impossible entity_version or as_of_revision fails
// their Gets closed too. Internal test: the Put boundary always stamps valid
// values, so the bad rows are written as raw SQL.
func TestGetRejectsForgedMetadata(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatalf("seedEpoch: %v", err)
	}
	s := &Store{db: db}

	runBody, err := encode(domain.Run{
		ID: "run-1", ProjectID: "proj-1",
		SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
	})
	if err != nil {
		t.Fatalf("encode run: %v", err)
	}
	conversationBody, err := encode(domain.Conversation{ID: "conv-1"})
	if err != nil {
		t.Fatalf("encode conversation: %v", err)
	}
	deliveryBody, err := encode(domain.AttentionDelivery{
		ItemID: "item-1", DeviceID: "device-1", Channel: "ntfy", Attempt: 1,
		SubmittedAt: time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		Status:      domain.DeliverySubmitted,
	})
	if err != nil {
		t.Fatalf("encode delivery: %v", err)
	}
	// A parent attention item to satisfy the deliveries foreign key; the
	// delivery Get never reads it, so a minimal raw row suffices.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO attention_items (id, project_id, conversation_id, entity_version, as_of_revision, body) VALUES ('item-1', 'proj-1', NULL, 1, 1, '{}')`); err != nil {
		t.Fatalf("insert parent item: %v", err)
	}

	seeds := map[string]func(entityVersion, asOfRevision int64){
		"run": func(ev, rev int64) {
			t.Helper()
			if _, err := db.ExecContext(ctx, `DELETE FROM runs`); err != nil {
				t.Fatalf("reset runs: %v", err)
			}
			if _, err := db.ExecContext(ctx,
				`INSERT INTO runs (id, project_id, policy_digest, entity_version, as_of_revision, body) VALUES ('run-1', 'proj-1', 'sha256:policy', ?, ?, ?)`,
				ev, rev, runBody); err != nil {
				t.Fatalf("insert run: %v", err)
			}
		},
		"conversation": func(ev, rev int64) {
			t.Helper()
			if _, err := db.ExecContext(ctx, `DELETE FROM conversations`); err != nil {
				t.Fatalf("reset conversations: %v", err)
			}
			if _, err := db.ExecContext(ctx,
				`INSERT INTO conversations (id, entity_version, as_of_revision, body) VALUES ('conv-1', ?, ?, ?)`,
				ev, rev, conversationBody); err != nil {
				t.Fatalf("insert conversation: %v", err)
			}
		},
		"delivery": func(ev, rev int64) {
			t.Helper()
			if _, err := db.ExecContext(ctx, `DELETE FROM attention_deliveries`); err != nil {
				t.Fatalf("reset deliveries: %v", err)
			}
			if _, err := db.ExecContext(ctx,
				`INSERT INTO attention_deliveries (item_id, device_id, channel, attempt, entity_version, as_of_revision, body) VALUES ('item-1', 'device-1', 'ntfy', 1, ?, ?, ?)`,
				ev, rev, deliveryBody); err != nil {
				t.Fatalf("insert delivery: %v", err)
			}
		},
	}
	reads := map[string]func() error{
		"run": func() error {
			return s.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetRun(ctx, "run-1")
				return err
			})
		},
		"conversation": func() error {
			return s.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetConversation(ctx, "conv-1")
				return err
			})
		},
		"delivery": func() error {
			return s.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetAttentionDelivery(ctx, "item-1", "device-1", "ntfy", 1)
				return err
			})
		},
	}

	for entity, seed := range seeds {
		read := reads[entity]
		// Control rows prove the fixtures themselves reconstruct, so the forged
		// cases below fail on the metadata alone.
		seed(1, 1)
		if err := read(); err != nil {
			t.Fatalf("control %s read: %v", entity, err)
		}
		forged := []struct {
			name    string
			ev, rev int64
		}{
			{"entity_version zero", 0, 1},
			{"entity_version negative", -1, 1},
			{"as_of_revision zero", 1, 0},
			{"as_of_revision negative", 1, -1},
		}
		for _, tc := range forged {
			t.Run(entity+" "+tc.name, func(t *testing.T) {
				seed(tc.ev, tc.rev)
				if err := read(); !errors.Is(err, errRowInconsistent) {
					t.Fatalf("error = %v, want errRowInconsistent", err)
				}
			})
		}
	}
}

// TestSnapshotRejectsForgedMetadata: the snapshot metadata is store-stamped,
// so values the Puts cannot produce (a zero or negative version or revision,
// or a command row past its write-once entity_version 1) mark a forged or
// corrupt row and must fail the read like any diverging column (#91
// acceptance 3). Internal test: the Put boundary always stamps valid values,
// so the bad rows are written as raw SQL.
func TestSnapshotRejectsForgedMetadata(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if err := seedEpoch(ctx, db); err != nil {
		t.Fatalf("seedEpoch: %v", err)
	}
	s := &Store{db: db}

	// A minimal valid item with no evidence, so reconstruction succeeds with no
	// approved-recipe set and only the metadata is at fault.
	runID := domain.RunID("run-1")
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "item-1", ProjectID: "proj-1",
		Subject: domain.Subject{Type: domain.SubjectRun, ID: "run-1", RunID: &runID},
		Type:    domain.AttentionReadyForFinalReview, Priority: domain.PriorityNormal,
		Reason:            "forged-metadata fixture",
		RequestedDecision: []domain.Action{domain.ActionOpenPR},
		PRHeadSHA:         "cafebabe", ItemVersion: 1,
		InterruptionClass: domain.InterruptionPlannedGate,
		Status:            domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	itemBody, err := encode(item)
	if err != nil {
		t.Fatalf("encode item: %v", err)
	}
	command, err := domain.NewCommand(domain.CommandInput{
		CommandID: "cmd-1", DeviceID: "device-1", ItemID: item.ID,
		ItemVersion: item.ItemVersion, PRHeadSHA: item.PRHeadSHA,
		ArtifactDigests: item.ArtifactDigests, Action: domain.ActionOpenPR,
	})
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}
	commandBody, err := encode(command)
	if err != nil {
		t.Fatalf("encode command: %v", err)
	}

	seedItem := func(entityVersion, asOfRevision int64) {
		t.Helper()
		if _, err := db.ExecContext(ctx, `DELETE FROM commands`); err != nil {
			t.Fatalf("reset commands: %v", err)
		}
		if _, err := db.ExecContext(ctx, `DELETE FROM attention_items`); err != nil {
			t.Fatalf("reset items: %v", err)
		}
		if _, err := db.ExecContext(ctx,
			`INSERT INTO attention_items (id, project_id, conversation_id, entity_version, as_of_revision, body) VALUES ('item-1', 'proj-1', NULL, ?, ?, ?)`,
			entityVersion, asOfRevision, itemBody); err != nil {
			t.Fatalf("insert item: %v", err)
		}
	}
	seedCommand := func(entityVersion, asOfRevision int64) {
		t.Helper()
		seedItem(1, 1)
		if _, err := db.ExecContext(ctx,
			`INSERT INTO commands (command_id, item_id, item_version, pr_head_sha, device_id, action, entity_version, as_of_revision, body) VALUES ('cmd-1', 'item-1', 1, 'cafebabe', 'device-1', 'open_pr', ?, ?, ?)`,
			entityVersion, asOfRevision, commandBody); err != nil {
			t.Fatalf("insert command: %v", err)
		}
	}
	readItem := func() error {
		return s.Read(ctx, func(tx *ReadTx) error {
			_, _, err := tx.GetAttentionItemSnapshot(ctx, "item-1")
			return err
		})
	}
	readCommand := func() error {
		return s.Read(ctx, func(tx *ReadTx) error {
			_, _, err := tx.GetCommandSnapshot(ctx, "cmd-1")
			return err
		})
	}

	// Control rows prove the fixtures themselves reconstruct, so the forged
	// cases below fail on the metadata alone.
	seedItem(1, 1)
	if err := readItem(); err != nil {
		t.Fatalf("control item read: %v", err)
	}
	seedCommand(1, 1)
	if err := readCommand(); err != nil {
		t.Fatalf("control command read: %v", err)
	}

	cases := []struct {
		name string
		seed func()
		read func() error
	}{
		{"item entity_version zero", func() { seedItem(0, 1) }, readItem},
		{"item entity_version negative", func() { seedItem(-1, 1) }, readItem},
		{"item as_of_revision zero", func() { seedItem(1, 0) }, readItem},
		{"item as_of_revision negative", func() { seedItem(1, -1) }, readItem},
		{"command entity_version zero", func() { seedCommand(0, 1) }, readCommand},
		{"command entity_version past write-once", func() { seedCommand(2, 1) }, readCommand},
		{"command as_of_revision zero", func() { seedCommand(1, 0) }, readCommand},
		{"command as_of_revision negative", func() { seedCommand(1, -1) }, readCommand},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.seed()
			if err := tc.read(); !errors.Is(err, errRowInconsistent) {
				t.Fatalf("error = %v, want errRowInconsistent", err)
			}
		})
	}
}
