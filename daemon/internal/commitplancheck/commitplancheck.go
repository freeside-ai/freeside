// Package commitplancheck implements the `freeside-commit-plan` command and
// `freesided commit-plan` verb: a convenience check that exports a writer's
// workspace into a temporary directory and runs the importer's own commit-plan
// check against the seeded base commit.
//
// This package grants no authority and is not on the Tier A list. The importer
// (daemon/internal/importer.Import) remains the sole authority at handoff and
// still verifies every commit plan as untrusted data.
package commitplancheck

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/gitrun"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/pathfold"
)

// DefaultBaseRef is the ref publish.Transport sets in every seeded checkout so
// the check still finds the base commit after the writer makes local commits.
const DefaultBaseRef = "refs/freeside/import-base"

// ReportVersion identifies the JSON output schema emitted by validate.
const ReportVersion = "freeside.commit-plan-check/v1"

// Report is the single JSON object printed to stdout by validate on exit 0 or 1.
type Report struct {
	Version  string                 `json:"version"`
	OK       bool                   `json:"ok"`
	BaseSHA  string                 `json:"base_sha"`
	Problems []importer.PlanProblem `json:"problems"`
}

// Run executes the commit-plan CLI with args (excluding argv[0]), writing one
// JSON Report to stdout on exit 0 (plan would be honored) or 1 (plan would not
// be honored), and one error line to stderr with nothing on stdout on exit 2
// (usage or environment error preventing the check from running).
//
// Run cancels on os.Interrupt and syscall.SIGTERM so deferred cleanup removes
// the temporary workspace export and git children exit before the process
// terminates, and restores default signal handling on the first signal so a
// second signal exits immediately.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	stopAfter := context.AfterFunc(ctx, stop)
	defer stopAfter()
	return runInDir(ctx, "", args, stdout, stderr)
}

func runInDir(ctx context.Context, dir string, args []string, stdout, stderr io.Writer) int {
	return runInDirWithExportFS(ctx, dir, args, stdout, stderr, nil)
}

