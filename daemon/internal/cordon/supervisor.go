package cordon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

// A launch's supervisor is `freesided cordon supervise <runs-root>
// <run-id>`: a process the daemon starts from its own executable, outside
// the rule file and in a session of its own, that runs one launch to its
// end. It is the canary's and the agent's parent, so it is the one process
// that learns the agent's exit status; it feeds the agent's streamed input
// (inbox.go), copies its output to the launch's files, watches the stall
// limit, runs the run-end proof, and writes the end record. None of that runs through the daemon, so the
// launch outlives it. The rule file denies a launch signaling a process
// that isn't a member and reaching ctl/, so the agent can neither kill its
// supervisor nor forge its records.

// superviseVerb is the supervisor's argv after the executable.
var superviseVerb = []string{"cordon", "supervise"}

// The supervisor's exit statuses. Only its parent learns them, so the end
// record, not the status, is how a launch ended; a status says only why a
// supervisor wrote none.
const (
	// supervisorDone: the end record is written, or recovery had claimed
	// the run, so the supervisor started nothing.
	supervisorDone = 0
	// supervisorFailed: it failed with nothing of the launch left alive.
	supervisorFailed = 1
	// supervisorUsage: its arguments are wrong; it read nothing.
	supervisorUsage = 2
	// supervisorUnproven: it could not start a canary the proof finds, or
	// could not query the host, so it started no program.
	supervisorUnproven = 3
	// supervisorBlocked + i: the proof failed after the launch ended by
	// AllEndedBy[i].
	supervisorBlocked = 16
)

// RunSupervisor is the supervise verb's entry point: args are the runs root
// and the run ID, and every path it reads or writes follows from them
// (layoutFor). It returns the process's exit status.
func RunSupervisor(args []string) int {
	// Before anything opens a file: a writer's token arrives on tokenFD,
	// which exec left open across exec, and nothing but the agent may hold
	// it.
	secret := inheritedSecret()
	if len(args) != 2 || !filepath.IsAbs(args[0]) || !plainRunID(args[1]) {
		if secret != nil {
			_ = secret.Close()
		}
		fmt.Fprintln(os.Stderr, "usage: freesided cordon supervise <runs-root> <run-id>")
		return supervisorUsage
	}
	// SIGTERM is how RequestCancellation reaches the supervisor.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	s := superviseEnv{l: layoutFor(args[0], args[1]), runID: args[1], start: startExec, secret: secret}
	host, err := newHostProcesses()
	if err == nil {
		s.host = host
		var found bool
		s.self, found, err = host.lookup(os.Getpid())
		if err == nil && !found {
			err = errors.New("the supervisor can't find itself")
		}
	}
	if err != nil {
		if secret != nil {
			_ = secret.Close()
		}
		fmt.Fprintf(os.Stderr, "cordon supervise %s: %v\n", args[1], err)
		return supervisorUnproven
	}
	return s.supervise(ctx, os.Stderr)
}

// inheritedSecret returns tokenFD when it is open as a pipe, marked
// close-on-exec so the canary never inherits it, and nil otherwise. It runs
// first, before anything this process opens could take descriptor 3.
func inheritedSecret() *os.File {
	var st unix.Stat_t
	if err := unix.Fstat(tokenFD, &st); err != nil || st.Mode&unix.S_IFMT != unix.S_IFIFO {
		return nil
	}
	syscall.CloseOnExec(tokenFD)
	return os.NewFile(tokenFD, "token")
}

// superviseEnv is what a supervisor runs with: its run, the host's
// processes, the process starter, its own identity for the claim, and the
// token's pipe when the daemon handed one. Tests run the body in process
// with their own.
type superviseEnv struct {
	l      layout
	runID  string
	host   hostProcesses
	start  func(processSpec) (process, error)
	self   hostProcess
	secret *os.File
}

// supervise runs the launch and writes why it failed, if it did, to
// stderr. It owns s.secret and closes it.
func (s superviseEnv) supervise(ctx context.Context, stderr io.Writer) int {
	defer func() {
		if s.secret != nil {
			_ = s.secret.Close()
		}
	}()
	status, err := s.run(ctx, stderr)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "cordon supervise %s: %v\n", s.runID, err)
	}
	return status
}

