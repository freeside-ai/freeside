package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// pubAuthoringTime is the fixed UTC instant the authoring fixtures are stamped
// with.
var pubAuthoringTime = time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)

// pubEvidenceArtifacts returns three evidence artifacts a publication may cite:
// an eligible normal-class verifier log, an eligible high-sensitivity log, and a
// legal but non-eligible agent artifact. All are built under fixtureRecipe.
func pubEvidenceArtifacts(t *testing.T) (eligible, highSensitivity, ineligible domain.Artifact) {
	t.Helper()
	recipe := fixtureRecipe
	approved := approvedFixtureRecipes()

	var err error
	eligible, err = domain.NewArtifact(domain.ArtifactInput{
		ID: "art-normal", Type: domain.ArtifactKindVerifyLog, Digest: "sha256:normal",
		Provenance: domain.Provenance{
			ProducerClass:            domain.ProducerVerifier,
			ProducerInvocationID:     "inv-1",
			HeadBinding:              domain.HeadBound,
			SourceHeadSHA:            "cafebabe",
			VerificationRecipeDigest: &recipe,
			SensitivityClass:         domain.SensitivityNormal,
		},
		Metadata: runMeta(),
	}, approved)
	if err != nil {
		t.Fatalf("eligible artifact: %v", err)
	}
	highSensitivity, err = domain.NewArtifact(domain.ArtifactInput{
		ID: "art-high", Type: domain.ArtifactKindVerifyLog, Digest: "sha256:high",
		Provenance: domain.Provenance{
			ProducerClass:            domain.ProducerVerifier,
			ProducerInvocationID:     "inv-1",
			HeadBinding:              domain.HeadBound,
			SourceHeadSHA:            "cafebabe",
			VerificationRecipeDigest: &recipe,
			SensitivityClass:         domain.SensitivityHigh,
		},
		Metadata: runMeta(),
	}, approved)
	if err != nil {
		t.Fatalf("high-sensitivity artifact: %v", err)
	}
	// A legal agent artifact is never publish-eligible; it validates but the
	// evidence gate must still refuse to link it.
	ineligible = domain.Artifact{
		ID: "art-agent", Type: domain.ArtifactKindImage, Digest: "sha256:agent",
		Provenance: domain.Provenance{
			ProducerClass: domain.ProducerAgent, ProducerInvocationID: "inv-2",
			HeadBinding: domain.HeadBound, SourceHeadSHA: "cafebabe", SensitivityClass: domain.SensitivityNormal,
		},
		Metadata: runMeta(),
	}
	return eligible, highSensitivity, ineligible
}

// seedPubRunAndArtifacts seeds run-1 and the three evidence artifacts.
func seedPubRunAndArtifacts(t *testing.T, ctx context.Context, s *store.Store) (eligible, high, ineligible domain.Artifact) {
	t.Helper()
	eligible, high, ineligible = pubEvidenceArtifacts(t)
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID: "run-1", ProjectID: "proj-1", SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy",
		}); err != nil {
			return err
		}
		for _, a := range []domain.Artifact{eligible, high, ineligible} {
			if err := tx.PutArtifact(ctx, a); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed run and artifacts: %v", err)
	}
	return eligible, high, ineligible
}

func newPubAuthoring(t *testing.T, runID domain.RunID, class domain.SensitivityClass, at time.Time, refs ...domain.PublicationEvidenceReference) domain.PublicationAuthoring {
	t.Helper()
	notes := "reviewer confirmed the diff"
	artifact, err := domain.NewPublicationAuthoring(domain.PublicationAuthoringInput{
		RunID: runID, Title: "Publish the closure summary",
		Body:           "The run closed the issue with an evidence-backed pull request.",
		ReviewerNotes:  &notes,
		EvidenceRefs:   refs,
		OutcomeSummary: "All required checks green; independent review clean.",
		Producer: domain.PublicationProducer{
			Site: "explain", Producer: "claude-opus/high",
			InputDigest: domain.Digest(contentaddr.Sum([]byte("inputs"))),
		},
		SensitivityClass: class, CreatedAt: at,
	})
	if err != nil {
		t.Fatalf("NewPublicationAuthoring: %v", err)
	}
	return artifact
}

func TestPublicationAuthoringRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	eligible, _, _ := seedPubRunAndArtifacts(t, ctx, s)

	ref := domain.PublicationEvidenceReference{ArtifactID: eligible.ID, Digest: eligible.Digest}
	first := newPubAuthoring(t, "run-1", domain.SensitivityNormal, pubAuthoringTime, ref)
	second := newPubAuthoring(t, "run-1", domain.SensitivityNormal, pubAuthoringTime.Add(time.Minute), ref)

	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutPublicationAuthoring(ctx, first); err != nil {
			return err
		}
		// A byte-identical replay converges to a no-op.
		if err := tx.PutPublicationAuthoring(ctx, first); err != nil {
			return err
		}
		return tx.PutPublicationAuthoring(ctx, second)
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		got, err := tx.GetPublicationAuthoring(ctx, first.Digest)
		if err != nil {
			return err
		}
		if got.Digest != first.Digest {
			t.Fatalf("get by digest = %q, want %q", got.Digest, first.Digest)
		}
		list, err := tx.ListPublicationAuthoringsForRun(ctx, "run-1")
		if err != nil {
			return err
		}
		if len(list) != 2 {
			t.Fatalf("list length = %d, want 2", len(list))
		}
		if list[0].Digest != first.Digest || list[1].Digest != second.Digest {
			t.Fatalf("list not oldest-first: %q, %q", list[0].Digest, list[1].Digest)
		}
		return nil
	}); err != nil {
		t.Fatalf("read: %v", err)
	}
}

// TestPublicationAuthoringListOrdersBySubsecondTime proves the oldest-first
// order is chronological even for two artifacts in the same second where one
// falls on the exact second and the other carries a fractional part. A SQL text
// sort over RFC3339Nano would invert them ("...07.5Z" sorts before "...07Z").
func TestPublicationAuthoringListOrdersBySubsecondTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	seedPubRunAndArtifacts(t, ctx, s)

	onSecond := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	halfSecond := onSecond.Add(500 * time.Millisecond)
	earlier := newPubAuthoring(t, "run-1", domain.SensitivityNormal, onSecond)
	later := newPubAuthoring(t, "run-1", domain.SensitivityNormal, halfSecond)

	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutPublicationAuthoring(ctx, later); err != nil {
			return err
		}
		return tx.PutPublicationAuthoring(ctx, earlier)
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		list, err := tx.ListPublicationAuthoringsForRun(ctx, "run-1")
		if err != nil {
			return err
		}
		if len(list) != 2 {
			t.Fatalf("list length = %d, want 2", len(list))
		}
		if !list[0].CreatedAt.Equal(onSecond) || !list[1].CreatedAt.Equal(halfSecond) {
			t.Fatalf("list not chronological: %s, %s", list[0].CreatedAt, list[1].CreatedAt)
		}
		return nil
	}); err != nil {
		t.Fatalf("read: %v", err)
	}
}

func TestPublicationAuthoringRejectsForgedDigest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	seedPubRunAndArtifacts(t, ctx, s)

	artifact := newPubAuthoring(t, "run-1", domain.SensitivityNormal, pubAuthoringTime)
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutPublicationAuthoring(ctx, artifact)
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	// A different body under the same content digest is an immutable conflict.
	// Force the digest to collide with the stored one while the body differs.
	forged := artifact
	forged.Title = "A conflicting title"
	forged.Digest = artifact.Digest
	err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutPublicationAuthoring(ctx, forged)
	})
	// The forged body fails its own content-address Validate before it can reach
	// the immutable-conflict comparison; either way the write is refused.
	if err == nil {
		t.Fatal("put of a divergent body under the same digest: want error")
	}
}

// TestPublicationAuthoringPutRejectsOversizedEncoding proves a body that fits
// its raw bound but expands past the encoded cap is refused at put, so no
// immutable-but-unreadable row is ever written.
func TestPublicationAuthoringPutRejectsOversizedEncoding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	seedPubRunAndArtifacts(t, ctx, s)

	notes := "reviewer confirmed the diff"
	oversized, err := domain.NewPublicationAuthoring(domain.PublicationAuthoringInput{
		RunID: "run-1", Title: "Publish the closure summary",
		Body:           strings.Repeat("\x01", domain.MaxPublicationAuthoringBodyBytes),
		ReviewerNotes:  &notes,
		OutcomeSummary: "All required checks green; independent review clean.",
		Producer: domain.PublicationProducer{
			Site: "explain", Producer: "claude-opus/high",
			InputDigest: domain.Digest(contentaddr.Sum([]byte("inputs"))),
		},
		SensitivityClass: domain.SensitivityNormal, CreatedAt: pubAuthoringTime,
	})
	if err != nil {
		t.Fatalf("constructor: %v", err)
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutPublicationAuthoring(ctx, oversized)
	}); err == nil {
		t.Fatal("put of an over-cap encoding: want error")
	}
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		list, err := tx.ListPublicationAuthoringsForRun(ctx, "run-1")
		if err != nil {
			return err
		}
		if len(list) != 0 {
			t.Fatalf("list length = %d, want 0", len(list))
		}
		return nil
	}); err != nil {
		t.Fatalf("read: %v", err)
	}
}

func TestPublicationAuthoringPutRequiresRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	// No run seeded; a reference-free authoring artifact still fails the run
	// foreign key.
	artifact := newPubAuthoring(t, "run-absent", domain.SensitivityNormal, pubAuthoringTime)
	err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutPublicationAuthoring(ctx, artifact)
	})
	if err == nil {
		t.Fatal("put for a nonexistent run: want error")
	}
}

func TestPublicationAuthoringEvidenceGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	eligible, high, ineligible := seedPubRunAndArtifacts(t, ctx, s)

	for _, tc := range []struct {
		name    string
		ref     domain.PublicationEvidenceReference
		class   domain.SensitivityClass
		wantErr error
	}{
		{
			name:    "missing artifact",
			ref:     domain.PublicationEvidenceReference{ArtifactID: "art-nope", Digest: "sha256:whatever"},
			class:   domain.SensitivityNormal,
			wantErr: store.ErrNotFound,
		},
		{
			name:    "digest mismatch",
			ref:     domain.PublicationEvidenceReference{ArtifactID: eligible.ID, Digest: "sha256:wrong"},
			class:   domain.SensitivityNormal,
			wantErr: domain.ErrPublicationAuthoringInconsistent,
		},
		{
			name:    "not publish-eligible",
			ref:     domain.PublicationEvidenceReference{ArtifactID: ineligible.ID, Digest: ineligible.Digest},
			class:   domain.SensitivityNormal,
			wantErr: domain.ErrPublicationAuthoringInconsistent,
		},
		{
			name:    "evidence more restrictive than target",
			ref:     domain.PublicationEvidenceReference{ArtifactID: high.ID, Digest: high.Digest},
			class:   domain.SensitivityNormal,
			wantErr: domain.ErrPublicationAuthoringInconsistent,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			artifact := newPubAuthoring(t, "run-1", tc.class, pubAuthoringTime, tc.ref)
			err := s.Write(ctx, func(tx *store.WriteTx) error {
				return tx.PutPublicationAuthoring(ctx, artifact)
			})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("put err = %v, want %v", err, tc.wantErr)
			}
		})
	}

	// The gate does not over-block: a normal-class target may cite a
	// high-sensitivity target when the target is at least as restrictive.
	highArtifact := newPubAuthoring(t, "run-1", domain.SensitivityHigh, pubAuthoringTime,
		domain.PublicationEvidenceReference{ArtifactID: high.ID, Digest: high.Digest})
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutPublicationAuthoring(ctx, highArtifact)
	}); err != nil {
		t.Fatalf("high-class target citing high-sensitivity evidence: %v", err)
	}
}

// TestPublicationAuthoringGetRejectsUnapprovedRecipe proves the gate fails
// closed on read: an artifact stored under an approving policy stops
// reconstructing once its evidence recipe leaves the approved set.
func TestPublicationAuthoringGetRejectsUnapprovedRecipe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := tempDBPath(t)

	approving := openStoreAt(t, path, store.Options{ApprovedRecipes: approvedFixtureRecipes()})
	eligible, _, _ := seedPubRunAndArtifacts(t, ctx, approving)
	artifact := newPubAuthoring(t, "run-1", domain.SensitivityNormal, pubAuthoringTime,
		domain.PublicationEvidenceReference{ArtifactID: eligible.ID, Digest: eligible.Digest})
	if err := approving.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutPublicationAuthoring(ctx, artifact)
	}); err != nil {
		t.Fatalf("seed authoring: %v", err)
	}
	if err := approving.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	closed := openStoreAt(t, path, store.Options{})
	err := closed.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.GetPublicationAuthoring(ctx, artifact.Digest)
		return err
	})
	if !errors.Is(err, domain.ErrPublishEligibleInconsistent) {
		t.Fatalf("get under empty policy err = %v, want ErrPublishEligibleInconsistent", err)
	}
	// The list is a whole-or-nothing read: one ungateable row fails it.
	err = closed.Read(ctx, func(tx *store.ReadTx) error {
		_, err := tx.ListPublicationAuthoringsForRun(ctx, "run-1")
		return err
	})
	if !errors.Is(err, domain.ErrPublishEligibleInconsistent) {
		t.Fatalf("list under empty policy err = %v, want ErrPublishEligibleInconsistent", err)
	}
}
