package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	// CapabilityManifestEncodingVersion tags the manifest's canonical encoding.
	CapabilityManifestEncodingVersion = 1
	// CapabilityManifestPolicyKey names the resolved-policy manifest set.
	CapabilityManifestPolicyKey = "execution.capability_manifests"
)

// CapabilityManifest is one predefined, content-addressed execution
// environment the operator may select for a retry. This first contract carries
// only the egress profile because that is the only dimension the runtime can
// re-gate at admission today.
type CapabilityManifest struct {
	EncodingVersion int           `json:"encoding_version"`
	Name            string        `json:"name"`
	EgressProfile   EgressProfile `json:"egress_profile"`
	Digest          Digest        `json:"digest"`
}

type canonicalCapabilityManifest struct {
	EncodingVersion int           `json:"encoding_version"`
	Name            string        `json:"name"`
	EgressProfile   EgressProfile `json:"egress_profile"`
}

func (m CapabilityManifest) canonical() canonicalCapabilityManifest {
	return canonicalCapabilityManifest{
		EncodingVersion: m.EncodingVersion,
		Name:            m.Name,
		EgressProfile:   m.EgressProfile,
	}
}

// NewCapabilityManifest builds a validated manifest and computes its content
// address from the explicit-version canonical encoding.
func NewCapabilityManifest(name string, egress EgressProfile) (CapabilityManifest, error) {
	m := CapabilityManifest{
		EncodingVersion: CapabilityManifestEncodingVersion,
		Name:            name,
		EgressProfile:   egress,
	}
	digest, err := m.ComputeDigest()
	if err != nil {
		return CapabilityManifest{}, err
	}
	m.Digest = digest
	if err := m.Validate(); err != nil {
		return CapabilityManifest{}, err
	}
	return m, nil
}

// ComputeDigest hashes the canonical encoding without the digest field.
func (m CapabilityManifest) ComputeDigest() (Digest, error) {
	body, err := json.Marshal(m.canonical())
	if err != nil {
		return "", fmt.Errorf("capability manifest canonical encoding: %w", err)
	}
	return Digest(contentaddr.Sum(body)), nil
}

// Validate reports whether the manifest is well-formed and authentically
// content-addressed.
func (m CapabilityManifest) Validate() error {
	if m.EncodingVersion != CapabilityManifestEncodingVersion {
		return fmt.Errorf("capability manifest encoding_version %d: %w",
			m.EncodingVersion, ErrCapabilityManifestInvalid)
	}
	if m.Name == "" || m.Name != strings.TrimSpace(m.Name) || !printableASCII(m.Name) {
		return fmt.Errorf("capability manifest name %q: %w", m.Name, ErrCapabilityManifestInvalid)
	}
	if !m.EgressProfile.valid() {
		return fmt.Errorf("capability manifest egress_profile %q: %w",
			m.EgressProfile, ErrCapabilityManifestInvalid)
	}
	if !contentaddr.Valid(string(m.Digest)) {
		return fmt.Errorf("capability manifest digest %q: %w", m.Digest, ErrInvalidDigest)
	}
	computed, err := m.ComputeDigest()
	if err != nil {
		return err
	}
	if m.Digest != computed {
		return fmt.Errorf("capability manifest digest %q, content resolves to %q: %w",
			m.Digest, computed, ErrCapabilityManifestDigestMismatch)
	}
	return nil
}

func printableASCII(value string) bool {
	for index := 0; index < len(value); index++ {
		if value[index] < 0x20 || value[index] > 0x7e {
			return false
		}
	}
	return true
}

// Encode emits the validated canonical persisted form.
func (m CapabilityManifest) Encode() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("capability manifest encode: %w", err)
	}
	return body, nil
}

