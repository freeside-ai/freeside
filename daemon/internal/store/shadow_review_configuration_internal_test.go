package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func TestShadowReviewConfigurationApprovalMigrationAppliesFromPriorHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0056_")
	if got := rawVersion(t, db); got != 55 {
		t.Fatalf("prior schema version = %d, want 55", got)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if got := rawVersion(t, db); got != 66 {
		t.Fatalf("schema version = %d, want 66", got)
	}
	for _, table := range []string{
		"shadow_review_configuration_approvals",
		"shadow_review_configuration_activations",
	} {
		var count int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&count); err != nil || count != 1 {
			t.Fatalf("table %s count=%d err=%v", table, count, err)
		}
	}
}

func TestShadowReviewConfigurationApprovalReconstructionRejectsTampering(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		tamper func(context.Context, *Store, domain.ShadowReviewConfigurationApproval) error
	}{
		{"body repo", func(ctx context.Context, st *Store, a domain.ShadowReviewConfigurationApproval) error {
			_, err := st.db.ExecContext(ctx,
				`UPDATE shadow_review_configuration_approvals SET body = json_set(body, '$.repo', 'other/repo') WHERE approval_digest = ?`, a.ApprovalDigest)
			return err
		}},
		{"body approval digest", func(ctx context.Context, st *Store, a domain.ShadowReviewConfigurationApproval) error {
			_, err := st.db.ExecContext(ctx,
				`UPDATE shadow_review_configuration_approvals SET body = json_set(body, '$.approval_digest', ?) WHERE approval_digest = ?`,
				internalShadowConfigurationDigest("f"), a.ApprovalDigest)
			return err
		}},
		{"copied repo", func(ctx context.Context, st *Store, a domain.ShadowReviewConfigurationApproval) error {
			_, err := st.db.ExecContext(ctx,
				`UPDATE shadow_review_configuration_approvals SET repo = 'other/repo' WHERE approval_digest = ?`, a.ApprovalDigest)
			return err
		}},
		{"copied repository id", func(ctx context.Context, st *Store, a domain.ShadowReviewConfigurationApproval) error {
			_, err := st.db.ExecContext(ctx,
				`UPDATE shadow_review_configuration_approvals SET repository_id = repository_id + 1 WHERE approval_digest = ?`, a.ApprovalDigest)
			return err
		}},
		{"copied source", func(ctx context.Context, st *Store, a domain.ShadowReviewConfigurationApproval) error {
			_, err := st.db.ExecContext(ctx,
				`UPDATE shadow_review_configuration_approvals SET source = 'other' WHERE approval_digest = ?`, a.ApprovalDigest)
			return err
		}},
		{"copied configuration digest", func(ctx context.Context, st *Store, a domain.ShadowReviewConfigurationApproval) error {
			_, err := st.db.ExecContext(ctx,
				`UPDATE shadow_review_configuration_approvals SET configuration_digest = ? WHERE approval_digest = ?`,
				internalShadowConfigurationDigest("e"), a.ApprovalDigest)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, st, approval := seededInternalShadowConfigurationApproval(t)
			st.db.SetMaxOpenConns(1)
			if _, err := st.db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
				t.Fatal(err)
			}
			if err := tc.tamper(ctx, st, approval); err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
				t.Fatal(err)
			}
			err := st.Read(ctx, func(tx *ReadTx) error {
				inspection, err := tx.InspectCurrentShadowReviewConfigurationApproval(
					ctx, approval.Repo, approval.Source,
				)
				if err == nil && inspection.ReconstructionError == nil {
					t.Fatalf("tampered approval reconstructed: %#v", inspection)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			err = st.Read(ctx, func(tx *ReadTx) error {
				return tx.RequireShadowReviewConfigurationApproved(
					ctx, approval.Source, approval.ConfigurationDigest,
				)
			})
			if !errors.Is(err, domain.ErrShadowReviewConfigUnapproved) {
				t.Fatalf("tampered gate error = %v, want unapproved", err)
			}
		})
	}
}