func runInDirWithExportFS(
	ctx context.Context,
	dir string,
	args []string,
	stdout, stderr io.Writer,
	wrapExportFS func(fs.FS) fs.FS,
) int {
	failEnv := func(format string, a ...any) int {
		_, _ = fmt.Fprintf(stderr, "freeside-commit-plan: "+format+"\n", a...)
		return 2
	}
	if len(args) == 0 || args[0] != "validate" {
		return failEnv("usage: freeside-commit-plan validate [-base <rev>]")
	}
	flags := flag.NewFlagSet("freeside-commit-plan validate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	baseRef := flags.String("base", "", "base commit or ref to compare against (default "+DefaultBaseRef+")")
	if err := flags.Parse(args[1:]); err != nil {
		return failEnv("%v", err)
	}
	if flags.NArg() != 0 {
		return failEnv("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}

	workDir := dir
	if workDir == "" {
		var err error
		workDir, err = os.Getwd()
		if err != nil {
			return failEnv("determine working directory: %v", err)
		}
	}

	tmpDir, err := os.MkdirTemp("", "freeside-commit-plan-")
	if err != nil {
		return failEnv("create temporary directory: %v", err)
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()

	repoRoot, baseSHA, err := resolveRepoAndBase(ctx, workDir, *baseRef, filepath.Join(tmpDir, "git"))
	if err != nil {
		return failEnv("%v", err)
	}

	if err := ctx.Err(); err != nil {
		return failEnv("export workspace: %v", err)
	}
	srcFS := os.DirFS(repoRoot)
	if wrapExportFS != nil {
		srcFS = wrapExportFS(srcFS)
	}
	handoffDir := filepath.Join(tmpDir, "handoff")
	if _, err := export.Export(contextFS{ctx: ctx, base: srcFS}, handoffDir, export.DefaultOptions()); err != nil {
		return failEnv("export workspace: %v", err)
	}

	res, err := importer.CheckCommitPlan(ctx, handoffDir, repoRoot, importer.Options{
		BaseSHA: baseSHA,
		Policy: importer.Policy{
			CommitPlan: domain.CommitPlanPlanPreferred,
		},
	})
	if err != nil {
		return failEnv("check commit plan: %v", err)
	}
	if err := ctx.Err(); err != nil {
		return failEnv("check commit plan: %v", err)
	}

	problems := res.Problems
	if problems == nil {
		problems = []importer.PlanProblem{}
	}
	report := Report{
		Version:  ReportVersion,
		OK:       res.Honored,
		BaseSHA:  baseSHA,
		Problems: problems,
	}
	if err := json.NewEncoder(stdout).Encode(report); err != nil {
		return failEnv("write report: %v", err)
	}
	if !res.Honored {
		return 1
	}
	return 0
}

var errGitResolve = errors.New("git resolve failed")

func resolveRepoAndBase(ctx context.Context, workDir, baseRef, scratch string) (repoRoot, baseSHA string, err error) {
	r, err := gitrun.New(gitrun.Options{
		Scratch: scratch,
		Class:   errGitResolve,
	})
	if err != nil {
		return "", "", fmt.Errorf("initialize git runner: %w", err)
	}
	topOut, err := r.Run(ctx, nil, "-C", workDir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", "", fmt.Errorf("find workspace repository root: %w", err)
	}
	repoRoot = strings.TrimSpace(string(topOut))
	if repoRoot == "" {
		return "", "", errors.New("not inside a git worktree")
	}

	ref := baseRef
	usingDefault := false
	if ref == "" {
		ref = DefaultBaseRef
		usingDefault = true
	}
	baseOut, err := r.Run(ctx, nil, "-C", repoRoot, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		if usingDefault {
			return "", "", fmt.Errorf("resolve default base %s (pass -base <rev> to name another base): %w", DefaultBaseRef, err)
		}
		return "", "", fmt.Errorf("resolve base %q: %w", ref, err)
	}
	baseSHA = strings.TrimSpace(string(baseOut))
	if !pathfold.ValidSHA1Hex(baseSHA) {
		return "", "", fmt.Errorf("base %q resolved to unsupported commit id %q", ref, baseSHA)
	}
	return repoRoot, baseSHA, nil
}

// contextFS wraps a filesystem so export.Export aborts promptly when ctx is
// canceled, allowing runInDirWithExportFS's deferred removal of the exported
// workspace copy to run before exit.
type contextFS struct {
	ctx  context.Context
	base fs.FS
}

var (
	_ fs.ReadLinkFS  = contextFS{}
	_ fs.ReadDirFile = contextReadDirFile{}
)

func (c contextFS) Open(name string) (fs.File, error) {
	if err := c.ctx.Err(); err != nil {
		return nil, err
	}
	f, err := c.base.Open(name)
	if err != nil {
		return nil, err
	}
	if rd, ok := f.(fs.ReadDirFile); ok {
		return contextReadDirFile{ctx: c.ctx, f: rd}, nil
	}
	return contextFile{ctx: c.ctx, f: f}, nil
}

func (c contextFS) Lstat(name string) (fs.FileInfo, error) {
	if err := c.ctx.Err(); err != nil {
		return nil, err
	}
	return fs.Lstat(c.base, name)
}

func (c contextFS) ReadLink(name string) (string, error) {
	if err := c.ctx.Err(); err != nil {
		return "", err
	}
	return fs.ReadLink(c.base, name)
}

type contextFile struct {
	ctx context.Context
	f   fs.File
}

func (c contextFile) Stat() (fs.FileInfo, error) { return c.f.Stat() }
func (c contextFile) Close() error               { return c.f.Close() }

func (c contextFile) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.f.Read(p)
}

type contextReadDirFile struct {
	ctx context.Context
	f   fs.ReadDirFile
}

func (c contextReadDirFile) Stat() (fs.FileInfo, error) { return c.f.Stat() }
func (c contextReadDirFile) Close() error               { return c.f.Close() }

func (c contextReadDirFile) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.f.Read(p)
}

func (c contextReadDirFile) ReadDir(n int) ([]fs.DirEntry, error) {
	if err := c.ctx.Err(); err != nil {
		return nil, err
	}
	return c.f.ReadDir(n)
}
