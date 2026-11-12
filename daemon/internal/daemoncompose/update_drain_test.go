package daemoncompose

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func newDrainTestStore(t *testing.T) (*store.Store, engine.UpdateDrain) {
	t.Helper()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	return st, engine.UpdateDrain{
		Store: st,
		Build: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Now:   func() time.Time { return time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC) },
	}
}

// TestProbeUnlessDrainingStartsNoProbeWhileDraining: with no drain the probe
// runs and counts until it ends; while draining the pass gets a nil probe,
// which reports earlier marks rather than an empty observation, and nothing
// counts.
func TestProbeUnlessDrainingStartsNoProbeWhileDraining(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st, drain := newDrainTestStore(t)
	running := engine.NewRunningWork()
	var probe CredentialIntegrityProbe = func(context.Context) ([]operations.CredentialIntegrityOutcome, error) {
		return nil, nil
	}

	if got, end, err := ProbeUnlessDraining(ctx, st, running, nil, nil); got != nil || err != nil {
		t.Fatalf("no probe configured = %v, %v; want none", got != nil, err)
	} else {
		end()
	}
	got, end, err := ProbeUnlessDraining(ctx, st, running, probe, nil)
	if got == nil || err != nil {
		t.Fatalf("no drain: probe = %v, %v; want the probe", got != nil, err)
	}
	if counts := running.Counts(); counts[domain.RestartBlockerProbe] != 1 {
		t.Fatalf("counts = %v, want one probe", counts)
	}
	end()

	if _, err := drain.Request(ctx); err != nil {
		t.Fatal(err)
	}
	got, end, err = ProbeUnlessDraining(ctx, st, running, probe, nil)
	if got != nil || err != nil {
		t.Fatalf("draining: probe = %v, %v; want none", got != nil, err)
	}
	end()
	if counts := running.Counts(); len(counts) != 0 {
		t.Fatalf("counts = %v, want none", counts)
	}
}

// signalWriter closes seen when a log line contains want.
type signalWriter struct {
	want string
	seen chan struct{}
	once sync.Once
}

func (w *signalWriter) Write(p []byte) (int, error) {
	if strings.Contains(string(p), w.want) {
		w.once.Do(func() { close(w.seen) })
	}
	return len(p), nil
}

// TestModelDiscoveryWaitsOutAnUpdateDrain: discovery runs once per start, so
// under a drain it waits rather than skip, starts its probe once the drain
// lifts, and gives up when its context ends.
func TestModelDiscoveryWaitsOutAnUpdateDrain(t *testing.T) {
	t.Parallel()
	st, drain := newDrainTestStore(t)
	if _, err := drain.Request(t.Context()); err != nil {
		t.Fatal(err)
	}
	waiting := &signalWriter{want: "model discovery waits", seen: make(chan struct{})}
	running := engine.NewRunningWork()
	refresh := modelCatalogRefresh{store: st, logger: slog.New(slog.NewTextHandler(waiting, nil)), running: running}
	type begun struct {
		end func()
		ok  bool
	}
	result := make(chan begun, 1)
	go func() {
		end, ok := refresh.beginProbe(t.Context(), modelCatalogTarget{enrollment: "enroll-1", generation: 1})
		result <- begun{end, ok}
	}()
	<-waiting.seen
	if counts := running.Counts(); len(counts) != 0 {
		t.Fatalf("a waiting probe counted %v", counts)
	}
	if _, err := drain.Cancel(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := <-result
	if !got.ok {
		t.Fatal("the probe gave up after the lift")
	}
	if counts := running.Counts(); counts[domain.RestartBlockerProbe] != 1 {
		t.Fatalf("counts = %v, want one probe", counts)
	}
	got.end()

	if _, err := drain.Request(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, ok := refresh.beginProbe(ctx, modelCatalogTarget{}); ok {
		t.Fatal("a canceled wait started a probe")
	}
}
