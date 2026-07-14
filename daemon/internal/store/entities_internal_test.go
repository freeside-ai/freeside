package store

import (
	"context"
	"errors"
	"testing"

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
