package cordon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/export"
)

// EndedBy names what ended a launch.
type EndedBy string

const (
	// EndedByExited means the agent CLI exited on its own.
	EndedByExited EndedBy = "exited"
	// EndedByCanceled means the launch's context was canceled.
	EndedByCanceled EndedBy = "canceled"
	// EndedByStalled means the agent wrote nothing for Config.StallLimit.
	EndedByStalled EndedBy = "stalled"
)

// AllEndedBy lists every valid EndedBy; it is the single registration point.
var AllEndedBy = []EndedBy{EndedByExited, EndedByCanceled, EndedByStalled}

func (e EndedBy) valid() bool {
	switch e {
	case EndedByExited, EndedByCanceled, EndedByStalled:
		return true
	default:
		return false
	}
}

// Seeder materializes the run's base checkout, working tree included, into
// dir, which must not exist yet. It has stage.Seeder's FetchBaseWorktree
// shape, so the daemon's transport seeder satisfies it, and is declared here
// so the stage driver can import this package without a cycle.
type Seeder interface {
	FetchBaseWorktree(ctx context.Context, repo, baseRef, baseSHA, dir string) error
}

// LaunchRequest is one writer launch.
type LaunchRequest struct {
	// RunID names the launch's directory under Config.RunsRoot. It is one
	// path component, and a launch refuses a RunID whose directory exists.
	RunID      string
	Repository string
	BaseRef    string
	BaseSHA    string
	// Prompt goes to the agent on stdin, so no file in the worktree holds
	// it.
	Prompt []byte
	// Args are the agent CLI's arguments.
	Args []string
}

// LaunchResult is how a launch ended, once no process from it is alive.
type LaunchResult struct {
	// ExitCode is the agent CLI's exit status, -1 when a signal ended it.
	ExitCode int
	EndedBy  EndedBy
	// TranscriptPath holds the agent's stdout alone, the stream the
	// export hands on as transcript evidence, as the ward's is.
	TranscriptPath string
	// StderrPath holds the agent's stderr.
	StderrPath string
}

// BlockedError is a launch whose end could not be proven: some process it
// started may still be alive. Err wraps ErrRunEndUnproven.
type BlockedError struct {
	RunID   string
	EndedBy EndedBy
	Err     error
}

func (e *BlockedError) Error() string {
	return fmt.Sprintf("cordon: launch %s is blocked after it %s: %v", e.RunID, e.EndedBy, e.Err)
}

func (e *BlockedError) Unwrap() error { return e.Err }

// runIDPattern keeps a RunID one plain path component.
var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// layout is a launch's directories. Every path follows from RunsRoot and
// RunID alone, so a restarted daemon can rebuild it (#3468).
type layout struct {
	// runRoot is RUN_ROOT, the only place the rules reopen: the worktree,
	// the CLI's config directory, and TMPDIR.
	runRoot, worktree, config, tmp string
	// ctl sits outside runRoot, under a protected root, so the launch can
	// neither read nor write it.
	ctl, rules, tag, control, transcript, stderr string
	// ended is the end record (record.go).
	ended string
}

func layoutFor(runsRoot, runID string) layout {
	dir := filepath.Join(runsRoot, runID)
	runRoot, ctl := filepath.Join(dir, "run"), filepath.Join(dir, "ctl")
	return layout{
		runRoot: runRoot, worktree: filepath.Join(runRoot, "worktree"),
		config: filepath.Join(runRoot, "config"), tmp: filepath.Join(runRoot, "tmp"),
		ctl: ctl, rules: filepath.Join(ctl, "rules.sb"), tag: filepath.Join(ctl, "tag"),
		control: filepath.Join(ctl, "control"), transcript: filepath.Join(ctl, "transcript"),
		stderr: filepath.Join(ctl, "stderr"), ended: filepath.Join(ctl, "ended.json"),
	}
}

