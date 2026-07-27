package ward

import (
	"errors"
	"os"
	"path/filepath"
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
	dir := filepath.Join(root, "checkout")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o700); err != nil {
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

func TestVerifyBaseProof(t *testing.T) {
	const nonce = "0123456789abcdef0123456789abcdef"
	good := []byte("nonce=" + nonce + "\ngit_dir=present\nhead_detached=yes\nbase_sha=" + testBaseSHA + "\n")

	got, err := verifyBaseProof(good, nonce)
	if err != nil {
		t.Fatalf("conforming proof: %v, want nil", err)
	}
	if got != testBaseSHA {
		t.Errorf("observed base = %q, want %q", got, testBaseSHA)
	}

	cases := []struct {
		name  string
		proof string
	}{
		{"empty", ""},
		{"omits nonce", "git_dir=present\nhead_detached=yes\nbase_sha=" + testBaseSHA + "\n"},
		{"omits git dir", "nonce=" + nonce + "\nhead_detached=yes\nbase_sha=" + testBaseSHA + "\n"},
		{"omits detached", "nonce=" + nonce + "\ngit_dir=present\nbase_sha=" + testBaseSHA + "\n"},
		{"omits sha", "nonce=" + nonce + "\ngit_dir=present\nhead_detached=yes\n"},
		// The nonce is what binds the proof to this invocation; without the
		// check, a file the image shipped with would satisfy the gate.
		{"foreign nonce", "nonce=" + strings.Repeat("f", 32) + "\ngit_dir=present\nhead_detached=yes\nbase_sha=" + testBaseSHA + "\n"},
		{"empty nonce", "nonce=\ngit_dir=present\nhead_detached=yes\nbase_sha=" + testBaseSHA + "\n"},
		{"no git dir", "nonce=" + nonce + "\ngit_dir=absent\nhead_detached=yes\nbase_sha=" + testBaseSHA + "\n"},
		{"symbolic head", "nonce=" + nonce + "\ngit_dir=present\nhead_detached=no\nbase_sha=" + testBaseSHA + "\n"},
		{"abbreviated sha", "nonce=" + nonce + "\ngit_dir=present\nhead_detached=yes\nbase_sha=" + testBaseSHA[:12] + "\n"},
		{"uppercase sha", "nonce=" + nonce + "\ngit_dir=present\nhead_detached=yes\nbase_sha=" + strings.ToUpper(testBaseSHA) + "\n"},
		{"ref-shaped sha", "nonce=" + nonce + "\ngit_dir=present\nhead_detached=yes\nbase_sha=ref: refs/heads/main\n"},
		{"unknown key", string(good) + "workspace_write=succeeded\n"},
		{"repeated key", string(good) + "base_sha=" + strings.Repeat("c", 40) + "\n"},
		{"repeated fixed key", string(good) + "git_dir=present\n"},
		{"not key=value", string(good) + "garbage\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sha, err := verifyBaseProof([]byte(tc.proof), nonce)
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
	_, err := verifyBaseProof([]byte("nonce="+nonce+"\ngit_dir="+planted+"\nhead_detached=yes\nbase_sha="+testBaseSHA+"\n"), nonce)
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
	got, err := verifySeedSource(cfg, dir)
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
			got, err := verifySeedSource(cfg, dir)
			wantCheckFailure(t, err, CheckWorkspaceSeeding)
			if got != "" {
				t.Errorf("rejected source still yielded a path %q", got)
			}
		})
	}
}
