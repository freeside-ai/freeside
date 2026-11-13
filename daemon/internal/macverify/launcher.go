package macverify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/procbound"
)

// errHelperAbandoned marks a helper operation the daemon stopped waiting for.
// The helper runs as root and may still be acting on the slot, so the slot
// cannot be torn down or released.
var errHelperAbandoned = errors.New("slot-run is still running")

// launcher is the room's seam over slot-run, so the room's sequencing and
// absence rule are testable without root.
type launcher interface {
	// prepare refuses a slot that owns a live process, resets its home,
	// temporary, and cache directories, and copies workdir into a fresh job
	// directory, installing profile as the job's sandbox profile. caches
	// names the dependency cache entries the job uses.
	prepare(ctx context.Context, slot, jobID, workdir string, profile []byte, caches jobCaches) (preparedJob, error)
	// run starts the command as the slot and returns a channel that receives
	// exactly one outcome once it is done.
	run(req runRequest, output io.Writer) (<-chan runOutcome, error)
	// reap kills every process the slot owns and reports the slot's uid.
	reap(ctx context.Context, slot string) (int, error)
	// cleanup removes the job directory and resets the home again.
	cleanup(ctx context.Context, slot, jobID string) error
	// ownsProcess reports whether uid owns a live process.
	ownsProcess(uid int) (bool, error)
}

// jobCaches names a job's dependency cache entries by key, "" for none:
// Swift is the entry whose seed prepare copies into the job directory and
// the home, and Go the entry whose module cache the command reads in place.
type jobCaches struct {
	Swift string
	Go    string
}

// preparedJob is what slot-run reports for a prepared slot. Caches are the
// entries it checked and set the job up from.
type preparedJob struct {
	Root   string
	UID    int
	Home   string
	Tmp    string
	Cache  string
	Caches jobCaches
	// Ports are the slot's reserved ports, as slot-run read them from
	// bin/ports.conf.
	Ports slotPortSet
}

// runRequest is one command launch as a slot. Env and Params are the exact
// environment and sandbox parameters the room expects; slot-run refuses the
// run unless they are the ones it derives itself.
type runRequest struct {
	Slot   string
	JobID  string
	Env    []string
	Params []string
	Argv   []string
}

// runOutcome is a finished run: the command's exit status (-1 for a signal
// death), or Err when slot-run failed before the command ran.
type runOutcome struct {
	ExitCode int
	Err      error
}

const (
	sudoPath = "/usr/bin/sudo"
	// maxHelperReport bounds what the daemon keeps of a helper operation's
	// own stdout and stderr.
	maxHelperReport = 64 << 10
)

// sudoLauncher reaches slot-run through `sudo -n`. The helper and everything
// it starts run as root or as the slot, so the daemon cannot signal them:
// a context bounds how long the daemon waits, never the helper itself.
type sudoLauncher struct {
	helper    string
	waitDelay time.Duration
}

func (l sudoLauncher) command(op string, args ...string) *exec.Cmd {
	return exec.Command(sudoPath, append([]string{"-n", l.helper, op}, args...)...) //nolint:gosec // G204: fixed helper path; slot-run validates every argument
}

// invoke runs one helper operation and returns its stdout. A helper still
// running when ctx ends is left running and reported as errHelperAbandoned.
func (l sudoLauncher) invoke(ctx context.Context, stdin []byte, op string, args ...string) ([]byte, error) {
	cmd := l.command(op, args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	stdout := procbound.NewTailBuffer(maxHelperReport)
	stderr := procbound.NewTailBuffer(maxHelperReport)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = l.waitDelay
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start slot-run %s: %w", op, err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return nil, fmt.Errorf("slot-run %s: %w: %s", op, err, bytes.TrimSpace(stderr.Bytes()))
		}
		return stdout.Bytes(), nil
	case <-ctx.Done():
		return nil, fmt.Errorf("slot-run %s: %w: %w", op, errHelperAbandoned, ctx.Err())
	}
}

func (l sudoLauncher) prepare(ctx context.Context, slot, jobID, workdir string, profile []byte, caches jobCaches) (preparedJob, error) {
	args := []string{slot, jobID, workdir}
	if caches.Swift != "" {
		args = append(args, "swift="+caches.Swift)
	}
	if caches.Go != "" {
		args = append(args, "go="+caches.Go)
	}
	out, err := l.invoke(ctx, profile, "prepare", args...)
	if err != nil {
		return preparedJob{}, err
	}
	return parsePrepared(out)
}

func (l sudoLauncher) run(req runRequest, output io.Writer) (<-chan runOutcome, error) {
	args := make([]string, 0, 3+len(req.Env)+len(req.Params)+len(req.Argv))
	args = append(args, req.Slot, req.JobID)
	args = append(args, req.Env...)
	args = append(args, req.Params...)
	args = append(args, "--")
	args = append(args, req.Argv...)
	cmd := l.command("run", args...)
	cmd.Stdout, cmd.Stderr = output, output
	cmd.WaitDelay = l.waitDelay
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start slot-run run: %w", err)
	}
	done := make(chan runOutcome, 1)
	go func() { done <- outcomeOf(cmd.Wait()) }()
	return done, nil
}