// launchEnv is the launch's whole environment. Nothing is inherited from
// the daemon. The four DISABLE variables are the ones the ward sets for the
// CLI (internal/exec/claude/spec.go).
func launchEnv(cfg Config, l layout) []string {
	return []string{
		"HOME=" + cfg.Home,
		"PATH=" + strings.Join(cfg.Path, ":"),
		"TMPDIR=" + l.tmp,
		"CLAUDE_CONFIG_DIR=" + l.config,
		"DISABLE_AUTOUPDATER=1",
		"DISABLE_TELEMETRY=1",
		"DISABLE_ERROR_REPORTING=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}
}

// sandboxExec is the one program a launch starts directly.
const sandboxExec = "/usr/bin/sandbox-exec"

// canaryProgram is the proof's positive control: it reads its stdin, which
// the backend holds open, so it stays alive until the proof kills it and
// exits by itself if the daemon dies.
const canaryProgram = "/bin/cat"

func confinedArgv(cfg Config, l layout, program string, args ...string) []string {
	argv := append([]string{sandboxExec, "-f", l.rules}, sandboxParams(cfg, l.runRoot, l.tag)...)
	return append(append(argv, program), args...)
}

// Launch runs one writer launch and returns once no process from it is
// alive. It seeds the worktree, starts the canary and then the agent under
// the rule file, watches the agent's output, and ends the launch when the
// agent exits, ctx is canceled, or the output stalls. Every ending runs the
// run-end proof; a launch whose end can't be proven returns a *BlockedError
// and no result. A launch that returns a result has written its end record
// (record.go) first, which is what Export requires.
//
// Nothing in the worktree is read, run, or inspected by the daemon once the
// agent starts; the seed runs before it.
func (b *Backend) Launch(ctx context.Context, req LaunchRequest) (LaunchResult, error) {
	if err := req.validate(); err != nil {
		return LaunchResult{}, err
	}
	// Refuse before starting anything a proof couldn't follow.
	host, err := b.newHost()
	if err != nil {
		return LaunchResult{}, err
	}
	l := layoutFor(b.cfg.RunsRoot, req.RunID)
	if err := b.prepare(ctx, req, l); err != nil {
		return LaunchResult{}, err
	}
	transcript, err := os.OpenFile(l.transcript, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("cordon: create transcript: %w", err)
	}
	defer func() { _ = transcript.Close() }()
	stderr, err := os.OpenFile(l.stderr, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("cordon: create stderr: %w", err)
	}
	defer func() { _ = stderr.Close() }()

	target := proofTarget{tag: l.tag, control: l.control}
	canaryHold, canaryInput := io.Pipe()
	canary, err := b.start(processSpec{
		argv: confinedArgv(b.cfg, l, canaryProgram), dir: l.runRoot, env: launchEnv(b.cfg, l), stdin: canaryHold,
	})
	if err != nil {
		return LaunchResult{}, fmt.Errorf("cordon: start the canary: %w", err)
	}
	target.canary = canary.pid()
	defer func() {
		// A backstop: the proof has already killed the canary, unless it
		// failed before it got that far.
		_ = canary.signalGroup(syscall.SIGKILL)
		_ = canaryInput.Close()
		_, _ = canary.wait()
		canary.closeIO(0)
	}()
	if err := awaitMember(ctx, host, target, b.cfg.EndProofTimeout); err != nil {
		return LaunchResult{}, err
	}

	activity := newActivity()
	agent, err := b.start(processSpec{
		argv:   confinedArgv(b.cfg, l, b.cfg.AgentCLI, req.Args...),
		dir:    l.worktree,
		env:    launchEnv(b.cfg, l),
		stdin:  strings.NewReader(string(req.Prompt)),
		stdout: activity.to(transcript),
		stderr: activity.to(stderr),
	})
	if err != nil {
		if proofErr := endLaunch(context.WithoutCancel(ctx), host, target, b.cfg.EndProofTimeout); proofErr != nil {
			return LaunchResult{}, &BlockedError{RunID: req.RunID, EndedBy: EndedByExited, Err: proofErr}
		}
		return LaunchResult{}, fmt.Errorf("cordon: start the agent: %w", err)
	}

	endedBy, exitCode, waitErr := b.watch(ctx, agent, activity)
	// The CLI's exit doesn't end its children (#3463), so every ending
	// runs the proof, and runs it even though ctx may be canceled.
	if err := endLaunch(context.WithoutCancel(ctx), host, target, b.cfg.EndProofTimeout); err != nil {
		return LaunchResult{}, &BlockedError{RunID: req.RunID, EndedBy: endedBy, Err: err}
	}
	// Every writer is dead, so the output reaches EOF at once.
	agent.closeIO(b.cfg.EndGrace)
	if waitErr != nil {
		return LaunchResult{}, fmt.Errorf("cordon: wait for the agent: %w", waitErr)
	}
	if err := activity.err(); err != nil {
		return LaunchResult{}, fmt.Errorf("cordon: write the agent's output: %w", err)
	}
	if err := errors.Join(transcript.Sync(), stderr.Sync()); err != nil {
		return LaunchResult{}, fmt.Errorf("cordon: sync the agent's output: %w", err)
	}
	if err := writeEndRecord(l, endRecord{
		RunID: req.RunID, Repository: req.Repository, BaseRef: req.BaseRef, BaseSHA: req.BaseSHA,
		EndedBy: endedBy, ExitCode: exitCode,
	}); err != nil {
		return LaunchResult{}, err
	}
	return LaunchResult{ExitCode: exitCode, EndedBy: endedBy, TranscriptPath: l.transcript, StderrPath: l.stderr}, nil
}

func (r LaunchRequest) validate() error {
	if !runIDPattern.MatchString(r.RunID) || strings.Contains(r.RunID, "..") {
		return fmt.Errorf("cordon: run id %q must be one plain path component", r.RunID)
	}
	if r.Repository == "" || r.BaseRef == "" || r.BaseSHA == "" {
		return errors.New("cordon: a launch needs its repository, base ref, and base commit")
	}
	return nil
}

// prepare claims the launch's directory, writes the rule file and the
// proof's two files, seeds the worktree, and gives it the evidence
// directory the agent stages evidence in. The tag and control exist
// from before anything starts until the proof ends, because sandbox_check
// reads a missing path as denied.
func (b *Backend) prepare(ctx context.Context, req LaunchRequest, l layout) error {
	if err := os.MkdirAll(b.cfg.RunsRoot, 0o700); err != nil {
		return fmt.Errorf("cordon: create runs root: %w", err)
	}
	if err := os.Mkdir(filepath.Dir(l.runRoot), 0o700); err != nil {
		return fmt.Errorf("cordon: claim launch directory: %w", err)
	}
	for _, dir := range []string{l.runRoot, l.config, l.tmp, l.ctl} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			return fmt.Errorf("cordon: create %s: %w", dir, err)
		}
	}
	files := []struct {
		path string
		body []byte
	}{{l.rules, b.rules}, {l.tag, nil}, {l.control, nil}}
	for _, f := range files {
		if err := writeExclusive(f.path, f.body); err != nil {
			return err
		}
	}
	if err := b.seeder.FetchBaseWorktree(ctx, req.Repository, req.BaseRef, req.BaseSHA, l.worktree); err != nil {
		return fmt.Errorf("cordon: seed worktree: %w", err)
	}
	if err := os.Mkdir(filepath.Join(l.worktree, export.EvidenceWorkspaceDir), 0o700); err != nil {
		return fmt.Errorf("cordon: create the evidence directory: %w", err)
	}
	return excludeReservedPaths(l.worktree)
}

