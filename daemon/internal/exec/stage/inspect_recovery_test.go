package stage

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// inspectChecked is Inspect after the current-policy check that Inspect
// starts, and only starts, for a preterminal invocation with no live session
// and no passed check, as after a restart or a session's end (#3268): when
// that Inspect answers running and not live, it waits for the check and
// returns the next Inspect's answer. A test that reads what Inspect answered
// before that check moved off the driver's lock uses it.
func inspectChecked(t *testing.T, ctx context.Context, d *Driver, id domain.InvocationID) (exec.Inspection, error) {
	t.Helper()
	d.mu.Lock()
	checked := d.currentChecks[id].state == currentCheckPassed
	d.mu.Unlock()
	got, err := d.Inspect(ctx, id)
	if checked || err != nil || got.Status != exec.StatusRunning || got.Live {
		return got, err
	}
	d.recoveries.Wait()
	return d.Inspect(ctx, id)
}

// inspectRecovered is inspectChecked followed by the recovery it started,
// read the way Inspect answered when it recovered inline: it waits for the
// recovery, then reports the phase recovery left without starting another.
// A test that expects one Inspect to recover uses it (#3215).
func inspectRecovered(t *testing.T, ctx context.Context, d *Driver, id domain.InvocationID) (exec.Inspection, error) {
	t.Helper()
	got, err := inspectChecked(t, ctx, d, id)
	d.recoveries.Wait()
	if err != nil || got.Status != exec.StatusRunning || got.Live {
		return got, err
	}
	d.mu.Lock()
	recoveryErr, failed := d.recoveryFailures[id]
	delete(d.recoveryFailures, id)
	d.mu.Unlock()
	if failed {
		return exec.Inspection{}, fmt.Errorf("reconcile invocation %s: %w", id, recoveryErr)
	}
	in, live, checking, err := d.inspectIntent(ctx, id)
	if errors.Is(err, ErrRecoveryRetryable) {
		return exec.Inspection{Status: exec.StatusRunning}, nil
	}
	if err != nil {
		return exec.Inspection{}, err
	}
	if checking {
		// Launching recovery consumed the check's mark, so this read started
		// the next check; wait for it so it outlives no test step.
		d.recoveries.Wait()
		return exec.Inspection{Status: exec.StatusRunning}, nil
	}
	switch in.Phase {
	case phaseCommitted:
		return exec.Inspection{Status: in.Result.Status}, nil
	case phaseLost:
		return exec.Inspection{Status: exec.StatusGone}, nil
	case phaseSeeding, phaseRunning, phaseExported, phaseImportPending:
	}
	return exec.Inspection{Status: exec.StatusRunning, Live: live}, nil
}
