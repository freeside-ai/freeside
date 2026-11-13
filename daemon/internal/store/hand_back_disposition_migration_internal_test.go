package store

import (
	"context"
	"database/sql"
	"io/fs"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

const externalDispositionTrigger = "external_finding_disposition_requires_admitted_cycle"

// rewindHandBackExternalDispositions returns a migrated store to the schema
// 0139 leaves: the external-finding disposition guard as 0090 wrote it,
// which admits an external_review round only.
func rewindHandBackExternalDispositions(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	body, err := fs.ReadFile(migrations.FS, "0090_external_finding_dispositions.sql")
	if err != nil {
		t.Fatal(err)
	}
	start := strings.Index(string(body), "CREATE TRIGGER "+externalDispositionTrigger)
	end := strings.Index(string(body)[max(start, 0):], "END;")
	if start < 0 || end < 0 {
		t.Fatal("0090 no longer holds the disposition guard")
	}
	for _, statement := range []string{
		`DROP TRIGGER ` + externalDispositionTrigger,
		string(body)[start : start+end+len("END;")],
		`DELETE FROM schema_migrations WHERE name = '0140_hand_back_external_dispositions.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// TestHandBackDispositionMigrationAdmitsHandBackRound proves migration 0140
// widens the guard to a hand_back cycle's round and nothing else: under the
// guard 0090 wrote, a disposition for a round a dispatched hand_back
// authority names is refused by direct SQL, and after the migration it is
// admitted (issue #3160).
func TestHandBackDispositionMigrationAdmitsHandBackRound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := seedExternalCycle(t)
	f.rewriteAuthority(t, `"origin":"`+string(domain.PublicationSuccessorExternalReview)+`"`,
		`"origin":"`+string(domain.PublicationSuccessorHandBack)+`"`)
	want := f.fixed(f.finding, f.fixedBy.InvocationID)

	rewindHandBackExternalDispositions(t, ctx, f.st.db)
	if err := f.insertRaw(t, want); err == nil ||
		!strings.Contains(err.Error(), "does not belong to an external review cycle round") {
		t.Fatalf("insert under the 0090 guard = %v, want its refusal", err)
	}
	if err := migrate(ctx, f.st.db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if err := f.insertRaw(t, want); err != nil {
		t.Fatalf("insert for a hand_back round after 0140: %v", err)
	}
}
