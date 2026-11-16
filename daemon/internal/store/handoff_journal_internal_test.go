package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestHandoffJournalColumnsAreCrossChecked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		stmt string
	}{
		{
			"ownership token body only",
			`UPDATE handoff_journal_records
			 SET body = json_set(body, '$.ownership_token', 'ffffffffffffffffffffffffffffffff')
			 WHERE run_id = 'journal-run'`,
		},
		{
			"writer complete column only",
			`UPDATE handoff_journal_records SET writer_complete = 1 WHERE run_id = 'journal-run'`,
		},
		{
			"cancellation column only",
			`UPDATE handoff_journal_records SET cancellation_requested = 1 WHERE run_id = 'journal-run'`,
		},
		{
			"writer failure body only",
			`UPDATE handoff_journal_records
			 SET body = json_set(body, '$.writer_failure_status', 7)
			 WHERE run_id = 'journal-run'`,
		},
		{
			"writer failure column only",
			`UPDATE handoff_journal_records SET writer_failure_status = 7 WHERE run_id = 'journal-run'`,
		},
		{
			"state binding body only",
			`UPDATE handoff_journal_records
			 SET body = json_set(body, '$.state', json('{
			   "config_root_fingerprint":"root",
			   "continuity_fingerprint":"continuity",
			   "session_scratch_fingerprint":"scratch",
			   "config_root_target":"/var/lib/freeside/claude-config",
			   "continuity_target":"/var/lib/freeside/claude-config/projects",
			   "session_scratch_target":"/var/lib/freeside/claude-config/session-env",
			   "config_root_read_only":true,
			   "continuity_read_only":false,
			   "session_scratch_read_only":false,
			   "config_root_digest":"aa",
			   "continuity_digest":"bb",
			   "session_scratch_digest":"cc"
			 }'))
			 WHERE run_id = 'journal-run'`,
		},
		{
			"state binding column only",
			`UPDATE handoff_journal_records
			 SET state_preparation = '{
			   "config_root_fingerprint":"root",
			   "continuity_fingerprint":"continuity",
			   "session_scratch_fingerprint":"scratch",
			   "config_root_target":"/var/lib/freeside/claude-config",
			   "continuity_target":"/var/lib/freeside/claude-config/projects",
			   "session_scratch_target":"/var/lib/freeside/claude-config/session-env",
			   "config_root_read_only":true,
			   "continuity_read_only":false,
			   "session_scratch_read_only":false,
			   "config_root_digest":"aa",
			   "continuity_digest":"bb",
			   "session_scratch_digest":"cc"
			 }'
			 WHERE run_id = 'journal-run'`,
		},
		{
			"instruction binding body only",
			`UPDATE handoff_journal_records
			 SET body = json_set(body, '$.instructions', json('{
			   "composition_version":"claude_explicit_bundle_v1",
			   "host_digest":"absent",
			   "repository_manifest_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			   "bundle_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			 }'))
			 WHERE run_id = 'journal-run'`,
		},
		{
			"instruction binding column only",
			`UPDATE handoff_journal_records
			 SET instruction_preparation = '{
			   "composition_version":"claude_explicit_bundle_v1",
			   "host_digest":"absent",
			   "repository_manifest_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			   "bundle_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			 }'
			 WHERE run_id = 'journal-run'`,
		},
		{
			"lease fence body only",
			`UPDATE handoff_journal_records
			 SET body = json_set(body, '$.lease.fence', 8) WHERE run_id = 'journal-run'`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "store.db"), Options{})
			identity := domain.AuthIdentity{
				ID: "auth-1", Provider: "claude", AuthStoreMutationLease: true,
				Interim: domain.InterimClientFacts{AuthStoreVolume: "provider-cred", RefreshStrategy: domain.RefreshOnDemand},
			}
			at := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
			if err := s.Write(ctx, func(tx *WriteTx) error {
				if err := tx.RecordAuthIdentity(ctx, identity, at); err != nil {
					return err
				}
				_, err := tx.BeginLeasedHandoffJournal(ctx, HandoffJournalRecord{
					RunID:          "journal-run",
					OwnershipToken: "00112233445566778899aabbccddeeff",
					SpecDigest:     strings.Repeat("ab", 32),
					OpenedAt:       at,
				}, identity.ID, "inv-1", at, at.Add(time.Hour))
				return err
			}); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if _, err := s.db.ExecContext(ctx, tc.stmt); err != nil {
				t.Fatalf("tamper: %v", err)
			}
			err := s.Read(ctx, func(tx *ReadTx) error {
				_, err := tx.GetHandoffJournal(ctx, "journal-run")
				return err
			})
			if !errors.Is(err, errRowInconsistent) {
				t.Fatalf("tampered read = %v, want %v", err, errRowInconsistent)
			}
		})
	}
}
