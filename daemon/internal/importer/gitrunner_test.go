package importer

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// rungit runs git in a fixture repo with a pinned identity and isolated
// config; fixture repos are test-owned, so failures are fatal.
func rungit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: test fixture running git over test-owned repos with test-chosen arguments
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=fixture",
		"GIT_AUTHOR_EMAIL=fixture@test.invalid",
		"GIT_AUTHOR_DATE=1700000000 +0000",
		"GIT_COMMITTER_NAME=fixture",
		"GIT_COMMITTER_EMAIL=fixture@test.invalid",
		"GIT_COMMITTER_DATE=1700000000 +0000",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// initBaseRepo creates a daemon-owned fixture checkout whose HEAD holds
// the given files.
func initBaseRepo(t *testing.T, files map[string]string) (dir, baseSHA string) {
	t.Helper()
	dir = t.TempDir()
	rungit(t, dir, "init", "-q")
	for path, content := range files {
		full := filepath.Join(dir, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rungit(t, dir, "add", "-A")
	rungit(t, dir, "commit", "-q", "--allow-empty", "-m", "base")
	return dir, rungit(t, dir, "rev-parse", "HEAD")
}

// newTestRunner builds a hardened runner against a fixture checkout.
func newTestRunner(t *testing.T, checkout string, opts Options) *gitRunner {
	t.Helper()
	g, err := newGitRunner(t.Context(), opts, checkout, t.TempDir())
	if err != nil {
		t.Fatalf("newGitRunner: %v", err)
	}
	return g
}

func TestVerifyBase(t *testing.T) {
	dir, first := initBaseRepo(t, map[string]string{"a.txt": "one\n"})
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rungit(t, dir, "add", "-A")
	rungit(t, dir, "commit", "-q", "-m", "second")
	second := rungit(t, dir, "rev-parse", "HEAD")
	g := newTestRunner(t, dir, testImportOptions(testBaseSHA))

	if err := g.verifyBase(t.Context(), second); err != nil {
		t.Fatalf("verifyBase(HEAD) = %v, want nil", err)
	}
	if err := g.verifyBase(t.Context(), first); !errors.Is(err, ErrBaseMismatch) {
		t.Fatalf("verifyBase(stale base) = %v, want %v", err, ErrBaseMismatch)
	}
	bogus := strings.Repeat("d", 40)
	if err := g.verifyBase(t.Context(), bogus); !errors.Is(err, ErrBaseMismatch) {
		t.Fatalf("verifyBase(unknown sha) = %v, want %v", err, ErrBaseMismatch)
	}
}

// TestCommitTreeStdinMatchesPriorMessageSemantics is the refute-first
// equivalence harness for moving commit messages off argv. It reconstructs the
// previous `commit-tree -m` invocation and compares object identities over a
// deterministic fuzzed corpus, so the trust-boundary hardening cannot silently
// change existing single-commit hashes or message cleanup behavior.
func TestCommitTreeStdinMatchesPriorMessageSemantics(t *testing.T) {
	dir, base := initBaseRepo(t, map[string]string{"a.txt": "base\n"})
	g := newTestRunner(t, dir, testImportOptions(base))
	tree := rungit(t, dir, "log", "-1", "--format=%T", base)
	alphabet := []rune("abc XYZ012\n\tø中")
	state := uint64(0x9e3779b97f4a7c15)
	for i := 0; i < 200; i++ {
		length := i % 73
		var message strings.Builder
		for range length {
			state = state*6364136223846793005 + 1442695040888963407
			message.WriteRune(alphabet[state%uint64(len(alphabet))])
		}
		if strings.TrimSpace(message.String()) == "" {
			message.WriteString("message")
		}
		got, err := g.commitTree(t.Context(), tree, base, message.String())
		if err != nil {
			t.Fatalf("stdin commit %d: %v", i, err)
		}
		old, err := g.run(t.Context(), nil, "commit-tree", tree, "-p", base, "-m", message.String())
		if err != nil {
			t.Fatalf("prior argv commit %d: %v", i, err)
		}
		if want := strings.TrimSpace(string(old)); got != want {
			t.Fatalf("message transport changed commit %d: stdin=%s argv=%s message=%q", i, got, want, message.String())
		}
	}
}

func TestNewGitRunnerRejectsSHA256Repos(t *testing.T) {
	dir := t.TempDir()
	rungit(t, dir, "init", "-q", "--object-format=sha256")
	_, err := newGitRunner(t.Context(), Options{BaseSHA: testBaseSHA}.withDefaults(), dir, t.TempDir())
	if !errors.Is(err, ErrUnsupportedRepo) {
		t.Fatalf("newGitRunner = %v, want %v", err, ErrUnsupportedRepo)
	}
}

// TestCommitIgnoresLocalConfig is the Codex round-7 regression: a
// commit-affecting value in the checkout's local .git/config
// (i18n.commitEncoding, commit.gpgsign) must not leak into the produced
// commit object, so the daemon-authored commit stays a pure function of
// its inputs. A leaked commit.gpgsign=true would also fail commit-tree
// outright (no signing key), so a clean commit here proves it is pinned
// off too.
func TestCommitIgnoresLocalConfig(t *testing.T) {
	checkout, base := initBaseRepo(t, map[string]string{"a.txt": "x\n"})
	rungit(t, checkout, "config", "i18n.commitEncoding", "ISO-8859-1")
	rungit(t, checkout, "config", "commit.gpgsign", "true")
	g := newTestRunner(t, checkout, testImportOptions(base))
	baseTree := rungit(t, checkout, "rev-parse", base+"^{tree}")
	commit, err := g.commitTree(t.Context(), baseTree, base, "msg")
	if err != nil {
		t.Fatalf("commitTree: %v", err)
	}
	if obj := rungit(t, checkout, "cat-file", "commit", commit); strings.Contains(obj, "\nencoding ") {
		t.Errorf("commit object carries an encoding header from local config:\n%s", obj)
	}
}

// TestIgnoresReplaceObjects is the round-9 P2 regression: a
// refs/replace/* substitution must not influence the import. Without
// GIT_NO_REPLACE_OBJECTS the runner would read the replacement tree
// while verifyBase (rev-parse) still saw the real base SHA.
func TestIgnoresReplaceObjects(t *testing.T) {
	checkout, base := initBaseRepo(t, map[string]string{"a.txt": "real\n"})
	// A second commit with different content, then replace the base
	// commit's object with it.
	if err := os.WriteFile(filepath.Join(checkout, "a.txt"), []byte("fake\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rungit(t, checkout, "add", "-A")
	rungit(t, checkout, "commit", "-q", "-m", "fake")
	fake := rungit(t, checkout, "rev-parse", "HEAD")
	rungit(t, checkout, "replace", base, fake)

	g := newTestRunner(t, checkout, testImportOptions(base))
	tree, err := g.baseTree(t.Context(), base)
	if err != nil {
		t.Fatalf("baseTree: %v", err)
	}
	// The real base blob must be read, not the replacement.
	digest, _, err := g.blobDigest(t.Context(), tree["a.txt"].oid)
	if err != nil {
		t.Fatal(err)
	}
	if digest != sha256Digest("real\n") {
		t.Errorf("read replacement content instead of the real base (a.txt = %s)", digest)
	}
}

// TestBaseMatchesDigest covers the round-13 SHA-256 verification helper:
// the elide/fromBase shortcut trusts it instead of git's SHA-1 identity.
// (The collision path — git SHA-1 equal but sha256 differing — cannot be
// unit-tested without a real SHA-1 collision; the helper's correctness is
// what the derivation relies on.)
func TestBlobMatchesDigest(t *testing.T) {
	checkout, _ := initBaseRepo(t, map[string]string{"a.txt": "hello\n"})
	base := rungit(t, checkout, "rev-parse", "HEAD")
	g := newTestRunner(t, checkout, testImportOptions(base))
	tree, err := g.baseTree(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	oid := tree["a.txt"].oid

	ok, err := g.blobMatchesDigest(t.Context(), oid, sha256Digest("hello\n"), 6)
	if err != nil || !ok {
		t.Fatalf("matching digest+size = %v, %v; want true", ok, err)
	}
	// Wrong size fails cheaply (no stream) and wrong digest fails on the
	// streamed hash; both must report no match.
	if ok, _ := g.blobMatchesDigest(t.Context(), oid, sha256Digest("hello\n"), 7); ok {
		t.Error("size mismatch reported a match")
	}
	if ok, _ := g.blobMatchesDigest(t.Context(), oid, sha256Digest("other\n"), 6); ok {
		t.Error("digest mismatch reported a match")
	}
}

func TestVerifyIngestedBlobsRejectsSHA1Collision(t *testing.T) {
	checkout, base := initBaseRepo(t, map[string]string{"a.txt": "other\n"})
	g := newTestRunner(t, checkout, testImportOptions(base))
	tree, err := g.baseTree(t.Context(), base)
	if err != nil {
		t.Fatal(err)
	}
	oid := tree["a.txt"].oid
	digest := sha256Digest("hello\n")
	// Model a SHA-1 collision without requiring collision fixture bytes:
	// verification derived oid for hello, but the object database holds
	// different same-sized bytes at that oid.
	expected := map[export.Digest]blobInfo{digest: {size: 6, gitOID: oid}}
	ingested := map[export.Digest]string{digest: oid}
	if err := g.verifyIngestedBlobs(t.Context(), []export.Digest{digest}, expected, ingested); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("verifyIngestedBlobs = %v, want %v", err, ErrDigestMismatch)
	}
}

func TestBaseTreeModes(t *testing.T) {
	dir, _ := initBaseRepo(t, map[string]string{"a.txt": "text\n", "d/b.txt": "b\n"})
	if err := os.Symlink("a.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	execPath := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(execPath, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // G306: fixture needs the exec bit; content is inert test data
		t.Fatal(err)
	}
	rungit(t, dir, "add", "-A")
	rungit(t, dir, "commit", "-q", "-m", "shapes")
	base := rungit(t, dir, "rev-parse", "HEAD")

	g := newTestRunner(t, dir, testImportOptions(testBaseSHA))
	tree, err := g.baseTree(t.Context(), base)
	if err != nil {
		t.Fatalf("baseTree: %v", err)
	}
	want := map[string]string{
		"a.txt": "100644", "d/b.txt": "100644", "link": "120000", "run.sh": "100755",
	}
	if len(tree) != len(want) {
		t.Fatalf("baseTree has %d entries, want %d: %v", len(tree), len(want), tree)
	}
	for path, mode := range want {
		if tree[path].mode != mode {
			t.Errorf("mode of %q = %s, want %s", path, tree[path].mode, mode)
		}
	}
	target, err := g.blobContent(t.Context(), tree["link"].oid)
	if err != nil || string(target) != "a.txt" {
		t.Fatalf("symlink target = %q, %v; want a.txt", target, err)
	}
	digest, size, err := g.blobDigest(t.Context(), tree["a.txt"].oid)
	if err != nil || digest != sha256Digest("text\n") || size != 5 {
		t.Fatalf("blobDigest = %s, %d, %v; want %s, 5", digest, size, err, sha256Digest("text\n"))
	}
}
