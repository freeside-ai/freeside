package store

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

var ledgerTables = []string{"follow_up_filing_intents", "follow_up_filing_attempts", "follow_up_filed_issues"}

// TestEncryptedRestoreRoundTripsFollowUpFilingLedger covers the
// encrypted-checkpoint restore path, which shares the delete guards with the
// plaintext path but copies rows through its own loop. The rows are written
// directly under a task proposal instance, so they would not reconstruct;
// that is deliberate: this test is about the wholesale copy and the guards.
// TestFollowUpFilingLedgerSurvivesPlaintextRestore reads a restored ledger
// back through the store.
func TestEncryptedRestoreRoundTripsFollowUpFilingLedger(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openTestStore(t)
	policy, err := domain.NewResolvedPolicy("ledger-restore-run", []domain.PolicyKey{{
		Key: "paths", Value: "daemon/", Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride, Digest: domain.Digest("sha256:" + strings.Repeat("a", 64)),
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
	var instance domain.ProposalInstance
	if err := s.Write(ctx, func(tx *WriteTx) error {
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
		}, policy.RunID, "project-1", time.Date(2026, 8, 11, 11, 0, 0, 0, time.UTC))
		if err != nil {
			return err
		}
		if err := tx.RecordWorkUnitDeclaration(ctx, declaration); err != nil {
			return err
		}
		instance, _, err = tx.AllocateProposalInstance(ctx,
			domain.ProposalAdmissionKey{Source: domain.ProposalSourceUpstreamEvent, UpstreamEventID: "event-ledger"},
			"batch-1", proposal, time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	const at = "2026-10-05T12:00:00Z"
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO follow_up_filing_intents
			(instance_id, repository_id, opened_at, pre_dispatch_issue_numbers,
			 pre_dispatch_bot_user_id, pre_dispatch_recorded_at)
			VALUES (?, 123, ?, '[7]', 4100, ?)`, []any{instance.ID, at, at}},
		{`INSERT INTO follow_up_filing_attempts
			(instance_id, ordinal, dispatch_started_at, response_class, response_recorded_at)
			VALUES (?, 1, ?, 'transient_rejection', ?)`, []any{instance.ID, at, at}},
		{`INSERT INTO follow_up_filed_issues (repository_id, issue_number, instance_id, body)
			VALUES (123, 40, ?, '{"issue_number":40}')`, []any{instance.ID}},
	} {
		if _, err := s.db.ExecContext(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed ledger rows: %v", err)
		}
	}
	rows := func() []string {
		t.Helper()
		var out []string
		for table, columns := range map[string]string{
			"follow_up_filing_intents": `instance_id || '|' || repository_id || '|' || opened_at || '|' ||
				pre_dispatch_issue_numbers || '|' || pre_dispatch_bot_user_id || '|' ||
				pre_dispatch_recorded_at || '|' || IFNULL(outcome, 'open')`,
			"follow_up_filing_attempts": `instance_id || '|' || ordinal || '|' || dispatch_started_at || '|' ||
				IFNULL(response_class, 'none')`,
			"follow_up_filed_issues": `repository_id || '|' || issue_number || '|' || instance_id || '|' || body`,
		} {
			result, err := s.db.QueryContext(ctx, `SELECT '`+table+`|' || `+columns+` FROM `+table) //nolint:gosec // G202: fixed test strings
			if err != nil {
				t.Fatal(err)
			}
			for result.Next() {
				var row string
				if err := result.Scan(&row); err != nil {
					t.Fatal(err)
				}
				out = append(out, row)
			}
			if err := result.Err(); err != nil {
				t.Fatal(err)
			}
			_ = result.Close()
		}
		slices.Sort(out)
		return out
	}
	want := rows()
	if len(want) != 3 {
		t.Fatalf("seeded ledger rows = %q, want one per table", want)
	}

	plaintext, err := serializeStoreCheckpoint(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	sourceDB, source, err := openDeserializedBackupDatabase(ctx, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDeserializedBackupDatabase(sourceDB, source)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO follow_up_filing_attempts
		(instance_id, ordinal, dispatch_started_at) VALUES (?, 2, ?)`, instance.ID, at); err != nil {
		t.Fatal(err)
	}

	if _, err := s.restoreFromDatabase(ctx, source); err != nil {
		t.Fatalf("restore over a non-empty ledger: %v", err)
	}
	if got := rows(); !slices.Equal(got, want) {
		t.Fatalf("restored ledger rows = %q, want %q", got, want)
	}
	for _, table := range ledgerTables {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM `+table); err == nil { //nolint:gosec // G202: table is a fixed test list
			t.Errorf("%s lost its delete guard in the restore", table)
		}
	}
}
