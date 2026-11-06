package importer

import (
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/execfixture"
	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// gitLog is a git wrapper that records each invocation's arguments before
// running the real git. Its path goes in Options.GitPath; the fixture helpers
// keep using the real git, so the log holds only the importer's processes.
type gitLog struct {
	path string
	log  string
}

func newGitLog(t *testing.T) gitLog {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	if realGit, err = filepath.Abs(realGit); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	l := gitLog{path: filepath.Join(dir, "git"), log: filepath.Join(dir, "calls")}
	// The runner replaces the environment, so both paths are baked in.
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> '%s'\nexec '%s' \"$@\"\n", l.log, realGit)
	execfixture.Write(t, l.path, []byte(script), 0o700)
	return l
}

func (l gitLog) calls(t *testing.T) []string {
	t.Helper()
	body, err := os.ReadFile(l.log)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
}

// catFiles returns every cat-file invocation. The runner prepends its -c
// overrides, so the subcommand is matched as a word, not a prefix.
func (l gitLog) catFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, c := range l.calls(t) {
		if strings.Contains(" "+c+" ", " cat-file ") {
			out = append(out, c)
		}
	}
	return out
}

// countMode counts the batch processes started in one mode.
func (l gitLog) countMode(t *testing.T, mode string) int {
	t.Helper()
	n := 0
	for _, c := range l.catFiles(t) {
		if strings.HasSuffix(c, " cat-file "+mode) {
			n++
		}
	}
	return n
}

// assertSizeOnly fails unless the import asked git for a size and never for
// object content: only `cat-file --batch` streams content (the old per-blob
// `cat-file blob` would also count), while `--batch-check` reports type and
// size alone. Requiring the size query proves the wrapper saw the reader, so
// the absence of a content read means something.
func (l gitLog) assertSizeOnly(t *testing.T) {
	t.Helper()
	calls := l.catFiles(t)
	if l.countMode(t, "--batch-check") != 1 {
		t.Fatalf("want one cat-file --batch-check, got:\n%s", strings.Join(calls, "\n"))
	}
	for _, c := range calls {
		if strings.HasSuffix(c, " cat-file --batch") || strings.Contains(c, " cat-file blob ") {
			t.Fatalf("import read base object content: %q\nall cat-file calls:\n%s", c, strings.Join(calls, "\n"))
		}
	}
}

func (l gitLog) options(base string) Options {
	opts := testImportOptions(base)
	opts.GitPath = l.path
	return opts
}

