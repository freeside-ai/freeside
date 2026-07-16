package export_test

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"go/build"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// buildWorkspace lays down the fixed fixture workspace the export goldens
// pin: nested files, an executable, symlinks, the workspace .git, a
// submodule marked by a .git file, and one file over the test blob cap.
// Contents are constant so the golden bytes are identical on darwin and
// linux.
func buildWorkspace(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFixture(t, dir, ".git/HEAD", "ref: refs/heads/main\n", 0o644)
	writeFixture(t, dir, "README.md", "# fixture readme\n", 0o644)
	writeFixture(t, dir, "bin/tool.sh", "#!/bin/sh\nexit 0\n", 0o755)
	writeFixture(t, dir, "media/big.bin", "0123456789abcdef0123456789abcdef", 0o644)
	writeFixture(t, dir, "nested/deep/data.txt", "deep content\n", 0o644)
	writeFixture(t, dir, "vendor/dep/.git", "gitdir: ../../.git/modules/dep\n", 0o644)
	writeFixture(t, dir, "vendor/dep/code.go", "package dep\n", 0o644)
	if err := os.Symlink("README.md", filepath.Join(dir, "docs-link")); err != nil {
		t.Fatal(err)
	}
	return dir
}

// testCap is between the small fixtures and media/big.bin's 32 bytes, so
// the goldens pin the blob_omitted form for exactly that one file.
const testCap = 20

// TestExportGolden pins the complete wire artifact for the fixture
// workspace: the manifest.json bytes on disk, which must equal the
// returned manifest's canonical encoding. Regenerate with:
// go test ./internal/export -run TestExportGolden -update.
func TestExportGolden(t *testing.T) {
	out := t.TempDir()
	m, err := export.Export(os.DirFS(buildWorkspace(t)), out, export.Options{MaxBlobBytes: testCap})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	onDisk, err := os.ReadFile(filepath.Join(out, export.ManifestFilename)) //nolint:gosec // test-owned temp dir
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := m.Encode()
	if err != nil {
		t.Fatalf("returned manifest must be valid: %v", err)
	}
	if string(onDisk) != string(encoded) {
		t.Error("manifest.json differs from the returned manifest's encoding")
	}
	golden.Assert(t, "export_workspace", onDisk)
}

// TestExportHostileGolden pins the wire form of the kinds a hostile
// workspace produces, over fstest.MapFS fakes an unprivileged test cannot
// create for real (device node, sticky bit, non-UTF-8 name).
func TestExportHostileGolden(t *testing.T) {
	fsys := fstest.MapFS{
		"bad\xffname":       {Data: []byte("x")},
		"dev/fake-disk":     {Mode: fs.ModeDevice},
		"link/rel":          {Mode: fs.ModeSymlink, Data: []byte("../target")},
		"payload.txt":       {Data: []byte("hostile payload\n")},
		"sticky/scratch":    {Data: []byte("s"), Mode: 0o644 | fs.ModeSticky},
		"sub/vendored/.git": {Mode: fs.ModeDir},
	}
	out := t.TempDir()
	m, err := export.Export(fsys, out, export.Options{})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	encoded, err := m.Encode()
	if err != nil {
		t.Fatalf("returned manifest must be valid: %v", err)
	}
	golden.Assert(t, "export_hostile", encoded)
}