// run runs the launch. said is the supervisor's stderr, for what fails
// without changing how the launch ends.
func (s *superviseEnv) run(ctx context.Context, said io.Writer) (int, error) {
	var req supervisorRequest
	if err := readRecord(s.l.request, &req); err != nil {
		return supervisorFailed, err
	}
	if err := req.check(s.runID); err != nil {
		return supervisorFailed, err
	}
	// Only a writer gets the prompt and the token's pipe, and only a check
	// sends both its streams to the transcript. The pipe the supervisor
	// holds must be the one the kind takes, checked before the claim, so a
	// check handed a pipe starts nothing.
	var writer, oneStream bool
	switch req.Kind {
	case kindWriter:
		writer = true
	case kindCheck:
		oneStream = true
	case kindProbe:
	}
	switch {
	case writer && s.secret == nil:
		return supervisorFailed, fmt.Errorf("cordon: a %s launch needs the token's pipe, and the supervisor holds none", req.Kind)
	case !writer && s.secret != nil:
		return supervisorFailed, fmt.Errorf("cordon: a %s launch takes no token, but the supervisor holds a pipe on descriptor %d", req.Kind, tokenFD)
	}
	won, err := claimRun(s.l, s.runID, &claimedProcess{PID: s.self.pid, Start: s.self.start})
	if err != nil {
		return supervisorFailed, err
	}
	if !won {
		// Recovery owns the run: it found no claim and ended it lost.
		return supervisorDone, nil
	}
	lim := limits{stall: time.Duration(req.StallLimit), endGrace: time.Duration(req.EndGrace), endProof: time.Duration(req.EndProofTimeout)}
	end := endRecord{RunID: s.runID, Repository: req.Repository, BaseRef: req.BaseRef, BaseSHA: req.BaseSHA}
	transcript, err := os.OpenFile(s.l.transcript, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return supervisorFailed, fmt.Errorf("cordon: create transcript: %w", err)
	}
	defer func() { _ = transcript.Close() }()
	stderr, err := os.OpenFile(s.l.stderr, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return supervisorFailed, fmt.Errorf("cordon: create stderr: %w", err)
	}
	defer func() { _ = stderr.Close() }()
	target := proofTarget{tag: s.l.tag, control: s.l.control}
	c, err := startCanary(s.start, s.host, &target, processSpec{argv: req.Canary.Argv, dir: req.Canary.Dir, env: req.Canary.Env}, lim.endProof)
	if err != nil {
		return failedStatus(err), err
	}
	defer c.close()
	// The claim is written, so a cancellation from here on is in the
	// marker, in ctx, or both (RequestCancellation writes the marker before
	// it reads the claim).
	canceled, err := cancellationRequested(s.l, s.runID)
	if err != nil {
		return s.failWithoutProgram(ctx, target, lim, err)
	}
	if canceled || ctx.Err() != nil {
		end.EndedBy, end.ExitCode = EndedByCanceled, -1
		if err := s.endWithoutProgram(ctx, target, lim, EndedByCanceled, &end); err != nil {
			return failedStatus(err), err
		}
		return supervisorDone, nil
	}
	spec := processSpec{argv: req.Program.Argv, dir: req.Program.Dir, env: req.Program.Env, secret: s.secret}
	var input *inputFeed
	if writer {
		prompt, err := os.ReadFile(s.l.prompt)
		if err == nil && !utf8.Valid(prompt) {
			err = errors.New("it is not UTF-8")
		}
		if err != nil {
			return s.failWithoutProgram(ctx, target, lim, fmt.Errorf("cordon: read the prompt: %w", err))
		}
		input = newInputFeed(s.l, s.runID, prompt)
		spec.stdin = input.reader
	}
	activity := newActivity()
	// A check's stderr shares its stdout's pipe, so the transcript keeps
	// the order the command wrote in; ctl/stderr stays, empty, so a run's
	// files don't depend on its kind.
	spec.stdout, spec.stderr, spec.mergeStderr = activity.to(transcript), activity.to(stderr), oneStream
	if input != nil {
		// The turn-end watch sees every byte, whatever becomes of the
		// transcript's writes, which never fail (activityStream).
		spec.stdout = io.MultiWriter(input.turnEnds, spec.stdout)
	}
	agent, err := s.start(spec)
	// The program holds the pipe now, or nothing does.
	if s.secret != nil {
		_ = s.secret.Close()
		s.secret = nil
	}
	if err != nil {
		return s.failWithoutProgram(ctx, target, lim, fmt.Errorf("cordon: start the agent: %w", err))
	}
	if input != nil {
		input.start()
	}
	endedBy, exitCode, waitErr := watch(ctx, lim, agent, activity)
	if input != nil {
		if err := input.stop(); err != nil {
			_, _ = fmt.Fprintf(said, "cordon supervise %s: the agent's input: %v\n", s.runID, err)
		}
	}
	// The CLI's exit doesn't end its children (#3463), so every ending
	// runs the proof, and runs it even though ctx may be canceled.
	if err := endLaunch(context.WithoutCancel(ctx), s.host, target, lim.endProof); err != nil {
		return blockedStatus(endedBy), &BlockedError{RunID: s.runID, EndedBy: endedBy, Err: err}
	}
	// Every writer is dead, so the output reaches EOF at once.
	agent.closeIO(lim.endGrace)
	if waitErr != nil {
		return supervisorFailed, fmt.Errorf("cordon: wait for the agent: %w", waitErr)
	}
	if err := activity.err(); err != nil {
		return supervisorFailed, fmt.Errorf("cordon: write the agent's output: %w", err)
	}
	if err := errors.Join(transcript.Sync(), stderr.Sync()); err != nil {
		return supervisorFailed, fmt.Errorf("cordon: sync the agent's output: %w", err)
	}
	end.EndedBy, end.ExitCode = endedBy, exitCode
	if err := writeEndRecord(s.l, end); err != nil {
		return supervisorFailed, err
	}
	return supervisorDone, nil
}

