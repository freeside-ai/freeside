package export_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// fullEvidenceManifest is a fixed, valid evidence manifest covering both
// head-binding modes, sorted bytewise by label; the golden doubles as a
// validation-positive case and pins the v1 wire bytes the importer will
// parse (including that head-independent provenance omits source_head_sha).
func fullEvidenceManifest() export.EvidenceManifest {
	return export.EvidenceManifest{
		Version: export.EvidenceManifestVersion,
		Entries: []export.EvidenceEntry{
			{
				Label:     "after-screenshot",
				MediaType: "image/png",
				Size:      2048,
				Digest:    *digestOf("4d"),
				Provenance: export.EvidenceProvenance{
					ProducerClass:        export.EvidenceProducerAgent,
					ProducerInvocationID: "agent-run-7f",
					HeadBinding:          export.EvidenceHeadBound,
					SourceHeadSHA:        strings.Repeat("4e", 20),
					SensitivityClass:     export.EvidenceSensitivityNormal,
				},
			},
			{
				Label:     "style-reference",
				MediaType: "image/jpeg",
				Size:      0,
				Digest:    *digestOf("5e"),
				Provenance: export.EvidenceProvenance{
					ProducerClass:        export.EvidenceProducerAgent,
					ProducerInvocationID: "agent-run-7f",
					HeadBinding:          export.EvidenceHeadIndependent,
					SensitivityClass:     export.EvidenceSensitivitySensitive,
				},
			},
		},
	}
}

// TestEvidenceGolden pins the evidence-channel wire format (the second
// workspace-exit channel, plan §5.6). Regenerate with:
// go test ./internal/export -run TestEvidenceGolden -update.
func TestEvidenceGolden(t *testing.T) {
	m := fullEvidenceManifest()
	got, err := m.Encode()
	if err != nil {
		t.Fatalf("fixture must be valid: %v", err)
	}
	golden.Assert(t, "evidence_manifest", got)
}

