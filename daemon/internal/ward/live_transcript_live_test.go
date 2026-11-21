package ward

// Opt-in proof of the live transcript read's exec route against the
// reference runtime (Apple container 1.1.0), following live_test.go:
//
//	FREESIDE_WARD_LIVE_TEST=1 go test ./internal/ward -run TestLiveTranscriptRead -v
//
// FREESIDE_WARD_LIVE_READ_IMAGE runs it against another image, such as the
// Claude agent image, whose coreutils the read meets in production; the
// default is the pinned alpine image (BusyBox). Not run in CI.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func liveReadImage() string {
	if image := os.Getenv("FREESIDE_WARD_LIVE_READ_IMAGE"); image != "" {
		return image
	}
	return liveImage
}

// liveReadContainer creates and starts a networkless container running
// command as root, and removes it at cleanup.
func liveReadContainer(t *testing.T, rt *CLIRuntime, name, command string) {
	t.Helper()
	ctx := context.Background()
	t.Cleanup(func() {
		_ = rt.StopContainer(ctx, name)
		_ = rt.DeleteContainer(ctx, name)
	})
	if err := rt.CreateContainer(ctx, ContainerSpec{
		Size: testContainerSize, Name: name, Image: liveReadImage(),
		Command:         []string{"sh", "-c", command},
		Labels:          []Label{{Key: "freeside.ward-live", Value: name}},
		NetworkDisabled: true,
	}); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if err := rt.StartContainer(ctx, name); err != nil {
		t.Fatalf("start %s: %v", name, err)
	}
}

// liveTranscriptWriterCommand makes the launch directory as the launcher
// does and appends one record every 200ms, exiting promptly on TERM.
func liveTranscriptWriterCommand() string {
	dir := "/var/lib/freeside-launch"
	return "trap 'exit 0' TERM; mkdir -p " + dir + "; chown 0:0 " + dir + "; chmod 0700 " + dir +
		"; i=0; while :; do printf '{\"n\":%d}\\n' \"$i\" >> " + WriterTranscriptPath +
		"; i=$((i+1)); sleep 0.2; done"
}

func timeStop(t *testing.T, rt *CLIRuntime, name string) time.Duration {
	t.Helper()
	started := time.Now()
	if err := rt.StopContainer(context.Background(), name); err != nil {
		t.Fatalf("stop %s: %v", name, err)
	}
	return time.Since(started)
}

// TestLiveTranscriptReadExec proves, on real VMs, that the read returns
// complete records from the daemon's offset and records appended after an
// earlier read, refuses a symbolic link at the transcript path, and reads a
// missing transcript as empty.
//
//nolint:paralleltest // Serial: live suite on the shared Apple container runtime
func TestLiveTranscriptReadExec(t *testing.T) {
	bin := requireLiveContainer(t)
	rt := NewCLIRuntime(bin)
	ctx := context.Background()
	stamp := time.Now().UnixNano()

	writer := fmt.Sprintf("%sliveread-%d-writer", conformanceObjectPrefix, stamp)
	liveReadContainer(t, rt, writer, liveTranscriptWriterCommand())
	time.Sleep(2 * time.Second)
	first, err := rt.ExecRead(ctx, writer, liveTranscriptReadArgv(0, 1<<20), 1<<20)
	if err != nil {
		t.Fatalf("first read: %v", err)
	}
	if !bytes.HasPrefix(first, []byte("{\"n\":0}\n")) {
		t.Fatalf("first read starts %q, want the first record", first[:min(len(first), 32)])
	}
	time.Sleep(time.Second)
	second, err := rt.ExecRead(ctx, writer, liveTranscriptReadArgv(int64(len(first)), 1<<20), 1<<20)
	if err != nil {
		t.Fatalf("second read: %v", err)
	}
	if len(second) == 0 || !bytes.HasPrefix(second, []byte("{\"n\":")) {
		t.Fatalf("second read = %q, want records appended after the first read", second)
	}
	capped, err := rt.ExecRead(ctx, writer, liveTranscriptReadArgv(0, 4), 4)
	if err != nil || string(capped) != "{\"n\"" {
		t.Fatalf("capped read = %q, %v; want the first 4 bytes", capped, err)
	}

	linked := fmt.Sprintf("%sliveread-%d-link", conformanceObjectPrefix, stamp)
	liveReadContainer(t, rt, linked, "mkdir -p /var/lib/freeside-launch; ln -s /etc/hostname "+
		WriterTranscriptPath+"; sleep 600")
	time.Sleep(2 * time.Second)
	if out, err := rt.ExecRead(ctx, linked, liveTranscriptReadArgv(0, 1<<20), 1<<20); err == nil {
		t.Fatalf("read through a symbolic link returned %q, want refusal", out)
	}

	empty := fmt.Sprintf("%sliveread-%d-empty", conformanceObjectPrefix, stamp)
	liveReadContainer(t, rt, empty, "sleep 600")
	time.Sleep(2 * time.Second)
	if out, err := rt.ExecRead(ctx, empty, liveTranscriptReadArgv(0, 1<<20), 1<<20); err != nil || len(out) != 0 {
		t.Fatalf("read before the transcript exists = %q, %v; want empty", out, err)
	}
}