// TestExportBindsBlobs holds acceptance criterion 3 against the real
// output tree: every within-cap regular entry has exactly the blob its
// digest names, with matching bytes and size; omitted blobs are absent;
// nothing else appears under the output directory.
func TestExportBindsBlobs(t *testing.T) {
	out := t.TempDir()
	m, err := export.Export(os.DirFS(buildWorkspace(t)), out, export.Options{MaxBlobBytes: testCap})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	wantBlobs := map[string]bool{}
	for _, e := range m.Entries {
		if e.Kind != export.EntryRegular {
			continue
		}
		hexName := string(*e.Digest)[len("sha256:"):]
		blobPath := filepath.Join(out, "blobs", "sha256", hexName)
		body, err := os.ReadFile(blobPath) //nolint:gosec // test-owned temp dir
		if e.BlobOmitted {
			if err == nil {
				t.Errorf("%s: blob present despite blob_omitted", e.Path)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: blob missing: %v", e.Path, err)
			continue
		}
		wantBlobs[hexName] = true
		if int64(len(body)) != *e.Size {
			t.Errorf("%s: blob size %d, manifest size %d", e.Path, len(body), *e.Size)
		}
		if got := fmt.Sprintf("sha256:%x", sha256.Sum256(body)); got != string(*e.Digest) {
			t.Errorf("%s: blob digest %s, manifest digest %s", e.Path, got, *e.Digest)
		}
	}

	var stray []string
	err = filepath.WalkDir(out, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(out, p)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == export.ManifestFilename || wantBlobs[filepath.Base(rel)] {
			return nil
		}
		stray = append(stray, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stray) > 0 {
		t.Errorf("stray files under output: %v", stray)
	}
}

// TestExportDeterministic holds acceptance criterion 2: two identically
// built workspaces export to byte-identical manifests and blob trees.
func TestExportDeterministic(t *testing.T) {
	outA, outB := t.TempDir(), t.TempDir()
	if _, err := export.Export(os.DirFS(buildWorkspace(t)), outA, export.Options{MaxBlobBytes: testCap}); err != nil {
		t.Fatalf("first Export: %v", err)
	}
	if _, err := export.Export(os.DirFS(buildWorkspace(t)), outB, export.Options{MaxBlobBytes: testCap}); err != nil {
		t.Fatalf("second Export: %v", err)
	}
	if !equalFileContents(t, outA, outB) {
		t.Error("exports of identical workspaces differ")
	}
}

func equalFileContents(t *testing.T, dirA, dirB string) bool {
	t.Helper()
	listA, listB := listFiles(t, dirA), listFiles(t, dirB)
	if !slices.Equal(listA, listB) {
		t.Logf("file lists differ: %v vs %v", listA, listB)
		return false
	}
	for _, rel := range listA {
		a, err := os.ReadFile(filepath.Join(dirA, filepath.FromSlash(rel))) //nolint:gosec // test-owned temp dir
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(dirB, filepath.FromSlash(rel))) //nolint:gosec // test-owned temp dir
		if err != nil {
			t.Fatal(err)
		}
		if string(a) != string(b) {
			t.Logf("%s differs between exports", rel)
			return false
		}
	}
	return true
}

func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(dir, p)
		if relErr != nil {
			return relErr
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	return files
}

// TestExportNeverExecutes holds acceptance criterion 5: a workspace whose
// executables, hooks, and fake git binary would each leave a sentinel file
// if anything ran them exports cleanly with no sentinel and with the
// scripts' bytes preserved verbatim in blobs.
func TestExportNeverExecutes(t *testing.T) {
	sentinel := filepath.Join(t.TempDir(), "executed")
	script := "#!/bin/sh\ntouch " + sentinel + "\n"

	dir := t.TempDir()
	writeFixture(t, dir, ".git/hooks/pre-commit", script, 0o755)
	writeFixture(t, dir, ".git/hooks/post-checkout", script, 0o755)
	writeFixture(t, dir, "bin/git", script, 0o755)
	writeFixture(t, dir, "run-me.sh", script, 0o755)

	out := t.TempDir()
	m, err := export.Export(os.DirFS(dir), out, export.Options{})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	if _, err := os.Lstat(sentinel); err == nil {
		t.Fatal("workspace content was executed during export")
	}

	for _, e := range m.Entries {
		if e.Kind != export.EntryRegular {
			continue
		}
		hexName := string(*e.Digest)[len("sha256:"):]
		body, err := os.ReadFile(filepath.Join(out, "blobs", "sha256", hexName)) //nolint:gosec // test-owned temp dir
		if err != nil {
			t.Fatalf("%s: blob missing: %v", e.Path, err)
		}
		if string(body) != script {
			t.Errorf("%s: blob bytes differ from the script's source", e.Path)
		}
	}
}

// TestExportPackageNeverImportsExec pins the structural form of the same
// guarantee: the helper cannot execute workspace content it has no code
// path to execute.
func TestExportPackageNeverImportsExec(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(pkg.Imports, "os/exec") {
		t.Fatal("the export package must never import os/exec")
	}
}

// TestExportAggregateBudget: many under-cap files cannot exhaust the
// exporter rootfs either; once the aggregate budget is spent, later files
// (in deterministic manifest order) are recorded blob_omitted.
func TestExportAggregateBudget(t *testing.T) {
	fsys := fstest.MapFS{
		"a.txt": {Data: []byte("aaaaaaaaaa")}, // 10 bytes
		"b.txt": {Data: []byte("bbbbbbbbbb")}, // 10 bytes
		"c.txt": {Data: []byte("cccccccccc")}, // 10 bytes
	}
	out := t.TempDir()
	m, err := export.Export(fsys, out, export.Options{MaxTotalBlobBytes: 25})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	omitted := map[string]bool{}
	for _, e := range m.Entries {
		omitted[e.Path] = e.BlobOmitted
	}
	want := map[string]bool{"a.txt": false, "b.txt": false, "c.txt": true}
	for p, w := range want {
		if omitted[p] != w {
			t.Errorf("%s: blob_omitted = %v, want %v", p, omitted[p], w)
		}
	}
}

// TestExportBudgetChargesWrittenBytes: a dedup hit writes nothing, so it
// spends no budget and is never recorded blob_omitted, even once the
// budget is exhausted.
func TestExportBudgetChargesWrittenBytes(t *testing.T) {
	fsys := fstest.MapFS{
		"one.txt": {Data: []byte("same bytes")},          // 10 bytes, stored
		"two.txt": {Data: []byte("same bytes")},          // dedup: free
		"zzz.txt": {Data: []byte("different contents!")}, // 19 bytes: over budget
	}
	out := t.TempDir()
	m, err := export.Export(fsys, out, export.Options{MaxTotalBlobBytes: 15})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	for _, e := range m.Entries {
		wantOmitted := e.Path == "zzz.txt"
		if e.BlobOmitted != wantOmitted {
			t.Errorf("%s: blob_omitted = %v, want %v", e.Path, e.BlobOmitted, wantOmitted)
		}
	}
}

// TestExportEntryCap: blobless entries evade the blob budgets, so the
// entry cap fails the export closed instead of accumulating an unbounded
// manifest.
func TestExportEntryCap(t *testing.T) {
	fsys := fstest.MapFS{
		"a": {}, "b": {}, "c": {}, // empty files: no blob bytes at all
	}
	_, err := export.Export(fsys, t.TempDir(), export.Options{MaxEntries: 2})
	if !errors.Is(err, export.ErrTooManyEntries) {
		t.Fatalf("Export = %v, want ErrTooManyEntries", err)
	}
	if _, err := export.Export(fsys, t.TempDir(), export.Options{MaxEntries: 3}); err != nil {
		t.Fatalf("Export at the cap: %v", err)
	}
}

// TestExportEntryCapSingleDirectory: the cap fires on a flat directory of
// many files too, not only on nested trees, so a single huge directory
// cannot slip past the count guard.
func TestExportEntryCapSingleDirectory(t *testing.T) {
	fsys := fstest.MapFS{}
	for i := range 2000 {
		fsys[fmt.Sprintf("f%04d", i)] = &fstest.MapFile{}
	}
	_, err := export.Export(fsys, t.TempDir(), export.Options{MaxEntries: 1000})
	if !errors.Is(err, export.ErrTooManyEntries) {
		t.Fatalf("Export = %v, want ErrTooManyEntries", err)
	}
}

// TestExportRejectsDirtyOutput: a pre-populated output directory (a
// retried export's leftovers, a stale path baked into the exporter
// rootfs) must fail closed, never masquerade as this export's output.
func TestExportRejectsDirtyOutput(t *testing.T) {
	fsys := fstest.MapFS{"a.txt": {Data: []byte("content")}}

	stale := t.TempDir()
	writeFixture(t, stale, "blobs/sha256/"+strings.Repeat("ab", 32), "not the advertised bytes", 0o600)
	_, err := export.Export(fsys, stale, export.Options{})
	if !errors.Is(err, export.ErrOutputNotEmpty) {
		t.Fatalf("Export = %v, want ErrOutputNotEmpty", err)
	}

	// Absent directories are created; empty existing ones are fine.
	absent := filepath.Join(t.TempDir(), "handoff")
	if _, err := export.Export(fsys, absent, export.Options{}); err != nil {
		t.Fatalf("Export into absent dir: %v", err)
	}
}

// TestExportEmptyWorkspace: an empty workspace is a valid export with an
// explicit empty entry list.
func TestExportEmptyWorkspace(t *testing.T) {
	out := t.TempDir()
	m, err := export.Export(os.DirFS(t.TempDir()), out, export.Options{})
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if len(m.Entries) != 0 {
		t.Fatalf("entries = %d, want 0", len(m.Entries))
	}
	if _, err := os.ReadFile(filepath.Join(out, export.ManifestFilename)); err != nil { //nolint:gosec // test-owned temp dir
		t.Fatalf("manifest.json missing: %v", err)
	}
}

// lyingFS serves one victim file shorter than its stat size, simulating a
// workspace mutating mid-export (the read-only mount and stopped agent VM
// make this unreachable in production; the helper still fails loud).
type lyingFS struct {
	inner  fstest.MapFS
	victim string
}

func (l lyingFS) Open(name string) (fs.File, error) {
	f, err := l.inner.Open(name)
	if err != nil {
		return nil, err
	}
	if name == l.victim {
		return emptyFile{f}, nil
	}
	return f, nil
}

func (l lyingFS) ReadDir(name string) ([]fs.DirEntry, error) { return l.inner.ReadDir(name) }
func (l lyingFS) ReadLink(name string) (string, error)       { return l.inner.ReadLink(name) }
func (l lyingFS) Lstat(name string) (fs.FileInfo, error)     { return l.inner.Lstat(name) }

// emptyFile stats normally but reads as instantly exhausted.
type emptyFile struct{ fs.File }

func (emptyFile) Read([]byte) (int, error) { return 0, io.EOF }

func TestExportDetectsChangedWorkspace(t *testing.T) {
	fsys := lyingFS{
		inner:  fstest.MapFS{"a.txt": {Data: []byte("ten bytes!")}},
		victim: "a.txt",
	}
	_, err := export.Export(fsys, t.TempDir(), export.Options{})
	if err == nil || !errors.Is(err, export.ErrWorkspaceChanged) {
		t.Fatalf("Export = %v, want ErrWorkspaceChanged", err)
	}
}

func writeFixture(t *testing.T, dir, rel, content string, mode fs.FileMode) {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile's mode is masked by umask; pin it so fixtures are exact.
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}
