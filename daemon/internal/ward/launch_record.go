package ward

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// ErrMemoryLimit marks a launch that failed after the guest kernel killed a
// process in its container at the declared memory limit.
var ErrMemoryLimit = errors.New("container killed at its memory limit")

// MemoryLimitError names a failed launch whose container hit its declared
// memory limit, with the class and size that were declared. Cause is the
// failure ward would otherwise have reported (a nonzero writer status, a
// missing review status file), so callers that match the underlying error
// still do.
type MemoryLimitError struct {
	Class LaunchClass
	Size  ContainerSize
	Cause error
}

func (e *MemoryLimitError) Error() string {
	return fmt.Sprintf("%s container killed at its memory limit (%s): %v", e.Class, e.Size, e.Cause)
}

func (e *MemoryLimitError) Is(target error) bool { return target == ErrMemoryLimit }

func (e *MemoryLimitError) Unwrap() error { return e.Cause }

// memoryLimitKillMarker is the kernel's oom-kill report for a kill forced by a
// cgroup's memory.max, as opposed to the whole guest running out of memory.
const memoryLimitKillMarker = "oom-kill:constraint=CONSTRAINT_MEMCG"

// bootLogReportsMemoryLimitKill reports whether a container's boot log holds
// the guest kernel's report of a memory-limit kill inside that container's
// own cgroup. Apple container 1.1.0 puts each container's processes in
// /container/<id> with memory.max set to the declared limit, and the kernel
// logs the kill with oom_memcg naming that cgroup. The exit status alone
// cannot tell this apart: a host SIGKILL also exits 137, and a child killed
// at the limit leaves the parent's exit status to the parent.
func bootLogReportsMemoryLimitKill(log []byte, id string) bool {
	want := "oom_memcg=/container/" + id
	for line := range bytes.Lines(log) {
		_, report, found := bytes.Cut(line, []byte(memoryLimitKillMarker))
		if !found {
			continue
		}
		for field := range strings.SplitSeq(strings.TrimSpace(string(report)), ",") {
			if field == want {
				return true
			}
		}
	}
	return false
}

// memoryLimitKilled reads a stopped container's boot log for a memory-limit
// kill. The log only names a failure; it never grants anything. A process
// that could write the guest kernel log could at most make a failed launch
// read as a memory-limit failure.
// bootLogReadTimeout bounds a boot-log read that runs detached from a
// canceled launch context.
const bootLogReadTimeout = 30 * time.Second

// errBootLogNotRead marks a started launch whose boot log was never read,
// because its container did not reach a clean stop.
var errBootLogNotRead = errors.New("boot log not read: the container did not reach a clean stop")

func (b runtimeOps) memoryLimitKilled(ctx context.Context, id string) launchObservation {
	log, err := b.rt.BootLog(ctx, id)
	if err != nil {
		return launchObservation{bootLogErr: fmt.Errorf("read boot log of %q: %w", id, err)}
	}
	return launchObservation{memoryLimitKill: bootLogReportsMemoryLimitKill(log, id)}
}

// helperStopped checks a stopped helper container (a seeder, observer, or
// exporter) before it is deleted. A helper killed at its memory limit fails
// its launch as that failure even when it exited 0: its seed, proof, or
// export may be partial, and helpers run no child whose kill a launch could
// survive the way a writer's build can. An unreadable boot log is not a
// failure; the proof checks that follow still judge the helper's output.
func (b runtimeOps) helperStopped(ctx context.Context, class LaunchClass, size ContainerSize, id string) error {
	if !b.memoryLimitKilled(ctx, id).memoryLimitKill {
		return nil
	}
	return &MemoryLimitError{Class: class, Size: size, Cause: fmt.Errorf("helper %s", id)}
}

// LaunchEnd is how a ward launch ended, as its launch record reports it. The
// zero value "" is invalid by design.
type LaunchEnd string

const (
	LaunchSucceeded LaunchEnd = "succeeded"
	LaunchFailed    LaunchEnd = "failed"
	// LaunchMemoryLimit is a failed launch whose container hit its declared
	// memory limit.
	LaunchMemoryLimit LaunchEnd = "memory_limit"
	LaunchCanceled    LaunchEnd = "canceled"
)

// AllLaunchEnds lists every valid LaunchEnd; it drives table-driven tests and
// is the single place a new end is registered.
var AllLaunchEnds = []LaunchEnd{LaunchSucceeded, LaunchFailed, LaunchMemoryLimit, LaunchCanceled}

func (e LaunchEnd) valid() bool {
	switch e {
	case LaunchSucceeded, LaunchFailed, LaunchMemoryLimit, LaunchCanceled:
		return true
	default:
		return false
	}
}

func launchEndFor(err error) LaunchEnd {
	switch {
	case err == nil:
		return LaunchSucceeded
	case errors.Is(err, ErrMemoryLimit):
		return LaunchMemoryLimit
	case errors.Is(err, ErrHandoffCanceled), errors.Is(err, context.Canceled):
		return LaunchCanceled
	default:
		return LaunchFailed
	}
}

// launchObservation is what ward learned about one launch's main container
// after it stopped.
type launchObservation struct {
	// memoryLimitKill reports a memory-limit kill in the container's boot log,
	// including one the launch survived (a build child killed while the agent
	// carried on).
	memoryLimitKill bool
	// bootLogErr is set when the boot log could not be read, so the record
	// says the question went unanswered instead of answering no.
	bootLogErr error
}

// nameFailure returns err as a MemoryLimitError when the launch failed and
// its container hit the limit, and err unchanged otherwise.
func (o launchObservation) nameFailure(class LaunchClass, size ContainerSize, err error) error {
	if err == nil || !o.memoryLimitKill {
		return err
	}
	return &MemoryLimitError{Class: class, Size: size, Cause: err}
}

// recordLaunch writes the one structured launch record per launch that sizes
// are tuned from. Apple container 1.1.0 reports no peak memory (container
// stats gives current usage only), so the record carries none.
func recordLaunch(
	logger *slog.Logger, runID string, class LaunchClass, size ContainerSize,
	err error, observed launchObservation,
) {
	if logger == nil {
		return
	}
	attrs := []any{
		"run", runID, "class", string(class),
		"cpus", size.CPUs, "memory_mib", size.MemoryMiB,
		"ended", string(launchEndFor(err)),
		"memory_limit_kill", observed.memoryLimitKill,
	}
	if observed.bootLogErr != nil {
		attrs = append(attrs, "boot_log_error", observed.bootLogErr.Error())
	}
	logger.Info("ward launch", attrs...)
}