// excludeReservedPaths appends Freeside's reserved workspace paths to the
// seeded checkout's own exclude file, as the ward's launcher does
// (internal/exec/claude/spec.go), so a tool that honors git excludes skips
// what the agent stages there. It runs before the agent starts, while the
// tree holds only the seed, and only when .git is a real directory.
func excludeReservedPaths(worktree string) error {
	gitDir := filepath.Join(worktree, ".git")
	info, err := os.Lstat(gitDir)
	if errors.Is(err, os.ErrNotExist) || (err == nil && !info.IsDir()) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cordon: inspect the seeded .git: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(gitDir, "info"), 0o700); err != nil {
		return fmt.Errorf("cordon: create .git/info: %w", err)
	}
	exclude := filepath.Join(gitDir, "info", "exclude")
	f, err := os.OpenFile(exclude, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600) //nolint:gosec // G304: the daemon's own seed
	if err != nil {
		return fmt.Errorf("cordon: open %s: %w", exclude, err)
	}
	_, err = fmt.Fprintf(f, "\n/%s/\n/%s\n", export.EvidenceWorkspaceDir, export.CommitPlanFilename)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("cordon: write %s: %w", exclude, err)
	}
	return nil
}

func writeExclusive(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: a path in the launch's own control directory
	if err != nil {
		return fmt.Errorf("cordon: create %s: %w", path, err)
	}
	_, err = f.Write(body)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("cordon: write %s: %w", path, err)
	}
	return nil
}

