package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// TestPublicationAuthoringMigrationAppliesFromHead proves 0078 applies on top of
// the prior schema and creates the authoring table at schema version 78.
func TestPublicationAuthoringMigrationAppliesFromHead(t *testing.T) {
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0077_")
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if got := rawVersion(t, db); got != 81 {
		t.Fatalf("schema version = %d, want 81", got)
	}
	assertTableExists(t, db, "publication_authorings", true)
}

// TestPublicationAuthoringReconstructionRejectsTampering seeds a valid,
// reference-free authoring artifact and proves each store-private lookup or
// integrity column is cross-checked against the decoded body on read: a
// tampered value fails closed rather than reconstructing.
func TestPublicationAuthoringReconstructionRejectsTampering(t *testing.T) {
	ctx := context.Background()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})

	artifact, err := domain.NewPublicationAuthoring(domain.PublicationAuthoringInput{
		RunID: "run-1", Title: "Publish the closure summary",
		Body:           "The run closed the issue with an evidence-backed pull request.",
		OutcomeSummary: "All required checks green; independent review clean.",
		Producer: domain.PublicationProducer{
			Site: "explain", Producer: "claude-opus/high",
			InputDigest: domain.Digest(contentaddr.Sum([]byte("inputs"))),
		},
		SensitivityClass: domain.SensitivityNormal,
		CreatedAt:        time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		// A second run so a tampered run_id column still satisfies the foreign
		// key, isolating the reconstruction cross-check from the FK.
		for _, id := range []domain.RunID{"run-1", "run-2"} {
			if err := tx.PutRun(ctx, domain.Run{
				ID: id, ProjectID: "proj-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
			}); err != nil {
				return err
			}
		}
		return tx.PutPublicationAuthoring(ctx, artifact)
	}); err != nil {
		t.Fatal(err)
	}

	encoded, err := artifact.Encode()
	if err != nil {
		t.Fatal(err)
	}
	const restore = `UPDATE publication_authorings SET run_id = 'run-1',
		created_at = ?, body_digest = ? WHERE content_digest = ?`
	original := formatTime(artifact.CreatedAt)
	originalBodyDigest := reviewBodyDigest(string(encoded))

	for _, tc := range []struct {
		name   string
		mutate string
	}{
		{"body integrity digest", `UPDATE publication_authorings SET body_digest = 'sha256:tampered' WHERE content_digest = ?`},
		{"run_id column", `UPDATE publication_authorings SET run_id = 'run-2' WHERE content_digest = ?`},
		{"created_at column", `UPDATE publication_authorings SET created_at = '2099-01-01T00:00:00Z' WHERE content_digest = ?`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := st.db.ExecContext(ctx, tc.mutate, artifact.Digest); err != nil {
				t.Fatal(err)
			}
			err := st.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetPublicationAuthoring(ctx, artifact.Digest)
				return err
			})
			if !errors.Is(err, errRowInconsistent) {
				t.Fatalf("get after %s tamper = %v, want errRowInconsistent", tc.name, err)
			}
			if _, err := st.db.ExecContext(ctx, restore, original, originalBodyDigest, artifact.Digest); err != nil {
				t.Fatal(err)
			}
		})
	}
}