// endWithoutProgram runs the proof for a launch whose program never
// started, so only the canary is a member, and writes end when it is set.
// A failed proof returns a *BlockedError.
func (s *superviseEnv) endWithoutProgram(ctx context.Context, target proofTarget, lim limits, endedBy EndedBy, end *endRecord) error {
	if err := endLaunch(context.WithoutCancel(ctx), s.host, target, lim.endProof); err != nil {
		return &BlockedError{RunID: s.runID, EndedBy: endedBy, Err: err}
	}
	if end == nil {
		return nil
	}
	return writeEndRecord(s.l, *end)
}

// failWithoutProgram ends a launch that failed with err before its program
// ran, with no end record. A proof that fails then blocks the launch, as
// any unproven end does, so its error decides the status.
func (s *superviseEnv) failWithoutProgram(ctx context.Context, target proofTarget, lim limits, err error) (int, error) {
	err = errors.Join(err, s.endWithoutProgram(ctx, target, lim, EndedByExited, nil))
	return failedStatus(err), err
}

// failedStatus is the exit status for err, which ended a launch with no end
// record.
func failedStatus(err error) int {
	var blocked *BlockedError
	switch {
	case errors.As(err, &blocked):
		return blockedStatus(blocked.EndedBy)
	case errors.Is(err, ErrRunEndUnproven):
		return supervisorUnproven
	default:
		return supervisorFailed
	}
}

func blockedStatus(endedBy EndedBy) int {
	for i, e := range AllEndedBy {
		if e == endedBy {
			return supervisorBlocked + i
		}
	}
	return supervisorBlocked + len(AllEndedBy)
}

// canary is a launch's positive control, running until the proof kills it.
type canary struct {
	p     process
	input io.Closer
}

// startCanary starts spec, which must be the canary under the launch's
// rule file, sets target.canary, and returns once it reads as a member. A
// canary that never reads as one means the proof can't find this launch's
// processes, so nothing else may start.
func startCanary(start func(processSpec) (process, error), host hostProcesses, target *proofTarget, spec processSpec, timeout time.Duration) (*canary, error) {
	hold, input := io.Pipe()
	spec.stdin = hold
	p, err := start(spec)
	if err != nil {
		_ = input.Close()
		return nil, fmt.Errorf("cordon: start the canary: %w", err)
	}
	c := &canary{p: p, input: input}
	target.canary = p.pid()
	if err := awaitMember(context.Background(), host, *target, timeout); err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}

