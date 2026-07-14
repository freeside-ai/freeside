package domain

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
)

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
// and carrying per-key provenance (plan §3.2, §5.12). The digest is a content
// address of the resolved keys (see ComputeDigest), not a caller-chosen label:
// Validate rejects a digest that does not match, so persistence cannot store
// arbitrary values or provenance under an expected digest and lock in a false
// attribution.
type ResolvedPolicy struct {
	RunID  RunID       `json:"run_id"`
	Digest Digest      `json:"digest"`
	Keys   []PolicyKey `json:"keys"`
}

// NewResolvedPolicy builds a resolved policy whose keys are in canonical order
// and whose digest is computed from them, so both are authentic by construction.
// Callers do not supply the digest, and the keys may arrive in any order; a
// caller-set digest or a non-canonical key order is what Validate exists to
// reject on the paths that bypass this constructor (deserialization, direct
// struct literals).
func NewResolvedPolicy(runID RunID, keys []PolicyKey) (ResolvedPolicy, error) {
	// Store the keys in canonical order, so the persisted body is byte-for-byte
	// the same form the digest addresses: a reordered retry then converges on
	// the same body instead of colliding with the stored one. The copy also
	// detaches from the caller's backing array, so a post-construction mutation
	// of a shared key cannot silently invalidate the digest computed here.
	canonical := append([]PolicyKey(nil), keys...)
	sort.Slice(canonical, func(i, j int) bool { return canonical[i].Key < canonical[j].Key })
	p := ResolvedPolicy{RunID: runID, Keys: canonical}
	digest, err := p.ComputeDigest()
	if err != nil {
		return ResolvedPolicy{}, err
	}
	p.Digest = digest
	if err := p.Validate(); err != nil {
		return ResolvedPolicy{}, err
	}
	return p, nil
}

// ComputeDigest returns the content address of the resolved policy: a sha256
// over its canonical serialization. The canonical form is the key set sorted
// ascending by Key and JSON-marshaled as an array of {key, value, provenance}
// objects; run_id and the digest field itself are excluded. The digest is
// therefore a pure, order-independent content address: identical resolved
// content (keys, values, and per-key provenance) yields an identical digest
// regardless of which run holds it or what order the keys arrive in. It sorts
// defensively so it is a true content address for any input; a value that also
// passes Validate is already canonically ordered, so its stored body marshals
// to these exact bytes.
func (p ResolvedPolicy) ComputeDigest() (Digest, error) {
	sorted := append([]PolicyKey(nil), p.Keys...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
	body, err := json.Marshal(sorted)
	if err != nil {
		return "", fmt.Errorf("resolved policy digest: %w", err)
	}
	return Digest(fmt.Sprintf("sha256:%x", sha256.Sum256(body))), nil
}

// Validate reports whether the resolved policy is well-formed: identified,
// canonically ordered, content-addressed by an authentic digest, and with every
// key fully attributed.
func (p ResolvedPolicy) Validate() error {
	if p.RunID == "" {
		return fmt.Errorf("resolved policy run_id: %w", ErrEmptyID)
	}
	if p.Digest == "" {
		return fmt.Errorf("resolved policy digest: %w", ErrEmptyField)
	}
	// A resolved policy resolves at least one key: rein resolves into settings
	// with per-key provenance (plan §5.12), so a zero-key policy is degenerate.
	// Rejecting it also removes the one representation ambiguity canonical order
	// does not: an empty slice marshals to "[]" but the digest's key copy
	// collapses to nil ("null"), so a stored body could differ from the bytes
	// the digest addresses. With at least one key, the stored keys and the
	// hashed keys are the same non-empty array.
	if len(p.Keys) == 0 {
		return fmt.Errorf("resolved policy keys: %w", ErrEmptyField)
	}
	// A key may resolve only once (a duplicate makes the effective value
	// ambiguous, plan §5.12), and the keys must be in canonical (key-sorted)
	// order. Canonical order is what makes the persisted body equal the form the
	// digest addresses, so the write-once store cannot hold two distinct bodies
	// for one content digest.
	seen := make(map[string]struct{}, len(p.Keys))
	prev := ""
	for i, k := range p.Keys {
		if err := k.Validate(); err != nil {
			return err
		}
		if _, dup := seen[k.Key]; dup {
			return fmt.Errorf("resolved policy key %q: %w", k.Key, ErrDuplicate)
		}
		if i > 0 && k.Key < prev {
			return fmt.Errorf("resolved policy key %q after %q: %w", k.Key, prev, ErrKeysNotCanonical)
		}
		seen[k.Key] = struct{}{}
		prev = k.Key
	}
	// The digest is a content address, not a caller label: recompute it from the
	// keys and reject a mismatch. encode calls Validate on write and decode
	// calls it on read, so a forged digest is refused at both trust boundaries.
	computed, err := p.ComputeDigest()
	if err != nil {
		return err
	}
	if p.Digest != computed {
		return fmt.Errorf("resolved policy %s digest %q, content resolves to %q: %w", p.RunID, p.Digest, computed, ErrPolicyDigestMismatch)
	}
	return nil
}
