package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

// rewindIssueCommentLedger returns a migrated store to the schema 0138
// leaves, apart from the effect_kind CHECK: it drops the three ledger tables
// and forgets 0139, whose rerun rebuilds effect_proposal_instances again. The
// wider CHECK it leaves admits every row 0138 could hold.
func rewindIssueCommentLedger(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	for _, statement := range []string{
		`DROP TABLE posted_issue_comments`,
		`DROP TABLE issue_comment_attempts`,
		`DROP TABLE issue_comment_intents`,
		`DELETE FROM schema_migrations WHERE name = '0139_issue_comment_ledger.sql'`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

// TestIssueCommentLedgerMigrationKeepsProposals: 0139 rebuilds
// effect_proposal_instances, so every row it held must come through
// unchanged, including a kind that binds no resolved policy (NULL policy
// columns), and the rebuilt CHECK must admit issue_comment and still refuse
// an unknown kind.
func TestIssueCommentLedgerMigrationKeepsProposals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
	policy, err := domain.NewResolvedPolicy("comment-migration-run", []domain.PolicyKey{{
		Key: "paths", Value: "daemon/", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: domain.Digest("sha256:" + strings.Repeat("a", 64)),
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := domain.NewEffectProposal(domain.EffectTaskProposal, domain.TaskProposalParameters{
		SubjectHandle: domain.OpaqueSubjectHandle(domain.WorkUnitIDForRun(policy.RunID)),
		Intent:        domain.TaskProposalIntentImplement, ExpectedCostUnits: 10,
		Scope: domain.TaskProposalScope{ComponentCount: 1, DeclaredPathCount: 1},
	}, policy)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)
	var instance domain.ProposalInstance
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: policy.RunID, ProjectID: "project-1", SpecDigest: "sha256:spec", PolicyDigest: policy.Digest,
		}); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, policy); err != nil {
			return err
		}
		declaration, err := domain.NewWorkUnitDeclaration(domain.WorkUnitDeclarationInput{
			CompletionCriterion: domain.CompletionBoundPRMerged,
			DeclaredPaths:       domain.CanonicalDeclaredPaths(policy),
		}, policy.RunID, "project-1", at.Add(-time.Hour))
		if err != nil {
			return err
		}
		if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
			return err
		}
		instance, _, err = tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{Source: domain.ProposalSourceUpstreamEvent, UpstreamEventID: "event-1"},
			"batch-1", proposal, at)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The migration copies rows without decoding them, so a raw adoption-kind
	// row is enough to prove the NULL policy columns survive the hold table.
	if _, err := st.db.ExecContext(ctx, `INSERT INTO effect_proposal_instances
		(instance_id, admission_key, proposal_batch_id, effect_kind, content_digest,
		 subject_handle, created_at, body)
		VALUES ('adoption-1', 'admission-adoption-1', 'batch-adoption', 'image_policy_adoption',
		        'sha256:adoption', 'project-1', '2026-11-13T09:00:00Z', '{}')`); err != nil {
		t.Fatal(err)
	}
	const rowsSQL = `SELECT instance_id, admission_key, proposal_batch_id, effect_kind, content_digest,
		COALESCE(resolved_policy_run_id, '<null>'), COALESCE(resolved_policy_digest, '<null>'),
		subject_handle, created_at, body FROM effect_proposal_instances ORDER BY instance_id`
	before := queryRows(t, ctx, st.db, rowsSQL)

	rewindIssueCommentLedger(t, ctx, st.db)
	if err := migrate(ctx, st.db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	assertAtHead(t, st.db)
	after := queryRows(t, ctx, st.db, rowsSQL)
	if strings.Join(before, "\n") != strings.Join(after, "\n") || len(after) != 2 {
		t.Fatalf("rows changed across 0139:\nbefore %q\nafter  %q", before, after)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetProposalInstance(ctx, instance.ID)
		return err
	}); err != nil {
		t.Fatalf("read the task proposal after 0139: %v", err)
	}
	for _, table := range []string{"issue_comment_intents", "issue_comment_attempts", "posted_issue_comments"} {
		assertTableExists(t, st.db, table, true)
	}
	insert := `INSERT INTO effect_proposal_instances
		(instance_id, admission_key, proposal_batch_id, effect_kind, content_digest,
		 resolved_policy_run_id, resolved_policy_digest, subject_handle, created_at, body)
		VALUES (?, ?, 'batch-check', ?, 'sha256:check', ?, ?, 'subject', '2026-11-13T09:00:00Z', '{}')`
	if _, err := st.db.ExecContext(ctx, insert, "check-comment", "admission-check-comment", "issue_comment",
		string(policy.RunID), string(policy.Digest)); err != nil {
		t.Fatalf("the rebuilt CHECK refuses issue_comment: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, insert, "check-unknown", "admission-check-unknown", "unknown_kind",
		string(policy.RunID), string(policy.Digest)); err == nil {
		t.Fatal("the rebuilt CHECK admits an unknown kind")
	}
}

func queryRows(t *testing.T, ctx context.Context, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		values := make([]string, len(columns))
		targets := make([]any, len(columns))
		for i := range values {
			targets[i] = &values[i]
		}
		if err := rows.Scan(targets...); err != nil {
			t.Fatal(err)
		}
		out = append(out, strings.Join(values, "|"))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
