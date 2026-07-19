package export_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// validEvidenceSourceManifest is a fixed, valid descriptor covering both
// head-binding modes: one head-bound source with a head, one head-independent
// without. Sources are intentionally in non-label order to prove the descriptor
// does not require pre-sorting (the helper sorts the emitted entries).
func validEvidenceSourceManifest() export.EvidenceSourceManifest {
	return export.EvidenceSourceManifest{
		Version: export.EvidenceSourceVersion,
		Sources: []export.EvidenceSource{
			{
				Label:                "style-reference",
				MediaType:            "image/jpeg",
				Path:                 ".freeside-evidence/style.jpg",
				HeadBinding:          export.EvidenceHeadIndependent,
				SensitivityClass:     export.EvidenceSensitivitySensitive,
				ProducerInvocationID: "agent-run-7f",
			},
			{
				Label:                "after-screenshot",
				MediaType:            "image/png",
				Path:                 ".freeside-evidence/shots/after.png",
				HeadBinding:          export.EvidenceHeadBound,
				SourceHeadSHA:        strings.Repeat("4e", 20),
				SensitivityClass:     export.EvidenceSensitivityNormal,
				ProducerInvocationID: "agent-run-7f",
			},
		},
	}
}

// TestEvidenceSourceValidate enumerates the invariants Validate guards, one
// mutation of the valid fixture per case.
func TestEvidenceSourceValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*export.EvidenceSourceManifest)
		wantErr error
	}{
		{"unknown version", func(m *export.EvidenceSourceManifest) {
			m.Version = "freeside.export.evidence-source/v0"
		}, export.ErrUnknownEvidenceSourceVersion},
		{"wire-manifest version", func(m *export.EvidenceSourceManifest) {
			m.Version = export.EvidenceManifestVersion
		}, export.ErrUnknownEvidenceSourceVersion},
		{"zero-value version", func(m *export.EvidenceSourceManifest) {
			m.Version = ""
		}, export.ErrUnknownEvidenceSourceVersion},
		{"nil sources", func(m *export.EvidenceSourceManifest) {
			m.Sources = nil
		}, export.ErrEmptyEvidenceSources},
		{"empty sources", func(m *export.EvidenceSourceManifest) {
			m.Sources = []export.EvidenceSource{}
		}, export.ErrEmptyEvidenceSources},
		{"duplicate label", func(m *export.EvidenceSourceManifest) {
			m.Sources[1].Label = m.Sources[0].Label
		}, export.ErrDuplicateEvidenceLabel},
		{"empty label", func(m *export.EvidenceSourceManifest) {
			m.Sources[0].Label = ""
		}, export.ErrInvalidLabel},
		{"non-UTF-8 label", func(m *export.EvidenceSourceManifest) {
			m.Sources[0].Label = "bad\xffname"
		}, export.ErrInvalidLabel},
		{"empty media_type", func(m *export.EvidenceSourceManifest) {
			m.Sources[0].MediaType = ""
		}, export.ErrInvalidMediaType},
		{"path outside subtree", func(m *export.EvidenceSourceManifest) {
			m.Sources[0].Path = "evidence/style.jpg"
		}, export.ErrInvalidEvidenceSourcePath},
		{"path is the reserved dir", func(m *export.EvidenceSourceManifest) {
			m.Sources[0].Path = ".freeside-evidence"
		}, export.ErrInvalidEvidenceSourcePath},
		{"path is the descriptor", func(m *export.EvidenceSourceManifest) {
			m.Sources[0].Path = ".freeside-evidence/evidence.json"
		}, export.ErrInvalidEvidenceSourcePath},
		{"path traversal", func(m *export.EvidenceSourceManifest) {
			m.Sources[0].Path = ".freeside-evidence/../secret"
		}, export.ErrInvalidEvidenceSourcePath},
		{"absolute path", func(m *export.EvidenceSourceManifest) {
			m.Sources[0].Path = "/.freeside-evidence/style.jpg"
		}, export.ErrInvalidEvidenceSourcePath},
		{"uncleaned path", func(m *export.EvidenceSourceManifest) {
			m.Sources[0].Path = ".freeside-evidence/./style.jpg"
		}, export.ErrInvalidEvidenceSourcePath},
		{"NUL in path", func(m *export.EvidenceSourceManifest) {
			m.Sources[0].Path = ".freeside-evidence/style\x00.jpg"
		}, export.ErrInvalidEvidenceSourcePath},
		{"empty invocation id", func(m *export.EvidenceSourceManifest) {
			m.Sources[0].ProducerInvocationID = ""
		}, export.ErrEmptyInvocationID},
		{"unknown head binding", func(m *export.EvidenceSourceManifest) {
			m.Sources[0].HeadBinding = "maybe_bound"
		}, export.ErrInvalidHeadBinding},
		{"head_independent with head", func(m *export.EvidenceSourceManifest) {
			m.Sources[0].SourceHeadSHA = strings.Repeat("4e", 20)
		}, export.ErrProvenanceInconsistent},
		{"head_bound without head", func(m *export.EvidenceSourceManifest) {
			m.Sources[1].SourceHeadSHA = ""
		}, export.ErrProvenanceInconsistent},
		{"unknown sensitivity", func(m *export.EvidenceSourceManifest) {
			m.Sources[0].SensitivityClass = "public"
		}, export.ErrInvalidSensitivityClass},
		{"zero-value sensitivity", func(m *export.EvidenceSourceManifest) {
			m.Sources[0].SensitivityClass = ""
		}, export.ErrInvalidSensitivityClass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := validEvidenceSourceManifest()
			tc.mutate(&m)
			if err := m.Validate(); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Validate() = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// TestEvidenceSourceValidateAccepts confirms the untouched fixture is valid, so
// every mutation case above fails for exactly the reason it names.
func TestEvidenceSourceValidateAccepts(t *testing.T) {
	if err := validEvidenceSourceManifest().Validate(); err != nil {
		t.Fatalf("valid fixture rejected: %v", err)
	}
}

// TestDecodeEvidenceSourceManifest covers the strict-but-not-canonical decode
// boundary the helper reads the agent descriptor through.
func TestDecodeEvidenceSourceManifest(t *testing.T) {
	valid := `{
	  "version": "freeside.export.evidence-source/v1",
	  "sources": [
	    {"label": "shot", "media_type": "image/png", "path": ".freeside-evidence/shot.png",
	     "head_binding": "head_independent", "sensitivity_class": "normal",
	     "producer_invocation_id": "run-1"}
	  ]
	}`
	t.Run("valid non-canonical formatting", func(t *testing.T) {
		m, err := export.DecodeEvidenceSourceManifest([]byte(valid))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(m.Sources) != 1 || m.Sources[0].Label != "shot" {
			t.Fatalf("unexpected decode: %+v", m)
		}
	})

	cases := []struct {
		name    string
		data    string
		wantErr error
	}{
		{"unknown producer_class field", `{"version":"freeside.export.evidence-source/v1","sources":[{"label":"a","media_type":"text/plain","path":".freeside-evidence/a","head_binding":"head_independent","sensitivity_class":"normal","producer_invocation_id":"r","producer_class":"agent"}]}`, nil},
		{"smuggled publish_eligible", `{"version":"freeside.export.evidence-source/v1","sources":[{"label":"a","media_type":"text/plain","path":".freeside-evidence/a","head_binding":"head_independent","sensitivity_class":"normal","producer_invocation_id":"r","publish_eligible":true}]}`, nil},
		{"unknown top-level field", `{"version":"freeside.export.evidence-source/v1","sources":[],"extra":1}`, nil},
		{"trailing content", valid + "\n{}", export.ErrTrailingContent},
		{"invalid utf-8", "{\"version\":\"\xff\"}", export.ErrInvalidUTF8},
		{"empty sources rejected", `{"version":"freeside.export.evidence-source/v1","sources":[]}`, export.ErrEmptyEvidenceSources},
		{"not json", `not json`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := export.DecodeEvidenceSourceManifest([]byte(tc.data))
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("decode err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}
