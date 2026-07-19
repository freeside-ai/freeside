package importer

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// These tests drive an adversarial workspace through the REAL export helper
// (export.Export, the logic /usr/local/bin/freeside-export wraps) and import its
// on-disk output, so ward's producer and the gauntlet's consumer are proven to
// agree on the shipped two-channel interface end to end (#170/#190) without a
// hand-written manifest or a container. The container-only VM/mount/network
// topology is covered by the opt-in live suite.

// pngMagic and jpegMagic are the smallest headers the importer's evidence
// magic check accepts for their media types (evidence is images-only, §5.15
// rule 3).
var pngMagic = []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}
var jpegMagic = []byte{0xFF, 0xD8, 0xFF}

func writeWorkspace(t *testing.T, dir, rel string, content []byte) {
	t.Helper()
	full := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("mkdir for %q: %v", rel, err)
	}
	if err := os.WriteFile(full, content, 0o600); err != nil {
		t.Fatalf("write %q: %v", rel, err)
	}
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// twoSourceDescriptor declares one head-independent PNG and one head-bound JPEG
// evidence source under the reserved subtree.
const twoSourceDescriptor = `{
  "version": "freeside.export.evidence-source/v1",
  "sources": [
    {"label": "after-shot", "media_type": "image/png", "path": ".freeside-evidence/after.png",
     "head_binding": "head_independent", "sensitivity_class": "normal",
     "producer_invocation_id": "agent-run-1"},
    {"label": "style-ref", "media_type": "image/jpeg", "path": ".freeside-evidence/style.jpg",
     "head_binding": "head_bound", "source_head_sha": "4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e4e",
     "sensitivity_class": "sensitive", "producer_invocation_id": "agent-run-1"}
  ]
}`

// realHandoff runs the real export helper over the workspace and returns the
// handoff directory.
func realHandoff(t *testing.T, workspace string, opts export.Options) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "handoff")
	if _, err := export.Export(os.DirFS(workspace), out, opts); err != nil {
		t.Fatalf("export.Export: %v", err)
	}
	return out
}

// TestImportRealHelperEvidenceToClaims proves the evidence channel the helper
// emits from a .freeside-evidence descriptor imports into the expected labeled
// agent claims, with a clean repo change committing normally.
func TestImportRealHelperEvidenceToClaims(t *testing.T) {
	checkout, base := initBaseRepo(t, map[string]string{"keep.txt": "keep\n"})

	pngBytes := append(append([]byte{}, pngMagic...), []byte("after image bytes")...)
	jpegBytes := append(append([]byte{}, jpegMagic...), []byte("style image bytes")...)

	workspace := t.TempDir()
	writeWorkspace(t, workspace, "keep.txt", []byte("keep\n"))            // unchanged
	writeWorkspace(t, workspace, "feature.txt", []byte("new feature\n"))  // added
	writeWorkspace(t, workspace, ".freeside-evidence/evidence.json", []byte(twoSourceDescriptor))
	writeWorkspace(t, workspace, ".freeside-evidence/after.png", pngBytes)
	writeWorkspace(t, workspace, ".freeside-evidence/style.jpg", jpegBytes)

	res, err := Import(t.Context(), realHandoff(t, workspace, export.Options{}), cloneAtBase(t, checkout), testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	// A clean repo change plus valid evidence commits and carries both claims.
	if res.CommitSHA == "" {
		t.Fatalf("clean import produced no commit: findings=%+v", res.Findings)
	}
	if len(res.Claims) != 2 {
		t.Fatalf("claims = %d, want 2: %+v", len(res.Claims), res.Claims)
	}
	got := map[string]string{}
	for _, c := range res.Claims {
		got[c.Label] = string(c.Digest)
		if string(c.Artifact) != string(c.Digest) {
			t.Errorf("claim %q artifact %q != digest %q", c.Label, c.Artifact, c.Digest)
		}
	}
	if got["after-shot"] != sha256Hex(pngBytes) {
		t.Errorf("after-shot digest = %q, want %q", got["after-shot"], sha256Hex(pngBytes))
	}
	if got["style-ref"] != sha256Hex(jpegBytes) {
		t.Errorf("style-ref digest = %q, want %q", got["style-ref"], sha256Hex(jpegBytes))
	}

	// The reserved subtree never entered the repo channel: no .freeside-evidence
	// change is committed.
	for _, c := range res.Changes {
		if strings.HasPrefix(c.Path, ".freeside-evidence") {
			t.Errorf("repo change carries a reserved-subtree path %q", c.Path)
		}
	}
}

// TestImportRealHelperBothChannelsWithOmittedBlob drives both channels through
// the helper at once with an over-cap repo blob: the omitted-and-changed blob
// blocks the commit as a publish-blocking finding, while the evidence channel
// still yields its claims. It proves the two channels are independent and both
// flow through the real helper output.
func TestImportRealHelperBothChannelsWithOmittedBlob(t *testing.T) {
	checkout, base := initBaseRepo(t, map[string]string{"big.bin": "small base\n"})

	pngBytes := append(append([]byte{}, pngMagic...), []byte("evidence")...)

	workspace := t.TempDir()
	// A new big.bin larger than the blob cap: the helper records it blob_omitted,
	// and the importer treats an omitted-but-changed blob as publish-blocking.
	writeWorkspace(t, workspace, "big.bin", make([]byte, 4096))
	writeWorkspace(t, workspace, ".freeside-evidence/evidence.json",
		[]byte(`{"version":"freeside.export.evidence-source/v1","sources":[`+
			`{"label":"shot","media_type":"image/png","path":".freeside-evidence/shot.png",`+
			`"head_binding":"head_independent","sensitivity_class":"normal","producer_invocation_id":"r"}]}`))
	writeWorkspace(t, workspace, ".freeside-evidence/shot.png", pngBytes)

	res, err := Import(t.Context(), realHandoff(t, workspace, export.Options{MaxBlobBytes: 1024}), cloneAtBase(t, checkout), testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA != "" {
		t.Fatalf("an omitted-and-changed blob must block the commit: %+v", res)
	}
	blocked := false
	for _, f := range res.Findings {
		if f.Kind == FindingBlobOmitted && f.Path == "big.bin" {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("expected a blob_omitted finding for big.bin: %+v", res.Findings)
	}
	// The evidence channel is independent of the repo-channel finding.
	if len(res.Claims) != 1 || res.Claims[0].Label != "shot" {
		t.Fatalf("claims = %+v, want the one shot claim", res.Claims)
	}
}
