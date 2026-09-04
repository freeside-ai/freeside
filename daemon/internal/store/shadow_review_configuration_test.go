package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func shadowConfigurationDigest(char string) domain.Digest {
	return domain.Digest("sha256:" + strings.Repeat(char, 64))
}

func shadowConfigurationApproval(
	t *testing.T, repo string, repositoryID int64, configuration domain.Digest,
) domain.ShadowReviewConfigurationApproval {
	t.Helper()
	approval, err := domain.NewShadowReviewConfigurationApproval(
		domain.ShadowReviewConfigurationApprovalInput{
			Repo: repo, RepositoryID: repositoryID,
			Source:              domain.ShadowReviewClaudeLocal,
			ConfigurationDigest: configuration,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return approval
}

func TestShadowReviewConfigurationApprovalRotationReplayAndRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "freeside.db")
	st := storetest.Open(t, path, store.Options{})
	profile := trustProfileForRepo(
		t, "example/repo", 44, shadowConfigurationDigest("1"),
	)
	approvalA := shadowConfigurationApproval(
		t, profile.Repo, profile.RepositoryID, shadowConfigurationDigest("a"),
	)
	approvalB := shadowConfigurationApproval(
		t, profile.Repo, profile.RepositoryID, shadowConfigurationDigest("b"),
	)
	t0 := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordTrustProfile(ctx, profile, t0); err != nil {
			return err
		}
		if err := tx.RecordInactiveShadowReviewConfigurationApproval(ctx, approvalA, t0); err != nil {
			return err
		}
		return tx.ActivateShadowReviewConfigurationApproval(
			ctx, profile.Repo, approvalA.Source, approvalA.ApprovalDigest, t0,
		)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordInactiveShadowReviewConfigurationApproval(
			ctx, approvalB, t0.Add(time.Minute),
		); err != nil {
			return err
		}
		return tx.ActivateShadowReviewConfigurationApproval(
			ctx, profile.Repo, approvalB.Source, approvalB.ApprovalDigest, t0.Add(time.Minute),
		)
	}); err != nil {
		t.Fatal(err)
	}
	// Re-recording A is persistence replay, not an owner activation decision.
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordInactiveShadowReviewConfigurationApproval(ctx, approvalA, t0)
	}); err != nil {
		t.Fatal(err)
	}
	assertCurrentShadowConfiguration(t, st, approvalB)
	// Explicit A -> B -> A is representable.
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.ActivateShadowReviewConfigurationApproval(
			ctx, profile.Repo, approvalA.Source, approvalA.ApprovalDigest, t0.Add(2*time.Minute),
		)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st = storetest.Open(t, path, store.Options{})
	assertCurrentShadowConfiguration(t, st, approvalA)
}

func assertCurrentShadowConfiguration(
	t *testing.T, st *store.Store, want domain.ShadowReviewConfigurationApproval,
) {
	t.Helper()
	if err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		inspection, err := tx.InspectCurrentShadowReviewConfigurationApproval(
			context.Background(), want.Repo, want.Source,
		)
		if err == nil && (inspection.ReconstructionError != nil ||
			inspection.Approval != want) {
			t.Fatalf("current shadow approval = %#v, want %#v", inspection, want)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestRequireShadowReviewConfigurationApprovedIsIndependentAndExact(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	routed := shadowConfigurationDigest("1")
	effective := shadowConfigurationDigest("a")
	profileA := trustProfileForRepo(t, "example/alpha", 1, routed)
	profileB := trustProfileForRepo(t, "example/beta", 2, routed)
	approvalA := shadowConfigurationApproval(t, profileA.Repo, profileA.RepositoryID, effective)
	approvalB := shadowConfigurationApproval(t, profileB.Repo, profileB.RepositoryID, effective)
	t0 := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		for _, profile := range []domain.AutomationTrustProfile{profileA, profileB} {
			if err := tx.RecordTrustProfile(ctx, profile, t0); err != nil {
				return err
			}
		}
		for _, approval := range []domain.ShadowReviewConfigurationApproval{approvalA, approvalB} {
			if err := tx.RecordInactiveShadowReviewConfigurationApproval(ctx, approval, t0); err != nil {
				return err
			}
			if err := tx.ActivateShadowReviewConfigurationApproval(
				ctx, approval.Repo, approval.Source, approval.ApprovalDigest, t0,
			); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		if err := tx.RequireShadowReviewConfigurationApproved(
			ctx, domain.ShadowReviewClaudeLocal, effective,
		); err != nil {
			return err
		}
		// Shadow authority never satisfies the routed-review gate.
		if err := tx.RequireReviewConfigurationApproved(ctx, effective); !errors.Is(
			err, domain.ErrReviewConfigurationUnapproved,
		) {
			t.Fatalf("shadow digest passed routed gate: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("matching shadow approvals: %v", err)
	}
	for _, tc := range []struct {
		name      string
		source    domain.ShadowReviewSource
		effective domain.Digest
	}{
		{"configuration drift", domain.ShadowReviewClaudeLocal, shadowConfigurationDigest("b")},
		{"source mismatch", "unregistered", effective},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := st.Read(ctx, func(tx *store.ReadTx) error {
				return tx.RequireShadowReviewConfigurationApproved(ctx, tc.source, tc.effective)
			})
			if !errors.Is(err, domain.ErrShadowReviewConfigUnapproved) {
				t.Fatalf("gate error = %v, want unapproved", err)
			}
		})
	}
	// A profile identity rotation invalidates only the old shadow approval.
	rotatedProfile := trustProfileForRepo(t, profileA.Repo, 3, routed)
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(ctx, rotatedProfile, t0.Add(time.Minute))
	}); err != nil {
		t.Fatal(err)
	}
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		return tx.RequireShadowReviewConfigurationApproved(
			ctx, domain.ShadowReviewClaudeLocal, effective,
		)
	})
	if !errors.Is(err, domain.ErrShadowReviewConfigUnapproved) {
		t.Fatalf("repository identity drift error = %v, want unapproved", err)
	}
	if got, err := readTrustProfile(st, profileB.Repo); err != nil || !reflect.DeepEqual(got, profileB) {
		t.Fatalf("valid v6 profile changed by shadow approval: got=%#v err=%v", got, err)
	}
}

func TestRequireShadowReviewConfigurationApprovedFailsClosedOnAbsenceButPropagatesSourceError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t, store.Options{})
	profile := trustProfileForRepo(t, "example/repo", 1, shadowConfigurationDigest("1"))
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordTrustProfile(ctx, profile, time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		return tx.RequireShadowReviewConfigurationApproved(
			ctx, domain.ShadowReviewClaudeLocal, shadowConfigurationDigest("a"),
		)
	})
	if !errors.Is(err, domain.ErrShadowReviewConfigUnapproved) ||
		!errors.Is(err, store.ErrNotFound) {
		t.Fatalf("absent approval error = %v, want unapproved + not found", err)
	}
	err = st.Read(ctx, func(tx *store.ReadTx) error {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		return tx.RequireShadowReviewConfigurationApproved(
			canceled, domain.ShadowReviewClaudeLocal, shadowConfigurationDigest("a"),
		)
	})
	if !errors.Is(err, context.Canceled) ||
		errors.Is(err, domain.ErrShadowReviewConfigUnapproved) {
		t.Fatalf("canceled gate error = %v, want operational context cancellation", err)
	}
}

func readTrustProfile(
	st *store.Store, repo string,
) (domain.AutomationTrustProfile, error) {
	var profile domain.AutomationTrustProfile
	err := st.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		profile, err = tx.LatestTrustProfile(context.Background(), repo)
		return err
	})
	return profile, err
}
