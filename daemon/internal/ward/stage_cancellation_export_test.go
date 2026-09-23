package ward

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// StageCancellationTestFixture connects external-package stage tests to the
// controlled ward runtime without exposing test plumbing in production code.
type StageCancellationTestFixture struct {
	Backend    *Backend
	SeedRoot   string
	ExportRoot string
	Waiting    <-chan struct{}

	t       *testing.T
	fx      *handoffFixture
	journal *fakeJournal
	leaser  *fakeLeaser
}

func NewStageCancellationTestFixture(t *testing.T) *StageCancellationTestFixture {
	t.Helper()
	fx := newHandoffFixture(t)
	j := fx.journalled()
	_, leaser := fx.leased(t)
	fx.cfg.SeedRoot = t.TempDir()
	waiting := make(chan struct{})
	var agentStarted, blocked atomic.Bool
	fx.rt.onStart = func(id string) error {
		if strings.HasSuffix(id, "-agent") {
			fx.rt.runningInspects[id] = 100
			agentStarted.Store(true)
		}
		return nil
	}
	// This fixture models a valid setup-token volume; the generic fake
	// observer otherwise emits only the opaque credential proof fields.
	fx.rt.observerProof = func(id string, proof []byte) []byte {
		if strings.HasSuffix(id, "-cred-pre") || strings.HasSuffix(id, "-cred-post") {
			return append(proof, []byte("cred_manifest=setup_token\n")...)
		}
		return proof
	}
	fx.cfg.Sleep = func(ctx context.Context, _ time.Duration) error {
		if agentStarted.Load() && blocked.CompareAndSwap(false, true) {
			close(waiting)
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}
	return &StageCancellationTestFixture{
		Backend: fx.backend(t), SeedRoot: fx.cfg.SeedRoot,
		ExportRoot: fx.cfg.ExportRoot, Waiting: waiting,
		t: t, fx: fx, journal: j, leaser: leaser,
	}
}

func (f *StageCancellationTestFixture) Base() domain.BaseRevision { return testBaseRevision() }

func (f *StageCancellationTestFixture) AuthStoreVolume(
	ctx context.Context, id domain.AuthIdentityID,
) (string, error) {
	return f.leaser.AuthStoreVolume(ctx, id)
}

func (f *StageCancellationTestFixture) FetchBase(
	ctx context.Context, repo, baseRef, baseSHA, dir string,
) error {
	return f.FetchBaseWorktree(ctx, repo, baseRef, baseSHA, dir)
}

func (f *StageCancellationTestFixture) FetchBaseWorktree(
	ctx context.Context, repo, _, baseSHA, dir string,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	base := testBaseRevision()
	if repo != base.Repo || baseSHA != base.BaseSHA {
		return fmt.Errorf("unexpected seed base %q@%q", repo, baseSHA)
	}
	src := writeSeedCheckoutFor(f.t, f.t.TempDir(), baseSHA, repo, base.RepositoryID)
	return os.Rename(src, dir)
}

func (f *StageCancellationTestFixture) ReopenBackend(t *testing.T) *Backend {
	t.Helper()
	return f.fx.backend(t)
}

func (f *StageCancellationTestFixture) AssertCanceled(t *testing.T, runID string) {
	t.Helper()
	rec := f.journal.snapshot(runID)
	if rec == nil || !rec.CancellationRequested || rec.Outcome == nil ||
		*rec.Outcome != HandoffCanceled || rec.WriterComplete || rec.ExportDir != "" {
		t.Fatalf("ward cancellation record = %+v", rec)
	}
	names := namesFor(runID)
	mark := f.fx.rt.callIndex("journal-cancellation-requested " + runID)
	stop := f.fx.rt.callIndex("stop-container " + names.Agent)
	if mark < 0 || stop < 0 || mark > stop {
		t.Fatalf("cancellation intent mark=%d, writer stop=%d", mark, stop)
	}
	f.fx.assertReaped(t)
	if err := f.Backend.HandoffQuiescent(t.Context(), runID); err != nil {
		t.Fatalf("fresh ward absence proof: %v", err)
	}
}

func (f *StageCancellationTestFixture) WriterStarts(runID string) int {
	want := "start-container " + namesFor(runID).Agent
	f.fx.rt.mu.Lock()
	defer f.fx.rt.mu.Unlock()
	starts := 0
	for _, call := range f.fx.rt.calls {
		if call == want {
			starts++
		}
	}
	return starts
}
