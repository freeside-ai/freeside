package contract

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const (
	ArtifactsCaseCompleteReadback = "complete_labeled_set_reads_back"
	ArtifactsCaseIdempotentReplay = "idempotent_identical_replay"
	ArtifactsCaseConflictOnDiffer = "differing_set_conflicts"
	ArtifactsCaseEmptySetNoop     = "empty_set_records_nothing"
	ArtifactsCaseSnapshotOnRecord = "snapshot_survives_caller_mutation"
)

var artifactsCases = map[string]struct{}{
	ArtifactsCaseCompleteReadback: {}, ArtifactsCaseIdempotentReplay: {},
	ArtifactsCaseConflictOnDiffer: {}, ArtifactsCaseEmptySetNoop: {},
	ArtifactsCaseSnapshotOnRecord: {},
}

// ArtifactsHarness realizes an Artifacts implementation for the contract runner.
// The runner owns the assertions; a harness only forwards RecordClaims to the
// implementation under test and exposes its durable readback, so the fake and
// production implementations are held to one RecordClaims contract together.
type ArtifactsHarness interface {
	// PrepareInvocation establishes any precondition the implementation needs
	// before claims for id may be recorded. The store-backed adapter foreign-keys
	// the claim record to a persisted invocation row; an in-memory fake makes this
	// a no-op.
	PrepareInvocation(t *testing.T, id domain.InvocationID)
	// RecordClaims records the set through the implementation and returns its
	// error verbatim, so the runner can classify a conflict via IsConflict.
	RecordClaims(t *testing.T, id domain.InvocationID, claims []domain.AgentClaim) error
	// ReadClaims returns the durably recorded set for id; found is false when no
	// record exists (the empty-set case records nothing).
	ReadClaims(t *testing.T, id domain.InvocationID) (claims []domain.AgentClaim, found bool, err error)
	// IsConflict reports whether a RecordClaims error is the write-once conflict a
	// differing re-record must raise, hiding each implementation's own sentinel.
	IsConflict(err error) bool
}

// ArtifactsFactory constructs an isolated implementation harness per case.
type ArtifactsFactory struct {
	New              func(*testing.T) ArtifactsHarness
	KnownDivergences []KnownDivergence
}

// RunArtifactsContract runs the reusable Artifacts.RecordClaims contract against
// one implementation factory: a complete labeled set survives readback intact,
// an identical replay converges, any differing set is a write-once conflict, and
// an empty set records nothing.
func RunArtifactsContract(t *testing.T, factory ArtifactsFactory) {
	t.Helper()
	if factory.New == nil {
		t.Fatal("artifacts contract: nil factory")
	}
	divergences := divergenceMap(t, factory.KnownDivergences, artifactsCases)

	runCase(t, ArtifactsCaseCompleteReadback, divergences, func(t *testing.T) error {
		h := factory.New(t)
		id := domain.InvocationID("contract-artifacts-complete")
		h.PrepareInvocation(t, id)
		claims := artifactsClaimSet(id)
		if err := h.RecordClaims(t, id, claims); err != nil {
			return fmt.Errorf("record complete set: %w", err)
		}
		got, found, err := h.ReadClaims(t, id)
		if err != nil {
			return fmt.Errorf("read complete set: %w", err)
		}
		if !found {
			return errors.New("recorded claim set reads back missing")
		}
		return sameClaims(claims, got)
	})

	runCase(t, ArtifactsCaseIdempotentReplay, divergences, func(t *testing.T) error {
		h := factory.New(t)
		id := domain.InvocationID("contract-artifacts-replay")
		h.PrepareInvocation(t, id)
		claims := artifactsClaimSet(id)
		if err := h.RecordClaims(t, id, claims); err != nil {
			return fmt.Errorf("record: %w", err)
		}
		if err := h.RecordClaims(t, id, claims); err != nil {
			return fmt.Errorf("identical replay errored, want idempotent success: %w", err)
		}
		got, found, err := h.ReadClaims(t, id)
		if err != nil {
			return fmt.Errorf("read after replay: %w", err)
		}
		if !found {
			return errors.New("claim set reads back missing after idempotent replay")
		}
		return sameClaims(claims, got)
	})

	runCase(t, ArtifactsCaseConflictOnDiffer, divergences, func(t *testing.T) error {
		h := factory.New(t)
		id := domain.InvocationID("contract-artifacts-conflict")
		h.PrepareInvocation(t, id)
		claims := artifactsClaimSet(id)
		if err := h.RecordClaims(t, id, claims); err != nil {
			return fmt.Errorf("record: %w", err)
		}
		differing := append([]domain.AgentClaim(nil), claims...)
		differing[0].Label = "relabeled"
		err := h.RecordClaims(t, id, differing)
		if err == nil {
			return errors.New("differing re-record succeeded, want a write-once conflict")
		}
		if !h.IsConflict(err) {
			return fmt.Errorf("differing re-record error, want a write-once conflict: %w", err)
		}
		return nil
	})

	runCase(t, ArtifactsCaseSnapshotOnRecord, divergences, func(t *testing.T) error {
		h := factory.New(t)
		id := domain.InvocationID("contract-artifacts-snapshot")
		h.PrepareInvocation(t, id)
		original := artifactsClaimSet(id)
		claims := artifactsClaimSet(id)
		if err := h.RecordClaims(t, id, claims); err != nil {
			return fmt.Errorf("record: %w", err)
		}
		// Mutate the caller-owned set after recording, both a top-level field and a
		// nested Text pointer. A faithful record snapshots at write time, so neither
		// the readback nor the replay identity may observe the mutation.
		claims[0].Label = "mutated-after-record"
		claims[1].Text.Content = "mutated body"
		got, found, err := h.ReadClaims(t, id)
		if err != nil {
			return fmt.Errorf("read after mutation: %w", err)
		}
		if !found {
			return errors.New("claim record missing after caller mutation")
		}
		if err := sameClaims(original, got); err != nil {
			return fmt.Errorf("post-record mutation leaked into the record: %w", err)
		}
		if err := h.RecordClaims(t, id, original); err != nil {
			return fmt.Errorf("original replay after mutation errored, want idempotent success: %w", err)
		}
		return nil
	})

	runCase(t, ArtifactsCaseEmptySetNoop, divergences, func(t *testing.T) error {
		h := factory.New(t)
		id := domain.InvocationID("contract-artifacts-empty")
		h.PrepareInvocation(t, id)
		if err := h.RecordClaims(t, id, nil); err != nil {
			return fmt.Errorf("empty record errored, want a no-op: %w", err)
		}
		_, found, err := h.ReadClaims(t, id)
		if err != nil {
			return fmt.Errorf("read after empty record: %w", err)
		}
		if found {
			return errors.New("empty claim set left a record")
		}
		return nil
	})
}