// An import checks every unchanged base blob through one batch reader, not
// one or two cat-file processes per file.
func TestImportStartsAtMostTwoCatFiles(t *testing.T) {
	const count = 60
	files := make(map[string]string, count)
	for i := range count {
		files[fmt.Sprintf("src/file-%02d.txt", i)] = fmt.Sprintf("content %d\n", i)
	}
	checkout, base := initBaseRepo(t, files)
	workspace := t.TempDir()
	for name, body := range files {
		full := filepath.Join(workspace, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	handoff := exportWorkspace(t, workspace)
	git := newGitLog(t)
	res, err := Import(t.Context(), handoff, cloneAtBase(t, checkout), git.options(base))
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if len(res.Changes) != 0 || len(res.Findings) != 0 {
		t.Fatalf("unchanged workspace imported as %+v", res)
	}
	calls := git.catFiles(t)
	if len(calls) > 2 || git.countMode(t, "--batch-check") != 1 || git.countMode(t, "--batch") != 1 {
		t.Fatalf("import started %d cat-file processes, want one --batch-check and one --batch:\n%s", len(calls), strings.Join(calls, "\n"))
	}
}

// A manifest size claim that differs from the base blob is decided from the
// size query alone; git never streams the base object the manifest chose.
func TestImportSizeClaimMismatchNeverReadsBaseContent(t *testing.T) {
	t.Run("omitted regular entry", func(t *testing.T) {
		checkout, base := initBaseRepo(t, map[string]string{"big.bin": "the base content is long\n"})
		mode, size, digest := "0644", int64(1), sha256Digest("x")
		handoff := handoffFromEntries(t, []export.Entry{
			{Path: "big.bin", Kind: export.EntryRegular, Mode: &mode, Size: &size, Digest: &digest, BlobOmitted: true},
		})
		git := newGitLog(t)
		res, err := Import(t.Context(), handoff, cloneAtBase(t, checkout), git.options(base))
		if err != nil {
			t.Fatalf("Import: %v", err)
		}
		if res.CommitSHA != "" || !hasFinding(res, FindingBlobOmitted, "big.bin") {
			t.Fatalf("result = %+v, want a blob_omitted finding and no commit", res)
		}
		git.assertSizeOnly(t)
	})
	t.Run("symlink", func(t *testing.T) {
		checkout, parent := initBaseRepo(t, nil)
		large := filepath.Join(t.TempDir(), "large-symlink-target")
		if err := os.WriteFile(large, []byte(strings.Repeat("x", 1<<20)), 0o600); err != nil {
			t.Fatal(err)
		}
		oid := rungit(t, checkout, "hash-object", "-w", large)
		rungit(t, checkout, "update-index", "--add", "--cacheinfo", "120000,"+oid+",ln")
		base := rungit(t, checkout, "commit-tree", rungit(t, checkout, "write-tree"), "-p", parent, "-m", "large symlink")
		rungit(t, checkout, "update-ref", "HEAD", base)

		target := "short"
		handoff := handoffFromEntries(t, []export.Entry{{Path: "ln", Kind: export.EntrySymlink, Target: &target}})
		git := newGitLog(t)
		res, err := Import(t.Context(), handoff, cloneNoCheckout(t, checkout), git.options(base))
		if err != nil {
			t.Fatalf("Import: %v", err)
		}
		if res.CommitSHA != "" || !hasFinding(res, FindingNonRegularChange, "ln") {
			t.Fatalf("result = %+v, want a non_regular_change finding and no commit", res)
		}
		git.assertSizeOnly(t)
	})
}

// tagInBlobEntry commits a base whose tree names an annotated tag of a blob
// in a blob-mode entry, alongside ordinary entries, and returns the base and
// the tag object's own size. Only a hand-built tree can hold one: mktree
// refuses the type mismatch, so the tree is written byte for byte.
func tagInBlobEntry(t *testing.T, mode, name, tagged string, others map[string]string) (checkout, base string, tagSize int64) {
	t.Helper()
	checkout, parent := initBaseRepo(t, others)
	blobFile := filepath.Join(t.TempDir(), "tagged")
	if err := os.WriteFile(blobFile, []byte(tagged), 0o600); err != nil {
		t.Fatal(err)
	}
	blob := rungit(t, checkout, "hash-object", "-w", blobFile)
	rungit(t, checkout, "tag", "-a", "blob-tag", "-m", "tag", blob)
	tag := rungit(t, checkout, "rev-parse", "blob-tag")
	tagSize, err := strconv.ParseInt(rungit(t, checkout, "cat-file", "-s", tag), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	// Git orders tree entries by name; the fixtures keep every name a flat
	// file, so a byte sort of "<name>" is git's order.
	entries := map[string]string{name: mode + " " + name + "\x00" + string(mustHex(t, tag))}
	for path := range others {
		oid := rungit(t, checkout, "rev-parse", parent+":"+path)
		entries[path] = "100644 " + path + "\x00" + string(mustHex(t, oid))
	}
	var raw strings.Builder
	for _, path := range slices.Sorted(maps.Keys(entries)) {
		raw.WriteString(entries[path])
	}
	treeFile := filepath.Join(t.TempDir(), "tree")
	if err := os.WriteFile(treeFile, []byte(raw.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	tree := rungit(t, checkout, "hash-object", "-t", "tree", "-w", treeFile)
	base = rungit(t, checkout, "commit-tree", tree, "-p", parent, "-m", "tag in a blob entry")
	rungit(t, checkout, "update-ref", "HEAD", base)
	return checkout, base, tagSize
}

func mustHex(t *testing.T, oid string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(oid)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// A base entry the importer reads as a file must name a blob. A tag there
// used to report its own small size to `cat-file -s` while `cat-file blob`
// peeled it, so a blob of any size passed the size gate. The size query now
// refuses the tag before git streams anything.
func TestImportRefusesATagInABlobEntry(t *testing.T) {
	const tagged = "the tagged blob's content\n"
	t.Run("omitted regular entry", func(t *testing.T) {
		checkout, base, tagSize := tagInBlobEntry(t, "100644", "a.txt", tagged, nil)
		// The claim matches the tag's size, which passed the old size gate,
		// and the peeled blob's digest.
		mode, digest := "0644", sha256Digest(tagged)
		handoff := handoffFromEntries(t, []export.Entry{
			{Path: "a.txt", Kind: export.EntryRegular, Mode: &mode, Size: &tagSize, Digest: &digest, BlobOmitted: true},
		})
		git := newGitLog(t)
		if _, err := Import(t.Context(), handoff, cloneNoCheckout(t, checkout), git.options(base)); !isNotABlob(err) {
			t.Fatalf("Import = %v, want %v refusing a tag", err, ErrGitPlumbing)
		}
		git.assertSizeOnly(t)
	})
	t.Run("symlink", func(t *testing.T) {
		checkout, base, tagSize := tagInBlobEntry(t, "120000", "ln", tagged, nil)
		// A target as long as the tag passed the old size gate.
		target := strings.Repeat("t", int(tagSize))
		handoff := handoffFromEntries(t, []export.Entry{{Path: "ln", Kind: export.EntrySymlink, Target: &target}})
		git := newGitLog(t)
		if _, err := Import(t.Context(), handoff, cloneNoCheckout(t, checkout), git.options(base)); !isNotABlob(err) {
			t.Fatalf("Import = %v, want %v refusing a tag", err, ErrGitPlumbing)
		}
		git.assertSizeOnly(t)
	})
	t.Run("base ignore file", func(t *testing.T) {
		checkout, base, _ := tagInBlobEntry(t, "100644", ".gitignore", "dist/\n", nil)
		// An added file outside the allowlist makes the import consult the
		// base ignore rules. No other base file is unchanged, so no other
		// base content is read.
		handoff := ignoredFixtureHandoff(t, map[string]string{"dist.js": "generated\n"})
		git := newGitLog(t)
		opts := git.options(base)
		opts.Policy.Allowlist = []string{"*.ts"}
		if _, err := Import(t.Context(), handoff, cloneNoCheckout(t, checkout), opts); !isNotABlob(err) {
			t.Fatalf("Import = %v, want %v refusing a tag", err, ErrGitPlumbing)
		}
		git.assertSizeOnly(t)
	})
}

// A base ignore file over the per-file byte cap is refused from its size
// alone; its content is never read.
func TestImportRefusesAnOversizedBaseIgnoreFileUnread(t *testing.T) {
	rules := "dist/\n# " + strings.Repeat("padding ", 16) + "\n"
	checkout, base := initBaseRepo(t, map[string]string{".gitignore": rules})
	handoff := ignoredFixtureHandoff(t, map[string]string{"dist/main.js": "generated\n"})
	git := newGitLog(t)
	opts := git.options(base)
	opts.Policy.Allowlist = []string{"*.ts"}
	opts.Policy.MaxBlobBytes = int64(len(rules)) - 1
	if _, err := Import(t.Context(), handoff, cloneAtBase(t, checkout), opts); !errors.Is(err, ErrUnsupportedRepo) {
		t.Fatalf("Import = %v, want %v", err, ErrUnsupportedRepo)
	}
	git.assertSizeOnly(t)
}

// isNotABlob reports the refusal of a tag where a blob belongs, not some
// other plumbing failure.
func isNotABlob(err error) bool {
	return errors.Is(err, ErrGitPlumbing) && strings.Contains(err.Error(), "is a tag, not a blob")
}

func hasFinding(res Result, kind FindingKind, path string) bool {
	for _, f := range res.Findings {
		if f.Kind == kind && f.Path == path {
			return true
		}
	}
	return false
}
