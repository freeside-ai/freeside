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

	valid := domain.ResolvedPolicy{
		RunID:  "run-1",
		Digest: "sha256:policy",
		Keys:   []domain.PolicyKey{attributed},
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

	// A key may resolve only once.
	dup := domain.ResolvedPolicy{RunID: "run-1", Digest: "sha256:policy", Keys: []domain.PolicyKey{attributed, attributed}}
	if err := dup.Validate(); !errors.Is(err, domain.ErrDuplicate) {
		t.Fatalf("duplicate policy key accepted: %v", err)
	}
}