func TestShadowReviewConfigurationActivationCopiedKeysFailClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		query string
		value any
	}{
		{
			"repository id",
			`UPDATE shadow_review_configuration_activations SET repository_id = ? WHERE id = (SELECT MAX(id) FROM shadow_review_configuration_activations)`,
			int64(99),
		},
		{
			"approval digest",
			`UPDATE shadow_review_configuration_activations SET approval_digest = ? WHERE id = (SELECT MAX(id) FROM shadow_review_configuration_activations)`,
			internalShadowConfigurationDigest("f"),
		},
		{
			"configuration digest",
			`UPDATE shadow_review_configuration_activations SET configuration_digest = ? WHERE id = (SELECT MAX(id) FROM shadow_review_configuration_activations)`,
			internalShadowConfigurationDigest("e"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, st, approval := seededInternalShadowConfigurationApproval(t)
			st.db.SetMaxOpenConns(1)
			if _, err := st.db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.ExecContext(ctx, tc.query, tc.value); err != nil {
				t.Fatal(err)
			}
			if _, err := st.db.ExecContext(ctx, `PRAGMA foreign_keys = ON`); err != nil {
				t.Fatal(err)
			}
			err := st.Read(ctx, func(tx *ReadTx) error {
				return tx.RequireShadowReviewConfigurationApproved(
					ctx, approval.Source, approval.ConfigurationDigest,
				)
			})
			if !errors.Is(err, domain.ErrShadowReviewConfigUnapproved) ||
				!errors.Is(err, errRowInconsistent) {
				t.Fatalf("tampered activation error = %v, want unapproved + row inconsistent", err)
			}
		})
	}
}

func TestShadowReviewConfigurationGateRejectsUnreadableCurrentTrustProfile(t *testing.T) {
	t.Parallel()
	ctx, st, approval := seededInternalShadowConfigurationApproval(t)
	if _, err := st.db.ExecContext(ctx,
		`UPDATE trust_profiles SET body = json_set(body, '$.repository_id', 99) WHERE repo = ?`,
		approval.Repo,
	); err != nil {
		t.Fatal(err)
	}
	err := st.Read(ctx, func(tx *ReadTx) error {
		return tx.RequireShadowReviewConfigurationApproved(
			ctx, approval.Source, approval.ConfigurationDigest,
		)
	})
	if !errors.Is(err, domain.ErrShadowReviewConfigUnapproved) ||
		!errors.Is(err, domain.ErrProfileDigestMismatch) {
		t.Fatalf("unreadable current profile error = %v, want unapproved + digest mismatch", err)
	}
}

func seededInternalShadowConfigurationApproval(
	t *testing.T,
) (context.Context, *Store, domain.ShadowReviewConfigurationApproval) {
	t.Helper()
	ctx := context.Background()
	st := openTemplateStoreAt(t, filepath.Join(t.TempDir(), "freeside.db"), Options{})
	profile := internalShadowTrustProfile(
		t, "example/repo", 44, internalShadowConfigurationDigest("1"),
	)
	approval, err := domain.NewShadowReviewConfigurationApproval(
		domain.ShadowReviewConfigurationApprovalInput{
			Repo: profile.Repo, RepositoryID: profile.RepositoryID,
			Source:              domain.ShadowReviewClaudeLocal,
			ConfigurationDigest: internalShadowConfigurationDigest("a"),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	if err := st.WriteInternal(ctx, func(tx *InternalTx) error {
		if err := tx.RecordTrustProfile(ctx, profile, t0); err != nil {
			return err
		}
		if err := tx.RecordInactiveShadowReviewConfigurationApproval(ctx, approval, t0); err != nil {
			return err
		}
		return tx.ActivateShadowReviewConfigurationApproval(
			ctx, approval.Repo, approval.Source, approval.ApprovalDigest, t0,
		)
	}); err != nil {
		t.Fatal(err)
	}
	return ctx, st, approval
}

func internalShadowConfigurationDigest(char string) domain.Digest {
	return domain.Digest("sha256:" + strings.Repeat(char, 64))
}

func internalShadowTrustProfile(
	t *testing.T, repo string, repositoryID int64, reviewDigest domain.Digest,
) domain.AutomationTrustProfile {
	t.Helper()
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: repo, RepositoryID: repositoryID,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        internalShadowConfigurationDigest("2"),
		Review: domain.ReviewSettings{
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: reviewDigest,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return profile
}
