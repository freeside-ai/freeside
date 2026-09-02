package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/migrations"
)

func storeReadinessResolution(t *testing.T, key domain.RequirementKey, class domain.VerificationCheckClass, kind domain.RequirementKind, applicable bool) domain.RequirementResolution {
	t.Helper()
	resolution, err := domain.NewRequirementResolution(domain.RequirementResolutionInput{
		RequirementKey: key, CheckClass: class, Kind: kind, Applicable: applicable,
		RequirementSetDigest:    "sha256:requirements",
		FloorRegistryGeneration: domain.CurrentVerificationFloorRegistryGeneration,
		ResolvedPolicyDigest:    "sha256:policy",
	})
	if err != nil {
		t.Fatal(err)
	}
	return resolution
}

func storeReadinessFixture(t *testing.T) (domain.RequirementResolution, domain.RequirementResolution, domain.CheckProof, domain.ValidatedDegradedWaiver, domain.WaiverLifecycleEvent) {
	t.Helper()
	now := time.Date(2026, 8, 11, 2, 3, 4, 0, time.UTC)
	resolution := storeReadinessResolution(t, "repo-policy", domain.CheckClassRepoChangePolicy, domain.RequirementRequired, true)
	cleanResolution := storeReadinessResolution(t, "clean-check", domain.CheckClassCleanVerification, domain.RequirementRequired, true)
	proof, err := domain.NewCheckProof(cleanResolution, "head", nil, "sha256:recipe")
	if err != nil {
		t.Fatal(err)
	}
	event, err := domain.NewWaiverLifecycleEvent("waiver-1", 1, domain.WaiverLifecycleGranted, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	waiver, err := domain.NewValidatedDegradedWaiver(resolution, "waiver-1", "repo_change_policy",
		domain.WaiverAuthorityHumanApproval, "sha256:grant", event, now)
	if err != nil {
		t.Fatal(err)
	}
	return resolution, cleanResolution, proof, waiver, event
}

func readinessFixtureSets() map[domain.Digest][]domain.RequirementDefinition {
	return map[domain.Digest][]domain.RequirementDefinition{
		"sha256:requirements": {
			{
				Key: "repo-policy", Class: domain.CheckClassRepoChangePolicy,
				Kind: domain.RequirementRequired, Applicable: true,
			},
			{
				Key: "clean-check", Class: domain.CheckClassCleanVerification,
				Kind: domain.RequirementRequired, Applicable: true,
			},
		},
	}
}

func readinessWaiverOptions(grant domain.Digest) Options {
	return Options{
		ApprovedRecipes: map[domain.Digest]bool{"sha256:recipe": true},
		WaiverGrantApprovals: map[domain.WaiverGrantingAuthority]map[domain.Digest]bool{
			domain.WaiverAuthorityHumanApproval: {grant: true},
		},
		TrustedRequirementSets: readinessFixtureSets(),
	}
}

func TestVerificationReadinessMigrationAppliesFromHead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openRaw(t)
	migrateThrough(t, ctx, db, "0038_")
	if got := rawVersion(t, db); got != 37 {
		t.Fatalf("prior version = %d, want 37", got)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		t.Fatal(err)
	}
	if got := rawVersion(t, db); got != 64 {
		t.Fatalf("schema version = %d, want 64", got)
	}
	for _, table := range []string{"requirement_resolutions", "check_proofs", "degraded_waivers", "waiver_lifecycle_events"} {
		assertTableExists(t, db, table, true)
	}
}

func TestVerificationFloorRegistryGenerationOnlyTightens(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name       string
		configured uint64
		compiled   uint64
		want       uint64
		wantErr    error
	}{
		{name: "default", configured: 0, compiled: 2, want: 2},
		{name: "current", configured: 2, compiled: 2, want: 2},
		{name: "tightened", configured: 3, compiled: 2, want: 3},
		{name: "regressed", configured: 1, compiled: 2, wantErr: domain.ErrVerificationFloorRegressed},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := resolveVerificationFloorRegistryGeneration(test.configured, test.compiled)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("resolveVerificationFloorRegistryGeneration(%d, %d) error = %v, want %v", test.configured, test.compiled, err, test.wantErr)
			}
			if got != test.want {
				t.Fatalf("resolveVerificationFloorRegistryGeneration(%d, %d) = %d, want %d", test.configured, test.compiled, got, test.want)
			}
		})
	}
}