// outcomeOf maps the run's wait error the way verify.ProcRoom does, with one
// addition: slot-run's reserved status means it refused before handing over,
// which is a room failure. prepare has already passed every check run
// repeats, so a command that itself exits with that status is the only way
// to reach it after a successful prepare; it reads as a room failure too
// rather than as a verdict on the candidate.
func outcomeOf(err error) runOutcome {
	if err == nil {
		return runOutcome{}
	}
	// A command that exited 0 while a descendant still held the output pipe
	// is not a clean verification.
	if errors.Is(err, exec.ErrWaitDelay) {
		return runOutcome{ExitCode: -1}
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if exit.ExitCode() == helperRefused {
			return runOutcome{Err: fmt.Errorf("slot-run refused the run (exit %d)", helperRefused)}
		}
		return runOutcome{ExitCode: exit.ExitCode()}
	}
	return runOutcome{Err: fmt.Errorf("slot-run run: %w", err)}
}

func (l sudoLauncher) reap(ctx context.Context, slot string) (int, error) {
	out, err := l.invoke(ctx, nil, "reap", slot)
	if err != nil {
		return 0, err
	}
	fields, err := parseReport(out, "uid")
	if err != nil {
		return 0, err
	}
	return parseUID(fields["uid"])
}

func (l sudoLauncher) cleanup(ctx context.Context, slot, jobID string) error {
	_, err := l.invoke(ctx, nil, "cleanup", slot, jobID)
	return err
}

func (sudoLauncher) ownsProcess(uid int) (bool, error) { return uidOwnsProcess(uid) }

// parsePrepared reads prepare's report: exactly root, uid, home, tmp, cache,
// swift_cache, and go_cache, with every path clean and absolute and each
// cache either none or a cache key.
func parsePrepared(out []byte) (preparedJob, error) {
	fields, err := parseReport(out, "root", "uid", "home", "tmp", "cache", "ports", "swift_cache", "go_cache")
	if err != nil {
		return preparedJob{}, err
	}
	uid, err := parseUID(fields["uid"])
	if err != nil {
		return preparedJob{}, err
	}
	for _, key := range []string{"root", "home", "tmp", "cache"} {
		path := fields[key]
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return preparedJob{}, fmt.Errorf("slot-run reported %s %q, not a clean absolute path", key, path)
		}
	}
	var caches [2]string
	for i, key := range []string{"swift_cache", "go_cache"} {
		switch value := fields[key]; {
		case value == "none":
		case swiftCacheKeyPattern.MatchString(value):
			caches[i] = value
		default:
			return preparedJob{}, fmt.Errorf("slot-run reported %s %q, not none or a cache key", key, value)
		}
	}
	ports, err := parsePorts(fields["ports"])
	if err != nil {
		return preparedJob{}, err
	}
	return preparedJob{
		Root: fields["root"], UID: uid, Home: fields["home"], Tmp: fields["tmp"], Cache: fields["cache"],
		Caches: jobCaches{Swift: caches[0], Go: caches[1]}, Ports: ports,
	}, nil
}

// parsePorts reads a slot's reserved ports as slot-run reports them, in
// slotPortSet.String's form, each in canonical decimal.
func parsePorts(s string) (slotPortSet, error) {
	var set slotPortSet
	parts := strings.Split(s, ",")
	if len(parts) != len(set) {
		return set, fmt.Errorf("slot-run reported ports %q, not %d ports", s, len(set))
	}
	for i, part := range parts {
		port, err := strconv.Atoi(part)
		if err != nil || port < minReservedPort || port > 65535 || strconv.Itoa(port) != part {
			return set, fmt.Errorf("slot-run reported ports %q, not %d ports", s, len(set))
		}
		set[i] = port
	}
	return set, nil
}

// parseReport reads key=value lines and requires exactly the given keys,
// each once.
func parseReport(out []byte, keys ...string) (map[string]string, error) {
	fields := make(map[string]string, len(keys))
	for line := range strings.Lines(string(out)) {
		key, value, ok := strings.Cut(strings.TrimSuffix(line, "\n"), "=")
		if !ok {
			return nil, fmt.Errorf("slot-run report line %q is not key=value", line)
		}
		if _, dup := fields[key]; dup {
			return nil, fmt.Errorf("slot-run reported %s twice", key)
		}
		fields[key] = value
	}
	if len(fields) != len(keys) {
		return nil, fmt.Errorf("slot-run reported %d fields, want %v", len(fields), keys)
	}
	for _, key := range keys {
		if _, ok := fields[key]; !ok {
			return nil, fmt.Errorf("slot-run report is missing %s", key)
		}
	}
	return fields, nil
}

func parseUID(s string) (int, error) {
	uid, err := strconv.Atoi(s)
	if err != nil || uid <= 0 || strconv.Itoa(uid) != s {
		return 0, fmt.Errorf("slot-run reported uid %q, not a positive integer", s)
	}
	return uid, nil
}
