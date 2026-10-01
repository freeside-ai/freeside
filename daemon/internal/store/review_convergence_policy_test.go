package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// resolveReviewConvergencePolicy seeds one run whose resolved policy carries
// the given review keys and returns what the convergence controller decodes.
func resolveReviewConvergencePolicy(
	t *testing.T, reviewKeys map[string]string,
) (store.ReviewConvergencePolicy, domain.Digest, error) {
	t.Helper()
	ctx := context.Background()
	runID := domain.RunID("run-policy")
	provenance := domain.KeyProvenance{
		Source: domain.ProvenancePreset,
		Digest: domain.Digest("sha256:" + strings.Repeat("a", 64)),
	}
	keys := []domain.PolicyKey{{Key: "paths", Value: "daemon/**", Provenance: provenance}}
	for key, value := range reviewKeys {
		keys = append(keys, domain.PolicyKey{Key: key, Value: value, Provenance: provenance})
	}
	resolved, err := domain.NewResolvedPolicy(runID, keys)
	if err != nil {
		t.Fatal(err)
	}
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: runID, ProjectID: "project-1", SpecDigest: adjSpecDigest, PolicyDigest: resolved.Digest,
		}); err != nil {
			return err
		}
		return tx.PutResolvedPolicy(ctx, resolved)
	}); err != nil {
		t.Fatalf("seed resolved policy: %v", err)
	}
	var policy store.ReviewConvergencePolicy
	err = st.Read(ctx, func(tx *store.ReadTx) error {
		var readErr error
		policy, readErr = tx.ReviewConvergencePolicy(ctx, runID)
		return readErr
	})
	return policy, resolved.Digest, err
}

// TestReviewConvergencePolicyWithoutDriftKeysIsUnchanged pins the landed
// defaults: a policy that predates the drift keys resolves to the same values
// as before, with the growth rule and the audit off. The audit route resolves
// to its recorded default, which nothing reads while the audit is off.
func TestReviewConvergencePolicyWithoutDriftKeysIsUnchanged(t *testing.T) {
	t.Parallel()
	got, digest, err := resolveReviewConvergencePolicy(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := store.ReviewConvergencePolicy{
		Digest:                        digest,
		ContinueWhile:                 store.ReviewContinueWhileNewMaterialFindings,
		LowValueStreakBeforeAttention: 2,
		HardRoundLimit:                25,
		DriftAuditRoute:               domain.DriftAuditRouteAuto,
	}
	if got != want {
		t.Fatalf("policy without drift keys = %+v, want %+v", got, want)
	}
}

func TestReviewConvergencePolicyDecodesDriftGrowthStreak(t *testing.T) {
	t.Parallel()
	got, _, err := resolveReviewConvergencePolicy(t, map[string]string{
		"review.drift_growth_streak_before_attention": "3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.DriftGrowthStreakBeforeAttention != 3 {
		t.Fatalf("growth streak = %d, want 3", got.DriftGrowthStreakBeforeAttention)
	}
	if got.LowValueStreakBeforeAttention != 2 || got.HardRoundLimit != 25 {
		t.Fatalf("growth key changed a landed default: %+v", got)
	}
}

// TestReviewConvergencePolicyRejectsInvalidDriftGrowthStreak proves the key
// fails closed like the landed streak and limit keys: a set key never
// degrades to "off".
func TestReviewConvergencePolicyRejectsInvalidDriftGrowthStreak(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"0", "-1", "two", "1.5", " 2"} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()
			_, _, err := resolveReviewConvergencePolicy(t, map[string]string{
				"review.drift_growth_streak_before_attention": value,
			})
			if !errors.Is(err, domain.ErrNonPositive) {
				t.Fatalf("growth streak %q: got %v, want ErrNonPositive", value, err)
			}
		})
	}
}

func TestReviewConvergencePolicyDecodesDriftAuditKeys(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		keys      map[string]string
		wantAfter int
		wantRoute domain.DriftAuditRoute
	}{
		{
			"after only defaults the route to auto",
			map[string]string{"review.drift_audit_after": "6"},
			6, domain.DriftAuditRouteAuto,
		},
		{
			"after at its minimum",
			map[string]string{"review.drift_audit_after": "1"},
			1, domain.DriftAuditRouteAuto,
		},
		{
			"route auto",
			map[string]string{"review.drift_audit_after": "6", "review.drift_audit_route": "auto"},
			6, domain.DriftAuditRouteAuto,
		},
		{
			"route park",
			map[string]string{"review.drift_audit_after": "6", "review.drift_audit_route": "park"},
			6, domain.DriftAuditRoutePark,
		},
		{
			"route without after leaves the audit off",
			map[string]string{"review.drift_audit_route": "park"},
			0, domain.DriftAuditRoutePark,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, digest, err := resolveReviewConvergencePolicy(t, tc.keys)
			if err != nil {
				t.Fatal(err)
			}
			want := store.ReviewConvergencePolicy{
				Digest:                        digest,
				ContinueWhile:                 store.ReviewContinueWhileNewMaterialFindings,
				LowValueStreakBeforeAttention: 2,
				HardRoundLimit:                25,
				DriftAuditAfter:               tc.wantAfter,
				DriftAuditRoute:               tc.wantRoute,
			}
			if got != want {
				t.Fatalf("policy = %+v, want %+v", got, want)
			}
		})
	}
}

// TestReviewConvergencePolicyRejectsInvalidDriftAuditKeys proves both audit
// keys fail closed like the landed keys: a set key never degrades to "off" or
// to the default route.
func TestReviewConvergencePolicyRejectsInvalidDriftAuditKeys(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"0", "-1", "six", "1.5", " 6", ""} {
		t.Run("after "+value, func(t *testing.T) {
			t.Parallel()
			_, _, err := resolveReviewConvergencePolicy(t, map[string]string{
				"review.drift_audit_after": value,
			})
			if !errors.Is(err, domain.ErrNonPositive) {
				t.Fatalf("audit after %q: got %v, want ErrNonPositive", value, err)
			}
		})
	}
	for _, value := range []string{"", "Auto", "automatic", "off", " park"} {
		t.Run("route "+value, func(t *testing.T) {
			t.Parallel()
			_, _, err := resolveReviewConvergencePolicy(t, map[string]string{
				"review.drift_audit_route": value,
			})
			if !errors.Is(err, domain.ErrParentKeyMismatch) {
				t.Fatalf("audit route %q: got %v, want ErrParentKeyMismatch", value, err)
			}
		})
	}
}
