package cordon

import (
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
	// EndedByLost means recovery ended a run no supervisor recorded an end
	// for: its supervisor died, or never started it. Recovery writes it only
	// once the run-end proof has passed, and a lost run is never exported.
	EndedByLost EndedBy = "lost"
)

// AllEndedBy lists every valid EndedBy; it is the single registration point.
var AllEndedBy = []EndedBy{EndedByExited, EndedByCanceled, EndedByStalled, EndedByLost}

func (e EndedBy) valid() bool {
	switch e {
	case EndedByExited, EndedByCanceled, EndedByStalled, EndedByLost:
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
	// Seeder, when set, seeds this launch in place of the backend's own. A
	// host launcher sets it to the tree the daemon sent over its
	// connection, since a host holds no forge credential to fetch with.
	Seeder Seeder
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
// started may still be alive. Err wraps ErrRunEndUnproven. EndedBy is
// empty when the supervisor died before it said how the launch ended.
type BlockedError struct {
	RunID   string
	EndedBy EndedBy
	Err     error
}

func (e *BlockedError) Error() string {
	if e.EndedBy == "" {
		return fmt.Sprintf("cordon: launch %s is blocked: %v", e.RunID, e.Err)
	}
	return fmt.Sprintf("cordon: launch %s is blocked after it %s: %v", e.RunID, e.EndedBy, e.Err)
}

func (e *BlockedError) Unwrap() error { return e.Err }

// runIDPattern keeps a RunID one plain path component.
var runIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func plainRunID(runID string) bool {
	return runIDPattern.MatchString(runID) && !strings.Contains(runID, "..")
}

// layout is a launch's directories. Every path follows from RunsRoot and
// RunID alone, so a restarted daemon can rebuild it (#3468).
type layout struct {
	// runRoot is RUN_ROOT, the only place the rules reopen: the worktree,
	// the CLI's config directory, TMPDIR, and mise's state directory.
	runRoot, worktree, config, tmp, miseState string
	// ctl sits outside runRoot, under a protected root, so the launch can
	// neither read nor write it.
	ctl, rules, tag, control, transcript, stderr string
	// ended and exportRecord are the end and export records, and request,
	// claim, and cancel the supervisor's request, its claim, and the
	// cancellation marker (record.go).
	ended, exportRecord, request, claim, cancel string
	// prompt is the agent's stdin, and supervisorStderr the supervisor's
	// own stderr, where it says why it failed.
	prompt, supervisorStderr string
	// dir is the launch's directory; export is the export's output, beside
	// run/ and ctl/: outside RUN_ROOT, so no launch can write it.
	dir, export string
}

func layoutFor(runsRoot, runID string) layout {
	dir := filepath.Join(runsRoot, runID)
	runRoot, ctl := filepath.Join(dir, "run"), filepath.Join(dir, "ctl")
	return layout{
		runRoot: runRoot, worktree: filepath.Join(runRoot, "worktree"),
		config: filepath.Join(runRoot, "config"), tmp: filepath.Join(runRoot, "tmp"),
		miseState: filepath.Join(runRoot, "mise-state"), ctl: ctl,
		rules: filepath.Join(ctl, "rules.sb"), tag: filepath.Join(ctl, "tag"),
		control: filepath.Join(ctl, "control"), transcript: filepath.Join(ctl, "transcript"),
		stderr: filepath.Join(ctl, "stderr"), ended: filepath.Join(ctl, "ended.json"),
		exportRecord: filepath.Join(ctl, "export.json"), request: filepath.Join(ctl, "request.json"),
		claim: filepath.Join(ctl, "supervisor.json"), cancel: filepath.Join(ctl, "cancel.json"),
		prompt: filepath.Join(ctl, "prompt"), supervisorStderr: filepath.Join(ctl, "supervisor.stderr"),
		dir: dir, export: filepath.Join(dir, "export"),
	}
}

// launchEnv is the launch's whole environment. Nothing is inherited from
// the daemon. The four DISABLE variables are the ones the ward sets for the
// CLI (internal/exec/claude/spec.go).
//
// MISE_STATE_DIR is where mise keeps trusted-configs and tracked-configs. A
// plain mise install trusts the project's config by itself and records it
// there, so the trust names the run's worktree and is removed with the
// run's directory instead of staying in the owner's home, where a later
// unconfined mise would read the config as trusted (#3601). mise's install
// and cache directories stay the owner's, so a tool one run installs stays
// shared with later runs.
func launchEnv(cfg Config, l layout) []string {
	return []string{
		"HOME=" + cfg.Home,
		"PATH=" + strings.Join(cfg.Path, ":"),
		"TMPDIR=" + l.tmp,
		"CLAUDE_CONFIG_DIR=" + l.config,
		"MISE_STATE_DIR=" + l.miseState,
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
// its starter holds open, so it stays alive until the proof kills it and
// exits by itself if its starter dies. Recovery starts a fresh one under the
// run's rule file, which reads as a member just the same.
const canaryProgram = "/bin/cat"

func confinedArgv(cfg Config, l layout, program string, args ...string) []string {
	argv := append([]string{sandboxExec, "-f", l.rules}, sandboxParams(cfg, l.runRoot, l.tag)...)
	return append(append(argv, program), args...)
}

// cliSettings is the agent CLI's one settings object. It turns the CLI's
// own confinement mode off: Seatbelt can't be applied twice, so with it on
// every Bash call fails before it starts (docs/spikes/cordoned-writer.md,
// the bashsandbox step); the cordon is the CLI's confinement. It also
// carries the warded writer's settings (attributionOffSettings in
// internal/exec/claude/spec.go): commit and PR attribution empty, so the
// CLI doesn't ask for a Co-Authored-By trailer that commit plans refuse,
// and the bundled agents-md plugin off, so safe mode can't load repository
// instructions through it. The CLI takes one --settings, so Launch places
// this before the caller's arguments and refuses a caller argument that
// would set the CLI's settings again.
var cliSettings = []string{
	"--settings",
	`{"sandbox":{"enabled":false},"attribution":{"commit":"","pr":""},"enabledPlugins":{"agents-md@builtin":false}}`,
}

// agentArgs is the agent CLI's whole argument list for a launch.
func agentArgs(args []string) []string {
	return append(slices.Clone(cliSettings), args...)
}

// Launch runs one writer launch and returns once no process from it is
// alive. It seeds the worktree and starts the launch's supervisor
// (supervisor.go), which starts the canary and then the agent under the
// rule file, watches the agent's output, and ends the launch when the agent
// exits, the launch is canceled, or the output stalls. Every ending runs the
// run-end proof; a launch whose end can't be proven returns a *BlockedError
// and no result. A launch that returns a result has its end record
// (record.go), which is what Export requires.
//
// The supervisor outlives the daemon, so a daemon that exits mid-launch
// loses nothing: Recover adopts the run. Canceling ctx requests the run's
// cancellation and keeps waiting for its end; Detach returns at once and
// leaves it running.
//
// Nothing in the worktree is read, run, or inspected by the daemon once the
// agent starts; the seed runs before it.
func (b *Backend) Launch(ctx context.Context, req LaunchRequest) (LaunchResult, error) {
	if err := req.validate(); err != nil {
		return LaunchResult{}, err
	}
	// Whatever RequireProof says: an approval the launching program holds
	// reaches the run whether or not a pass declared the class.
	if err := b.approvalRefusal(); err != nil {
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
	l, err := b.claimLaunch(req.RunID)
	if err != nil {
		return LaunchResult{}, err
	}
	if err := b.seed(ctx, req, l); err != nil {
		return LaunchResult{}, err
	}
	if err := writeExclusive(l.prompt, req.Prompt); err != nil {
		return LaunchResult{}, err
	}
	token := req.Token.Reveal()
	defer clear(token)
	return b.supervise(ctx, l, supervisorRequest{
		RunID: req.RunID, Repository: req.Repository, BaseRef: req.BaseRef, BaseSHA: req.BaseSHA,
		Program: supervisedProcess{
			Argv: confinedArgv(b.cfg, l, b.cfg.AgentCLI, agentArgs(req.Args)...),
			Dir:  l.worktree,
			Env:  agentEnv(b.cfg, l),
		},
		Prompt: true, Secret: true,
	}, token)
}

func (r LaunchRequest) validate() error {
	if !plainRunID(r.RunID) {
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

// claimLaunch refuses before claiming anything when the backend is
// detached or no proof could follow the launch, then claims runID's
// directory (prepare).
func (b *Backend) claimLaunch(runID string) (layout, error) {
	if err := b.attached(); err != nil {
		return layout{}, err
	}
	if _, err := b.newHost(); err != nil {
		return layout{}, err
	}
	l := layoutFor(b.cfg.RunsRoot, runID)
	return l, b.prepare(l)
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
	for _, dir := range []string{l.runRoot, l.config, l.tmp, l.miseState, l.ctl} {
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
	seeder := b.seeder
	if req.Seeder != nil {
		seeder = req.Seeder
	}
	if err := seeder.FetchBaseWorktree(ctx, req.Repository, req.BaseRef, req.BaseSHA, l.worktree); err != nil {
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

// limits are a launch's three durations, from Config by way of the
// supervisor's request.
type limits struct {
	stall, endGrace, endProof time.Duration
}

// watch waits for the agent to exit, ending it first if ctx is canceled or
// its output is silent for the stall limit. It returns once the agent
// itself has exited or, if it outlasts SIGKILL, once the proof's timeout
// passes; the proof deals with everything else.
func watch(ctx context.Context, lim limits, agent process, activity *activity) (EndedBy, int, error) {
	exited := make(chan waitResult, 1)
	go func() {
		code, err := agent.wait()
		exited <- waitResult{code, err}
	}()
	stall := time.NewTimer(lim.stall)
	defer stall.Stop()
	for {
		select {
		case r := <-exited:
			return EndedByExited, r.exitCode, r.err
		case <-ctx.Done():
			r := stop(lim, agent, exited)
			return EndedByCanceled, r.exitCode, r.err
		case <-stall.C:
			if idle := time.Since(activity.last()); idle < lim.stall {
				stall.Reset(lim.stall - idle)
				continue
			}
			r := stop(lim, agent, exited)
			return EndedByStalled, r.exitCode, r.err
		}
	}
}

// stop ends the agent's process group: SIGTERM, the end grace for it to
// exit, then SIGKILL. A descendant that left the group is the proof's.
func stop(lim limits, agent process, exited <-chan waitResult) waitResult {
	_ = agent.signalGroup(syscall.SIGTERM)
	select {
	case r := <-exited:
		return r
	case <-time.After(lim.endGrace):
	}
	_ = agent.signalGroup(syscall.SIGKILL)
	select {
	case r := <-exited:
		return r
	case <-time.After(lim.endProof):
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
