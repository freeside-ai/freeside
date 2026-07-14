package domain_test

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestResolvedPolicyProvenance is acceptance criterion 8: a resolved-policy
// record whose key lacks provenance is rejected; a fully attributed record
// validates.
func TestResolvedPolicyProvenance(t *testing.T) {
	attributed := domain.PolicyKey{
		Key:   "rein",
		Value: "tight",
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenancePreset,
			Digest: "sha256:preset",
		},
	}

	valid, err := domain.NewResolvedPolicy("run-1", []domain.PolicyKey{attributed})
	if err != nil {
		t.Fatalf("fully attributed policy rejected: %v", err)
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("fully attributed policy rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*domain.PolicyKey)
	}{
		{"no source", func(k *domain.PolicyKey) { k.Provenance.Source = "" }},
		{"invalid source", func(k *domain.PolicyKey) { k.Provenance.Source = "guessed" }},
		{"no digest", func(k *domain.PolicyKey) { k.Provenance.Digest = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := attributed
			tt.mutate(&k)
			p := domain.ResolvedPolicy{RunID: "run-1", Digest: "sha256:policy", Keys: []domain.PolicyKey{k}}
			if err := p.Validate(); !errors.Is(err, domain.ErrMissingKeyProvenance) {
				t.Fatalf("error = %v, want ErrMissingKeyProvenance", err)
			}
		})
	}

	// A key may resolve only once. The duplicate is caught before the digest
	// check, so a hand-set digest here is immaterial.
	dup := domain.ResolvedPolicy{RunID: "run-1", Digest: "sha256:policy", Keys: []domain.PolicyKey{attributed, attributed}}
	if err := dup.Validate(); !errors.Is(err, domain.ErrDuplicate) {
		t.Fatalf("duplicate policy key accepted: %v", err)
	}
}

// TestResolvedPolicyRejectsEmpty: a zero-key policy is degenerate and is
// rejected on every path. Rejecting it also closes the empty-vs-nil
// representation gap: an empty slice marshals to "[]" while the digest's key
// copy collapses to nil ("null"), so a stored body could otherwise differ from
// the bytes the digest addresses.
func TestResolvedPolicyRejectsEmpty(t *testing.T) {
	if _, err := domain.NewResolvedPolicy("run-1", nil); !errors.Is(err, domain.ErrEmptyField) {
		t.Fatalf("nil keys error = %v, want ErrEmptyField", err)
	}
	if _, err := domain.NewResolvedPolicy("run-1", []domain.PolicyKey{}); !errors.Is(err, domain.ErrEmptyField) {
		t.Fatalf("empty keys error = %v, want ErrEmptyField", err)
	}
	// A struct literal that bypasses the constructor is rejected too, whichever
	// empty representation it carries.
	nilLit := domain.ResolvedPolicy{RunID: "run-1", Digest: "sha256:x"}
	if err := nilLit.Validate(); !errors.Is(err, domain.ErrEmptyField) {
		t.Fatalf("nil-keys literal error = %v, want ErrEmptyField", err)
	}
	emptyLit := domain.ResolvedPolicy{RunID: "run-1", Digest: "sha256:x", Keys: []domain.PolicyKey{}}
	if err := emptyLit.Validate(); !errors.Is(err, domain.ErrEmptyField) {
		t.Fatalf("empty-keys literal error = %v, want ErrEmptyField", err)
	}
}

// TestResolvedPolicyDigest covers #33: the digest is an authenticated content
// address of the keys, verified (not trusted) at the trust boundary, so a
// forged digest is rejected and any content change changes the digest and
// cannot be stored under the old one.
func TestResolvedPolicyDigest(t *testing.T) {
	rein := domain.PolicyKey{
		Key: "rein", Value: "tight",
		Provenance: domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: "sha256:preset"},
	}
	budget := domain.PolicyKey{
		Key: "budget", Value: "500",
		Provenance: domain.KeyProvenance{Source: domain.ProvenanceOverride, Digest: "sha256:override"},
	}

	base, err := domain.NewResolvedPolicy("run-1", []domain.PolicyKey{rein, budget})
	if err != nil {
		t.Fatalf("NewResolvedPolicy: %v", err)
	}
	if base.Digest == "" {
		t.Fatal("constructor left an empty digest")
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("constructed policy rejected: %v", err)
	}

	// A forged digest is rejected on a path that bypasses the constructor
	// (this is what decode hits on read and a struct literal hits on write).
	forged := base
	forged.Digest = "sha256:forged"
	if err := forged.Validate(); !errors.Is(err, domain.ErrPolicyDigestMismatch) {
		t.Fatalf("forged digest error = %v, want ErrPolicyDigestMismatch", err)
	}

	// Key order does not change the address: the constructor canonicalizes, so
	// the same content in any order yields the same digest and the same keys.
	reordered, err := domain.NewResolvedPolicy("run-1", []domain.PolicyKey{budget, rein})
	if err != nil {
		t.Fatalf("NewResolvedPolicy reordered: %v", err)
	}
	if reordered.Digest != base.Digest {
		t.Fatalf("key order changed the digest: %q != %q", reordered.Digest, base.Digest)
	}
	if reordered.Keys[0].Key != base.Keys[0].Key || reordered.Keys[1].Key != base.Keys[1].Key {
		t.Fatalf("constructor did not canonicalize key order: %v vs %v", reordered.Keys, base.Keys)
	}

	// A non-canonical key order that bypasses the constructor is rejected, so
	// the persisted body is always the canonical form the digest addresses (a
	// reordered body cannot collide with the stored one in the write-once store).
	unsorted := domain.ResolvedPolicy{RunID: "run-1", Digest: base.Digest, Keys: []domain.PolicyKey{rein, budget}}
	if err := unsorted.Validate(); !errors.Is(err, domain.ErrKeysNotCanonical) {
		t.Fatalf("non-canonical key order error = %v, want ErrKeysNotCanonical", err)
	}

	// The run does not change the address: the same content under a different
	// run is the same content digest (run binding is enforced elsewhere).
	otherRun, err := domain.NewResolvedPolicy("run-2", []domain.PolicyKey{rein, budget})
	if err != nil {
		t.Fatalf("NewResolvedPolicy other run: %v", err)
	}
	if otherRun.Digest != base.Digest {
		t.Fatalf("run id changed the content digest: %q != %q", otherRun.Digest, base.Digest)
	}

	// Changing any content field changes the digest and cannot be stored under
	// the old one.
	mutations := []struct {
		name   string
		mutate func(k *domain.PolicyKey)
	}{
		{"value", func(k *domain.PolicyKey) { k.Value = "loose" }},
		{"provenance source", func(k *domain.PolicyKey) { k.Provenance.Source = domain.ProvenanceOverride }},
		{"provenance digest", func(k *domain.PolicyKey) { k.Provenance.Digest = "sha256:other" }},
	}
	for _, tt := range mutations {
		t.Run(tt.name, func(t *testing.T) {
			k := rein
			tt.mutate(&k)
			changed, err := domain.NewResolvedPolicy("run-1", []domain.PolicyKey{k, budget})
			if err != nil {
				t.Fatalf("NewResolvedPolicy: %v", err)
			}
			if changed.Digest == base.Digest {
				t.Fatalf("%s change did not change the digest", tt.name)
			}
			underOld := changed
			underOld.Digest = base.Digest
			if err := underOld.Validate(); !errors.Is(err, domain.ErrPolicyDigestMismatch) {
				t.Fatalf("changed content under old digest error = %v, want ErrPolicyDigestMismatch", err)
			}
		})
	}
}
