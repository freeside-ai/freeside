package domain

import "fmt"

// KeyProvenance records where one resolved-policy key's value came from: a
// preset default or an explicit override, plus the digest of the source it was
// resolved from (plan §3.2, §5.12). Every key must carry it.
type KeyProvenance struct {
	Source ProvenanceSource `json:"source"`
	Digest Digest           `json:"digest"`
}

// PolicyKey is one resolved policy setting with its per-key provenance.
type PolicyKey struct {
	Key        string        `json:"key"`
	Value      string        `json:"value"`
	Provenance KeyProvenance `json:"provenance"`
}

// Validate reports whether the key is well-formed and fully attributed. A key
// whose provenance is missing (no source or no digest) is rejected: rein
// resolves into policy with per-key provenance, and an unattributed key breaks
// that guarantee (plan §5.12).
func (k PolicyKey) Validate() error {
	if k.Key == "" {
		return fmt.Errorf("policy key: %w", ErrEmptyField)
	}
	if !k.Provenance.Source.valid() {
		return fmt.Errorf("policy key %q provenance source %q: %w", k.Key, k.Provenance.Source, ErrMissingKeyProvenance)
	}
	if k.Provenance.Digest == "" {
		return fmt.Errorf("policy key %q provenance digest: %w", k.Key, ErrMissingKeyProvenance)
	}
	return nil
}

// ResolvedPolicy is the per-run policy that rein resolves into, digest-addressed
// and carrying per-key provenance (plan §3.2, §5.12).
type ResolvedPolicy struct {
	RunID  RunID       `json:"run_id"`
	Digest Digest      `json:"digest"`
	Keys   []PolicyKey `json:"keys"`
}

// Validate reports whether the resolved policy is well-formed: identified,
// digest-addressed, and with every key fully attributed.
func (p ResolvedPolicy) Validate() error {
	if p.RunID == "" {
		return fmt.Errorf("resolved policy run_id: %w", ErrEmptyID)
	}
	if p.Digest == "" {
		return fmt.Errorf("resolved policy digest: %w", ErrEmptyField)
	}
	// A key may resolve only once: a duplicate makes the effective value
	// order-dependent rather than unambiguous (plan §5.12 per-key provenance).
	seen := make(map[string]struct{}, len(p.Keys))
	for _, k := range p.Keys {
		if err := k.Validate(); err != nil {
			return err
		}
		if _, dup := seen[k.Key]; dup {
			return fmt.Errorf("resolved policy key %q: %w", k.Key, ErrDuplicate)
		}
		seen[k.Key] = struct{}{}
	}
	return nil
}
