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

// EvidenceManifestVersion identifies the evidence channel's wire format: the
// second of the exactly two channels that leave an agent workspace (plan
// §5.6). It is a separate schema from the repo-change manifest, versioned
// independently; any incompatible change bumps this string.
const EvidenceManifestVersion = "freeside.export.evidence/v1"

// EvidenceFilename is the evidence manifest's name inside the handoff
// directory, beside the repo channel's manifest.json.
const EvidenceFilename = "evidence.json"

// EvidenceBlobsDirname is the handoff subdirectory holding evidence content
// blobs, digest-addressed like the repo channel's blobs but physically
// separate from them: the two channels never mix (plan §5.6), so an evidence
// digest never resolves through a repo-change blob or manifest entry, and
// cross-channel substitution is structurally impossible rather than merely
// checked.
const EvidenceBlobsDirname = "evidence"

// EvidenceProducerClass is the producer class an evidence-manifest entry may
// declare. The manifest is written inside the agent workspace, so the only
// valid member is "agent" by design (plan §5.15 rule 2): verifier and daemon
// artifacts are authored after import under trusted recipes and never pass
// through this channel. The token matches the domain ProducerClass wire form,
// but the type is local: the helper is a standalone binary and imports
// nothing from the shared domain package.
type EvidenceProducerClass string

// EvidenceProducerAgent is the sole valid producer class at this seam.
const EvidenceProducerAgent EvidenceProducerClass = "agent"

// AllEvidenceProducerClasses lists every valid EvidenceProducerClass. Its
// single member is the contract, not an accident: widening it would open the
// agent channel to producer classes it must never carry.
var AllEvidenceProducerClasses = []EvidenceProducerClass{EvidenceProducerAgent}

func (c EvidenceProducerClass) valid() bool {
	switch c {
	case EvidenceProducerAgent:
		return true
	default:
		return false
	}
}

// EvidenceHeadBinding mirrors the domain HeadBinding wire tokens: whether an
// artifact's validity is bound to the head it was produced at. The explicit
// mode, not the presence of a head, is the source of truth, so an omitted
// head can never be read two ways.
type EvidenceHeadBinding string

const (
	// EvidenceHeadBound marks an artifact invalidated by a remediation head;
	// it must name its source head.
	EvidenceHeadBound EvidenceHeadBinding = "head_bound"
	// EvidenceHeadIndependent marks an artifact deliberately decoupled from
	// any head; it must not carry one.
	EvidenceHeadIndependent EvidenceHeadBinding = "head_independent"
)

// AllEvidenceHeadBindings lists every valid EvidenceHeadBinding.
var AllEvidenceHeadBindings = []EvidenceHeadBinding{EvidenceHeadBound, EvidenceHeadIndependent}

func (b EvidenceHeadBinding) valid() bool {
	switch b {
	case EvidenceHeadBound, EvidenceHeadIndependent:
		return true
	default:
		return false
	}
}

// EvidenceSensitivityClass mirrors the domain SensitivityClass wire tokens:
// the artifact's confidentiality tier.
type EvidenceSensitivityClass string

const (
	EvidenceSensitivityNormal    EvidenceSensitivityClass = "normal"
	EvidenceSensitivitySensitive EvidenceSensitivityClass = "sensitive"
	EvidenceSensitivityHigh      EvidenceSensitivityClass = "high_sensitivity"
)

// AllEvidenceSensitivityClasses lists every valid EvidenceSensitivityClass.
var AllEvidenceSensitivityClasses = []EvidenceSensitivityClass{
	EvidenceSensitivityNormal,
	EvidenceSensitivitySensitive,
	EvidenceSensitivityHigh,
}

func (c EvidenceSensitivityClass) valid() bool {
	switch c {
	case EvidenceSensitivityNormal, EvidenceSensitivitySensitive, EvidenceSensitivityHigh:
		return true
	default:
		return false
	}
}