func TestReadinessRecordsRecoverAndReGateAfterRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	resolution, cleanResolution, proof, waiver, event := storeReadinessFixture(t)
	st, err := Open(ctx, path, readinessWaiverOptions(waiver.GrantDigest))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.RecordRequirementResolution(ctx, resolution); err != nil {
			return err
		}
		if err := tx.RecordRequirementResolution(ctx, cleanResolution); err != nil {
			return err
		}
		if err := tx.RecordCheckProof(ctx, proof); err != nil {
			return err
		}
		return tx.RecordValidatedDegradedWaiver(ctx, waiver, event)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(ctx, path, readinessWaiverOptions(waiver.GrantDigest))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test cleanup; assertions report read failures
	if err := st.Read(ctx, func(tx *ReadTx) error {
		if got, err := tx.GetRequirementResolution(ctx, resolution.Digest); err != nil || got.Digest != resolution.Digest {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		if got, err := tx.GetCheckProof(ctx, proof.Digest); err != nil || got.Digest != proof.Digest {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		if got, err := tx.GetValidatedDegradedWaiver(ctx, waiver.ID); err != nil || got.ID != waiver.ID {
			return errors.Join(err, domain.ErrParentKeyMismatch)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWaiverRevocationFailsClosedAndFloorTightensOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	resolution, _, _, waiver, grant := storeReadinessFixture(t)
	st, err := Open(ctx, path, readinessWaiverOptions(waiver.GrantDigest))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.RecordRequirementResolution(ctx, resolution); err != nil {
			return err
		}
		if err := tx.RecordValidatedDegradedWaiver(ctx, waiver, grant); err != nil {
			return err
		}
		revoked, err := domain.NewWaiverLifecycleEvent(waiver.ID, 2, domain.WaiverLifecycleRevoked,
			&grant.EventDigest, grant.RecordedAt.Add(time.Minute))
		if err != nil {
			return err
		}
		return tx.RecordWaiverLifecycleEvent(ctx, revoked)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetValidatedDegradedWaiver(ctx, waiver.ID)
		if !errors.Is(err, domain.ErrWaiverLifecycleInactive) {
			t.Fatalf("revoked waiver error = %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	tightened, err := Open(ctx, path, Options{
		VerificationFloorRegistryGeneration: 2,
		WaiverGrantApprovals:                readinessWaiverOptions(waiver.GrantDigest).WaiverGrantApprovals,
		TrustedRequirementSets:              readinessFixtureSets(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer tightened.Close() //nolint:errcheck // test cleanup; assertions report read failures
	if err := tightened.Read(ctx, func(tx *ReadTx) error {
		_, err := tx.GetRequirementResolution(ctx, resolution.Digest)
		if !errors.Is(err, domain.ErrVerificationFloorRegressed) {
			t.Fatalf("tightened floor error = %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestRequirementResolutionReGatesAgainstTrustedRegistry enumerates the
// caller-forgeable resolution axes: an unregistered set, a requirement key the
// set never defined, and applicability or requiredness the registered
// definition would not have produced all fail closed at persistence.
func TestRequirementResolutionReGatesAgainstTrustedRegistry(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	foreign, err := domain.NewRequirementResolution(domain.RequirementResolutionInput{
		RequirementKey: "repo-policy", CheckClass: domain.CheckClassRepoChangePolicy,
		Kind: domain.RequirementRequired, Applicable: true,
		RequirementSetDigest:    "sha256:unregistered-set",
		FloorRegistryGeneration: domain.CurrentVerificationFloorRegistryGeneration,
		ResolvedPolicyDigest:    "sha256:policy",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		resolution domain.RequirementResolution
		wantErr    error
	}{
		{name: "unregistered set", resolution: foreign, wantErr: domain.ErrRequirementSetUntrusted},
		{
			name:       "unregistered key",
			resolution: storeReadinessResolution(t, "invented-key", domain.CheckClassRepoChangePolicy, domain.RequirementRequired, true),
			wantErr:    domain.ErrRequirementDefinitionMismatch,
		},
		{
			name:       "forged inapplicability",
			resolution: storeReadinessResolution(t, "repo-policy", domain.CheckClassRepoChangePolicy, domain.RequirementRequired, false),
			wantErr:    domain.ErrRequirementDefinitionMismatch,
		},
		{
			name:       "forged optional kind",
			resolution: storeReadinessResolution(t, "repo-policy", domain.CheckClassRepoChangePolicy, domain.RequirementOptional, true),
			wantErr:    domain.ErrRequirementDefinitionMismatch,
		},
		{
			name:       "forged check class",
			resolution: storeReadinessResolution(t, "repo-policy", domain.CheckClassIndependentReview, domain.RequirementRequired, true),
			wantErr:    domain.ErrRequirementDefinitionMismatch,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{TrustedRequirementSets: readinessFixtureSets()})
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close() //nolint:errcheck // test cleanup; assertions report failures
			err = st.Write(ctx, func(tx *WriteTx) error {
				return tx.RecordRequirementResolution(ctx, test.resolution)
			})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("record error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

// TestCheckProofReGatesRecipeAuthorityByClass proves proof persistence and
// reconstruction re-run the current recipe authority for the proof's class: an
// unapproved clean-verification recipe is rejected at write, a recipe revoked
// after write is rejected at read, and a class with no registered authority
// fails closed.
func TestCheckProofReGatesRecipeAuthorityByClass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	resolution, cleanResolution, proof, waiver, _ := storeReadinessFixture(t)
	unapproved, err := domain.NewCheckProof(cleanResolution, "head", nil, "sha256:unapproved-recipe")
	if err != nil {
		t.Fatal(err)
	}
	policyProof, err := domain.NewCheckProof(resolution, "head", nil, "sha256:recipe")
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(ctx, path, readinessWaiverOptions(waiver.GrantDigest))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.RecordRequirementResolution(ctx, resolution); err != nil {
			return err
		}
		if err := tx.RecordRequirementResolution(ctx, cleanResolution); err != nil {
			return err
		}
		if err := tx.RecordCheckProof(ctx, unapproved); !errors.Is(err, domain.ErrUnapprovedRecipe) {
			t.Fatalf("unapproved recipe error = %v", err)
		}
		if err := tx.RecordCheckProof(ctx, policyProof); !errors.Is(err, domain.ErrCheckProofAuthorityUnregistered) {
			t.Fatalf("unregistered authority error = %v", err)
		}
		return tx.RecordCheckProof(ctx, proof)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	revoked := readinessWaiverOptions(waiver.GrantDigest)
	revoked.ApprovedRecipes = nil
	st, err = Open(ctx, path, revoked)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test cleanup; assertions report failures
	if err := st.Read(ctx, func(tx *ReadTx) error {
		if _, err := tx.GetCheckProof(ctx, proof.Digest); !errors.Is(err, domain.ErrUnapprovedRecipe) {
			t.Fatalf("revoked recipe read error = %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestIndependentReviewProofRequiresAssertedAuthority proves the store never
// trusts a caller-supplied independent-review recipe on its own: the check-proof
// recipe gate fails closed at both write and read unless the caller asserts the
// run-scoped authority through AuthorizeIndependentReviewRecipe. Independent
// review's approval is run-trust-context-scoped, so it cannot live in a
// daemon-owned Open-time registry; this keeps the returned-object trust
// boundary defense-in-depth even though the authority itself is re-derived in
// the engine's evaluation boundary.
func TestIndependentReviewProofRequiresAssertedAuthority(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "store.db")
	sets := map[domain.Digest][]domain.RequirementDefinition{
		"sha256:requirements": {{
			Key: "independent-review", Class: domain.CheckClassIndependentReview,
			Kind: domain.RequirementRequired, Applicable: true,
		}},
	}
	resolution := storeReadinessResolution(t, "independent-review", domain.CheckClassIndependentReview, domain.RequirementRequired, true)
	proof, err := domain.NewCheckProof(resolution, "head", nil, "sha256:review-recipe")
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(ctx, path, Options{TrustedRequirementSets: sets})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.RecordRequirementResolution(ctx, resolution); err != nil {
			return err
		}
		// Fails closed with no asserted authority, exactly as a class with no
		// registered authority does, rather than trusting the recipe.
		if err := tx.RecordCheckProof(ctx, proof); !errors.Is(err, domain.ErrCheckProofAuthorityUnregistered) {
			t.Fatalf("unauthorized independent-review write error = %v", err)
		}
		// The caller asserts the run-scoped authority it re-derived, and the
		// same recipe now persists.
		tx.AuthorizeIndependentReviewRecipe("sha256:review-recipe")
		return tx.RecordCheckProof(ctx, proof)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(ctx, path, Options{TrustedRequirementSets: sets})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test cleanup; assertions report failures
	if err := st.Read(ctx, func(tx *ReadTx) error {
		// Reconstruction fails closed too: a reader without the asserted
		// authority cannot receive the proof as trusted.
		if _, err := tx.GetCheckProof(ctx, proof.Digest); !errors.Is(err, domain.ErrCheckProofAuthorityUnregistered) {
			t.Fatalf("unauthorized independent-review read error = %v", err)
		}
		tx.AuthorizeIndependentReviewRecipe("sha256:review-recipe")
		if got, err := tx.GetCheckProof(ctx, proof.Digest); err != nil || got.Digest != proof.Digest {
			t.Fatalf("authorized independent-review read = %v, %v", got, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDegradedWaiverRequiresDaemonOwnedGrantApproval(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	resolution, _, _, waiver, event := storeReadinessFixture(t)
	st, err := Open(ctx, filepath.Join(t.TempDir(), "store.db"), Options{TrustedRequirementSets: readinessFixtureSets()})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close() //nolint:errcheck // test cleanup; assertions report failures
	err = st.Write(ctx, func(tx *WriteTx) error {
		if err := tx.RecordRequirementResolution(ctx, resolution); err != nil {
			return err
		}
		return tx.RecordValidatedDegradedWaiver(ctx, waiver, event)
	})
	if !errors.Is(err, domain.ErrWaiverInconsistent) {
		t.Fatalf("unapproved waiver error = %v, want authority rejection", err)
	}
}