// TestEvidenceManifestValidate enumerates the invariants Validate guards,
// one mutation of the valid fixture per case.
func TestEvidenceManifestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*export.EvidenceManifest)
		wantErr error
	}{
		{"unknown version", func(m *export.EvidenceManifest) {
			m.Version = "freeside.export.evidence/v0"
		}, export.ErrUnknownEvidenceVersion},
		{"repo-manifest version", func(m *export.EvidenceManifest) {
			m.Version = export.ManifestVersion
		}, export.ErrUnknownEvidenceVersion},
		{"zero-value version", func(m *export.EvidenceManifest) {
			m.Version = ""
		}, export.ErrUnknownEvidenceVersion},
		{"null entries", func(m *export.EvidenceManifest) {
			m.Entries = nil
		}, export.ErrNullEntries},
		{"unsorted entries", func(m *export.EvidenceManifest) {
			m.Entries[0], m.Entries[1] = m.Entries[1], m.Entries[0]
		}, export.ErrEvidenceNotCanonical},
		{"duplicate label", func(m *export.EvidenceManifest) {
			m.Entries[1].Label = m.Entries[0].Label
		}, export.ErrEvidenceNotCanonical},
		{"empty label", func(m *export.EvidenceManifest) {
			m.Entries[0].Label = ""
		}, export.ErrInvalidLabel},
		{"non-UTF-8 label", func(m *export.EvidenceManifest) {
			m.Entries[0].Label = "bad\xffname"
		}, export.ErrInvalidLabel},
		{"NUL in label", func(m *export.EvidenceManifest) {
			m.Entries[0].Label = "bad\x00name"
		}, export.ErrInvalidLabel},
		{"empty media_type", func(m *export.EvidenceManifest) {
			m.Entries[0].MediaType = ""
		}, export.ErrInvalidMediaType},
		{"negative size", func(m *export.EvidenceManifest) {
			m.Entries[0].Size = -1
		}, export.ErrNegativeSize},
		{"digest without prefix", func(m *export.EvidenceManifest) {
			m.Entries[0].Digest = export.Digest(strings.Repeat("4d", 32))
		}, export.ErrInvalidDigest},
		{"empty digest", func(m *export.EvidenceManifest) {
			m.Entries[0].Digest = ""
		}, export.ErrInvalidDigest},
		{"verifier producer", func(m *export.EvidenceManifest) {
			m.Entries[0].Provenance.ProducerClass = "verifier"
		}, export.ErrInvalidEvidenceProducer},
		{"daemon producer", func(m *export.EvidenceManifest) {
			m.Entries[0].Provenance.ProducerClass = "daemon"
		}, export.ErrInvalidEvidenceProducer},
		{"zero-value producer", func(m *export.EvidenceManifest) {
			m.Entries[0].Provenance.ProducerClass = ""
		}, export.ErrInvalidEvidenceProducer},
		{"empty invocation id", func(m *export.EvidenceManifest) {
			m.Entries[0].Provenance.ProducerInvocationID = ""
		}, export.ErrEmptyInvocationID},
		{"unknown head binding", func(m *export.EvidenceManifest) {
			m.Entries[0].Provenance.HeadBinding = "maybe_bound"
		}, export.ErrInvalidHeadBinding},
		{"zero-value head binding", func(m *export.EvidenceManifest) {
			m.Entries[0].Provenance.HeadBinding = ""
		}, export.ErrInvalidHeadBinding},
		{"head_bound without head", func(m *export.EvidenceManifest) {
			m.Entries[0].Provenance.SourceHeadSHA = ""
		}, export.ErrProvenanceInconsistent},
		{"head_independent with head", func(m *export.EvidenceManifest) {
			m.Entries[1].Provenance.SourceHeadSHA = strings.Repeat("4e", 20)
		}, export.ErrProvenanceInconsistent},
		{"unknown sensitivity", func(m *export.EvidenceManifest) {
			m.Entries[0].Provenance.SensitivityClass = "public"
		}, export.ErrInvalidSensitivityClass},
		{"zero-value sensitivity", func(m *export.EvidenceManifest) {
			m.Entries[0].Provenance.SensitivityClass = ""
		}, export.ErrInvalidSensitivityClass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := fullEvidenceManifest()
			tc.mutate(&m)
			if err := m.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestEvidenceWireExcludesTrustFields proves the agent side has no
// publish_eligible and no verification_recipe_digest by construction: the
// wire structs carry no JSON field with either name at any level. Trusted
// policy computes eligibility after import; the agent never supplies it
// (plan §5.15 rule 2).
func TestEvidenceWireExcludesTrustFields(t *testing.T) {
	forbidden := map[string]bool{
		"publish_eligible":           true,
		"verification_recipe_digest": true,
	}
	for _, typ := range []reflect.Type{
		reflect.TypeOf(export.EvidenceManifest{}),
		reflect.TypeOf(export.EvidenceEntry{}),
		reflect.TypeOf(export.EvidenceProvenance{}),
	} {
		for i := 0; i < typ.NumField(); i++ {
			tag := typ.Field(i).Tag.Get("json")
			name, _, _ := strings.Cut(tag, ",")
			if forbidden[name] {
				t.Errorf("%s.%s carries forbidden wire field %q", typ.Name(), typ.Field(i).Name, name)
			}
		}
	}
}

// TestEvidenceStrictDecodeRejectsTrustFields pins the importer's re-gate
// behavior at this seam: a manifest smuggling a trust bit at any level is
// malformed input to a strict decoder, not an ignored extra.
func TestEvidenceStrictDecodeRejectsTrustFields(t *testing.T) {
	wire, err := fullEvidenceManifest().Encode()
	if err != nil {
		t.Fatalf("fixture must be valid: %v", err)
	}
	entry := func(m map[string]any) map[string]any {
		return m["entries"].([]any)[0].(map[string]any)
	}
	provenance := func(m map[string]any) map[string]any {
		return entry(m)["provenance"].(map[string]any)
	}
	cases := []struct {
		name   string
		inject func(m map[string]any)
	}{
		{"publish_eligible on provenance", func(m map[string]any) {
			provenance(m)["publish_eligible"] = true
		}},
		{"publish_eligible on entry", func(m map[string]any) {
			entry(m)["publish_eligible"] = true
		}},
		{"publish_eligible on manifest", func(m map[string]any) {
			m["publish_eligible"] = true
		}},
		{"verification_recipe_digest on provenance", func(m map[string]any) {
			provenance(m)["verification_recipe_digest"] = "sha256:" + strings.Repeat("6f", 32)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m map[string]any
			if err := json.Unmarshal(wire, &m); err != nil {
				t.Fatal(err)
			}
			tc.inject(m)
			smuggled, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := export.DecodeEvidenceManifest(smuggled); err == nil {
				t.Fatal("strict decode accepted a smuggled trust field")
			}
		})
	}
}

// TestDecodeEvidenceManifest pins the decode boundary itself: the golden
// bytes round-trip, and trailing content is malformed input.
func TestDecodeEvidenceManifest(t *testing.T) {
	want := fullEvidenceManifest()
	wire, err := want.Encode()
	if err != nil {
		t.Fatalf("fixture must be valid: %v", err)
	}
	got, err := export.DecodeEvidenceManifest(wire)
	if err != nil {
		t.Fatalf("DecodeEvidenceManifest: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	// Trailing content of every shape is malformed: a second JSON value, a
	// bare closing delimiter (invisible to dec.More), and stray bytes.
	for _, trailer := range []string{"{}", "]", "}", "x"} {
		if _, err := export.DecodeEvidenceManifest(append(wire, trailer...)); !errors.Is(err, export.ErrTrailingContent) {
			t.Errorf("trailing %q error = %v, want ErrTrailingContent", trailer, err)
		}
	}

	// encoding/json launders invalid UTF-8 to U+FFFD instead of failing, so
	// raw hostile bytes inside a label would decode into a valid-looking
	// manifest; the boundary rejects them before decode.
	hostile := []byte(strings.Replace(string(wire), "after-screenshot", "after-\xffshot", 1))
	if _, err := export.DecodeEvidenceManifest(hostile); !errors.Is(err, export.ErrInvalidUTF8) {
		t.Errorf("invalid UTF-8 error = %v, want ErrInvalidUTF8", err)
	}

	// Only Encode's exact byte form is accepted: duplicate keys (last-wins
	// under encoding/json) and re-formatted whitespace both decode cleanly
	// but are not the canonical bytes.
	var reformatted bytes.Buffer
	if err := json.Compact(&reformatted, wire); err != nil {
		t.Fatal(err)
	}
	if _, err := export.DecodeEvidenceManifest(reformatted.Bytes()); !errors.Is(err, export.ErrNotCanonicalEncoding) {
		t.Errorf("compact re-encoding error = %v, want ErrNotCanonicalEncoding", err)
	}
	duplicated := []byte(strings.Replace(string(wire),
		`"label": "after-screenshot",`,
		`"label": "decoy", "label": "after-screenshot",`, 1))
	if _, err := export.DecodeEvidenceManifest(duplicated); !errors.Is(err, export.ErrNotCanonicalEncoding) {
		t.Errorf("duplicate-key error = %v, want ErrNotCanonicalEncoding", err)
	}

	// Empty has exactly one wire form, "entries": []: the null encoding a
	// nil slice would produce is invalid, so a hostile "entries": null
	// manifest cannot pass as a degenerate empty one.
	empty := export.EvidenceManifest{Version: export.EvidenceManifestVersion, Entries: []export.EvidenceEntry{}}
	emptyWire, err := empty.Encode()
	if err != nil {
		t.Fatalf("empty manifest must encode: %v", err)
	}
	if _, err := export.DecodeEvidenceManifest(emptyWire); err != nil {
		t.Errorf("empty round-trip: %v", err)
	}
	nullEntries := []byte(strings.Replace(string(emptyWire), `"entries": []`, `"entries": null`, 1))
	if _, err := export.DecodeEvidenceManifest(nullEntries); !errors.Is(err, export.ErrNullEntries) {
		t.Errorf("null-entries error = %v, want ErrNullEntries", err)
	}
}