// EvidenceProvenance is the typed provenance an evidence entry must carry
// (plan §5.15 rule 2). It deliberately has no publish_eligible and no
// verification_recipe_digest field: trusted policy computes eligibility (the
// agent never supplies it), and agent output is never produced under a
// verification recipe. The importer strict-decodes this manifest, so a
// hostile manifest smuggling either field is malformed input, not an ignored
// extra.
type EvidenceProvenance struct {
	ProducerClass        EvidenceProducerClass `json:"producer_class"`
	ProducerInvocationID string                `json:"producer_invocation_id"`
	HeadBinding          EvidenceHeadBinding   `json:"head_binding"`
	// omitempty so head-independent provenance (which must not carry a head)
	// omits the field entirely and head-bound provenance always emits it,
	// matching the domain Provenance wire shape.
	SourceHeadSHA    string                   `json:"source_head_sha,omitempty"`
	SensitivityClass EvidenceSensitivityClass `json:"sensitivity_class"`
}

// Validate reports whether the provenance is well-formed: the agent producer
// class, a producer invocation, and a consistent explicit head-binding mode.
func (p EvidenceProvenance) Validate() error {
	if !p.ProducerClass.valid() {
		return fmt.Errorf("evidence provenance producer_class %q: %w", p.ProducerClass, ErrInvalidEvidenceProducer)
	}
	if p.ProducerInvocationID == "" {
		return fmt.Errorf("evidence provenance producer_invocation_id: %w", ErrEmptyInvocationID)
	}
	if !p.HeadBinding.valid() {
		return fmt.Errorf("evidence provenance head_binding %q: %w", p.HeadBinding, ErrInvalidHeadBinding)
	}
	// Head-bound evidence must name its head; head-independent evidence must
	// not carry one, or the two representations would be ambiguous. The
	// explicit mode is the single source of truth (domain Provenance holds
	// the same rule).
	switch p.HeadBinding {
	case EvidenceHeadBound:
		if p.SourceHeadSHA == "" {
			return fmt.Errorf("head_bound evidence provenance lacks source_head_sha: %w", ErrProvenanceInconsistent)
		}
	case EvidenceHeadIndependent:
		if p.SourceHeadSHA != "" {
			return fmt.Errorf("head_independent evidence provenance carries source_head_sha %q: %w", p.SourceHeadSHA, ErrProvenanceInconsistent)
		}
	}
	if !p.SensitivityClass.valid() {
		return fmt.Errorf("evidence provenance sensitivity_class %q: %w", p.SensitivityClass, ErrInvalidSensitivityClass)
	}
	return nil
}

// EvidenceEntry is one agent-produced artifact in the evidence manifest. It
// is self-contained: the digest addresses a blob under EvidenceBlobsDirname,
// never a repo-change manifest entry. Every field is required; the entry
// carries exactly what the importer needs to route the artifact into a
// labeled domain agent claim with magic/type/size validation.
type EvidenceEntry struct {
	Label      string             `json:"label"`
	MediaType  string             `json:"media_type"`
	Size       int64              `json:"size"`
	Digest     Digest             `json:"digest"`
	Provenance EvidenceProvenance `json:"provenance"`
}

// validLabel reports whether s can serve as an evidence label: non-empty
// valid UTF-8 free of NUL, so labels survive as canonical JSON strings and
// sort deterministically by their bytes.
func validLabel(s string) bool {
	return s != "" && utf8.ValidString(s) && !strings.ContainsRune(s, 0)
}

// Validate reports whether the entry is well-formed.
func (e EvidenceEntry) Validate() error {
	if !validLabel(e.Label) {
		return fmt.Errorf("evidence entry label %q: %w", e.Label, ErrInvalidLabel)
	}
	if e.MediaType == "" {
		return fmt.Errorf("evidence entry %q media_type: %w", e.Label, ErrInvalidMediaType)
	}
	if e.Size < 0 {
		return fmt.Errorf("evidence entry %q: %w", e.Label, ErrNegativeSize)
	}
	if !e.Digest.valid() {
		return fmt.Errorf("evidence entry %q: %w", e.Label, ErrInvalidDigest)
	}
	if err := e.Provenance.Validate(); err != nil {
		return fmt.Errorf("evidence entry %q: %w", e.Label, err)
	}
	return nil
}

