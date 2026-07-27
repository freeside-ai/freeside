package ward

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// testBaseSHA is a syntactically valid resolved commit; the fixtures never
// need a real one, only one the gate's shape rules accept.
const testBaseSHA = "0123456789abcdef0123456789abcdef01234567"

func testBaseRevision() domain.BaseRevision {
	return domain.BaseRevision{
		Repo:         "example/repo",
		RepositoryID: 42,
		BaseRef:      "main",
		BaseSHA:      testBaseSHA,
	}
}

// writeSeedCheckout materializes a minimal daemon-owned checkout under root and
// returns its path: a detached .git/HEAD holding sha, plus one ordinary file.
// It mirrors what publish.Transport.FetchBase leaves behind (HEAD detached at
// the base, no symlinks), which is the only shape the gate accepts.
func writeSeedCheckout(t *testing.T, root, sha string) string {
	t.Helper()
	return writeSeedCheckoutFor(t, root, sha, testBaseRevision().Repo)
}

// writeSeedCheckoutFor is writeSeedCheckout bound to a named repository, so a
// test can build the cross-repository source the binding check exists to
// refuse. The config mirrors what publish.Transport.FetchBase stamps.
func writeSeedCheckoutFor(t *testing.T, root, sha, repo string) string {
	t.Helper()
	dir := filepath.Join(root, "checkout")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "[core]\n\tbare = false\n[freeside \"transport\"]\n\trepo = " + repo + "\n"
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte(sha+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeGitConfig replaces a fixture checkout's config, so a test can shape the
// daemon-authored repository binding the gate re-gates against.
func writeGitConfig(t *testing.T, dir, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".git", "config"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWorkspaceSeedValidate(t *testing.T) {
	valid := WorkspaceSeed{Mode: SeedBaseCheckout, SourceDir: "/seeds/checkout", Base: testBaseRevision()}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid base_checkout seed: validate() = %v, want nil", err)
	}
	if err := (WorkspaceSeed{Mode: SeedBlank}).validate(); err != nil {
		t.Fatalf("valid blank seed: validate() = %v, want nil", err)
	}

	cases := []struct {
		name string
		seed WorkspaceSeed
	}{
		{"zero value", WorkspaceSeed{}},
		{"unknown mode", WorkspaceSeed{Mode: SeedMode("base-checkout")}},
		// A blank seed carrying seeding inputs is ambiguous, not harmless: the
		// caller either named the wrong mode or is passing a base nothing will
		// ever verify.
		{"blank with source", WorkspaceSeed{Mode: SeedBlank, SourceDir: "/seeds/checkout"}},
		{"blank with base", WorkspaceSeed{Mode: SeedBlank, Base: testBaseRevision()}},
		{"missing source", WorkspaceSeed{Mode: SeedBaseCheckout, Base: testBaseRevision()}},
		{"relative source", WorkspaceSeed{Mode: SeedBaseCheckout, SourceDir: "seeds/checkout", Base: testBaseRevision()}},
		{"root source", WorkspaceSeed{Mode: SeedBaseCheckout, SourceDir: "/", Base: testBaseRevision()}},
		{"uncleaned source", WorkspaceSeed{Mode: SeedBaseCheckout, SourceDir: "/seeds/../seeds/checkout", Base: testBaseRevision()}},
		{"traversal source", WorkspaceSeed{Mode: SeedBaseCheckout, SourceDir: "/seeds/../../etc", Base: testBaseRevision()}},
		// ':' separates <container>:<path> in the copy CLI, so it is refused
		// rather than escaped.
		{"source with container delimiter", WorkspaceSeed{Mode: SeedBaseCheckout, SourceDir: "/seeds/other:/etc", Base: testBaseRevision()}},
		{"source with control character", WorkspaceSeed{Mode: SeedBaseCheckout, SourceDir: "/seeds/check\nout", Base: testBaseRevision()}},
		{"missing base", WorkspaceSeed{Mode: SeedBaseCheckout, SourceDir: "/seeds/checkout"}},
		{"base without repo", WorkspaceSeed{Mode: SeedBaseCheckout, SourceDir: "/seeds/checkout", Base: domain.BaseRevision{RepositoryID: 42, BaseRef: "main", BaseSHA: testBaseSHA}}},
		{"base without repository id", WorkspaceSeed{Mode: SeedBaseCheckout, SourceDir: "/seeds/checkout", Base: domain.BaseRevision{Repo: "example/repo", BaseRef: "main", BaseSHA: testBaseSHA}}},
		{"base without ref", WorkspaceSeed{Mode: SeedBaseCheckout, SourceDir: "/seeds/checkout", Base: domain.BaseRevision{Repo: "example/repo", RepositoryID: 42, BaseSHA: testBaseSHA}}},
		// domain.BaseRevision requires only a non-empty SHA; the gate compares
		// this value byte for byte against an observed HEAD, so it pins the
		// exact shape as well.
		{"abbreviated base sha", WorkspaceSeed{Mode: SeedBaseCheckout, SourceDir: "/seeds/checkout", Base: domain.BaseRevision{Repo: "example/repo", RepositoryID: 42, BaseRef: "main", BaseSHA: testBaseSHA[:12]}}},
		{"uppercase base sha", WorkspaceSeed{Mode: SeedBaseCheckout, SourceDir: "/seeds/checkout", Base: domain.BaseRevision{Repo: "example/repo", RepositoryID: 42, BaseRef: "main", BaseSHA: strings.ToUpper(testBaseSHA)}}},
		{"ref-shaped base sha", WorkspaceSeed{Mode: SeedBaseCheckout, SourceDir: "/seeds/checkout", Base: domain.BaseRevision{Repo: "example/repo", RepositoryID: 42, BaseRef: "main", BaseSHA: "refs/heads/main"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.seed.validate(); !errors.Is(err, ErrInvalidHandoffSpec) {
				t.Errorf("validate() = %v, want ErrInvalidHandoffSpec", err)
			}
		})
	}
}

func TestAllSeedModesValid(t *testing.T) {
	if (SeedMode("")).valid() {
		t.Error("the zero SeedMode is valid; it must not be")
	}
	for _, m := range AllSeedModes {
		if !m.valid() {
			t.Errorf("AllSeedModes carries %q, which valid() rejects", m)
		}
	}
	seen := map[SeedMode]bool{}
	for _, m := range AllSeedModes {
		if seen[m] {
			t.Errorf("AllSeedModes repeats %q", m)
		}
		seen[m] = true
	}
}

// dropField marks a proof field the fixture should omit entirely, as distinct
// from one carrying a wrong value.
const dropField = "\x00drop"

func TestVerifyBaseProof(t *testing.T) {
	const nonce = "0123456789abcdef0123456789abcdef"
	const tree = "feedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedfacefeedface"
	good := []byte("nonce=" + nonce + "\ngit_dir=present\nhead_detached=yes\nbase_sha=" + testBaseSHA +
		"\nworktree=present\nirregular=absent\ntree_sha256=" + tree + "\n")

	got, err := verifyBaseProof(good, nonce, tree)
	if err != nil {
		t.Fatalf("conforming proof: %v, want nil", err)
	}
	if got != testBaseSHA {
		t.Errorf("observed base = %q, want %q", got, testBaseSHA)
	}

	// proofWith renders a full proof with one field replaced, so each case
	// differs from the conforming fixture only in the thing under test.
	proofWith := func(overrides map[string]string) string {
		fields := []struct{ key, val string }{
			{baseProofNonceKey, nonce},
			{baseProofGitDirKey, "present"},
			{baseProofDetachedKey, "yes"},
			{baseProofSHAKey, testBaseSHA},
			{baseProofWorktreeKey, "present"},
			{baseProofIrregularKey, "absent"},
			{baseProofTreeKey, tree},
		}
		var b strings.Builder
		for _, f := range fields {
			v, replaced := overrides[f.key]
			if replaced && v == dropField {
				continue
			}
			if !replaced {
				v = f.val
			}
			b.WriteString(f.key + "=" + v + "\n")
		}
		return b.String()
	}

	cases := []struct {
		name  string
		proof string
	}{
		{"empty", ""},
		{"omits nonce", proofWith(map[string]string{baseProofNonceKey: dropField})},
		{"omits git dir", proofWith(map[string]string{baseProofGitDirKey: dropField})},
		{"omits detached", proofWith(map[string]string{baseProofDetachedKey: dropField})},
		{"omits sha", proofWith(map[string]string{baseProofSHAKey: dropField})},
		{"omits worktree", proofWith(map[string]string{baseProofWorktreeKey: dropField})},
		{"omits irregular", proofWith(map[string]string{baseProofIrregularKey: dropField})},
		// A symlink slipped into the source between the walk and the copy.
		{"irregular entry present", proofWith(map[string]string{baseProofIrregularKey: "present"})},
		{"omits tree digest", proofWith(map[string]string{baseProofTreeKey: dropField})},
		// The nonce is what binds the proof to this invocation; without the
		// check, a file the image shipped with would satisfy the gate.
		{"foreign nonce", proofWith(map[string]string{baseProofNonceKey: strings.Repeat("f", 32)})},
		{"empty nonce", proofWith(map[string]string{baseProofNonceKey: ""})},
		{"no git dir", proofWith(map[string]string{baseProofGitDirKey: "absent"})},
		{"symbolic head", proofWith(map[string]string{baseProofDetachedKey: "no"})},
		// The case the intended producer actually causes: a checkout whose
		// worktree was never materialized. HEAD alone would have passed.
		{"no working tree", proofWith(map[string]string{baseProofWorktreeKey: "absent"})},
		// Content that is not the content the host verified: a partial or
		// altered copy that still carries the right HEAD.
		{"tree digest mismatch", proofWith(map[string]string{baseProofTreeKey: strings.Repeat("a", 64)})},
		{"tree digest unset", proofWith(map[string]string{baseProofTreeKey: "none"})},
		{"abbreviated sha", proofWith(map[string]string{baseProofSHAKey: testBaseSHA[:12]})},
		{"uppercase sha", proofWith(map[string]string{baseProofSHAKey: strings.ToUpper(testBaseSHA)})},
		{"ref-shaped sha", proofWith(map[string]string{baseProofSHAKey: "ref: refs/heads/main"})},
		{"unknown key", string(good) + "workspace_write=succeeded\n"},
		{"repeated key", string(good) + "base_sha=" + strings.Repeat("c", 40) + "\n"},
		{"repeated fixed key", string(good) + "git_dir=present\n"},
		{"not key=value", string(good) + "garbage\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sha, err := verifyBaseProof([]byte(tc.proof), nonce, tree)
			wantCheckFailure(t, err, CheckObservedBaseIdentity)
			if sha != "" {
				t.Errorf("rejected proof still yielded a base %q", sha)
			}
		})
	}
}

// TestVerifyBaseProofNeverEchoesContent holds the proof parser to the same
// rule as check 5's: the bytes come out of an archive nothing has scanned, so
// a refusal names the violation and never repeats what it read.
func TestVerifyBaseProofNeverEchoesContent(t *testing.T) {
	const nonce = "0123456789abcdef0123456789abcdef"
	const planted = "attacker-controlled-fixture-value"
	_, err := verifyBaseProof([]byte("nonce="+nonce+"\ngit_dir="+planted+"\nhead_detached=yes\nbase_sha="+testBaseSHA+
		"\nworktree=present\nirregular=absent\ntree_sha256="+strings.Repeat("e", 64)+"\n"), nonce, strings.Repeat("e", 64))
	if err == nil {
		t.Fatal("unexpected value accepted, want a failure")
	}
	if strings.Contains(err.Error(), planted) {
		t.Errorf("failure echoes proof content: %v", err)
	}
	// The nonce is likewise withheld: naming it would confirm a guess.
	if strings.Contains(err.Error(), nonce) {
		t.Errorf("failure echoes the invocation nonce: %v", err)
	}
}

func TestVerifySeedSourceAcceptsDaemonOwnedCheckout(t *testing.T) {
	root := t.TempDir()
	dir := writeSeedCheckout(t, root, testBaseSHA)
	cfg := testConfig()
	cfg.SeedRoot = root
	got, digest, err := verifySeedSource(cfg, dir, testBaseRevision().Repo)
	if err != nil {
		t.Fatalf("verifySeedSource() = %v, want nil", err)
	}
	// The resolved path is what the caller stages, so it must come back
	// resolved: staging the caller's spelling would leave containment advisory.
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("resolved source = %q, want %q", got, want)
	}
	// The digest is the expectation the observer is held to, so it must cover
	// the tree the host just walked, deterministically.
	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(digest) {
		t.Errorf("tree digest = %q, want a sha256 hex digest", digest)
	}
	if again := digestOfDir(t, want); again != digest {
		t.Errorf("tree digest is not deterministic: %q then %q", digest, again)
	}
	// A single changed byte anywhere in the tree must change it, or a partial
	// or altered copy would attest as the verified one.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("tampered\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, changed, err := verifySeedSource(cfg, dir, testBaseRevision().Repo)
	if err != nil {
		t.Fatal(err)
	}
	if changed == digest {
		t.Error("tree digest is unchanged after altering a file's content")
	}

	// A git tree records the executable bit, so a workspace whose scripts lost
	// it is not the approved tree even with identical bytes.
	script := filepath.Join(dir, "run.sh")
	//nolint:gosec // the pair of modes is the property under test
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, plain, err := verifySeedSource(cfg, dir, testBaseRevision().Repo)
	if err != nil {
		t.Fatal(err)
	}
	//nolint:gosec // granting the executable bit is exactly what this asserts is detected
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	_, executable, err := verifySeedSource(cfg, dir, testBaseRevision().Repo)
	if err != nil {
		t.Fatal(err)
	}
	if plain == executable {
		t.Error("tree digest is unchanged after a file gains the executable bit")
	}
}

func TestVerifySeedSourceFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		// build returns the source directory to verify and may adjust cfg; both
		// the checkout and the seed root are real files, because containment is
		// the property under test and a fake path cannot exercise it.
		build func(t *testing.T, root string, cfg *Config) string
	}{
		{"unconfigured seed root", func(t *testing.T, root string, cfg *Config) string {
			dir := writeSeedCheckout(t, root, testBaseSHA)
			cfg.SeedRoot = ""
			return dir
		}},
		{"seed root does not exist", func(t *testing.T, root string, cfg *Config) string {
			dir := writeSeedCheckout(t, root, testBaseSHA)
			cfg.SeedRoot = filepath.Join(root, "absent")
			return dir
		}},
		{"source does not exist", func(_ *testing.T, root string, _ *Config) string {
			return filepath.Join(root, "absent")
		}},
		{"source is a file", func(t *testing.T, root string, _ *Config) string {
			p := filepath.Join(root, "afile")
			if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
				t.Fatal(err)
			}
			return p
		}},
		{"source outside the seed root", func(t *testing.T, root string, cfg *Config) string {
			outside := t.TempDir()
			cfg.SeedRoot = root
			return writeSeedCheckout(t, outside, testBaseSHA)
		}},
		{"source symlinked out of the seed root", func(t *testing.T, root string, cfg *Config) string {
			outside := t.TempDir()
			real := writeSeedCheckout(t, outside, testBaseSHA)
			link := filepath.Join(root, "checkout")
			if err := os.Symlink(real, link); err != nil {
				t.Fatal(err)
			}
			cfg.SeedRoot = root
			return link
		}},
		{"tree contains a symlink", func(t *testing.T, root string, _ *Config) string {
			dir := writeSeedCheckout(t, root, testBaseSHA)
			if err := os.Symlink(filepath.Join(dir, "README.md"), filepath.Join(dir, "link.md")); err != nil {
				t.Fatal(err)
			}
			return dir
		}},
		{"tree contains a dangling symlink", func(t *testing.T, root string, _ *Config) string {
			dir := writeSeedCheckout(t, root, testBaseSHA)
			if err := os.Symlink(filepath.Join(dir, "absent"), filepath.Join(dir, "dangling")); err != nil {
				t.Fatal(err)
			}
			return dir
		}},
		{"tree exceeds the entry budget", func(t *testing.T, root string, cfg *Config) string {
			dir := writeSeedCheckout(t, root, testBaseSHA)
			cfg.MaxSeedEntries = 2
			return dir
		}},
		{"tree exceeds the byte budget", func(t *testing.T, root string, cfg *Config) string {
			dir := writeSeedCheckout(t, root, testBaseSHA)
			cfg.MaxSeedBytes = 1
			return dir
		}},
		{"tree contains a non-regular file", func(t *testing.T, root string, _ *Config) string {
			dir := writeSeedCheckout(t, root, testBaseSHA)
			fifo := filepath.Join(dir, "pipe")
			if err := syscall.Mkfifo(fifo, 0o600); err != nil {
				t.Skipf("mkfifo unsupported here: %v", err)
			}
			return dir
		}},
		// The cross-repository case: a seed root holds every managed
		// repository's checkout, and forks share commits, so another
		// repository's checkout can carry the declared SHA and satisfy every
		// tree-shaped check here. Only the daemon-authored binding separates
		// them.
		{"bound to a different repository", func(t *testing.T, root string, _ *Config) string {
			return writeSeedCheckoutFor(t, root, testBaseSHA, "example/fork")
		}},
		{"no repository binding", func(t *testing.T, root string, _ *Config) string {
			dir := writeSeedCheckout(t, root, testBaseSHA)
			writeGitConfig(t, dir, "[core]\n\tbare = false\n")
			return dir
		}},
		{"no git config at all", func(t *testing.T, root string, _ *Config) string {
			dir := writeSeedCheckout(t, root, testBaseSHA)
			if err := os.Remove(filepath.Join(dir, ".git", "config")); err != nil {
				t.Fatal(err)
			}
			return dir
		}},
		// An include could define the binding in a file this check never reads,
		// so the config cannot be evaluated here at all.
		{"config carries an include", func(t *testing.T, root string, _ *Config) string {
			dir := writeSeedCheckout(t, root, testBaseSHA)
			writeGitConfig(t, dir, "[include]\n\tpath = other\n[freeside \"transport\"]\n\trepo = "+testBaseRevision().Repo+"\n")
			return dir
		}},
		{"binding repeated with conflicting values", func(t *testing.T, root string, _ *Config) string {
			dir := writeSeedCheckout(t, root, testBaseSHA)
			writeGitConfig(t, dir, "[freeside \"transport\"]\n\trepo = "+testBaseRevision().Repo+"\n\trepo = example/fork\n")
			return dir
		}},
		// git treats a subsection name case-sensitively, so a differently-cased
		// header is a different section and carries no binding at all.
		{"binding under a differently cased subsection", func(t *testing.T, root string, _ *Config) string {
			dir := writeSeedCheckout(t, root, testBaseSHA)
			writeGitConfig(t, dir, "[freeside \"Transport\"]\n\trepo = "+testBaseRevision().Repo+"\n")
			return dir
		}},
		{"binding commented out", func(t *testing.T, root string, _ *Config) string {
			dir := writeSeedCheckout(t, root, testBaseSHA)
			writeGitConfig(t, dir, "[freeside \"transport\"]\n\t# repo = "+testBaseRevision().Repo+"\n")
			return dir
		}},
		{"no git head", func(t *testing.T, root string, _ *Config) string {
			dir := writeSeedCheckout(t, root, testBaseSHA)
			if err := os.Remove(filepath.Join(dir, ".git", "HEAD")); err != nil {
				t.Fatal(err)
			}
			return dir
		}},
		{"git head is a directory", func(t *testing.T, root string, _ *Config) string {
			dir := writeSeedCheckout(t, root, testBaseSHA)
			head := filepath.Join(dir, ".git", "HEAD")
			if err := os.Remove(head); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(head, 0o700); err != nil {
				t.Fatal(err)
			}
			return dir
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			cfg := testConfig()
			cfg.SeedRoot = root
			dir := tc.build(t, root, &cfg)
			got, digest, err := verifySeedSource(cfg, dir, testBaseRevision().Repo)
			wantCheckFailure(t, err, CheckWorkspaceSeeding)
			if got != "" || digest != "" {
				t.Errorf("rejected source still yielded path %q digest %q", got, digest)
			}
		})
	}
}
