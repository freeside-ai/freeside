package cordon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/export"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
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
	// Token is the enrolled identity's Claude setup token. It goes to the
	// agent alone, on its own pipe (tokenFD), and never into argv, an
	// environment, the launch's directory, or the canary; a launch without
	// one is refused.
	Token ward.SetupToken
	// RequireProof runs the attached suite's pre-job probe before anything
	// else and refuses the launch when no recorded pass declares the class
	// or the probe fails, which also withdraws the declaration. Every
	// unattended launch must set it.
	RequireProof bool
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
	// ended and exportRecord are the end and export records (record.go).
	ended, exportRecord string
	// export is the export's output, beside run/ and ctl/: outside
	// RUN_ROOT, so no launch can write it.
	export string
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
		exportRecord: filepath.Join(ctl, "export.json"), export: filepath.Join(dir, "export"),
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

// tokenFD is the descriptor the agent reads its token from: the first of
// exec.Cmd's ExtraFiles. The CLI reads the descriptor
// CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR names to end of file and keeps the
// token out of its tools' environment and descriptors (#3463), so the
// token stays off the prompt's stdin and out of every environment.
const tokenFD = 3

// agentEnv is the agent's environment: the launch's, plus the name of the
// descriptor its token arrives on.
func agentEnv(cfg Config, l layout) []string {
	return append(launchEnv(cfg, l), fmt.Sprintf("CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR=%d", tokenFD))
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

// cliSettings fixes the agent CLI's own confinement mode off. Seatbelt
// can't be applied twice, so with it on every Bash call fails before it
// starts (docs/spikes/cordoned-writer.md, the bashsandbox step); the cordon
// is the CLI's confinement. Launch places it before the caller's arguments
// and refuses a caller argument that would set the CLI's settings again.
var cliSettings = []string{"--settings", `{"sandbox":{"enabled":false}}`}

// agentArgs is the agent CLI's whole argument list for a launch.
func agentArgs(args []string) []string {
	return append(slices.Clone(cliSettings), args...)
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
	if req.RequireProof {
		suite := b.suite.Load()
		if suite == nil {
			return LaunchResult{}, failure("declaration", "no conformance suite is attached to the backend")
		}
		if err := suite.ProbeBeforeJob(ctx); err != nil {
			return LaunchResult{}, err
		}
	}
	ln, err := b.openLaunch(ctx, req.RunID, func(l layout) error {
		return b.seed(ctx, req, l)
	})
	if err != nil {
		return LaunchResult{}, err
	}
	defer ln.close()
	exitCode, endedBy, err := ln.run(ctx, processSpec{
		argv:   confinedArgv(b.cfg, ln.l, b.cfg.AgentCLI, agentArgs(req.Args)...),
		dir:    ln.l.worktree,
		env:    agentEnv(b.cfg, ln.l),
		stdin:  bytes.NewReader(req.Prompt),
		secret: req.Token.Reveal(),
	})
	if err != nil {
		return LaunchResult{}, err
	}
	// Only a writer's launch is exported, so only it records its end.
	if err := writeEndRecord(ln.l, endRecord{
		RunID: req.RunID, Repository: req.Repository, BaseRef: req.BaseRef, BaseSHA: req.BaseSHA,
		EndedBy: endedBy, ExitCode: exitCode,
	}); err != nil {
		return LaunchResult{}, err
	}
	return LaunchResult{ExitCode: exitCode, EndedBy: endedBy, TranscriptPath: ln.l.transcript, StderrPath: ln.l.stderr}, nil
}

func (r LaunchRequest) validate() error {
	if !runIDPattern.MatchString(r.RunID) || strings.Contains(r.RunID, "..") {
		return fmt.Errorf("cordon: run id %q must be one plain path component", r.RunID)
	}
	if r.Repository == "" || r.BaseRef == "" || r.BaseSHA == "" {
		return errors.New("cordon: a launch needs its repository, base ref, and base commit")
	}
	for _, arg := range r.Args {
		if arg == cliSettings[0] || strings.HasPrefix(arg, cliSettings[0]+"=") {
			return fmt.Errorf("cordon: a launch fixes the agent CLI's %s itself", cliSettings[0])
		}
	}
	token := r.Token.Reveal()
	defer clear(token)
	if len(token) == 0 {
		return errors.New("cordon: a launch needs the identity's setup token")
	}
	return nil
}

// launch is one launch between its canary joining and its run-end proof:
// its directory is prepared, its canary is a member, and it runs one
// program. Build it with openLaunch and always close it.
type launch struct {
	b                  *Backend
	runID              string
	l                  layout
	host               hostProcesses
	target             proofTarget
	transcript, stderr *os.File
	canary             process
	canaryInput        io.Closer
	ran                bool
}

// openLaunch claims runID's directory, writes the rule file and the proof's
// files, runs prepare on the layout (the seed, or a probe's staging),
// creates the output files, and starts the canary, returning once it reads
// as a member. A launch that fails here started nothing the proof would
// need to end but the canary, which close kills.
func (b *Backend) openLaunch(ctx context.Context, runID string, prepare func(layout) error) (_ *launch, err error) {
	// Refuse before starting anything a proof couldn't follow.
	host, err := b.newHost()
	if err != nil {
		return nil, err
	}
	l := layoutFor(b.cfg.RunsRoot, runID)
	if err := b.prepare(l); err != nil {
		return nil, err
	}
	if err := prepare(l); err != nil {
		return nil, err
	}
	ln := &launch{b: b, runID: runID, l: l, host: host, target: proofTarget{tag: l.tag, control: l.control}}
	defer func() {
		if err != nil {
			ln.close()
		}
	}()
	if ln.transcript, err = os.OpenFile(l.transcript, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
		return nil, fmt.Errorf("cordon: create transcript: %w", err)
	}
	if ln.stderr, err = os.OpenFile(l.stderr, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
		return nil, fmt.Errorf("cordon: create stderr: %w", err)
	}
	canaryHold, canaryInput := io.Pipe()
	ln.canaryInput = canaryInput
	if ln.canary, err = b.start(processSpec{
		argv: confinedArgv(b.cfg, l, canaryProgram), dir: l.runRoot, env: launchEnv(b.cfg, l), stdin: canaryHold,
	}); err != nil {
		return nil, fmt.Errorf("cordon: start the canary: %w", err)
	}
	ln.target.canary = ln.canary.pid()
	if err := awaitMember(ctx, ln.host, ln.target, b.cfg.EndProofTimeout); err != nil {
		return nil, err
	}
	return ln, nil
}

// run starts spec, which must already be confined (confinedArgv), with its
// output going to the launch's transcript and stderr files, watches it as
// Launch describes, and ends the launch with the run-end proof. A launch
// runs one program: the proof kills its canary. run clears spec.secret once
// the program holds it, so the daemon keeps no copy beyond its caller's.
func (ln *launch) run(ctx context.Context, spec processSpec) (int, EndedBy, error) {
	if ln.ran {
		return 0, "", errors.New("cordon: a launch runs one program")
	}
	ln.ran = true
	b := ln.b
	activity := newActivity()
	spec.stdout, spec.stderr = activity.to(ln.transcript), activity.to(ln.stderr)
	agent, err := b.start(spec)
	// The pipe holds the secret now, or nothing does.
	clear(spec.secret)
	if err != nil {
		if proofErr := endLaunch(context.WithoutCancel(ctx), ln.host, ln.target, b.cfg.EndProofTimeout); proofErr != nil {
			return 0, "", &BlockedError{RunID: ln.runID, EndedBy: EndedByExited, Err: proofErr}
		}
		return 0, "", fmt.Errorf("cordon: start the agent: %w", err)
	}

	endedBy, exitCode, waitErr := b.watch(ctx, agent, activity)
	// The CLI's exit doesn't end its children (#3463), so every ending
	// runs the proof, and runs it even though ctx may be canceled.
	if err := endLaunch(context.WithoutCancel(ctx), ln.host, ln.target, b.cfg.EndProofTimeout); err != nil {
		return 0, "", &BlockedError{RunID: ln.runID, EndedBy: endedBy, Err: err}
	}
	// Every writer is dead, so the output reaches EOF at once.
	agent.closeIO(b.cfg.EndGrace)
	if waitErr != nil {
		return 0, "", fmt.Errorf("cordon: wait for the agent: %w", waitErr)
	}
	if err := activity.err(); err != nil {
		return 0, "", fmt.Errorf("cordon: write the agent's output: %w", err)
	}
	if err := errors.Join(ln.transcript.Sync(), ln.stderr.Sync()); err != nil {
		return 0, "", fmt.Errorf("cordon: sync the agent's output: %w", err)
	}
	return exitCode, endedBy, nil
}

// close is a backstop: the proof has already killed the canary, unless it
// failed before it got that far.
func (ln *launch) close() {
	if ln.canary != nil {
		_ = ln.canary.signalGroup(syscall.SIGKILL)
		_ = ln.canaryInput.Close()
		_, _ = ln.canary.wait()
		ln.canary.closeIO(0)
	}
	if ln.transcript != nil {
		_ = ln.transcript.Close()
	}
	if ln.stderr != nil {
		_ = ln.stderr.Close()
	}
}

// prepare claims the launch's directory and writes the rule file and the
// proof's two files. The tag and control exist from before anything starts
// until the proof ends, because sandbox_check reads a missing path as
// denied.
func (b *Backend) prepare(l layout) error {
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
	return nil
}

// seed is a writer launch's preparation: it seeds the worktree and gives
// it the evidence directory the agent stages evidence in.
func (b *Backend) seed(ctx context.Context, req LaunchRequest, l layout) error {
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
