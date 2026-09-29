package ward

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// The fixtures are real Apple container 1.1.0 boot logs. The memory-limit one
// ran `tail /dev/zero` under --cpus 2 --memory 256M; the SIGKILL one was
// killed from the host with `container kill --signal KILL`. Both exited 137,
// so only the kernel's memcg report tells them apart.
func readBootLogFixture(t *testing.T, name string) []byte {
	t.Helper()
	body, err := os.ReadFile("testdata/" + name) //nolint:gosec // fixed testdata names
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestBootLogNamesAMemoryLimitKillInTheContainersOwnCgroup(t *testing.T) {
	t.Parallel()
	killed := readBootLogFixture(t, "boot-log-memory-limit-kill.txt")
	if !bootLogReportsMemoryLimitKill(killed, "memory-limit-fixture") {
		t.Fatal("the real memory-limit kill was not recognized")
	}
	// The report names the cgroup exactly: another container's id, or one
	// that merely shares a prefix, does not match.
	for _, id := range []string{"sigkill-fixture", "memory-limit", "memory-limit-fixture-2"} {
		if bootLogReportsMemoryLimitKill(killed, id) {
			t.Fatalf("a kill in /container/memory-limit-fixture matched %q", id)
		}
	}
	sigkill := readBootLogFixture(t, "boot-log-sigkill.txt")
	if bootLogReportsMemoryLimitKill(sigkill, "sigkill-fixture") {
		t.Fatal("a host SIGKILL read as a memory-limit kill")
	}
	global := []byte("oom-kill:constraint=CONSTRAINT_NONE,oom_memcg=/container/x,task=tail\n")
	if bootLogReportsMemoryLimitKill(global, "x") {
		t.Fatal("a guest-wide out-of-memory kill read as the container's limit")
	}
}

func TestLaunchEndsAreValid(t *testing.T) {
	t.Parallel()
	for _, end := range AllLaunchEnds {
		if !end.valid() {
			t.Errorf("%q is registered but invalid", end)
		}
	}
	if LaunchEnd("").valid() {
		t.Error("the zero end is valid")
	}
	cases := map[LaunchEnd]error{
		LaunchSucceeded:   nil,
		LaunchFailed:      ErrWriterFailed,
		LaunchMemoryLimit: &MemoryLimitError{Class: LaunchWriter, Cause: ErrWriterFailed},
		LaunchCanceled:    context.Canceled,
	}
	for want, err := range cases {
		if got := launchEndFor(err); got != want {
			t.Errorf("launchEndFor(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestMemoryLimitErrorKeepsItsCause(t *testing.T) {
	t.Parallel()
	size := DefaultLaunchSize(LaunchWriter)
	err := launchObservation{memoryLimitKill: true}.nameFailure(LaunchWriter, size, writerFailureError(137))
	if !errors.Is(err, ErrMemoryLimit) || !errors.Is(err, ErrWriterFailed) {
		t.Fatalf("err = %v, want both ErrMemoryLimit and ErrWriterFailed", err)
	}
	if !strings.Contains(err.Error(), size.String()) {
		t.Fatalf("err %q does not name the size", err)
	}
	if got := (launchObservation{}).nameFailure(LaunchWriter, size, ErrWriterFailed); errors.Is(got, ErrMemoryLimit) {
		t.Fatalf("an unkilled failure was renamed: %v", got)
	}
	if got := (launchObservation{memoryLimitKill: true}).nameFailure(LaunchWriter, size, nil); got != nil {
		t.Fatalf("a surviving launch was named a failure: %v", got)
	}
}

func TestRecordLaunchWritesOneStructuredRecord(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	recordLaunch(logger, "run-1", LaunchReview, DefaultLaunchSize(LaunchReview), nil,
		launchObservation{memoryLimitKill: true})
	line := buf.String()
	for _, want := range []string{
		"msg=\"ward launch\"", "run=run-1", "class=review", "cpus=4", "memory_mib=1024",
		"ended=succeeded", "memory_limit_kill=true",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("record %q lacks %q", line, want)
		}
	}
	buf.Reset()
	recordLaunch(logger, "run-1", LaunchReview, DefaultLaunchSize(LaunchReview), ErrWriterFailed,
		launchObservation{bootLogErr: errors.New("boot log unavailable")})
	if !strings.Contains(buf.String(), "boot_log_error=") || !strings.Contains(buf.String(), "ended=failed") {
		t.Fatalf("record %q does not report the unanswered boot log", buf.String())
	}
	recordLaunch(nil, "run-1", LaunchReview, ContainerSize{}, nil, launchObservation{})
}