// close is a backstop: the proof has already killed the canary, unless it
// failed before it got that far.
func (c *canary) close() {
	_ = c.p.signalGroup(syscall.SIGKILL)
	_ = c.input.Close()
	_, _ = c.p.wait()
	c.p.closeIO(0)
}

// supervisorSpec is how the daemon starts a supervisor.
type supervisorSpec struct {
	argv []string
	// stderr is the supervisor's ctl/supervisor.stderr, and secret the
	// token's pipe for tokenFD, or nil. The starter owns and closes both.
	stderr, secret *os.File
}

// supervisorProcess is a started supervisor. Tests substitute their own
// (Backend.startSupervisor).
type supervisorProcess interface {
	// wait blocks until the supervisor exits and returns its exit status,
	// -1 when a signal ended it.
	wait() int
}

type execSupervisor struct{ cmd *exec.Cmd }

// startSupervisorExec starts the supervisor in a session of its own, so the
// daemon's process group, which launchd ends with the daemon's job, doesn't
// hold it, with no environment from the daemon, stdin and stdout on
// /dev/null, and nothing that kills it when the daemon exits.
func startSupervisorExec(spec supervisorSpec) (supervisorProcess, error) {
	defer func() {
		_ = spec.stderr.Close()
		if spec.secret != nil {
			_ = spec.secret.Close()
		}
	}()
	cmd := exec.Command(spec.argv[0], spec.argv[1:]...) //nolint:gosec,noctx // G204: the daemon's own executable; no context, since nothing may end it with the daemon
	cmd.Dir = "/"
	cmd.Env = []string{}
	cmd.Stderr = spec.stderr
	if spec.secret != nil {
		cmd.ExtraFiles = []*os.File{spec.secret}
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return execSupervisor{cmd: cmd}, nil
}

func (s execSupervisor) wait() int {
	_ = s.cmd.Wait()
	return s.cmd.ProcessState.ExitCode()
}

// supervise writes req, completed with the canary and the configuration's
// limits, as l's supervisor request, starts the supervisor with secret on
// tokenFD when it is set, and waits for the launch's end (awaitSupervisor).
func (b *Backend) supervise(ctx context.Context, l layout, req supervisorRequest, secret []byte) (LaunchResult, error) {
	req.Version = requestVersion
	req.Canary = supervisedProcess{Argv: confinedArgv(b.cfg, l, canaryProgram), Dir: l.runRoot, Env: launchEnv(b.cfg, l)}
	req.StallLimit, req.EndGrace, req.EndProofTimeout = int64(b.cfg.StallLimit), int64(b.cfg.EndGrace), int64(b.cfg.EndProofTimeout)
	if err := writeRecord(l.request, req); err != nil {
		return LaunchResult{}, err
	}
	stderr, err := os.OpenFile(l.supervisorStderr, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("cordon: create the supervisor's stderr: %w", err)
	}
	spec := supervisorSpec{argv: append([]string{b.daemonPath}, append(superviseVerb, b.cfg.RunsRoot, req.RunID)...), stderr: stderr}
	if secret != nil {
		if spec.secret, err = secretPipe(secret); err != nil {
			_ = stderr.Close()
			return LaunchResult{}, err
		}
	}
	sup, err := b.startSupervisor(spec)
	if err != nil {
		return LaunchResult{}, fmt.Errorf("cordon: start the supervisor: %w", err)
	}
	exited := make(chan int, 1)
	go func() { exited <- sup.wait() }()
	status, err := b.awaitSupervisor(ctx, req.RunID, exited, nil, true)
	if err != nil {
		return LaunchResult{}, err
	}
	end, err := readEndRecord(l, req.RunID)
	if errors.Is(err, ErrNoEndRecord) {
		return LaunchResult{}, supervisorError(l, req.RunID, status)
	}
	if err != nil {
		return LaunchResult{}, err
	}
	return LaunchResult{ExitCode: end.ExitCode, EndedBy: end.EndedBy, TranscriptPath: l.transcript, StderrPath: l.stderr}, nil
}

// supervisorPollInterval paces recovery's checks that an adopted
// supervisor, which isn't its child, is still alive.
const supervisorPollInterval = 100 * time.Millisecond

// awaitSupervisor waits until the run's supervisor has exited: exited
// delivers its status when this process is its parent, and gone, polled
// otherwise, reports it gone with status -1. Canceling ctx requests the
// run's cancellation and keeps waiting when cancelRun is set, retrying a
// request that failed, since returning would leave the run alive, and
// returning the failure once the supervisor exits if no request took; it
// returns ctx's error otherwise. Detach returns ErrDetached at once. Only a
// requested cancellation signals anything.
func (b *Backend) awaitSupervisor(ctx context.Context, runID string, exited <-chan int, gone func() (bool, error), cancelRun bool) (int, error) {
	var tick <-chan time.Time
	if gone != nil {
		t := time.NewTicker(supervisorPollInterval)
		defer t.Stop()
		tick = t.C
	}
	done := ctx.Done()
	var retryCancel <-chan time.Time
	var cancelErr error
	requestCancellation := func() {
		retryCancel, cancelErr = nil, b.RequestCancellation(context.WithoutCancel(ctx), runID)
		if cancelErr != nil {
			retryCancel = time.After(supervisorPollInterval)
		}
	}
	for {
		select {
		case status := <-exited:
			if cancelErr != nil {
				return status, fmt.Errorf("cordon: request the cancellation of %s: %w", runID, cancelErr)
			}
			return status, nil
		case <-done:
			if !cancelRun {
				return -1, ctx.Err()
			}
			done = nil
			requestCancellation()
		case <-retryCancel:
			requestCancellation()
		case <-b.detached:
			return -1, fmt.Errorf("%w: %s keeps running", ErrDetached, runID)
		case <-tick:
			isGone, err := gone()
			if err != nil || isGone {
				return -1, err
			}
		}
	}
}

// supervisorError says why a supervisor that exited with status wrote no
// end record, adding the last line of its stderr.
func supervisorError(l layout, runID string, status int) error {
	said := lastLine(l.supervisorStderr)
	switch {
	case status == supervisorDone:
		return fmt.Errorf("cordon: launch %s was claimed by recovery before its supervisor started it", runID)
	case status == supervisorFailed:
		return fmt.Errorf("cordon: launch %s failed: %s", runID, said)
	case status == supervisorUnproven:
		return fmt.Errorf("%w: launch %s: %s", ErrRunEndUnproven, runID, said)
	case status >= supervisorBlocked && status < supervisorBlocked+len(AllEndedBy):
		return &BlockedError{RunID: runID, EndedBy: AllEndedBy[status-supervisorBlocked], Err: fmt.Errorf("%w: %s", ErrRunEndUnproven, said)}
	default:
		return &BlockedError{RunID: runID, Err: fmt.Errorf("%w: the supervisor ended with status %d before it recorded an end: %s",
			ErrRunEndUnproven, status, said)}
	}
}

// maxSaid bounds what supervisorError reads of the supervisor's stderr.
const maxSaid = 4 << 10

// lastLine returns the last non-empty line of the end of the file at path,
// or a note that there is none.
func lastLine(path string) string {
	f, err := os.Open(path) //nolint:gosec // G304: the supervisor's stderr in the launch's own control directory
	if err != nil {
		return "the supervisor's stderr is unreadable"
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err == nil && info.Size() > maxSaid {
		_, _ = f.Seek(-maxSaid, io.SeekEnd)
	}
	body, _ := io.ReadAll(io.LimitReader(f, maxSaid))
	body = bytes.TrimSpace(body)
	if i := bytes.LastIndexByte(body, '\n'); i >= 0 {
		body = body[i+1:]
	}
	if len(body) == 0 {
		return "the supervisor said nothing"
	}
	return string(body)
}
