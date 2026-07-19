package export

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// EvidenceWorkspaceDir is the reserved top-level workspace subtree that holds
// the agent's transient evidence-channel staging: the descriptor plus the
// artifact files it names. The export walk skips this subtree (like the
// workspace's own .git), so evidence never enters the repo-change channel (plan
// §5.6: the two channels never mix); the trusted helper reaches its contents
// only through the declared descriptor. The subtree is per-run claim material,
// not durable repo content, and is expected to be gitignored in the agent image.
//
// It is deliberately distinct from the trusted control-plane directory
// (.freeside/, home of verify recipes): evidence is untrusted agent output and
// must never share a namespace with control-plane configuration.
const EvidenceWorkspaceDir = ".freeside-evidence"

// EvidenceDescriptorPath is the descriptor's fixed workspace path: the
// agent-authored declaration of which staged files become evidence entries and
// with what provenance.
const EvidenceDescriptorPath = EvidenceWorkspaceDir + "/evidence.json"

// EvidenceSourceVersion identifies the evidence *source descriptor* wire format
// (the agent-facing input the helper reads), independent of and distinct from
// the evidence *manifest* the helper emits (EvidenceManifestVersion). Any
// incompatible change bumps this string.
const EvidenceSourceVersion = "freeside.export.evidence-source/v1"

// EvidenceSource is one agent-declared evidence artifact: a staged file under
// the reserved subtree plus the typed provenance to attach to it. producer_class
// is deliberately absent from the schema: this channel admits only the agent
// class (evidence.go), which the helper forces, so the descriptor cannot even
// express a different one and DisallowUnknownFields rejects any attempt to.
type EvidenceSource struct {
	Label     string `json:"label"`
	MediaType string `json:"media_type"`
	// Path is the workspace-relative path to the artifact file. It must be a
	// canonical path under EvidenceWorkspaceDir and, at emission time, resolve
	// to a regular file the symlink-safe walk actually discovered there.
	Path             string                   `json:"path"`
	HeadBinding      EvidenceHeadBinding      `json:"head_binding"`
	SourceHeadSHA    string                   `json:"source_head_sha,omitempty"`
	SensitivityClass EvidenceSensitivityClass `json:"sensitivity_class"`
	// ProducerInvocationID is the agent's own run identity; it is an untrusted
	// claim, routed as such by the importer, never a trusted producer bit.
	ProducerInvocationID string `json:"producer_invocation_id"`
}

// provenance projects the source's provenance fields, with the producer class
// forced to agent, so EvidenceProvenance.Validate can enforce the shared
// invocation-id / head-binding / sensitivity rules in one place.
func (s EvidenceSource) provenance() EvidenceProvenance {
	return EvidenceProvenance{
		ProducerClass:        EvidenceProducerAgent,
		ProducerInvocationID: s.ProducerInvocationID,
		HeadBinding:          s.HeadBinding,
		SourceHeadSHA:        s.SourceHeadSHA,
		SensitivityClass:     s.SensitivityClass,
	}
}

// validEvidenceSourcePath reports whether p is a canonical workspace path
// strictly under the reserved evidence subtree and not the descriptor itself.
// It is a syntactic gate; the emitter separately resolves the path through a
// symlink-safe walk and requires a regular file, so an intermediate symlink or
// a non-regular target cannot escape or be hashed.
func validEvidenceSourcePath(p string) bool {
	return validCanonicalPath(p) &&
		strings.HasPrefix(p, EvidenceWorkspaceDir+"/") &&
		p != EvidenceDescriptorPath
}

// Validate reports whether the source is well-formed: a valid label, a
// non-empty media type, a canonical in-subtree path, and consistent provenance.
func (s EvidenceSource) Validate() error {
	if !validLabel(s.Label) {
		return fmt.Errorf("evidence source label %q: %w", s.Label, ErrInvalidLabel)
	}
	if s.MediaType == "" {
		return fmt.Errorf("evidence source %q media_type: %w", s.Label, ErrInvalidMediaType)
	}
	if !validEvidenceSourcePath(s.Path) {
		return fmt.Errorf("evidence source %q path %q: %w", s.Label, s.Path, ErrInvalidEvidenceSourcePath)
	}
	if err := s.provenance().Validate(); err != nil {
		return fmt.Errorf("evidence source %q: %w", s.Label, err)
	}
	return nil
}

// EvidenceSourceManifest is the agent-authored descriptor read from
// EvidenceDescriptorPath: schema version plus the sources to emit. Unlike the
// wire manifests, sources need not be pre-sorted (the helper sorts the emitted
// entries by label); only their labels must be unique, since duplicate labels
// cannot both survive into the canonical evidence manifest.
type EvidenceSourceManifest struct {
	Version string           `json:"version"`
	Sources []EvidenceSource `json:"sources"`
}

// Validate reports whether the descriptor is well-formed: the known version, at
// least one source (a present-but-empty descriptor is a mistake, not "no
// evidence" — the agent expresses that by omitting the descriptor), every source
// valid, and unique labels.
func (m EvidenceSourceManifest) Validate() error {
	if m.Version != EvidenceSourceVersion {
		return fmt.Errorf("evidence source descriptor version %q: %w", m.Version, ErrUnknownEvidenceSourceVersion)
	}
	if len(m.Sources) == 0 {
		return fmt.Errorf("evidence source descriptor: %w", ErrEmptyEvidenceSources)
	}
	seen := make(map[string]bool, len(m.Sources))
	for _, s := range m.Sources {
		if err := s.Validate(); err != nil {
			return err
		}
		if seen[s.Label] {
			return fmt.Errorf("evidence source label %q: %w", s.Label, ErrDuplicateEvidenceLabel)
		}
		seen[s.Label] = true
	}
	return nil
}

// DecodeEvidenceSourceManifest parses the agent-authored descriptor. It is
// strict but, unlike DecodeEvidenceManifest, deliberately not
// canonical-or-reject: the descriptor is written by the untrusted agent with
// arbitrary (still-valid) JSON formatting, not by a trusted Encode, so requiring
// byte-canonical input would reject legitimate descriptors. Strictness instead
// comes from a UTF-8 pre-check, DisallowUnknownFields (so a smuggled
// producer_class, publish_eligible, or verification_recipe_digest is malformed
// input, not an ignored extra), EOF-after-one-value, and full validation. The
// decoded value is re-emitted canonically as evidence.json and independently
// re-gated by the importer's DecodeEvidenceManifest, so a duplicate JSON key
// collapsing last-wins here cannot smuggle an unvalidated value downstream.
func DecodeEvidenceSourceManifest(data []byte) (EvidenceSourceManifest, error) {
	if !utf8.Valid(data) {
		return EvidenceSourceManifest{}, fmt.Errorf("decode evidence source descriptor: %w", ErrInvalidUTF8)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m EvidenceSourceManifest
	if err := dec.Decode(&m); err != nil {
		return EvidenceSourceManifest{}, fmt.Errorf("decode evidence source descriptor: %w", err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return EvidenceSourceManifest{}, fmt.Errorf("decode evidence source descriptor: %w", ErrTrailingContent)
	}
	if err := m.Validate(); err != nil {
		return EvidenceSourceManifest{}, err
	}
	return m, nil
}