// TestLiveTranscriptReadDoesNotHoldStop proves an exec process in flight
// when the container stops neither delays the stop beyond a baseline stop
// of the same container nor outlives it: the exec call returns promptly
// once the container stops, instead of running to its own end.
//
//nolint:paralleltest // Serial: live suite on the shared Apple container runtime
func TestLiveTranscriptReadDoesNotHoldStop(t *testing.T) {
	bin := requireLiveContainer(t)
	rt := NewCLIRuntime(bin)
	stamp := time.Now().UnixNano()

	baseline := fmt.Sprintf("%sliveread-%d-baseline", conformanceObjectPrefix, stamp)
	liveReadContainer(t, rt, baseline, liveTranscriptWriterCommand())
	time.Sleep(2 * time.Second)
	baselineStop := timeStop(t, rt, baseline)

	watched := fmt.Sprintf("%sliveread-%d-watched", conformanceObjectPrefix, stamp)
	liveReadContainer(t, rt, watched, liveTranscriptWriterCommand())
	time.Sleep(2 * time.Second)
	// A read wedged inside the guest: the fixed read never runs this long,
	// so this stands in for the worst case, a read still running at stop.
	const wedge = 60 * time.Second
	type result struct {
		err      error
		returned time.Time
	}
	done := make(chan result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*wedge)
		defer cancel()
		_, err := rt.ExecRead(ctx, watched, []string{"sh", "-c", fmt.Sprintf("sleep %d", int(wedge.Seconds()))}, 1024)
		done <- result{err: err, returned: time.Now()}
	}()
	time.Sleep(2 * time.Second)
	select {
	case r := <-done:
		t.Fatalf("the wedged exec returned before the stop: %v", r.err)
	default:
	}
	watchedStop := timeStop(t, rt, watched)
	stopped := time.Now()
	t.Logf("stop without a read %s, with a wedged read %s", baselineStop, watchedStop)
	if watchedStop > baselineStop+3*time.Second {
		t.Errorf("stop with a wedged read took %s, baseline %s", watchedStop, baselineStop)
	}
	select {
	case r := <-done:
		if after := r.returned.Sub(stopped); after > 10*time.Second {
			t.Errorf("the wedged exec returned %s after the stop", after)
		}
		t.Logf("the wedged exec returned %s after the stop: %v", r.returned.Sub(stopped), r.err)
	case <-time.After(wedge):
		t.Fatal("the wedged exec outlived the container's stop")
	}
	rep, err := rt.Inspect(context.Background(), watched)
	if err != nil {
		t.Fatalf("inspect after stop: %v", err)
	}
	if rep.State != StateStopped {
		t.Errorf("state after stop = %q", rep.State)
	}
	if err := rt.DeleteContainer(context.Background(), watched); err != nil && !strings.Contains(err.Error(), "not found") {
		t.Errorf("delete after a wedged read: %v", err)
	}
}
