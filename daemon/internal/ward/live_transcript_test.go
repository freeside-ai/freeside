package ward

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// liveFixture is a backend with one writer container in the fake runtime,
// running and carrying owner, for the live read's own checks.
type liveFixture struct {
	rt        *fakeRuntime
	b         *Backend
	container string
	owner     Label
	claim     objectClaim
}

const liveTestRunID = "live-run"

func newLiveFixture(t *testing.T) *liveFixture {
	t.Helper()
	fx := newHandoffFixture(t)
	owner := Label{Key: ownershipLabelKey, Value: "owner-token"}
	container := namesFor(liveTestRunID).Agent
	fx.rt.ctrs[container] = &fakeCtr{
		spec:    ContainerSpec{Name: container, Labels: []Label{owner}},
		started: true,
		created: "created-1",
	}
	fx.rt.runningInspects[container] = 1 << 20
	return &liveFixture{
		rt: fx.rt, b: fx.backend(t), container: container, owner: owner,
		claim: objectClaim{attempted: true, owned: true, fingerprint: "created-1"},
	}
}

func (lf *liveFixture) register() func() {
	return lf.b.live.register(liveTestRunID, lf.container, lf.claim, lf.owner)
}

func (lf *liveFixture) execReads() int {
	lf.rt.mu.Lock()
	defer lf.rt.mu.Unlock()
	n := 0
	for _, call := range lf.rt.calls {
		if strings.HasPrefix(call, "exec-read ") {
			n++
		}
	}
	return n
}

// TestReadLiveTranscriptReadsTheRegisteredWriter: a registered, running,
// owned writer is read with the fixed argv, from the daemon's offset, under
// the daemon's cap.
func TestReadLiveTranscriptReadsTheRegisteredWriter(t *testing.T) {
	t.Parallel()
	lf := newLiveFixture(t)
	defer lf.register()()
	var gotArgv []string
	var gotCap int
	lf.rt.onExecRead = func(_ context.Context, id string, argv []string, maxBytes int) ([]byte, error) {
		if id != lf.container {
			t.Errorf("exec read targeted %q, want %q", id, lf.container)
		}
		gotArgv, gotCap = argv, maxBytes
		return []byte("{}\n"), nil
	}
	out, err := lf.b.ReadLiveTranscript(context.Background(), liveTestRunID, 41, 1024)
	if err != nil {
		t.Fatalf("ReadLiveTranscript = %v", err)
	}
	if string(out) != "{}\n" {
		t.Errorf("read %q", out)
	}
	want := []string{"sh", "-c", liveTranscriptReadScript, "sh", WriterTranscriptPath, "42", "1024"}
	if !slices.Equal(gotArgv, want) {
		t.Errorf("argv = %q, want %q", gotArgv, want)
	}
	if gotCap != 1024 {
		t.Errorf("cap = %d, want 1024", gotCap)
	}
}

// TestReadLiveTranscriptRefusesWithoutProof: the read execs nothing for a
// run no handoff registered, a container that no longer carries this
// handoff's ownership evidence, or a stopped one.
func TestReadLiveTranscriptRefusesWithoutProof(t *testing.T) {
	t.Parallel()
	cases := map[string]func(*liveFixture) func(){
		"unregistered": func(*liveFixture) func() { return func() {} },
		"foreign label": func(lf *liveFixture) func() {
			lf.rt.ctrs[lf.container].spec.Labels = []Label{{Key: ownershipLabelKey, Value: "someone-else"}}
			lf.claim.fingerprint = ""
			return lf.register()
		},
		"replaced container": func(lf *liveFixture) func() {
			lf.rt.ctrs[lf.container].created = "created-2"
			return lf.register()
		},
		"labels unobserved": func(lf *liveFixture) func() {
			lf.rt.onInspect = func(_ string, rep InspectReport) (InspectReport, error) {
				rep.LabelsObserved = false
				return rep, nil
			}
			return lf.register()
		},
		"stopped": func(lf *liveFixture) func() {
			lf.rt.ctrs[lf.container].stopped = true
			return lf.register()
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			lf := newLiveFixture(t)
			defer setup(lf)()
			lf.rt.onExecRead = func(context.Context, string, []string, int) ([]byte, error) {
				return []byte("leak\n"), nil
			}
			out, err := lf.b.ReadLiveTranscript(context.Background(), liveTestRunID, 0, 1024)
			if !errors.Is(err, ErrNoLiveWriter) || out != nil {
				t.Fatalf("ReadLiveTranscript = %q, %v; want ErrNoLiveWriter", out, err)
			}
			if n := lf.execReads(); n != 0 {
				t.Errorf("%d exec reads, want 0", n)
			}
		})
	}
}

