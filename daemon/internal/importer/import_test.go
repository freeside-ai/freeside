package importer

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

// exportWorkspace runs the real export helper over a workspace, so
// these fixtures exercise the actual #73 wire contract end to end.
func exportWorkspace(t *testing.T, workspace string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "handoff")
	if _, err := export.Export(os.DirFS(workspace), out, export.Options{}); err != nil {
		t.Fatalf("export.Export: %v", err)
	}
	return out
}

// cloneAtBase produces the fresh daemon-owned checkout the import runs
// against.
func cloneAtBase(t *testing.T, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "clone")
	rungit(t, src, "clone", "-q", "--no-hardlinks", ".", dst)
	return dst
}

// cloneNoCheckout is cloneAtBase for a base tree holding a path the host
// filesystem cannot materialize (a non-UTF-8 name on APFS): HEAD is set
// to the base, but no working tree is written. The importer never
// touches the working tree, so this exercises the import fully.
func cloneNoCheckout(t *testing.T, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "clone")
	rungit(t, src, "clone", "-q", "--no-checkout", "--no-hardlinks", ".", dst)
	return dst
}

// hexEncode is the manifest's path_hex encoding: lowercase hex of the
// raw name bytes.
func hexEncode(s string) string {
	return hex.EncodeToString([]byte(s))
}

// manifestSortKey mirrors the export package's canonical sort key: the
// decoded raw name bytes for an invalid_path entry, the Path otherwise.
func manifestSortKey(e export.Entry) string {
	if e.Kind == export.EntryInvalidPath {
		raw, err := hex.DecodeString(e.PathHex)
		if err != nil {
			return e.PathHex
		}
		return string(raw)
	}
	return e.Path
}

// regularEntryFor builds a manifest entry bound to content by real
// digest and size.
func regularEntryFor(path, content string, omitted bool) export.Entry {
	mode := "0644"
	size := int64(len(content))
	digest := sha256Digest(content)
	return export.Entry{Path: path, Kind: export.EntryRegular, Mode: &mode, Size: &size, Digest: &digest, BlobOmitted: omitted}
}