// artifactsClaimSet is the canonical two-claim fixture the contract records: an
// image claim and a text claim whose digest binds its inline content, so a
// faithful readback must carry the label, provenance (sensitivity included), and
// inline text, not the artifact identity alone. Provenance names id as its
// producer so the set is well-formed for that invocation.
func artifactsClaimSet(id domain.InvocationID) []domain.AgentClaim {
	text := domain.ClaimText{
		MediaType: domain.MediaTypeTextMarkdown,
		Content:   "All checks green; the diff touches only docs.",
	}
	provenance := domain.Provenance{
		ProducerClass:        domain.ProducerAgent,
		ProducerInvocationID: id,
		HeadBinding:          domain.HeadBound,
		SourceHeadSHA:        "cafebabe",
		SensitivityClass:     domain.SensitivityNormal,
	}
	createdAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return []domain.AgentClaim{
		{
			Label: "screenshot", Artifact: "art-image", Digest: "sha256:img", Provenance: provenance,
			Metadata: domain.EvidenceMetadata{
				MediaType: domain.EvidenceMediaImagePNG, SizeBytes: 4096, CreatedAt: createdAt,
				Source: domain.EvidenceSourceClaim, Availability: domain.EvidenceAvailable,
			},
		},
		{
			Label: "change summary", Artifact: "art-text", Digest: text.ComputeDigest(),
			Text: &text, Provenance: provenance,
			Metadata: domain.EvidenceMetadata{
				MediaType: domain.EvidenceMediaTextMarkdown, SizeBytes: int64(len(text.Content)), CreatedAt: createdAt,
				Source: domain.EvidenceSourceClaim, Availability: domain.EvidenceAvailable,
			},
		},
	}
}

// sameClaims compares two claim sets by their canonical JSON, the encoding the
// record round-trips through: a dropped label, missing inline text, or elided
// provenance field surfaces as a body difference.
func sameClaims(want, got []domain.AgentClaim) error {
	wantBody, err := json.Marshal(want)
	if err != nil {
		return fmt.Errorf("marshal expected claims: %w", err)
	}
	gotBody, err := json.Marshal(got)
	if err != nil {
		return fmt.Errorf("marshal read-back claims: %w", err)
	}
	if string(wantBody) != string(gotBody) {
		return fmt.Errorf("claim set round-trip mismatch:\nwant: %s\ngot:  %s", wantBody, gotBody)
	}
	return nil
}