// TestUnregisterDoesNotWaitForARead: removal returns while a read is wedged
// inside exec, and the wedged read ends at once as ErrNoLiveWriter, not at
// its timeout.
func TestUnregisterDoesNotWaitForARead(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		lf := newLiveFixture(t)
		unregister := lf.register()
		inExec := make(chan struct{})
		lf.rt.onExecRead = func(ctx context.Context, _ string, _ []string, _ int) ([]byte, error) {
			close(inExec)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		started := time.Now()
		var readErr error
		var wg sync.WaitGroup
		wg.Go(func() {
			_, readErr = lf.b.ReadLiveTranscript(context.Background(), liveTestRunID, 0, 1024)
		})
		<-inExec
		unregister()
		wg.Wait()
		if !errors.Is(readErr, ErrNoLiveWriter) {
			t.Errorf("wedged read = %v, want ErrNoLiveWriter", readErr)
		}
		if elapsed := time.Since(started); elapsed != 0 {
			t.Errorf("wedged read ended after %s, want at removal", elapsed)
		}
		if lf.b.live.lookup(liveTestRunID) != nil {
			t.Error("registration survived removal")
		}
	})
}

// TestReadLiveTranscriptTimesOut: an exec that never returns is cut at
// liveReadTimeout and reported as a failure, not as the end of the writer.
func TestReadLiveTranscriptTimesOut(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		lf := newLiveFixture(t)
		defer lf.register()()
		lf.rt.onExecRead = func(ctx context.Context, _ string, _ []string, _ int) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		started := time.Now()
		_, err := lf.b.ReadLiveTranscript(context.Background(), liveTestRunID, 0, 1024)
		if err == nil || errors.Is(err, ErrNoLiveWriter) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ReadLiveTranscript = %v, want a deadline failure", err)
		}
		if elapsed := time.Since(started); elapsed != liveReadTimeout {
			t.Errorf("read ended after %s, want %s", elapsed, liveReadTimeout)
		}
	})
}

// TestRegisterRemovesOnlyItsOwnEntry: a stale removal never removes a later
// registration of the same run.
func TestRegisterRemovesOnlyItsOwnEntry(t *testing.T) {
	t.Parallel()
	lf := newLiveFixture(t)
	first := lf.register()
	second := lf.register()
	first()
	if lf.b.live.lookup(liveTestRunID) == nil {
		t.Fatal("a stale removal dropped the current registration")
	}
	second()
	if lf.b.live.lookup(liveTestRunID) != nil {
		t.Fatal("removal left the registration")
	}
}

// TestHandoffRegistersTheWriterOnlyWhileItRuns: a live read during the
// writer wait reaches the writer, and the registration is gone before the
// writer's delete, on the success path and when the wait fails.
func TestHandoffRegistersTheWriterOnlyWhileItRuns(t *testing.T) {
	t.Parallel()
	for _, waitFails := range []bool{false, true} {
		name := "writer stops"
		if waitFails {
			name = "wait fails"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fx := newHandoffFixture(t)
			hs := testHandoffSpec()
			agent := namesFor(hs.RunID).Agent
			// Each wait poll and each live read inspects once: the writer
			// reads running to two waits and two reads, then stops.
			fx.rt.runningInspects[agent] = 4
			var b *Backend
			var mu sync.Mutex
			var reads []error
			fx.cfg.Sleep = func(ctx context.Context, _ time.Duration) error {
				if b.live.lookup(hs.RunID) == nil {
					return nil
				}
				_, err := b.ReadLiveTranscript(ctx, hs.RunID, 0, 1024)
				mu.Lock()
				reads = append(reads, err)
				mu.Unlock()
				if waitFails {
					return errors.New("wait interrupted")
				}
				return nil
			}
			fx.rt.onExecRead = func(context.Context, string, []string, int) ([]byte, error) {
				return []byte("{}\n"), nil
			}
			registeredAtDelete := false
			fx.rt.onDeleteContainer = func(id string) (bool, error) {
				if id == agent && b.live.lookup(hs.RunID) != nil {
					registeredAtDelete = true
				}
				return false, nil
			}
			b = fx.backend(t)
			res, err := b.Handoff(context.Background(), hs)
			if res != nil {
				t.Cleanup(func() { _ = os.RemoveAll(res.ExportDir) })
			}
			if waitFails != (err != nil) {
				t.Fatalf("Handoff = %v, want failure %v", err, waitFails)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(reads) == 0 {
				t.Fatal("no live read ran during the writer wait")
			}
			for _, rerr := range reads {
				if rerr != nil {
					t.Errorf("live read during the wait = %v", rerr)
				}
			}
			if registeredAtDelete {
				t.Error("the writer was still registered at its delete")
			}
			if b.live.lookup(hs.RunID) != nil {
				t.Error("the writer was still registered after Handoff returned")
			}
		})
	}
}
