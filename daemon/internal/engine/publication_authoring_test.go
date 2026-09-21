package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/inference"
)

// TestProductionAuthoringCheckpointKeyBindsCandidate proves the authoring key
// binds the run, head, and base, so a new head or a base advance selects a
// distinct key. That is what makes the next clean review get its own author run
// and never render an artifact written for a different candidate.
func TestProductionAuthoringCheckpointKeyBindsCandidate(t *testing.T) {
	base := productionAuthoringCheckpointKey("run-1", "head-a", "base-a")
	for name, key := range map[string]string{
		"same":          productionAuthoringCheckpointKey("run-1", "head-a", "base-a"),
		"new head":      productionAuthoringCheckpointKey("run-1", "head-b", "base-a"),
		"base advance":  productionAuthoringCheckpointKey("run-1", "head-a", "base-b"),
		"different run": productionAuthoringCheckpointKey("run-2", "head-a", "base-a"),
	} {
		switch name {
		case "same":
			if key != base {
				t.Fatalf("same candidate produced different keys: %q vs %q", key, base)
			}
		default:
			if key == base {
				t.Fatalf("%s did not change the authoring key", name)
			}
		}
	}
}

// TestValidateAuthoringCheckpoint proves the reconstruction boundary fails
// closed: only a checkpoint whose identity matches the expected candidate and
// whose fallback/artifact state is internally consistent is accepted.
func TestValidateAuthoringCheckpoint(t *testing.T) {
	want := productionAuthoringCheckpoint{
		Version: productionAuthoringCheckpointVersion, RunID: "run-1",
		HeadSHA: "head-a", BaseSHA: "base-a",
	}
	valid := func(mut func(*productionAuthoringCheckpoint)) productionAuthoringCheckpoint {
		c := want
		mut(&c)
		return c
	}
	accepted := map[string]productionAuthoringCheckpoint{
		"fallback": valid(func(c *productionAuthoringCheckpoint) { c.Fallback = true; c.Reason = "unavailable" }),
		"success":  valid(func(c *productionAuthoringCheckpoint) { c.ArtifactDigest = "digest-1" }),
	}
	for name, got := range accepted {
		if err := validateAuthoringCheckpoint(got, want); err != nil {
			t.Fatalf("%s should validate: %v", name, err)
		}
	}
	rejected := map[string]productionAuthoringCheckpoint{
		"wrong version":     valid(func(c *productionAuthoringCheckpoint) { c.Version = "9"; c.Fallback = true }),
		"wrong run":         valid(func(c *productionAuthoringCheckpoint) { c.RunID = "run-2"; c.Fallback = true }),
		"wrong head":        valid(func(c *productionAuthoringCheckpoint) { c.HeadSHA = "head-b"; c.Fallback = true }),
		"wrong base":        valid(func(c *productionAuthoringCheckpoint) { c.BaseSHA = "base-b"; c.Fallback = true }),
		"fallback + digest": valid(func(c *productionAuthoringCheckpoint) { c.Fallback = true; c.ArtifactDigest = "digest-1" }),
		"neither":           valid(func(c *productionAuthoringCheckpoint) {}),
	}
	for name, got := range rejected {
		err := validateAuthoringCheckpoint(got, want)
		if err == nil {
			t.Fatalf("%s should fail closed", name)
		}
		if !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Fatalf("%s: err = %v, want ErrParentKeyMismatch", name, err)
		}
	}
}

// TestValidAuthoredPublicationTitle proves the authored-title contract matches
// the single-line contract literal and v1 titles hold: non-empty, trimmed, and
// free of embedded carriage returns or newlines.
func TestValidAuthoredPublicationTitle(t *testing.T) {
	valid := []string{"Authored PR title", "Fix the pane focus race"}
	for _, title := range valid {
		if !validAuthoredPublicationTitle(title) {
			t.Fatalf("%q should be a valid title", title)
		}
	}
	invalid := map[string]string{
		"empty":        "",
		"leading ws":   " Padded title",
		"trailing ws":  "Padded title ",
		"newline":      "Title\nsecond line",
		"carriage ret": "Title\rmore",
		"only ws":      "   ",
	}
	for name, title := range invalid {
		if validAuthoredPublicationTitle(title) {
			t.Fatalf("%s (%q) should be rejected", name, title)
		}
	}
}

// TestReadArtifactTextVerifiesDigest proves the instruction-snapshot read fails
// closed when the store returns bytes that do not hash to the requested digest,
// so corrupted or incorrectly restored control-plane text cannot be substituted.
func TestReadArtifactTextVerifiesDigest(t *testing.T) {
	body := []byte("trusted instruction snapshot")
	w := &productionPublicationWorkflow{artifacts: remediationArtifactStore{body: body}}

	if got, err := w.readArtifactText(""); err != nil || got != "" {
		t.Fatalf("empty digest: got %q err %v", got, err)
	}
	if got, err := w.readArtifactText(domain.Digest(contentaddr.Sum(body))); err != nil || got != string(body) {
		t.Fatalf("matching digest: got %q err %v", got, err)
	}
	// The store returns `body`, which does not hash to this requested digest.
	_, err := w.readArtifactText(domain.Digest(contentaddr.Sum([]byte("different"))))
	if err == nil || !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Fatalf("mismatch: err = %v, want ErrParentKeyMismatch", err)
	}
}