// EvidenceManifest is the normalized description of one workspace's evidence
// channel: schema version plus entries sorted bytewise by label. Like the
// repo-change manifest it carries no counts, timestamps, or host details, so
// identical evidence yields byte-identical output.
type EvidenceManifest struct {
	Version string          `json:"version"`
	Entries []EvidenceEntry `json:"entries"`
}

// Validate reports whether the manifest is well-formed: the known version,
// every entry valid, and entries in canonical order (strictly ascending by
// raw label bytes, so the encoding is deterministic and labels are unique).
func (m EvidenceManifest) Validate() error {
	if m.Version != EvidenceManifestVersion {
		return fmt.Errorf("evidence manifest version %q: %w", m.Version, ErrUnknownEvidenceVersion)
	}
	// Entries is a required non-null array (the wire convention): a nil
	// slice marshals as "entries": null, which would give the canonical
	// boundary two accepted encodings of empty and let a non-array
	// manifest through, so nil is invalid and the empty manifest is
	// exactly "entries": [].
	if m.Entries == nil {
		return fmt.Errorf("evidence manifest entries: %w", ErrNullEntries)
	}
	for i, e := range m.Entries {
		if err := e.Validate(); err != nil {
			return err
		}
		if i > 0 && bytes.Compare([]byte(m.Entries[i-1].Label), []byte(e.Label)) >= 0 {
			return fmt.Errorf("evidence entry %d: %w", i, ErrEvidenceNotCanonical)
		}
	}
	return nil
}

// DecodeEvidenceManifest is the boundary the importer consumes evidence.json
// through, and it is canonical-or-reject: the only accepted wire form is the
// exact byte form Encode produces. A strict decode (unknown fields and
// trailing content are malformed input, never ignored extras, so a manifest
// smuggling a trust bit like publish_eligible fails here) and full
// validation come first for precise errors, but the final gate re-encodes
// the decoded value and requires byte equality with the input, which closes
// the decoder leniencies no field check can see: invalid UTF-8 laundered to
// U+FFFD during decode, duplicate keys resolved last-wins, and
// non-canonical whitespace or ordering. The trusted helper writes this file
// via Encode, so a mismatch is hostile or corrupt, never legitimate. Size
// and entry-count caps are importer policy applied before these bytes
// arrive.
func DecodeEvidenceManifest(data []byte) (EvidenceManifest, error) {
	// Checked before decode because encoding/json replaces invalid UTF-8
	// with U+FFFD instead of failing, which would launder hostile bytes
	// into a valid-looking manifest; the canonical-equality gate below
	// would also catch it, but this names the failure precisely.
	if !utf8.Valid(data) {
		return EvidenceManifest{}, fmt.Errorf("decode evidence manifest: %w", ErrInvalidUTF8)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m EvidenceManifest
	if err := dec.Decode(&m); err != nil {
		return EvidenceManifest{}, fmt.Errorf("decode evidence manifest: %w", err)
	}
	// Require EOF after the one value: dec.More reports another value, so a
	// bare trailing delimiter slips past it; the second decode that must
	// return io.EOF rejects every trailing byte (the repo's strict-decoder
	// convention, e.g. verify/recipe.go, importer/handoff.go).
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return EvidenceManifest{}, fmt.Errorf("decode evidence manifest: %w", ErrTrailingContent)
	}
	canonical, err := m.Encode()
	if err != nil {
		return EvidenceManifest{}, err
	}
	if !bytes.Equal(data, canonical) {
		return EvidenceManifest{}, fmt.Errorf("decode evidence manifest: %w", ErrNotCanonicalEncoding)
	}
	return m, nil
}

// Encode returns the manifest's wire bytes: the validated value marshaled
// with two-space indentation plus a trailing newline, the exact byte form
// written to evidence.json and pinned by the golden tests.
func (m EvidenceManifest) Encode() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode evidence manifest: %w", err)
	}
	return append(body, '\n'), nil
}
