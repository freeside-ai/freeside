package stage

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// stallingStartAuthority holds the first AuthenticateStart of its invocation,
// as a commit-author check that reaches a stalled GitHub would, until release
// closes, and closes reached when it starts holding.
type stallingStartAuthority struct {
	stubAuthority
	id               domain.InvocationID
	reached, release chan struct{}
	once             sync.Once
}

func (a *stallingStartAuthority) AuthenticateStart(
	ctx context.Context, id domain.InvocationID, spec exec.StartSpec,
) error {
	if id == a.id {
		held := false
		a.once.Do(func() { held = true })
		if held {
			close(a.reached)
			<-a.release
		}
	}
	return a.stubAuthority.AuthenticateStart(ctx, id, spec)
}

// returnsWithin fails t when call does not return within ten seconds.
func returnsWithin(t *testing.T, what string, call func()) {
	t.Helper()
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		call()
	}()
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s waited on the stalled call", what)
	}
}

// TestStartAuthenticationHoldsNoDriverLock: while one start's first
// current-authority check hangs, another invocation's Inspect, which takes
// the driver's lock as every session commit does, still answers (#3215).
func TestStartAuthenticationHoldsNoDriverLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := newTestDriver(t, &stubGate{}, newStubExports())
	authority := &stallingStartAuthority{id: testInvoke, reached: make(chan struct{}), release: make(chan struct{})}
	d.authority = authority
	spec := testStartSpec()
	inputs := stageInputs(t, &spec)
	started := make(chan error, 1)
	go func() {
		started <- d.StartWithInputs(ctx, testInvoke, spec,
			func(context.Context) (exec.StageInputs, error) { return inputs, nil })
	}()
	<-authority.reached
	returnsWithin(t, "another invocation's Inspect", func() {
		if _, err := d.Inspect(ctx, "inv-implement-run-2"); !errors.Is(err, exec.ErrUnknownInvocation) {
			t.Errorf("Inspect of an unstarted invocation = %v, want ErrUnknownInvocation", err)
		}
	})
	close(authority.release)
	if err := <-started; err != nil {
		t.Fatalf("StartWithInputs: %v", err)
	}
	waitSessionDone(t, d, testInvoke)
}

// TestInspectRecoversOutsideTheCaller: while the recovery of an orphaned
// handoff hangs on ward, Inspect reports the invocation running rather than
// waiting, and a later Inspect reads the phase the recovery left (#3215).
func TestInspectRecoversOutsideTheCaller(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	reached, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	gate := &stubGate{
		handoffFn: func(ward.HandoffSpec) (*ward.HandoffResult, error) {
			return nil, errors.New("handoff returned before teardown was proven")
		},
		recoverFn: func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
			once.Do(func() { close(reached) })
			<-release
			return &ward.RecoveryResult{Outcome: ward.RecoveryLoss}, nil
		},
	}
	d := newTestDriver(t, gate, newStubExports())
	spec := testStartSpec()
	inputs := stageInputs(t, &spec)
	if err := d.StartWithInputs(ctx, testInvoke, spec,
		func(context.Context) (exec.StageInputs, error) { return inputs, nil },
	); err != nil {
		t.Fatalf("StartWithInputs: %v", err)
	}
	waitSessionDone(t, d, testInvoke)
	returnsWithin(t, "Inspect", func() {
		if status, err := d.Inspect(ctx, testInvoke); err != nil || status.Status != exec.StatusRunning {
			t.Errorf("Inspect while recovery runs = %+v, %v; want running", status, err)
		}
	})
	<-reached
	close(release)
	if status, err := inspectRecovered(t, ctx, d, testInvoke); err != nil || status.Status != exec.StatusGone {
		t.Fatalf("Inspect after recovery = %+v, %v; want gone", status, err)
	}
}

// TestAuthenticateStartRunsTheStartCheckAlone: the driver's exported start
// check returns the authority's refusal wrapped as Start reports it, and a
// check that hangs takes no driver lock, so another invocation's Inspect
// still answers while it runs (#3224).
func TestAuthenticateStartRunsTheStartCheckAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	refusal := errors.New("commit author is not the App bot")
	refused := newTestDriver(t, &stubGate{}, newStubExports())
	refused.authority = stubAuthority{startErr: refusal}
	if err := refused.AuthenticateStart(ctx, testInvoke, testStartSpec()); !errors.Is(err, refusal) {
		t.Fatalf("AuthenticateStart = %v, want the authority's refusal", err)
	}

	d := newTestDriver(t, &stubGate{}, newStubExports())
	authority := &stallingStartAuthority{id: testInvoke, reached: make(chan struct{}), release: make(chan struct{})}
	d.authority = authority
	checked := make(chan error, 1)
	go func() { checked <- d.AuthenticateStart(ctx, testInvoke, testStartSpec()) }()
	<-authority.reached
	returnsWithin(t, "another invocation's Inspect", func() {
		if _, err := d.Inspect(ctx, "inv-implement-run-2"); !errors.Is(err, exec.ErrUnknownInvocation) {
			t.Errorf("Inspect of an unstarted invocation = %v, want ErrUnknownInvocation", err)
		}
	})
	close(authority.release)
	if err := <-checked; err != nil {
		t.Fatalf("AuthenticateStart: %v", err)
	}
	if _, err := d.Inspect(ctx, testInvoke); !errors.Is(err, exec.ErrUnknownInvocation) {
		t.Fatalf("Inspect after the check = %v, want ErrUnknownInvocation: the check commits nothing", err)
	}
}
