package export_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// evidenceDescriptor is a fixed, valid descriptor referencing two staged
// sources (one nested), in non-label order to exercise the emitter's sort.
const evidenceDescriptor = `{
  "version": "freeside.export.evidence-source/v1",
  "sources": [
    {"label": "style-reference", "media_type": "image/jpeg", "path": ".freeside-evidence/style.jpg",
     "head_binding": "head_independent", "sensitivity_class": "sensitive",
     "producer_invocation_id": "agent-run-7f"},
    {"label": "after-screenshot", "media_type": "image/png", "path": ".freeside-evidence/shots/after.png",
     "head_binding": "head_bound", "source_head_sha": "4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e",
     "sensitivity_class": "normal", "producer_invocation_id": "agent-run-7f"}
  ]
}`

// buildEvidenceWorkspace writes a workspace with a repo file plus a reserved
// .freeside-evidence/ subtree holding the descriptor and its two source artifacts.
func buildEvidenceWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	mustWrite(t, dir, "README.md", "repo content\n")
	mustWrite(t, dir, ".freeside-evidence/evidence.json", evidenceDescriptor)
	mustWrite(t, dir, ".freeside-evidence/style.jpg", "style-bytes")
	mustWrite(t, dir, ".freeside-evidence/shots/after.png", "after-bytes")
	return dir
}

func mustWrite(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatalf("mkdir for %q: %v", rel, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("write %q: %v", rel, err)
	}
}

