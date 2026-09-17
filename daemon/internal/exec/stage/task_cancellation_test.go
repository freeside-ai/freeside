package stage

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func TestTaskCancellationRunningRecoveryKeepsRegistration(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(map[bool]string{false: "registered", true: "already-fenced"}[stopped], func(t *testing.T) {
			registered, cancellationRecorded := false, false
			gate := &stubGate{
				cancelFn: func(string) error { cancellationRecorded = true; return nil },
				recoverFn: func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
					if stopped && !cancellationRecorded {
						t.Fatal("fenced recovery entered ward without durable cancellation")
					}
					if !stopped && !registered {
						t.Fatal("running recovery escaped its task registration")
					}
					return &ward.RecoveryResult{Outcome: ward.RecoveryCanceled}, nil
				},
			}
			d := newTestDriver(t, gate, newStubExports())
			orphan(t, d, phaseRunning, nil)
			d.SetRecoveryLauncher(func(ctx context.Context, _ domain.RunID, launch func(context.Context) error) error {
				if stopped {
					return ErrTaskCancelled
				}
				registered = true
				defer func() { registered = false }()
				return launch(ctx)
			})
			if err := d.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			result, err := d.Collect(t.Context(), testInvoke)
			if err != nil || result.Status != exec.StatusCanceled {
				t.Fatalf("recovered outcome = %+v, %v", result, err)
			}
		})
	}
}

func TestTaskCancellationDuringRecoveryDoesNotRepeatStaleIntent(t *testing.T) {
	calls := 0
	gate := &stubGate{
		cancelFn: func(string) error { t.Fatal("repeated entered recovery as a fresh cancellation"); return nil },
		recoverFn: func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
			calls++
			return nil, context.Canceled
		},
	}
	d := newTestDriver(t, gate, newStubExports())
	orphan(t, d, phaseRunning, nil)
	d.SetRecoveryLauncher(func(ctx context.Context, _ domain.RunID, launch func(context.Context) error) error {
		if err := launch(ctx); err == nil {
			t.Fatal("fixture recovery did not fail")
		}
		return ErrTaskCancelled // The engine normalizes task-local cancellation.
	})
	if err := d.Reconcile(t.Context()); !errors.Is(err, ErrRecoveryRetryable) {
		t.Fatalf("interrupted recovery = %v", err)
	}
	if calls != 1 {
		t.Fatalf("recovery called %d times", calls)
	}
}

func TestCancelOrphanDoesNotResumePreJournalWork(t *testing.T) {
	for _, phase := range []phase{phaseSeeding, phaseRunning} {
		t.Run(string(phase), func(t *testing.T) {
			gate := &stubGate{
				cancelFn:       func(string) error { return ward.ErrJournalRecordNotFound },
				handoffStarted: func(string) (bool, error) { return false, nil },
			}
			d := newTestDriver(t, gate, newStubExports())
			orphan(t, d, phase, nil)
			if err := d.Cancel(t.Context(), testInvoke); err != nil {
				t.Fatal(err)
			}
			result, err := d.Collect(t.Context(), testInvoke)
			if err != nil || result.Status != exec.StatusCanceled {
				t.Fatalf("orphan cancellation = %+v, %v", result, err)
			}
			if err := d.Reconcile(t.Context()); err != nil {
				t.Fatalf("cancelled seed recovery: %v", err)
			}
			if err := d.CancelAndConfirm(t.Context(), testInvoke); !errors.Is(err, ErrRecoveryRetryable) {
				t.Fatalf("orphan host process acquired a false absence proof: %v", err)
			}
		})
	}
}

func TestCancelOrphanRecordsIntentBeforeWardRecovery(t *testing.T) {
	cancelled := false
	gate := &stubGate{
		cancelFn: func(string) error { cancelled = true; return nil },
		recoverFn: func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
			if !cancelled {
				t.Fatal("recovery ran before durable cancellation")
			}
			return &ward.RecoveryResult{Outcome: ward.RecoveryCanceled}, nil
		},
	}
	d := newTestDriver(t, gate, newStubExports())
	orphan(t, d, phaseRunning, nil)
	if err := d.Cancel(t.Context(), testInvoke); err != nil {
		t.Fatal(err)
	}
	result, err := d.Collect(t.Context(), testInvoke)
	if err != nil || result.Status != exec.StatusCanceled {
		t.Fatalf("orphan cancellation = %+v, %v", result, err)
	}
}

func TestCancelBoundedWaitDoesNotInventTermination(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := newTestDriver(t, &stubGate{}, newStubExports())
		orphan(t, d, phaseRunning, nil)
		cancelled := false
		done := make(chan struct{})
		d.running[testInvoke] = &session{cancel: func() { cancelled = true }, done: done}
		defer close(done)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := d.Cancel(ctx, testInvoke); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("nonresponsive child cancellation = %v", err)
		}
		if !cancelled {
			t.Fatal("child never received cancellation")
		}
		in, err := d.loadIntent(t.Context(), testInvoke)
		if err != nil || in.Phase != phaseRunning || in.Result != nil {
			t.Fatalf("timeout invented a terminal result: %+v, %v", in, err)
		}
	})
}
