package ward

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// tarEntry is one archive member for buildTar fixtures.
type tarEntry struct {
	name     string
	typeflag byte
	body     []byte
	linkname string
}

func buildTar(t *testing.T, entries []tarEntry) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "export.tar")
	f, err := os.Create(p) //nolint:gosec // test temp path
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)
	for _, e := range entries {
		tf := e.typeflag
		if tf == 0 {
			tf = tar.TypeReg
		}
		hdr := &tar.Header{
			Name:     e.name,
			Typeflag: tf,
			Size:     int64(len(e.body)),
			Mode:     0o644,
			Linkname: e.linkname,
		}
		if tf == tar.TypeDir {
			hdr.Mode = 0o755
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if len(e.body) > 0 {
			if _, err := tw.Write(e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

// fixtureBlob is the one exported file the fixture manifest references.
var fixtureBlob = []byte("agent-output\n")

func fixtureManifest(t *testing.T, blob []byte) ([]byte, string) {
	t.Helper()
	sum := sha256.Sum256(blob)
	hexDigest := hex.EncodeToString(sum[:])
	mode := "0644"
	size := int64(len(blob))
	digest := export.Digest("sha256:" + hexDigest)
	m := export.Manifest{
		Version: export.ManifestVersion,
		Entries: []export.Entry{{
			Path:   "result.txt",
			Kind:   export.EntryRegular,
			Mode:   &mode,
			Size:   &size,
			Digest: &digest,
		}},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return raw, hexDigest
}

// fixtureArchive is a valid exported rootfs: OS noise the gate must ignore
// (including a stray handoff-proof.txt, no longer part of the per-handoff path),
// and a handoff tree whose blob matches the manifest.
func fixtureArchive(t *testing.T) []tarEntry {
	t.Helper()
	manifest, hexDigest := fixtureManifest(t, fixtureBlob)
	return []tarEntry{
		{name: "etc/", typeflag: tar.TypeDir},
		{name: "etc/alpine-release", body: []byte("3.22.5\n")},
		{name: "bin/sh", typeflag: tar.TypeSymlink, linkname: "/bin/busybox"},
		{name: "handoff-proof.txt", body: validProof()},
		{name: "handoff/", typeflag: tar.TypeDir},
		{name: "handoff/manifest.json", body: manifest},
		{name: "handoff/blobs/", typeflag: tar.TypeDir},
		{name: "handoff/blobs/sha256/", typeflag: tar.TypeDir},
		{name: "handoff/blobs/sha256/" + hexDigest, body: fixtureBlob},
	}
}

func runVerifyExport(t *testing.T, b *Backend, entries []tarEntry) (*exportOutput, error) {
	t.Helper()
	return b.verifyExport(context.Background(), buildTar(t, entries), t.TempDir())
}

func TestVerifyExportValid(t *testing.T) {
	out, err := runVerifyExport(t, newTestBackend(t), fixtureArchive(t))
	if err != nil {
		t.Fatalf("verifyExport = %v, want nil", err)
	}
	if len(out.Manifest.Entries) != 1 || out.Manifest.Entries[0].Path != "result.txt" {
		t.Errorf("manifest = %+v, want the one result.txt entry", out.Manifest)
	}
	if _, err := os.Stat(filepath.Join(out.Dir, export.ManifestFilename)); err != nil {
		t.Errorf("extracted manifest: %v", err)
	}
}

// fixtureEvidence builds a valid evidence channel (canonical evidence.json plus
// its one blob) and returns the entries to append to a repo-channel archive.
func fixtureEvidence(t *testing.T) (entries []tarEntry, body []byte, hexDigest string, blob []byte) {
	t.Helper()
	blob = []byte("evidence-screenshot-bytes")
	sum := sha256.Sum256(blob)
	hexDigest = hex.EncodeToString(sum[:])
	em := export.EvidenceManifest{
		Version: export.EvidenceManifestVersion,
		Entries: []export.EvidenceEntry{{
			Label:     "after-shot",
			MediaType: "image/png",
			Size:      int64(len(blob)),
			Digest:    export.Digest("sha256:" + hexDigest),
			Provenance: export.EvidenceProvenance{
				ProducerClass:        export.EvidenceProducerAgent,
				ProducerInvocationID: "run-1",
				HeadBinding:          export.EvidenceHeadIndependent,
				SensitivityClass:     export.EvidenceSensitivityNormal,
			},
		}},
	}
	body, err := em.Encode()
	if err != nil {
		t.Fatal(err)
	}
	entries = []tarEntry{
		{name: "handoff/evidence.json", body: body},
		{name: "handoff/evidence/", typeflag: tar.TypeDir},
		{name: "handoff/evidence/sha256/", typeflag: tar.TypeDir},
		{name: "handoff/evidence/sha256/" + hexDigest, body: blob},
	}
	return entries, body, hexDigest, blob
}

// evidenceArchive is a valid two-channel export: the repo fixture plus the
// evidence channel.
func evidenceArchive(t *testing.T) []tarEntry {
	t.Helper()
	ev, _, _, _ := fixtureEvidence(t)
	return append(fixtureArchive(t), ev...)
}

// TestVerifyExportEvidenceValid proves a valid evidence channel is accepted,
// verified, and surfaced on the export output.
func TestVerifyExportEvidenceValid(t *testing.T) {
	out, err := runVerifyExport(t, newTestBackend(t), evidenceArchive(t))
	if err != nil {
		t.Fatalf("verifyExport = %v, want nil", err)
	}
	if !out.EvidencePresent {
		t.Fatal("EvidencePresent = false, want true")
	}
	if len(out.Evidence.Entries) != 1 || out.Evidence.Entries[0].Label != "after-shot" {
		t.Errorf("evidence = %+v, want the one after-shot entry", out.Evidence)
	}
	// The repo channel is unaffected.
	if len(out.Manifest.Entries) != 1 || out.Manifest.Entries[0].Path != "result.txt" {
		t.Errorf("repo manifest = %+v, want the one result.txt entry", out.Manifest)
	}
}

// TestVerifyExportNoEvidence proves an absent evidence channel is the
// pre-evidence shape, not an error.
func TestVerifyExportNoEvidence(t *testing.T) {
	out, err := runVerifyExport(t, newTestBackend(t), fixtureArchive(t))
	if err != nil {
		t.Fatalf("verifyExport = %v, want nil", err)
	}
	if out.EvidencePresent {
		t.Error("EvidencePresent = true with no evidence channel")
	}
}

func TestVerifyExportCommitPlanDeclaredMemberAndScan(t *testing.T) {
	plan := []byte(`{"version":"freeside.commit-plan/v1","groups":[]}`)
	entries := append(fixtureArchive(t), tarEntry{name: "handoff/" + export.CommitPlanFilename, body: plan})
	var scanned []byte
	cfg := testConfig()
	cfg.Scanner = scannerFunc(func(_ context.Context, dir string) error {
		var err error
		// #nosec G304 -- dir is the ward-owned extraction directory and the leaf is fixed.
		scanned, err = os.ReadFile(filepath.Join(dir, export.CommitPlanFilename))
		return err
	})
	b, err := New(stubRuntime{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	out, err := runVerifyExport(t, b, entries)
	if err != nil {
		t.Fatalf("verifyExport: %v", err)
	}
	if !out.CommitPlanPresent || string(scanned) != string(plan) {
		t.Fatalf("plan present=%v scanned=%q", out.CommitPlanPresent, scanned)
	}
	withStray := append(append([]tarEntry(nil), entries...), tarEntry{name: "handoff/stray", body: []byte("x")})
	if out, err := runVerifyExport(t, b, withStray); err == nil || out != nil {
		t.Fatalf("declared plan weakened stray rejection: out=%v err=%v", out, err)
	}

	cfg.Scanner = scannerFunc(func(context.Context, string) error { return errors.New("literal secret") })
	b, err = New(stubRuntime{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := runVerifyExport(t, b, entries); err == nil || out != nil {
		t.Fatalf("scanner refusal released plan output: out=%v err=%v", out, err)
	}

	secretEntries := append(fixtureArchive(t), tarEntry{
		name: "handoff/" + export.CommitPlanFilename,
		body: []byte(`{"version":"freeside.commit-plan/v1","token":"ghp_literal"}`),
	})
	cfg.Scanner = scannerFunc(func(_ context.Context, dir string) error {
		// #nosec G304 -- dir is the ward-owned extraction directory and the leaf is fixed.
		raw, err := os.ReadFile(filepath.Join(dir, export.CommitPlanFilename))
		if err != nil {
			return err
		}
		if bytes.Contains(raw, []byte("ghp_")) {
			return errors.New("literal secret")
		}
		return nil
	})
	b, err = New(stubRuntime{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := runVerifyExport(t, b, secretEntries); err == nil || out != nil {
		t.Fatalf("literal plan secret passed check 7: out=%v err=%v", out, err)
	}
}

// TestVerifyExportEvidenceViolations induces evidence-channel check-7
// violations and asserts each fails closed as export_verification.
func TestVerifyExportEvidenceViolations(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T) []tarEntry
	}{
		{"evidence blob content mismatch", func(t *testing.T) []tarEntry {
			es := evidenceArchive(t)
			es[len(es)-1].body = []byte("tampered")
			return es
		}},
		{"evidence blob missing", func(t *testing.T) []tarEntry {
			es := evidenceArchive(t)
			return es[:len(es)-1]
		}},
		{"evidence manifest not canonical", func(t *testing.T) []tarEntry {
			es := evidenceArchive(t)
			_, body, _, _ := fixtureEvidence(t)
			// Strip the trailing newline Encode appends: valid JSON, but not the
			// canonical byte form DecodeEvidenceManifest requires.
			es[len(es)-4].body = bytes.TrimRight(body, "\n")
			return es
		}},
		{"evidence stray blob", func(t *testing.T) []tarEntry {
			es := evidenceArchive(t)
			stray := sha256.Sum256([]byte("stray-evidence"))
			return append(es, tarEntry{
				name: "handoff/evidence/sha256/" + hex.EncodeToString(stray[:]),
				body: []byte("stray-evidence"),
			})
		}},
		{"evidence file with no channel", func(t *testing.T) []tarEntry {
			// evidence.json present but decodes empty is valid; here we drop the
			// evidence.json so a lone evidence/ blob is an orphan.
			es := evidenceArchive(t)
			// Remove the evidence.json entry (index len-4), leaving evidence/ blobs.
			return append(es[:len(es)-4], es[len(es)-3:]...)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runVerifyExport(t, newTestBackend(t), tc.mutate(t))
			var cf *ConformanceFailure
			if !errors.As(err, &cf) || cf.Check != CheckExportVerification {
				t.Fatalf("verifyExport = %v, want export_verification failure", err)
			}
		})
	}
}

// TestVerifyExportViolations induces check 7 violations in the exported
// archive and asserts each fails closed with the right check (acceptance 2 for
// check 7). Check 5's proof is no longer part of this path: it is attested by
// the conformance probe (TestSuiteInExporterProbe), not per handoff.
func TestVerifyExportViolations(t *testing.T) {
	_, wantHex := fixtureManifest(t, fixtureBlob)

	cases := []struct {
		name      string
		mutate    func(*testing.T, []tarEntry) []tarEntry
		wantCheck Check
	}{
		{
			"blob content mismatch",
			func(_ *testing.T, es []tarEntry) []tarEntry {
				es[8].body = []byte("tampered-output\n")
				return es
			},
			CheckExportVerification,
		},
		{
			"blob size mismatch",
			func(_ *testing.T, es []tarEntry) []tarEntry {
				es[8].body = append([]byte(nil), fixtureBlob...)
				es[8].body = append(es[8].body, '\n')
				return es
			},
			CheckExportVerification,
		},
		{
			"missing blob",
			func(_ *testing.T, es []tarEntry) []tarEntry {
				return es[:8]
			},
			CheckExportVerification,
		},
		{
			"unreferenced stray file",
			func(_ *testing.T, es []tarEntry) []tarEntry {
				return append(es, tarEntry{name: "handoff/extra.bin", body: []byte("no provenance")})
			},
			CheckExportVerification,
		},
		{
			"unreferenced stray blob",
			func(_ *testing.T, es []tarEntry) []tarEntry {
				stray := sha256.Sum256([]byte("stray"))
				return append(es, tarEntry{
					name: "handoff/blobs/sha256/" + hex.EncodeToString(stray[:]),
					body: []byte("stray"),
				})
			},
			CheckExportVerification,
		},
		{
			"unreferenced directory",
			func(_ *testing.T, es []tarEntry) []tarEntry {
				return append(es, tarEntry{name: "handoff/extra/", typeflag: tar.TypeDir})
			},
			CheckExportVerification,
		},
		{
			"directory below blob root",
			func(_ *testing.T, es []tarEntry) []tarEntry {
				return append(es, tarEntry{name: "handoff/blobs/sha256/junk/", typeflag: tar.TypeDir})
			},
			CheckExportVerification,
		},
		{
			"symlink inside handoff output",
			func(_ *testing.T, es []tarEntry) []tarEntry {
				return append(es, tarEntry{
					name: "handoff/link", typeflag: tar.TypeSymlink, linkname: "../../etc/passwd",
				})
			},
			CheckExportVerification,
		},
		{
			"hardlink inside handoff output",
			func(_ *testing.T, es []tarEntry) []tarEntry {
				return append(es, tarEntry{
					name: "handoff/link", typeflag: tar.TypeLink, linkname: "etc/alpine-release",
				})
			},
			CheckExportVerification,
		},
		{
			"path traversal out of the archive",
			func(_ *testing.T, es []tarEntry) []tarEntry {
				return append(es, tarEntry{name: "handoff/../../escape.txt", body: []byte("x")})
			},
			CheckExportVerification,
		},
		{
			"manifest with unknown field",
			func(t *testing.T, es []tarEntry) []tarEntry {
				es[5].body = []byte(`{"version":"` + export.ManifestVersion + `","entries":[],"count":0}`)
				return es
			},
			CheckExportVerification,
		},
		{
			"manifest with wrong version",
			func(t *testing.T, es []tarEntry) []tarEntry {
				es[5].body = []byte(`{"version":"freeside.export.manifest/v0","entries":[]}`)
				return es
			},
			CheckExportVerification,
		},
		{
			"manifest with trailing bytes",
			func(t *testing.T, es []tarEntry) []tarEntry {
				es[5].body = append(append([]byte(nil), es[5].body...), []byte("\n{\"trailing\":true}")...)
				return es
			},
			CheckExportVerification,
		},
		{
			"manifest with trailing garbage",
			func(t *testing.T, es []tarEntry) []tarEntry {
				es[5].body = append(append([]byte(nil), es[5].body...), []byte(" garbage")...)
				return es
			},
			CheckExportVerification,
		},
		{
			"missing manifest",
			func(_ *testing.T, es []tarEntry) []tarEntry {
				return append(es[:5:5], es[6:]...)
			},
			CheckExportVerification,
		},
		{
			"blob digest key does not match manifest",
			func(_ *testing.T, es []tarEntry) []tarEntry {
				other := sha256.Sum256([]byte("other"))
				es[8].name = "handoff/blobs/sha256/" + hex.EncodeToString(other[:])
				return es
			},
			CheckExportVerification,
		},
		{
			"blob stored under wrong digest path",
			func(_ *testing.T, es []tarEntry) []tarEntry {
				// Same bytes, but a lying path: referenced blob absent.
				es[8].name = "handoff/blobs/sha256/" + wantHex[:32] + wantHex[:32]
				return es
			},
			CheckExportVerification,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries := tc.mutate(t, fixtureArchive(t))
			_, err := runVerifyExport(t, newTestBackend(t), entries)
			var cf *ConformanceFailure
			if !errors.As(err, &cf) {
				t.Fatalf("verifyExport = %v, want ConformanceFailure", err)
			}
			if cf.Check != tc.wantCheck {
				t.Errorf("Check = %q, want %q (reason: %s)", cf.Check, tc.wantCheck, cf.Reason)
			}
		})
	}
}

// TestVerifyExportScannerRefusal proves the §5.4 hook gates the export: a
// scanner error is a check 7 failure and no output is released. The
// scanner's error text is withheld from the failure reason, since it may
// quote the matched secret (ConformanceFailure reasons never carry
// credential material).
func TestVerifyExportScannerRefusal(t *testing.T) {
	const secret = "FREESIDE_FAKE_PROVIDER_CREDENTIAL_DO_NOT_EXPORT" //nolint:gosec // inert test marker, not a credential
	cfg := testConfig()
	cfg.Scanner = scannerFunc(func(context.Context, string) error {
		return fmt.Errorf("leak: matched %s at result.txt", secret)
	})
	b, err := New(stubRuntime{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	out, err := runVerifyExport(t, b, fixtureArchive(t))
	var cf *ConformanceFailure
	if !errors.As(err, &cf) || cf.Check != CheckExportVerification {
		t.Fatalf("verifyExport = %v, want export_verification failure", err)
	}
	if out != nil {
		t.Error("scanner refusal still released output")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("scanner error leaked the matched secret into the failure reason: %v", err)
	}
}

// TestVerifyExportScannerSeesOutput proves the scanner runs against the
// extracted output directory (the live test greps it for the credential
// marker).
func TestVerifyExportScannerSeesOutput(t *testing.T) {
	var scanned string
	cfg := testConfig()
	cfg.Scanner = scannerFunc(func(_ context.Context, dir string) error {
		scanned = dir
		return nil
	})
	b, err := New(stubRuntime{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	out, err := runVerifyExport(t, b, fixtureArchive(t))
	if err != nil {
		t.Fatal(err)
	}
	if scanned != out.Dir {
		t.Errorf("scanner saw %q, output dir is %q", scanned, out.Dir)
	}
}

// TestVerifyExportRedactsPaths proves an archive-derived filename (which is
// workspace content and could itself embed a credential) never appears in a
// conformance failure reason.
func TestVerifyExportRedactsPaths(t *testing.T) {
	const secretName = "AKIAFREESIDEFAKESECRET"
	entries := append(fixtureArchive(t), tarEntry{
		name: "handoff/" + secretName + ".txt",
		body: []byte("unreferenced"),
	})
	_, err := runVerifyExport(t, newTestBackend(t), entries)
	var cf *ConformanceFailure
	if !errors.As(err, &cf) || cf.Check != CheckExportVerification {
		t.Fatalf("verifyExport = %v, want export_verification failure", err)
	}
	if strings.Contains(err.Error(), secretName) {
		t.Errorf("failure reason leaked the archive filename: %v", err)
	}

	// Directory names are workspace content too, and are redacted under the
	// exact-tree allowlist just like file names.
	dirEntries := append(fixtureArchive(t), tarEntry{
		name:     "handoff/" + secretName + "/",
		typeflag: tar.TypeDir,
	})
	_, err = runVerifyExport(t, newTestBackend(t), dirEntries)
	if err == nil || strings.Contains(err.Error(), secretName) {
		t.Errorf("directory reason leaked or did not fail: %v", err)
	}

	// A traversal entry's raw name is redacted too.
	trav := append(fixtureArchive(t), tarEntry{name: "../" + secretName, body: []byte("x")})
	_, err = runVerifyExport(t, newTestBackend(t), trav)
	if err == nil || strings.Contains(err.Error(), secretName) {
		t.Errorf("traversal reason leaked or did not fail: %v", err)
	}
}

// TestVerifyExportCap proves the tar-bomb budget fails closed.
func TestVerifyExportCap(t *testing.T) {
	cfg := testConfig()
	cfg.MaxExportBytes = 8
	b, err := New(stubRuntime{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = runVerifyExport(t, b, fixtureArchive(t))
	var cf *ConformanceFailure
	if !errors.As(err, &cf) || cf.Check != CheckExportVerification {
		t.Fatalf("verifyExport = %v, want export_verification failure", err)
	}
	if !errors.Is(err, ErrConformance) {
		t.Error("cap failure must be a conformance failure")
	}
	if fmt.Sprint(err) == "" {
		t.Error("empty failure message")
	}
}

// dupEntryArchive returns a valid archive whose manifest references one blob
// from two distinct paths, which is legal for identical files. When
// secondSize is non-nil it overrides the second entry's declared size, modeling
// a hostile manifest that claims two different lengths for one shared digest.
func dupEntryArchive(t *testing.T, secondSize *int64) []tarEntry {
	t.Helper()
	sum := sha256.Sum256(fixtureBlob)
	hexDigest := hex.EncodeToString(sum[:])
	mode := "0644"
	size := int64(len(fixtureBlob))
	second := size
	if secondSize != nil {
		second = *secondSize
	}
	digest := export.Digest("sha256:" + hexDigest)
	m := export.Manifest{
		Version: export.ManifestVersion,
		Entries: []export.Entry{
			{Path: "a.txt", Kind: export.EntryRegular, Mode: &mode, Size: &size, Digest: &digest},
			{Path: "b.txt", Kind: export.EntryRegular, Mode: &mode, Size: &second, Digest: &digest},
		},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return []tarEntry{
		{name: "handoff-proof.txt", body: validProof()},
		{name: "handoff/", typeflag: tar.TypeDir},
		{name: "handoff/manifest.json", body: raw},
		{name: "handoff/blobs/", typeflag: tar.TypeDir},
		{name: "handoff/blobs/sha256/", typeflag: tar.TypeDir},
		{name: "handoff/blobs/sha256/" + hexDigest, body: fixtureBlob},
	}
}

// TestVerifyExportSharedDigest covers the (digest, size) blob dedup: two paths
// citing the same digest and size verify against one blob (no per-entry
// re-hash), while a second entry claiming a different size for that digest
// still fails closed rather than being skipped by a per-digest shortcut.
func TestVerifyExportSharedDigest(t *testing.T) {
	t.Run("identical files share one blob", func(t *testing.T) {
		out, err := runVerifyExport(t, newTestBackend(t), dupEntryArchive(t, nil))
		if err != nil {
			t.Fatalf("verifyExport = %v, want nil", err)
		}
		if len(out.Manifest.Entries) != 2 {
			t.Errorf("entries = %d, want 2", len(out.Manifest.Entries))
		}
	})

	t.Run("lying size for a shared digest fails closed", func(t *testing.T) {
		bogus := int64(len(fixtureBlob) + 1)
		_, err := runVerifyExport(t, newTestBackend(t), dupEntryArchive(t, &bogus))
		var cf *ConformanceFailure
		if !errors.As(err, &cf) || cf.Check != CheckExportVerification {
			t.Fatalf("verifyExport = %v, want export_verification failure", err)
		}
	})
}

// TestVerifyExportRedactsBlobDigest proves a blob-verification failure never
// echoes the manifest digest: a regular entry's 64-hex digest is
// archive-derived and could encode a credential, so it must not reach the
// conformance reason (or logs).
func TestVerifyExportRedactsBlobDigest(t *testing.T) {
	secretHex := strings.Repeat("dead", 16) // 64 hex chars, credential-shaped
	mode := "0644"
	size := int64(1)
	digest := export.Digest("sha256:" + secretHex)
	m := export.Manifest{
		Version: export.ManifestVersion,
		Entries: []export.Entry{{
			Path: "result.txt", Kind: export.EntryRegular, Mode: &mode, Size: &size, Digest: &digest,
		}},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	// No blob for secretHex, so verifyBlob fails "missing".
	entries := []tarEntry{
		{name: "handoff-proof.txt", body: validProof()},
		{name: "handoff/", typeflag: tar.TypeDir},
		{name: "handoff/manifest.json", body: raw},
		{name: "handoff/blobs/", typeflag: tar.TypeDir},
		{name: "handoff/blobs/sha256/", typeflag: tar.TypeDir},
	}
	_, err = runVerifyExport(t, newTestBackend(t), entries)
	var cf *ConformanceFailure
	if !errors.As(err, &cf) || cf.Check != CheckExportVerification {
		t.Fatalf("verifyExport = %v, want export_verification failure", err)
	}
	if strings.Contains(cf.Reason, secretHex) {
		t.Errorf("failure reason leaked the manifest digest: %s", cf.Reason)
	}
}

// TestVerifyExportRejectsDuplicateManifestKeys proves a manifest whose raw
// bytes carry a duplicate key fails closed even though encoding/json would
// collapse it to a valid last-value-wins struct with a matching blob. The raw
// bytes are the artifact released to the gauntlet, so the validated view must
// not be allowed to diverge from them.
func TestVerifyExportRejectsDuplicateManifestKeys(t *testing.T) {
	sum := sha256.Sum256(fixtureBlob)
	hexDigest := hex.EncodeToString(sum[:])
	mode := "0644"
	size := int64(len(fixtureBlob))
	digest := export.Digest("sha256:" + hexDigest)
	m := export.Manifest{
		Version: export.ManifestVersion,
		Entries: []export.Entry{{Path: "a.txt", Kind: export.EntryRegular, Mode: &mode, Size: &size, Digest: &digest}},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	// A contradictory decoy digest before the real one: last-value-wins keeps
	// the real (blob-matching) digest, so the manifest would otherwise validate;
	// only the duplicate-key gate rejects it.
	marker := `"digest":"` + string(digest) + `"`
	dup := strings.Replace(string(raw), marker, `"digest":"sha256:`+strings.Repeat("0", 64)+`",`+marker, 1)
	if dup == string(raw) {
		t.Fatal("failed to inject a duplicate digest key")
	}
	entries := []tarEntry{
		{name: "handoff-proof.txt", body: validProof()},
		{name: "handoff/", typeflag: tar.TypeDir},
		{name: "handoff/manifest.json", body: []byte(dup)},
		{name: "handoff/blobs/", typeflag: tar.TypeDir},
		{name: "handoff/blobs/sha256/", typeflag: tar.TypeDir},
		{name: "handoff/blobs/sha256/" + hexDigest, body: fixtureBlob},
	}
	_, err = runVerifyExport(t, newTestBackend(t), entries)
	var cf *ConformanceFailure
	if !errors.As(err, &cf) || cf.Check != CheckExportVerification {
		t.Fatalf("verifyExport = %v, want export_verification failure", err)
	}
	if !strings.Contains(cf.Reason, "canonical") {
		t.Errorf("reason = %q, want the non-canonical manifest failure", cf.Reason)
	}
}

// TestVerifyExportManifestCap proves a manifest larger than MaxManifestBytes
// fails closed instead of being read whole into the daemon heap.
func TestVerifyExportManifestCap(t *testing.T) {
	cfg := testConfig()
	cfg.MaxManifestBytes = 8 // any real manifest exceeds this
	b, err := New(stubRuntime{}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	_, err = runVerifyExport(t, b, fixtureArchive(t))
	var cf *ConformanceFailure
	if !errors.As(err, &cf) || cf.Check != CheckExportVerification {
		t.Fatalf("verifyExport = %v, want export_verification failure", err)
	}
	if !strings.Contains(err.Error(), "cap") {
		t.Errorf("reason = %q, want the manifest cap failure", err.Error())
	}
}

// TestExtractHandoffMetadataBudgets proves zero-byte directory floods and
// pathological names fail before the corresponding host objects are created.
func TestExtractHandoffMetadataBudgets(t *testing.T) {
	t.Run("entry count", func(t *testing.T) {
		cfg := testConfig()
		cfg.MaxExportEntries = 1
		b, err := New(stubRuntime{}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		dest := t.TempDir()
		entries := []tarEntry{
			{name: "handoff/", typeflag: tar.TypeDir},
			{name: "handoff/first/", typeflag: tar.TypeDir},
			{name: "handoff/refused/", typeflag: tar.TypeDir},
		}
		err = b.extractHandoff(buildTar(t, entries), dest)
		var cf *ConformanceFailure
		if !errors.As(err, &cf) || cf.Check != CheckExportVerification {
			t.Fatalf("extractHandoff = %v, want export_verification failure", err)
		}
		if _, statErr := os.Stat(filepath.Join(dest, "refused")); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("entry beyond the cap reached the host filesystem: %v", statErr)
		}
	})

	t.Run("implicit parent count", func(t *testing.T) {
		cfg := testConfig()
		cfg.MaxExportEntries = 1
		b, err := New(stubRuntime{}, cfg)
		if err != nil {
			t.Fatal(err)
		}
		dest := t.TempDir()
		err = b.extractHandoff(buildTar(t, []tarEntry{
			{name: "handoff/", typeflag: tar.TypeDir},
			{name: "handoff/one/two/three/file", body: []byte("x")},
		}), dest)
		var cf *ConformanceFailure
		if !errors.As(err, &cf) || cf.Check != CheckExportVerification {
			t.Fatalf("extractHandoff = %v, want export_verification failure", err)
		}
		if _, statErr := os.Stat(filepath.Join(dest, "one")); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("implicit parent reached the host filesystem: %v", statErr)
		}
	})

	t.Run("path length", func(t *testing.T) {
		b := newTestBackend(t)
		name := "handoff/" + strings.Repeat("x", maxArchivePathBytes)
		err := b.extractHandoff(buildTar(t, []tarEntry{{name: name, typeflag: tar.TypeDir}}), t.TempDir())
		var cf *ConformanceFailure
		if !errors.As(err, &cf) || cf.Check != CheckExportVerification {
			t.Fatalf("extractHandoff = %v, want export_verification failure", err)
		}
		if strings.Contains(err.Error(), name) {
			t.Error("path-length failure echoed the hostile archive name")
		}
	})
}

// TestExtractFileCapBoundary distinguishes an exact fit from a one-byte
// overflow, including an empty file after the byte budget is fully consumed.
func TestExtractFileCapBoundary(t *testing.T) {
	t.Run("exact fit", func(t *testing.T) {
		data := []byte("12345678")
		dest := filepath.Join(t.TempDir(), "exact")
		n, err := extractFile(bytes.NewReader(data), dest, int64(len(data)))
		if err != nil || n != int64(len(data)) {
			t.Fatalf("extractFile = (%d, %v), want (%d, nil)", n, err, len(data))
		}
		if got, err := os.ReadFile(dest); err != nil || !bytes.Equal(got, data) { //nolint:gosec // dest is a test-owned path under t.TempDir
			t.Fatalf("extracted bytes = %q, %v; want %q", got, err, data)
		}
	})

	t.Run("one byte over", func(t *testing.T) {
		data := []byte("123456789")
		dest := filepath.Join(t.TempDir(), "overflow")
		if _, err := extractFile(bytes.NewReader(data), dest, int64(len(data)-1)); err == nil {
			t.Fatal("extractFile accepted a one-byte overflow")
		}
	})

	t.Run("empty at exhausted budget", func(t *testing.T) {
		dest := filepath.Join(t.TempDir(), "empty")
		if n, err := extractFile(bytes.NewReader(nil), dest, 0); err != nil || n != 0 {
			t.Fatalf("extractFile = (%d, %v), want (0, nil)", n, err)
		}
	})
}