// DecodeCapabilityManifest strictly decodes and revalidates one manifest.
func DecodeCapabilityManifest(body []byte) (CapabilityManifest, error) {
	var manifest CapabilityManifest
	if err := strictjson.Decode(
		body, &manifest, strictjson.RejectInvalidUTF8, MaxAgentFragmentBytes,
	); err != nil {
		return CapabilityManifest{}, fmt.Errorf("capability manifest decode: %w", err)
	}
	if err := manifest.Validate(); err != nil {
		return CapabilityManifest{}, err
	}
	return manifest, nil
}

// CapabilityManifestOffer is the non-authoritative card projection of a
// policy manifest. The accepted digest is re-derived from policy before use.
type CapabilityManifestOffer struct {
	Name          string        `json:"name"`
	EgressProfile EgressProfile `json:"egress_profile"`
	Digest        Digest        `json:"digest"`
}

// Offer returns the manifest's card projection.
func (m CapabilityManifest) Offer() CapabilityManifestOffer {
	return CapabilityManifestOffer{
		Name: m.Name, EgressProfile: m.EgressProfile, Digest: m.Digest,
	}
}

// Validate reports whether an offer is structurally usable. Authenticity is
// established by matching it to a re-derived policy manifest.
func (o CapabilityManifestOffer) Validate() error {
	if o.Name == "" || o.Name != strings.TrimSpace(o.Name) ||
		!o.EgressProfile.valid() || !contentaddr.Valid(string(o.Digest)) {
		return fmt.Errorf("capability manifest offer %q/%q/%q: %w",
			o.Name, o.EgressProfile, o.Digest, ErrCapabilityManifestInvalid)
	}
	return nil
}

// CapabilityManifestsFromPolicy strictly reconstructs the canonical manifest
// set from resolved policy. One malformed member rejects the whole set.
func CapabilityManifestsFromPolicy(policy ResolvedPolicy) ([]CapabilityManifest, error) {
	var (
		value string
		found bool
	)
	for _, key := range policy.Keys {
		if key.Key == CapabilityManifestPolicyKey {
			value = key.Value
			found = true
			break
		}
	}
	if !found {
		return nil, nil
	}
	if value == "" {
		return nil, fmt.Errorf("policy %s is empty: %w",
			CapabilityManifestPolicyKey, ErrCapabilityManifestInvalid)
	}
	var manifests []CapabilityManifest
	if err := strictjson.Decode(
		[]byte(value), &manifests, strictjson.RejectInvalidUTF8, strictjson.Limit(1<<20),
	); err != nil {
		return nil, fmt.Errorf("policy %s: %w", CapabilityManifestPolicyKey, err)
	}
	if manifests == nil {
		return nil, fmt.Errorf("policy %s must be a JSON array: %w",
			CapabilityManifestPolicyKey, ErrCapabilityManifestInvalid)
	}
	canonical, err := json.Marshal(manifests)
	if err != nil || !bytes.Equal(canonical, []byte(value)) {
		return nil, fmt.Errorf("policy %s is not canonical JSON: %w",
			CapabilityManifestPolicyKey, errors.Join(err, ErrCapabilityManifestInvalid))
	}
	seenNames := make(map[string]struct{}, len(manifests))
	seenDigests := make(map[Digest]struct{}, len(manifests))
	for index, manifest := range manifests {
		if err := manifest.Validate(); err != nil {
			return nil, fmt.Errorf("policy %s manifest[%d]: %w",
				CapabilityManifestPolicyKey, index, err)
		}
		if _, duplicate := seenNames[manifest.Name]; duplicate {
			return nil, fmt.Errorf("policy %s name %q: %w",
				CapabilityManifestPolicyKey, manifest.Name, ErrDuplicate)
		}
		if _, duplicate := seenDigests[manifest.Digest]; duplicate {
			return nil, fmt.Errorf("policy %s digest %q: %w",
				CapabilityManifestPolicyKey, manifest.Digest, ErrDuplicate)
		}
		seenNames[manifest.Name] = struct{}{}
		seenDigests[manifest.Digest] = struct{}{}
	}
	sort.Slice(manifests, func(i, j int) bool { return manifests[i].Name < manifests[j].Name })
	return manifests, nil
}