// awaitMember waits for the canary to read as a member. sandbox-exec
// applies the rules only once it runs, so a just-started process isn't one
// yet. A canary that never reads as one means the proof can't find this
// launch's processes, so the agent never starts.
func awaitMember(ctx context.Context, host hostProcesses, target proofTarget, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		isMember, err := host.member(target.canary, target)
		if err != nil {
			return fmt.Errorf("%w: query the canary %d: %w", ErrRunEndUnproven, target.canary, err)
		}
		if isMember {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%w: the canary %d never read as a member: %w", ErrRunEndUnproven, target.canary, ctx.Err())
		case <-time.After(proofPollInterval):
		}
	}
}

type waitResult struct {
	exitCode int
	err      error
}

// watch waits for the agent to exit, ending it first if ctx is canceled or
// its output is silent for StallLimit. It returns once the agent itself has
// exited or, if it outlasts SIGKILL, once EndProofTimeout passes; the
// proof deals with everything else.
func (b *Backend) watch(ctx context.Context, agent process, activity *activity) (EndedBy, int, error) {
	exited := make(chan waitResult, 1)
	go func() {
		code, err := agent.wait()
		exited <- waitResult{code, err}
	}()
	stall := time.NewTimer(b.cfg.StallLimit)
	defer stall.Stop()
	for {
		select {
		case r := <-exited:
			return EndedByExited, r.exitCode, r.err
		case <-ctx.Done():
			r := b.stop(agent, exited)
			return EndedByCanceled, r.exitCode, r.err
		case <-stall.C:
			if idle := time.Since(activity.last()); idle < b.cfg.StallLimit {
				stall.Reset(b.cfg.StallLimit - idle)
				continue
			}
			r := b.stop(agent, exited)
			return EndedByStalled, r.exitCode, r.err
		}
	}
}

// stop ends the agent's process group: SIGTERM, EndGrace for it to exit,
// then SIGKILL. A descendant that left the group is the proof's.
func (b *Backend) stop(agent process, exited <-chan waitResult) waitResult {
	_ = agent.signalGroup(syscall.SIGTERM)
	select {
	case r := <-exited:
		return r
	case <-time.After(b.cfg.EndGrace):
	}
	_ = agent.signalGroup(syscall.SIGKILL)
	select {
	case r := <-exited:
		return r
	case <-time.After(b.cfg.EndProofTimeout):
		// The proof will fail to kill it too, and block the launch.
		return waitResult{exitCode: -1}
	}
}

// activity notes when the last byte arrived on either of the agent's output
// streams, which is the stall limit's heartbeat, and keeps the first error
// writing them to their files. Its streams keep accepting output after a
// write error, so a full disk never stops the agent reading; Launch reports
// the error once the launch has ended.
type activity struct {
	mu       sync.Mutex
	lastByte time.Time
	writeErr error
}

func newActivity() *activity {
	return &activity{lastByte: time.Now()}
}

// to returns a stream that copies to w and counts toward the heartbeat.
func (a *activity) to(w io.Writer) io.Writer { return activityStream{a: a, w: w} }

type activityStream struct {
	a *activity
	w io.Writer
}

func (s activityStream) Write(p []byte) (int, error) {
	s.a.mu.Lock()
	defer s.a.mu.Unlock()
	if len(p) > 0 {
		s.a.lastByte = time.Now()
	}
	if s.a.writeErr == nil {
		_, s.a.writeErr = s.w.Write(p)
	}
	return len(p), nil
}

func (a *activity) last() time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastByte
}

func (a *activity) err() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.writeErr
}