// TestEmitEvidenceGolden pins the evidence.json the helper emits from the
// descriptor. Regenerate with:
// go test ./internal/export -run TestEmitEvidenceGolden -update.
func TestEmitEvidenceGolden(t *testing.T) {
	out := t.TempDir()
	if _, err := export.Export(os.DirFS(buildEvidenceWorkspace(t)), out, export.Options{}); err != nil {
		t.Fatalf("export: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(out, export.EvidenceFilename))
	if err != nil {
		t.Fatalf("read evidence.json: %v", err)
	}
	golden.Assert(t, "evidence_emitted", body)

	// The emitted evidence.json must round-trip the importer's strict decoder.
	if _, err := export.DecodeEvidenceManifest(body); err != nil {
		t.Fatalf("emitted evidence.json is not canonical wire form: %v", err)
	}
}

// TestEmitEvidenceExcludedFromRepo proves the reserved subtree never enters the
// repo channel: neither the descriptor, the source files, nor the .freeside-evidence
// directory appears in the repo manifest.
func TestEmitEvidenceExcludedFromRepo(t *testing.T) {
	out := t.TempDir()
	m, err := export.Export(os.DirFS(buildEvidenceWorkspace(t)), out, export.Options{})
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	for _, e := range m.Entries {
		if e.Path == ".freeside-evidence" || strings.HasPrefix(e.Path, ".freeside-evidence/") {
			t.Errorf("repo manifest carries reserved-subtree entry %q", e.Path)
		}
	}
	// The repo file is still present, so the skip is scoped, not global.
	var sawRepo bool
	for _, e := range m.Entries {
		if e.Path == "README.md" {
			sawRepo = true
		}
	}
	if !sawRepo {
		t.Errorf("repo manifest dropped a non-reserved file")
	}
}

// TestEmitEvidenceAbsentDescriptor confirms a workspace with no descriptor
// emits no evidence channel (the importer's pre-evidence shape).
func TestEmitEvidenceAbsentDescriptor(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, dir, "README.md", "repo content\n")
	out := t.TempDir()
	if _, err := export.Export(os.DirFS(dir), out, export.Options{}); err != nil {
		t.Fatalf("export: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, export.EvidenceFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("evidence.json present with no descriptor: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, export.EvidenceBlobsDirname)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("evidence/ present with no descriptor: %v", err)
	}
}

// TestEmitEvidenceHostile enumerates malformed or adversarial declarations that
// must fail the whole export closed rather than emit a partial or lying channel.
func TestEmitEvidenceHostile(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(t *testing.T, dir string)
		opts    export.Options
		wantErr error
	}{
		{"source missing", func(t *testing.T, dir string) {
			mustWrite(t, dir, ".freeside-evidence/evidence.json", oneSource(".freeside-evidence/nope.png"))
		}, export.Options{}, export.ErrEvidenceSourceMissing},
		{"source is a directory", func(t *testing.T, dir string) {
			mustWrite(t, dir, ".freeside-evidence/thing/inside.txt", "x")
			mustWrite(t, dir, ".freeside-evidence/evidence.json", oneSource(".freeside-evidence/thing"))
		}, export.Options{}, export.ErrEvidenceSourceNotRegular},
		{"over per-blob cap", func(t *testing.T, dir string) {
			mustWrite(t, dir, ".freeside-evidence/big.png", "0123456789")
			mustWrite(t, dir, ".freeside-evidence/evidence.json", oneSource(".freeside-evidence/big.png"))
		}, export.Options{MaxEvidenceBlobBytes: 4}, export.ErrEvidenceBlobTooLarge},
		{"over aggregate cap", func(t *testing.T, dir string) {
			mustWrite(t, dir, ".freeside-evidence/a.png", "aaaa")
			mustWrite(t, dir, ".freeside-evidence/b.png", "bbbb")
			mustWrite(t, dir, ".freeside-evidence/evidence.json", twoSources(".freeside-evidence/a.png", ".freeside-evidence/b.png"))
		}, export.Options{MaxEvidenceTotalBytes: 6}, export.ErrEvidenceBudgetExhausted},
		{"descriptor malformed json", func(t *testing.T, dir string) {
			mustWrite(t, dir, ".freeside-evidence/evidence.json", "{not json")
		}, export.Options{}, nil},
		{"descriptor unknown field", func(t *testing.T, dir string) {
			mustWrite(t, dir, ".freeside-evidence/evidence.json",
				`{"version":"freeside.export.evidence-source/v1","sources":[{"label":"a","media_type":"text/plain","path":".freeside-evidence/a","head_binding":"head_independent","sensitivity_class":"normal","producer_invocation_id":"r","publish_eligible":true}]}`)
		}, export.Options{}, nil},
		{"descriptor too large", func(t *testing.T, dir string) {
			mustWrite(t, dir, ".freeside-evidence/evidence.json", "["+strings.Repeat("0", 1<<20)+"]")
		}, export.Options{}, export.ErrEvidenceDescriptorTooLarge},
		{"empty descriptor rejected", func(t *testing.T, dir string) {
			mustWrite(t, dir, ".freeside-evidence/evidence.json",
				`{"version":"freeside.export.evidence-source/v1","sources":[]}`)
		}, export.Options{}, export.ErrEmptyEvidenceSources},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			mustWrite(t, dir, "README.md", "repo\n")
			tc.setup(t, dir)
			out := t.TempDir()
			_, err := export.Export(os.DirFS(dir), out, tc.opts)
			if err == nil {
				t.Fatalf("expected export to fail closed")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("export err = %v, want %v", err, tc.wantErr)
			}
			// A malformed evidence declaration fails the whole export atomically
			// BEFORE any output is written: neither evidence.json nor the repo
			// manifest.json exists. This is what makes the ward handoff fail
			// closed (it verifies output, not the helper's exit status) rather
			// than shipping a repo-only handoff that silently dropped evidence.
			if _, statErr := os.Stat(filepath.Join(out, export.EvidenceFilename)); statErr == nil {
				t.Errorf("evidence.json written despite emission failure")
			}
			if _, statErr := os.Stat(filepath.Join(out, export.ManifestFilename)); statErr == nil {
				t.Errorf("repo manifest.json written despite a malformed evidence declaration; export must fail closed before writing output")
			}
		})
	}
}

func oneSource(path string) string {
	return `{"version":"freeside.export.evidence-source/v1","sources":[` +
		`{"label":"a","media_type":"image/png","path":"` + path + `",` +
		`"head_binding":"head_independent","sensitivity_class":"normal","producer_invocation_id":"r"}]}`
}

func twoSources(a, b string) string {
	return `{"version":"freeside.export.evidence-source/v1","sources":[` +
		`{"label":"a","media_type":"image/png","path":"` + a + `","head_binding":"head_independent","sensitivity_class":"normal","producer_invocation_id":"r"},` +
		`{"label":"b","media_type":"image/png","path":"` + b + `","head_binding":"head_independent","sensitivity_class":"normal","producer_invocation_id":"r"}]}`
}
