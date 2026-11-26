//go:build darwin && cgo

package cordon

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/commitplancheck"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/gitenv"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
)

// gitCommitPlanSeeder seeds a real git worktree with refs/freeside/import-base,
// a local writer commit past the base, and the test's .freeside-commit-plan.json.
type gitCommitPlanSeeder struct {
	t       *testing.T
	plan    string
	baseSHA *string
}

func (s gitCommitPlanSeeder) FetchBaseWorktree(ctx context.Context, _, _, _, dir string) error {
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	runGit := func(args ...string) string {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...) //nolint:gosec // G204: test helper runs git with test-controlled arguments
		cmd.Env = append(gitenv.Scrubbed(),
			"GIT_CONFIG_GLOBAL="+os.DevNull,
			"GIT_CONFIG_SYSTEM="+os.DevNull,
			"GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=test",
			"GIT_AUTHOR_EMAIL=test@example.invalid",
			"GIT_COMMITTER_NAME=test",
			"GIT_COMMITTER_EMAIL=test@example.invalid",
			"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
			"GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			s.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "--object-format=sha1", "-b", "main")
	runGit("config", "maintenance.auto", "false")
	runGit("config", "gc.auto", "0")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("old\n"), 0o600); err != nil {
		return err
	}
	runGit("add", "a.txt")
	runGit("commit", "-m", "base")
	base := runGit("rev-parse", "HEAD")
	if s.baseSHA != nil {
		*s.baseSHA = base
	}
	runGit("update-ref", commitplancheck.DefaultBaseRef, base)
	// Advance HEAD with a local commit so the validator must use import-base.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("new\n"), 0o600); err != nil {
		return err
	}
	runGit("add", "a.txt")
	runGit("commit", "-m", "writer local commit")
	return os.WriteFile(filepath.Join(dir, export.CommitPlanFilename), []byte(s.plan), 0o600)
}

// TestHostLaunchFindsCommitPlanValidatorOnPath runs a real cordoned launch
// whose command invokes `freeside-commit-plan validate` by bare name and reads
// its JSON output, proving the per-launch tools shim and PATH work inside the
// Seatbelt cordon for both accepted and rejected plans.
func TestHostLaunchFindsCommitPlanValidatorOnPath(t *testing.T) {
	t.Parallel()
	f := newHostFixture(t)
	// newHostFixture sets a minimal PATH (go plus system directories) rather
	// than LaunchPath; prepend the host git's resolved directory so the
	// confined validator runs the same git binary as gitCommitPlanSeeder
	// (such as Homebrew's git instead of /usr/bin/git's xcrun shim).
	if gitPath, err := exec.LookPath("git"); err == nil {
		if resolved, err := filepath.EvalSymlinks(gitPath); err == nil {
			gitDir := filepath.Dir(resolved)
			f.cfg.Path = append([]string{gitDir}, f.cfg.Path...)
		}
	}
	b := f.backend(t)

	t.Run("valid plan", func(t *testing.T) {
		var baseSHA string
		req := hostRequest("plan-valid", "", "exec", "freeside-commit-plan", "validate")
		req.Seeder = gitCommitPlanSeeder{
			t:       t,
			plan:    `{"version":"freeside.commit-plan/v1","groups":[{"name":"all","message":"Update a","remainder":true}]}`,
			baseSHA: &baseSHA,
		}
		got, err := b.Launch(t.Context(), req)
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}
		stdout, err := os.ReadFile(got.TranscriptPath)
		if err != nil {
			t.Fatal(err)
		}
		stderr, err := os.ReadFile(got.StderrPath)
		if err != nil {
			t.Fatal(err)
		}
		if got.EndedBy != EndedByExited || got.ExitCode != 0 {
			t.Fatalf("Launch ended %s exit %d\nstdout: %s\nstderr: %s", got.EndedBy, got.ExitCode, stdout, stderr)
		}
		var rep commitplancheck.Report
		if err := json.Unmarshal(stdout, &rep); err != nil {
			t.Fatalf("unmarshal report: %v\nstdout=%q\nstderr=%q", err, stdout, stderr)
		}
		if rep.Version != commitplancheck.ReportVersion || !rep.OK || rep.BaseSHA != baseSHA || len(rep.Problems) != 0 {
			t.Fatalf("report = %+v, want ok with base %s", rep, baseSHA)
		}
	})

	t.Run("rejected plan", func(t *testing.T) {
		var baseSHA string
		req := hostRequest("plan-invalid", "", "exec", "freeside-commit-plan", "validate")
		req.Seeder = gitCommitPlanSeeder{
			t:       t,
			plan:    `{"version":"freeside.commit-plan/v1","groups":[{"name":"bad","message":"Fixes #1","remainder":true}]}`,
			baseSHA: &baseSHA,
		}
		got, err := b.Launch(t.Context(), req)
		if err != nil {
			t.Fatalf("Launch: %v", err)
		}
		stdout, err := os.ReadFile(got.TranscriptPath)
		if err != nil {
			t.Fatal(err)
		}
		stderr, err := os.ReadFile(got.StderrPath)
		if err != nil {
			t.Fatal(err)
		}
		if got.EndedBy != EndedByExited || got.ExitCode != 1 {
			t.Fatalf("Launch ended %s exit %d, want exit 1\nstdout: %s\nstderr: %s", got.EndedBy, got.ExitCode, stdout, stderr)
		}
		var rep commitplancheck.Report
		if err := json.Unmarshal(stdout, &rep); err != nil {
			t.Fatalf("unmarshal report: %v\nstdout=%q\nstderr=%q", err, stdout, stderr)
		}
		if rep.OK || rep.BaseSHA != baseSHA || len(rep.Problems) != 1 ||
			rep.Problems[0].Code != importer.PlanProblemMessageCloseDirective || rep.Problems[0].Group != 1 {
			t.Fatalf("report = %+v, want github_close_directive on group 1", rep)
		}
	})
}