// handoffFromEntries writes a handcrafted handoff: the encoded manifest
// plus a blob for each of the given contents. Entries are sorted here
// so fixtures stay valid as they grow.
func handoffFromEntries(t *testing.T, entries []export.Entry, contents ...string) string {
	t.Helper()
	dir := t.TempDir()
	blobDir := filepath.Join(dir, "blobs", "sha256")
	if err := os.MkdirAll(blobDir, 0o750); err != nil {
		t.Fatal(err)
	}
	for _, c := range contents {
		name := filepath.Join(blobDir, string(sha256Digest(c))[len("sha256:"):])
		if err := os.WriteFile(name, []byte(c), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return manifestSortKey(entries[i]) < manifestSortKey(entries[j]) })
	body, err := (export.Manifest{Version: export.ManifestVersion, Entries: entries}).Encode()
	if err != nil {
		t.Fatalf("encode fixture manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, export.ManifestFilename), body, 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// goldenResult pins changes and findings; the commit and tree names
// depend on fixture-repo object names and are asserted separately.
func goldenResult(t *testing.T, name string, r Result) {
	t.Helper()
	r.CommitSHA, r.TreeSHA = "", ""
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, name, append(body, '\n'))
}

func TestImportCleanEndToEnd(t *testing.T) {
	checkout, base := initBaseRepo(t, map[string]string{
		"a.txt":          "old\n",
		"del.txt":        "bye\n",
		"keep.txt":       "same\n",
		"dir/nested.txt": "n\n",
	})
	ws := t.TempDir()
	for path, content := range map[string]string{
		"a.txt":                 "new\n",
		"keep.txt":              "same\n",
		"dir/nested.txt":        "n\n",
		"deep/x/y.txt":          "y\n",
		".git/hooks/pre-commit": "#!/bin/sh\necho pwned\n",
		".git/config":           "[core]\n\thooksPath = /tmp\n",
	} {
		full := filepath.Join(ws, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(ws, "run.sh"), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // G306: fixture needs the exec bit; content is inert test data
		t.Fatal(err)
	}
	handoff := exportWorkspace(t, ws)

	clone := cloneAtBase(t, checkout)
	opts := testImportOptions(base)
	opts.ImportRef = "refs/freeside/imports/e2e"
	res, err := Import(t.Context(), handoff, clone, opts)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA == "" || res.TreeSHA == "" {
		t.Fatal("clean import produced no commit")
	}
	if len(res.Findings) != 0 {
		t.Fatalf("clean import produced findings: %+v", res.Findings)
	}
	goldenResult(t, "import_clean_result", res)

	if parent := rungit(t, clone, "log", "-1", "--format=%P", res.CommitSHA); parent != base {
		t.Errorf("commit parent = %s, want the enforced base %s", parent, base)
	}
	if ref := rungit(t, clone, "rev-parse", opts.ImportRef); ref != res.CommitSHA {
		t.Errorf("import ref = %s, want %s", ref, res.CommitSHA)
	}
	golden.Assert(t, "import_clean_tree", []byte(rungit(t, clone, "ls-tree", "-r", res.CommitSHA)+"\n"))

	// Determinism: a second fresh clone with the same pinned date must
	// yield the identical commit object.
	clone2 := cloneAtBase(t, checkout)
	res2, err := Import(t.Context(), handoff, clone2, opts)
	if err != nil {
		t.Fatalf("second Import: %v", err)
	}
	if res2.CommitSHA != res.CommitSHA {
		t.Errorf("re-import produced %s, first produced %s; want identical", res2.CommitSHA, res.CommitSHA)
	}
}

func TestImportBlockedBySymlink(t *testing.T) {
	checkout, base := initBaseRepo(t, map[string]string{"a.txt": "old\n"})
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(ws, "ln")); err != nil {
		t.Fatal(err)
	}
	handoff := exportWorkspace(t, ws)
	clone := cloneAtBase(t, checkout)
	res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA != "" {
		t.Fatal("blocked import still produced a commit")
	}
	goldenResult(t, "import_nonregular_result", res)
}

func TestImportUnchangedSymlinkImports(t *testing.T) {
	checkout, _ := initBaseRepo(t, map[string]string{"a.txt": "old\n"})
	if err := os.Symlink("a.txt", filepath.Join(checkout, "ln")); err != nil {
		t.Fatal(err)
	}
	rungit(t, checkout, "add", "-A")
	rungit(t, checkout, "commit", "-q", "-m", "symlink")
	base := rungit(t, checkout, "rev-parse", "HEAD")

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(ws, "ln")); err != nil {
		t.Fatal(err)
	}
	handoff := exportWorkspace(t, ws)
	clone := cloneAtBase(t, checkout)
	res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(res.Findings) != 0 || res.CommitSHA == "" {
		t.Fatalf("unchanged symlink must not block: %+v", res)
	}
	if len(res.Changes) != 1 || res.Changes[0].Path != "a.txt" {
		t.Fatalf("changes = %+v, want only a.txt", res.Changes)
	}
	tree := rungit(t, clone, "ls-tree", res.CommitSHA, "ln")
	if tree == "" || tree[:6] != "120000" {
		t.Errorf("ln in tree = %q, want a retained 120000 entry", tree)
	}
}

// TestImportSymlinkSizeMismatchIsChanged is the Codex round-16
// regression: a malformed base tree can give an arbitrarily large blob
// mode 120000. A short manifest target must be decided changed from the
// cheap size query without buffering that base blob.
func TestImportSymlinkSizeMismatchIsChanged(t *testing.T) {
	checkout, parent := initBaseRepo(t, nil)
	largePath := filepath.Join(t.TempDir(), "large-symlink-target")
	if err := os.WriteFile(largePath, []byte(strings.Repeat("x", 8<<20)), 0o600); err != nil {
		t.Fatal(err)
	}
	oid := rungit(t, checkout, "hash-object", "-w", largePath)
	rungit(t, checkout, "update-index", "--add", "--cacheinfo", "120000,"+oid+",ln")
	tree := rungit(t, checkout, "write-tree")
	base := rungit(t, checkout, "commit-tree", tree, "-p", parent, "-m", "malformed large symlink")
	rungit(t, checkout, "update-ref", "HEAD", base)

	target := "short"
	handoff := handoffFromEntries(t, []export.Entry{{Path: "ln", Kind: export.EntrySymlink, Target: &target}})
	res, err := Import(t.Context(), handoff, cloneNoCheckout(t, checkout), testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA != "" {
		t.Fatal("changed symlink still produced a commit")
	}
	if len(res.Findings) != 1 || res.Findings[0].Kind != FindingNonRegularChange || res.Findings[0].Path != "ln" {
		t.Fatalf("findings = %+v, want one non_regular_change for ln", res.Findings)
	}
}

// TestImportLossySymlinkTargetNeverElides is the Codex round-17
// regression: JSON renders an invalid target byte as U+FFFD, which must
// not compare equal to a base target that literally contains U+FFFD.
func TestImportLossySymlinkTargetNeverElides(t *testing.T) {
	checkout, _ := initBaseRepo(t, nil)
	if err := os.Symlink("bad\ufffdtarget", filepath.Join(checkout, "ln")); err != nil {
		t.Fatal(err)
	}
	rungit(t, checkout, "add", "-A")
	rungit(t, checkout, "commit", "-q", "-m", "replacement-rune symlink")
	base := rungit(t, checkout, "rev-parse", "HEAD")

	workspace := t.TempDir()
	if err := os.Symlink("bad\xfftarget", filepath.Join(workspace, "ln")); err != nil {
		t.Skipf("filesystem does not accept an invalid-UTF-8 symlink target: %v", err)
	}
	handoff := exportWorkspace(t, workspace)
	res, err := Import(t.Context(), handoff, cloneAtBase(t, checkout), testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA != "" || len(res.Findings) != 1 || res.Findings[0].Kind != FindingNonRegularChange {
		t.Fatalf("lossy symlink target was treated as unchanged: %+v", res)
	}
}

func TestImportBlobOmittedChangedBlocks(t *testing.T) {
	checkout, base := initBaseRepo(t, map[string]string{"b.txt": "old\n"})
	handoff := handoffFromEntries(t, []export.Entry{
		regularEntryFor("b.txt", "new-content\n", true),
	})
	clone := cloneAtBase(t, checkout)
	res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA != "" {
		t.Fatal("omitted changed blob still produced a commit")
	}
	goldenResult(t, "import_blob_omitted_result", res)
}

// TestImportOmittedSizeMismatchIsChanged is the Codex round-8 regression:
// an omitted entry whose declared size differs from the base blob's size
// is decided changed from the cheap size check alone, without streaming
// and hashing the base blob (a hostile size claim must not force hashing
// a large base object).
func TestImportOmittedSizeMismatchIsChanged(t *testing.T) {
	checkout, base := initBaseRepo(t, map[string]string{"big.bin": "the base content is long\n"})
	// Omitted entry claims a 1-byte size for the multi-byte base file:
	// the size check alone proves it changed.
	mode := "0644"
	size := int64(1)
	digest := sha256Digest("x")
	handoff := handoffFromEntries(t, []export.Entry{
		{Path: "big.bin", Kind: export.EntryRegular, Mode: &mode, Size: &size, Digest: &digest, BlobOmitted: true},
	})
	clone := cloneAtBase(t, checkout)
	res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA != "" {
		t.Fatal("a changed omitted entry blocks the commit")
	}
	blocked := false
	for _, f := range res.Findings {
		if f.Kind == FindingBlobOmitted && f.Path == "big.bin" {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("expected a blob_omitted finding for the size-mismatched entry: %+v", res.Findings)
	}
}

// TestImportOmittedReplacesNonRegular is the round-9 P2 regression: an
// omitted regular entry replacing a symlink at the same base path emits
// both blob_omitted and the non_regular_change §5.6 classification, not
// just blob_omitted.
func TestImportOmittedReplacesNonRegular(t *testing.T) {
	checkout, _ := initBaseRepo(t, map[string]string{"a.txt": "x\n"})
	if err := os.Symlink("a.txt", filepath.Join(checkout, "ln")); err != nil {
		t.Fatal(err)
	}
	rungit(t, checkout, "add", "-A")
	rungit(t, checkout, "commit", "-q", "-m", "symlink")
	base := rungit(t, checkout, "rev-parse", "HEAD")

	// An omitted regular entry at "ln" (was a symlink in base).
	mode := "0644"
	size := int64(1 << 30) // "oversized", blob omitted
	digest := sha256Digest("whatever")
	handoff := handoffFromEntries(t, []export.Entry{
		{Path: "ln", Kind: export.EntryRegular, Mode: &mode, Size: &size, Digest: &digest, BlobOmitted: true},
	})
	clone := cloneAtBase(t, checkout)
	res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA != "" {
		t.Fatal("expected the commit withheld")
	}
	kinds := map[FindingKind]bool{}
	for _, f := range res.Findings {
		if f.Path == "ln" {
			kinds[f.Kind] = true
		}
	}
	if !kinds[FindingBlobOmitted] || !kinds[FindingNonRegularChange] {
		t.Fatalf("expected both blob_omitted and non_regular_change for ln: %+v", res.Findings)
	}
}

func TestImportBlobOmittedUnchangedElides(t *testing.T) {
	checkout, base := initBaseRepo(t, map[string]string{
		"big.bin": "pretend this is oversized\n",
		"a.txt":   "old\n",
	})
	handoff := handoffFromEntries(t, []export.Entry{
		regularEntryFor("big.bin", "pretend this is oversized\n", true),
		regularEntryFor("a.txt", "new\n", false),
	}, "new\n")
	clone := cloneAtBase(t, checkout)
	res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(res.Findings) != 0 || res.CommitSHA == "" {
		t.Fatalf("unchanged oversized file must import cleanly: %+v", res)
	}
	if len(res.Changes) != 1 || res.Changes[0].Path != "a.txt" {
		t.Fatalf("changes = %+v, want only a.txt", res.Changes)
	}
}

func TestImportSubmodulePointerRetained(t *testing.T) {
	checkout, _ := initBaseRepo(t, map[string]string{"a.txt": "old\n"})
	const pointer = "a94a8fe5ccb19ba61c4c0873d391e987982fbbd3"
	rungit(t, checkout, "update-index", "--add", "--cacheinfo", "160000,"+pointer+",sub")
	rungit(t, checkout, "commit", "-q", "-m", "gitlink")
	base := rungit(t, checkout, "rev-parse", "HEAD")

	handoff := handoffFromEntries(t, []export.Entry{
		{Path: "sub", Kind: export.EntrySubmodule},
		regularEntryFor("a.txt", "new\n", false),
	}, "new\n")
	clone := cloneAtBase(t, checkout)
	res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(res.Findings) != 0 || res.CommitSHA == "" {
		t.Fatalf("existing submodule pointer must not block: %+v", res)
	}
	entry := rungit(t, clone, "ls-tree", res.CommitSHA, "sub")
	if entry == "" || entry[:6] != "160000" {
		t.Errorf("sub in tree = %q, want the retained 160000 pointer", entry)
	}
}

func TestImportNewSubmoduleBlockedAndOpaque(t *testing.T) {
	checkout, base := initBaseRepo(t, map[string]string{
		"sub/inner.txt": "data\n",
		"a.txt":         "old\n",
	})
	handoff := handoffFromEntries(t, []export.Entry{
		{Path: "sub", Kind: export.EntrySubmodule},
		regularEntryFor("a.txt", "new\n", false),
	}, "new\n")
	clone := cloneAtBase(t, checkout)
	res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA != "" {
		t.Fatal("new submodule still produced a commit")
	}
	for _, c := range res.Changes {
		if c.Path == "sub/inner.txt" {
			t.Fatal("opaque subtree suppression failed: base content under the submodule derived as deleted")
		}
	}
	goldenResult(t, "import_submodule_result", res)
}

func TestImportDeletedSymlinkBlocks(t *testing.T) {
	checkout, _ := initBaseRepo(t, map[string]string{"a.txt": "same\n"})
	if err := os.Symlink("a.txt", filepath.Join(checkout, "ln")); err != nil {
		t.Fatal(err)
	}
	rungit(t, checkout, "add", "-A")
	rungit(t, checkout, "commit", "-q", "-m", "symlink")
	base := rungit(t, checkout, "rev-parse", "HEAD")

	// The workspace dropped the symlink; the manifest holds only a.txt.
	handoff := handoffFromEntries(t, []export.Entry{
		regularEntryFor("a.txt", "same\n", false),
	}, "same\n")
	clone := cloneAtBase(t, checkout)
	res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA != "" {
		t.Fatal("deleting a symlink still produced a commit")
	}
	goldenResult(t, "import_deleted_symlink_result", res)
}

// TestImportInvalidPathDirectorySuppressesDeletions is the F1
// regression: an invalid_path directory (a non-UTF-8 name the exporter
// recorded without descending) must suppress deletions of base content
// beneath it exactly as a submodule does, so blindness never derives a
// phantom mass-deletion.
func TestImportInvalidPathDirectorySuppressesDeletions(t *testing.T) {
	dir := t.TempDir()
	rungit(t, dir, "init", "-q")
	rungit(t, dir, "commit", "-q", "--allow-empty", "-m", "base0")
	blobFile := filepath.Join(t.TempDir(), "content")
	if err := os.WriteFile(blobFile, []byte("data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blobSha := rungit(t, dir, "hash-object", "-w", blobFile)
	// A base directory whose name is non-UTF-8 (raw byte 0xe9), holding
	// two files. git trees carry raw path bytes, so this exists in the
	// object database though APFS could not check it out.
	const badDir = "caf\xe9"
	rungit(t, dir, "update-index", "--add", "--cacheinfo", "100644,"+blobSha+","+badDir+"/a.txt")
	rungit(t, dir, "update-index", "--add", "--cacheinfo", "100644,"+blobSha+","+badDir+"/b.txt")
	rungit(t, dir, "write-tree")
	tree := rungit(t, dir, "write-tree")
	commit := rungit(t, dir, "commit-tree", tree, "-p", rungit(t, dir, "rev-parse", "HEAD"), "-m", "nonutf8")
	rungit(t, dir, "update-ref", "HEAD", commit)
	base := rungit(t, dir, "rev-parse", "HEAD")

	// The workspace still holds that directory (recorded as one
	// invalid_path entry) plus a clean add.
	pathHex := hexEncode(badDir)
	handoff := handoffFromEntries(t, []export.Entry{
		{PathHex: pathHex, Kind: export.EntryInvalidPath},
		regularEntryFor("a.txt", "new\n", false),
	}, "new\n")
	clone := cloneNoCheckout(t, dir)
	res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA != "" {
		t.Fatal("an invalid_path entry is publish-blocking; no commit expected")
	}
	for _, c := range res.Changes {
		if c.Kind == ChangeDeleted {
			t.Errorf("phantom deletion derived from exporter blindness: %q", c.Path)
		}
	}
	sawInvalid := false
	for _, f := range res.Findings {
		if f.Kind == FindingInvalidPathEntry && f.PathHex == pathHex {
			sawInvalid = true
		}
	}
	if !sawInvalid {
		t.Errorf("expected an invalid_path finding, got %+v", res.Findings)
	}
}

// TestImportDeletesNonRepresentableBasePath is the round-10 regression:
// deleting a base path that is not canonical UTF-8 is blocking and
// reported losslessly by PathHex, never as a raw (JSON-lossy) Path.
func TestImportDeletesNonRepresentableBasePath(t *testing.T) {
	dir := t.TempDir()
	rungit(t, dir, "init", "-q")
	rungit(t, dir, "commit", "-q", "--allow-empty", "-m", "base0")
	blobFile := filepath.Join(t.TempDir(), "content")
	if err := os.WriteFile(blobFile, []byte("data\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blobSha := rungit(t, dir, "hash-object", "-w", blobFile)
	const badName = "caf\xe9.txt" // non-UTF-8
	rungit(t, dir, "update-index", "--add", "--cacheinfo", "100644,"+blobSha+","+badName)
	tree := rungit(t, dir, "write-tree")
	commit := rungit(t, dir, "commit-tree", tree, "-p", rungit(t, dir, "rev-parse", "HEAD"), "-m", "nonutf8")
	rungit(t, dir, "update-ref", "HEAD", commit)
	base := rungit(t, dir, "rev-parse", "HEAD")

	// The manifest omits the bad name (candidate deleted it) and adds a
	// clean file.
	handoff := handoffFromEntries(t, []export.Entry{
		regularEntryFor("a.txt", "new\n", false),
	}, "new\n")
	clone := cloneNoCheckout(t, dir)
	res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA != "" {
		t.Fatal("deleting a non-representable base path is blocking")
	}
	wantHex := hexEncode(badName)
	sawFinding, sawChange := false, false
	for _, f := range res.Findings {
		if f.Kind == FindingInvalidPathEntry && f.PathHex == wantHex {
			sawFinding = true
		}
	}
	for _, c := range res.Changes {
		if c.Kind == ChangeDeleted && c.PathHex == wantHex {
			sawChange = true
			if c.Path != "" {
				t.Errorf("non-representable deletion also carried a lossy Path %q", c.Path)
			}
		}
	}
	if !sawFinding || !sawChange {
		t.Fatalf("expected a blocking invalid_path finding and a PathHex deletion: findings=%+v changes=%+v", res.Findings, res.Changes)
	}
	// Marshaled Result must not contain the raw non-UTF-8 byte.
	blob, _ := json.Marshal(res)
	if strings.ContainsRune(string(blob), '�') || strings.Contains(string(blob), badName) {
		t.Error("Result JSON carried a lossy or raw non-representable path")
	}
}

// TestImportOmittedModeOnlyChange is the F2 regression: an oversized
// file whose blob the export caps omitted, chmod'd but byte-identical
// to base, imports cleanly by reusing the base object rather than
// blocking on a withheld blob.
func TestImportOmittedModeOnlyChange(t *testing.T) {
	const content = "pretend this is oversized\n"
	checkout, base := initBaseRepo(t, map[string]string{"big.sh": content})
	mode := "0755"
	size := int64(len(content))
	digest := sha256Digest(content)
	handoff := handoffFromEntries(t, []export.Entry{
		{Path: "big.sh", Kind: export.EntryRegular, Mode: &mode, Size: &size, Digest: &digest, BlobOmitted: true},
	})
	clone := cloneAtBase(t, checkout)
	res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatal("a mode-only change on an omitted blob is representable from base and must not block")
	}
	if len(res.Findings) != 0 {
		t.Fatalf("mode-only change produced findings: %+v", res.Findings)
	}
	entry := rungit(t, clone, "ls-tree", res.CommitSHA, "big.sh")
	if entry[:6] != "100755" {
		t.Errorf("big.sh mode = %q, want 100755", entry[:6])
	}
	// The committed object is the base blob (content unchanged).
	if got := rungit(t, clone, "show", res.CommitSHA+":big.sh"); got != "pretend this is oversized" {
		t.Errorf("big.sh content = %q, want the base content", got)
	}
}

// TestImportRejectsSpecialManifest is the Codex P2 regression: a FIFO
// (or a symlink) at manifest.json must fail closed with a typed error,
// never block the open or read through the link.
func TestImportRejectsSpecialManifest(t *testing.T) {
	checkout, base := initBaseRepo(t, map[string]string{"a.txt": "old\n"})

	t.Run("fifo manifest", func(t *testing.T) {
		dir := t.TempDir()
		fifo := filepath.Join(dir, export.ManifestFilename)
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Skipf("mkfifo unsupported: %v", err)
		}
		// A bare os.Open on this path would block forever waiting for a
		// writer; the hardened open must return promptly, fail closed.
		done := make(chan error, 1)
		go func() {
			_, err := Import(t.Context(), dir, cloneAtBase(t, checkout), testImportOptions(base))
			done <- err
		}()
		select {
		case err := <-done:
			if !errors.Is(err, ErrManifestUnreadable) {
				t.Fatalf("Import = %v, want %v", err, ErrManifestUnreadable)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Import blocked on a FIFO manifest instead of failing closed")
		}
	})

	t.Run("symlink manifest", func(t *testing.T) {
		dir := t.TempDir()
		secret := filepath.Join(t.TempDir(), "outside")
		if err := os.WriteFile(secret, []byte(`{"version":"x"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(secret, filepath.Join(dir, export.ManifestFilename)); err != nil {
			t.Fatal(err)
		}
		_, err := Import(t.Context(), dir, cloneAtBase(t, checkout), testImportOptions(base))
		if !errors.Is(err, ErrManifestUnreadable) {
			t.Fatalf("Import = %v, want %v (symlink must not be followed out of the handoff)", err, ErrManifestUnreadable)
		}
	})
}

// TestImportRejectsSpecialBlob is the sibling case: a special file
// planted at a blob path fails closed rather than blocking or being
// read as content.
func TestImportRejectsSpecialBlob(t *testing.T) {
	checkout, base := initBaseRepo(t, map[string]string{"a.txt": "old\n"})
	handoff := buildHandoff(t, []blobSpec{{path: "a.txt", content: "new\n"}})
	// Replace the blob file with a FIFO at the same digest name.
	blobName := filepath.Join(handoff, "blobs", "sha256", string(sha256Digest("new\n"))[len("sha256:"):])
	if err := os.Remove(blobName); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(blobName, 0o600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Import(t.Context(), handoff, cloneAtBase(t, checkout), testImportOptions(base))
		done <- err
	}()
	select {
	case err := <-done:
		// The layout audit rejects a non-regular blob-store entry before
		// any open; either way the import fails closed, never blocks.
		if err == nil {
			t.Fatal("Import accepted a FIFO blob")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Import blocked on a FIFO blob instead of failing closed")
	}
}

// TestImportBlobPresentModeOnlyChange is the Codex P2 sibling of F2: a
// blob-present file identical to base but chmod'd imports cleanly with
// no secret re-scan, reusing the base object.
func TestImportBlobPresentModeOnlyChange(t *testing.T) {
	token := "ghp_" + strings.Repeat("A", 36)
	content := "TOKEN=" + token + "\n"
	// Base already holds the token at 0644; the candidate only chmods it.
	checkout, base := initBaseRepo(t, map[string]string{"run.sh": content})
	mode := "0755"
	size := int64(len(content))
	digest := sha256Digest(content)
	handoff := handoffFromEntries(t, []export.Entry{
		{Path: "run.sh", Kind: export.EntryRegular, Mode: &mode, Size: &size, Digest: &digest},
	}, content)
	clone := cloneAtBase(t, checkout)
	res, err := Import(t.Context(), handoff, clone, testImportOptions(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatal("a mode-only change on an unchanged file must not block")
	}
	for _, f := range res.Findings {
		if f.Kind == FindingSecret {
			t.Fatalf("chmod on an unchanged token-bearing file must not re-flag a secret: %+v", f)
		}
	}
	entry := rungit(t, clone, "ls-tree", res.CommitSHA, "run.sh")
	if entry[:6] != "100755" {
		t.Errorf("run.sh mode = %q, want 100755", entry[:6])
	}
}

func TestImportBaseMismatchFailsClosed(t *testing.T) {
	checkout, base := initBaseRepo(t, map[string]string{"a.txt": "one\n"})
	if err := os.WriteFile(filepath.Join(checkout, "a.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rungit(t, checkout, "add", "-A")
	rungit(t, checkout, "commit", "-q", "-m", "ahead")

	handoff := handoffFromEntries(t, []export.Entry{
		regularEntryFor("a.txt", "three\n", false),
	}, "three\n")
	_, err := Import(t.Context(), handoff, checkout, testImportOptions(base))
	if !errors.Is(err, ErrBaseMismatch) {
		t.Fatalf("Import = %v, want %v", err, ErrBaseMismatch)
	}
}
