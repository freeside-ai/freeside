package stage

import (
	"context"
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func TestWorkflowClosureNeverRestartsOrFabricatesAResult(t *testing.T) {
	for _, ph := range []phase{phaseSeeding, phaseRunning} {
		t.Run(string(ph), func(t *testing.T) {
			gate := &stubGate{
				handoffStarted: func(string) (bool, error) { return false, nil },
				handoffFn: func(ward.HandoffSpec) (*ward.HandoffResult, error) {
					t.Fatal("closed workflow launched a handoff")
					return nil, nil
				},
				cancelFn: func(string) error { t.Fatal("closure requested cancellation"); return nil },
			}
			d := newTestDriver(t, gate, newStubExports())
			orphan(t, d, ph, nil)
			d.SetRecoveryLauncher(func(context.Context, domain.RunID, func(context.Context) error) error {
				return ErrWorkflowClosed
			})
			d.SetRecoveryObserver(func(context.Context, domain.RunID, func(context.Context) error) error {
				t.Fatal("unstarted work entered observation recovery")
				return nil
			})
			for range 2 {
				if err := d.Reconcile(t.Context()); err != nil {
					t.Fatal(err)
				}
				if _, err := d.Collect(t.Context(), testInvoke); !errors.Is(err, exec.ErrResultNotReady) {
					t.Fatalf("suppressed restart manufactured a result: %v", err)
				}
			}
			in, err := d.loadIntent(t.Context(), testInvoke)
			if err != nil || in.Phase != phaseSeeding || in.Result != nil {
				t.Fatalf("unstarted intent changed terminal meaning: %+v, %v", in, err)
			}
		})
	}
}

func TestWorkflowClosureRecoversAnExistingProviderUnderStopRegistration(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(map[bool]string{false: "closed", true: "closed and stopped"}[stopped], func(t *testing.T) {
			registered, cancelled, calls := false, false, 0
			gate := &stubGate{
				cancelFn: func(string) error {
					if !stopped {
						t.Fatal("administrative closure requested provider cancellation")
					}
					cancelled = true
					return nil
				},
				recoverFn: func(string, ward.HandoffSpec) (*ward.RecoveryResult, error) {
					calls++
					if stopped {
						if !cancelled {
							t.Fatal("Stop recovery lacked cancellation authority")
						}
						return &ward.RecoveryResult{Outcome: ward.RecoveryCanceled}, nil
					}
					if !registered {
						t.Fatal("observation escaped task Stop registration")
					}
					return &ward.RecoveryResult{Outcome: ward.RecoveryFailed, FailureStatus: 7}, nil
				},
			}
			d := newTestDriver(t, gate, newStubExports())
			orphan(t, d, phaseRunning, nil)
			d.SetRecoveryLauncher(func(context.Context, domain.RunID, func(context.Context) error) error {
				t.Fatal("existing handoff consulted the new-work launcher")
				return ErrWorkflowClosed
			})
			d.SetRecoveryObserver(func(ctx context.Context, _ domain.RunID, observe func(context.Context) error) error {
				if stopped {
					return ErrTaskCancelled
				}
				registered = true
				defer func() { registered = false }()
				return observe(ctx)
			})
			for range 2 {
				if err := d.Reconcile(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			result, err := d.Collect(t.Context(), testInvoke)
			want := exec.StatusFailed
			if stopped {
				want = exec.StatusCanceled
			}
			if err != nil || result.Status != want || calls != 1 {
				t.Fatalf("actual recovered result = %+v, calls=%d, err=%v", result, calls, err)
			}
		})
	}
}
