package stage

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
)

// inspectRecovered is Inspect followed by the recovery it started, read the
// way Inspect answered when it recovered inline: it waits for the recovery,
// then reports the phase recovery left without starting another. A test
// that expects one Inspect to recover uses it (#3215).
func inspectRecovered(t *testing.T, ctx context.Context, d *Driver, id domain.InvocationID) (exec.Inspection, error) {
	t.Helper()
	got, err := d.Inspect(ctx, id)
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
	in, live, err := d.inspectIntent(ctx, id)
	if errors.Is(err, ErrRecoveryRetryable) {
		return exec.Inspection{Status: exec.StatusRunning}, nil
	}
	if err != nil {
		return exec.Inspection{}, err
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