func TestRepositoryVisibilityForClass(t *testing.T) {
	for class, want := range map[domain.SensitivityClass]inference.RepositoryVisibility{
		domain.SensitivityNormal:    inference.RepositoryPublic,
		domain.SensitivitySensitive: inference.RepositoryPrivate,
		domain.SensitivityHigh:      inference.RepositoryPrivate,
	} {
		if got := repositoryVisibilityForClass(class); got != want {
			t.Fatalf("class %q -> %q, want %q", class, got, want)
		}
	}
}

func TestTrustedControlFileIsDigestConsistent(t *testing.T) {
	for _, content := range []string{"", "instruction body", "pr template\nwith lines"} {
		cf := trustedControlFile(content, "base-sha")
		if cf.Content != content {
			t.Fatalf("content = %q", cf.Content)
		}
		if cf.TrustedBaseCommit != "base-sha" {
			t.Fatalf("trusted base = %q", cf.TrustedBaseCommit)
		}
		if cf.Digest != contentaddr.Sum([]byte(content)) {
			t.Fatalf("digest is not consistent with content %q", content)
		}
	}
}

func TestBoundValidText(t *testing.T) {
	if got := boundValidText("short", 16); got != "short" {
		t.Fatalf("in-budget text changed: %q", got)
	}
	if got := boundValidText("abcdefghij", 4); got != "abcd" {
		t.Fatalf("truncation = %q, want abcd", got)
	}
	// Invalid UTF-8 bytes are dropped, keeping the field valid UTF-8.
	if got := boundValidText("a\xffb", 16); got != "ab" {
		t.Fatalf("invalid UTF-8 not dropped: %q", got)
	}
	// A multibyte rune is not split at the cap.
	if got := boundValidText("é"+"éé", 3); got != "é" { // each é is 2 bytes
		t.Fatalf("truncation split a rune: %q", got)
	}
}

// TestReadPRTemplate proves the template is read from the base commit's objects,
// not a working tree. The production FetchBase checkout has no working-tree
// files, so the test removes the worktree copy before reading to confirm the
// read resolves the committed blob rather than the filesystem.
func TestReadPRTemplate(t *testing.T) {
	ctx := context.Background()
	work := t.TempDir()
	if got := readPRTemplate(ctx, work, "", "base"); got != "" {
		t.Fatalf("empty checkout dir returned %q", got)
	}

	repo := t.TempDir()
	runRemediationGit(t, repo, nil, "init", "-q")
	runRemediationGit(t, repo, nil, "commit", "-q", "--allow-empty", "-m", "empty")
	emptyBase := strings.TrimSpace(string(runRemediationGit(t, repo, nil, "rev-parse", "HEAD")))
	if got := readPRTemplate(ctx, work, repo, emptyBase); got != "" {
		t.Fatalf("base without a template returned %q", got)
	}

	github := filepath.Join(repo, ".github")
	if err := os.MkdirAll(github, 0o750); err != nil {
		t.Fatal(err)
	}
	const body = "## Why\n\n## What\n"
	if err := os.WriteFile(filepath.Join(github, "PULL_REQUEST_TEMPLATE.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	runRemediationGit(t, repo, nil, "add", "-A")
	runRemediationGit(t, repo, nil, "commit", "-q", "-m", "add template")
	base := strings.TrimSpace(string(runRemediationGit(t, repo, nil, "rev-parse", "HEAD")))

	// Remove the working-tree copy: the production checkout has none, so a read
	// that still succeeds must resolve the base commit's blob.
	if err := os.Remove(filepath.Join(github, "PULL_REQUEST_TEMPLATE.md")); err != nil {
		t.Fatal(err)
	}
	if got := readPRTemplate(ctx, work, repo, base); got != body {
		t.Fatalf("template = %q, want %q", got, body)
	}
	// The earlier base still predates the template.
	if got := readPRTemplate(ctx, work, repo, emptyBase); got != "" {
		t.Fatalf("pre-template base returned %q", got)
	}

	// docs/ is a supported GitHub template location: a template stored only there
	// is resolved too.
	docsRepo := t.TempDir()
	runRemediationGit(t, docsRepo, nil, "init", "-q")
	if err := os.MkdirAll(filepath.Join(docsRepo, "docs"), 0o750); err != nil {
		t.Fatal(err)
	}
	const docsBody = "## Summary\n\n## Testing\n"
	if err := os.WriteFile(filepath.Join(docsRepo, "docs", "PULL_REQUEST_TEMPLATE.md"), []byte(docsBody), 0o600); err != nil {
		t.Fatal(err)
	}
	runRemediationGit(t, docsRepo, nil, "add", "-A")
	runRemediationGit(t, docsRepo, nil, "commit", "-q", "-m", "docs template")
	docsBase := strings.TrimSpace(string(runRemediationGit(t, docsRepo, nil, "rev-parse", "HEAD")))
	if got := readPRTemplate(ctx, work, docsRepo, docsBase); got != docsBody {
		t.Fatalf("docs/ template = %q, want %q", got, docsBody)
	}
}
