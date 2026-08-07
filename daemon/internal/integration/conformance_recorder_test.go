package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

// storeConformanceRecorder adapts the store's internal write boundary to
// ward.ConformanceRecorder: the wiring the #303/#237 suite runner will use.
// Ward cannot import the store, so this adapter is the composition seam, and
// this test proves the two halves fit.
type storeConformanceRecorder struct{ st *store.Store }

var _ ward.ConformanceRecorder = storeConformanceRecorder{}

func (r storeConformanceRecorder) RecordBackendConformance(
	ctx context.Context, record domain.BackendConformance,
) error {
	return r.st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordBackendConformance(ctx, record)
		return err
	})
}

// TestStoreBackedConformanceRecorder round-trips a record through the
// adapter: what ward's Full pass hands the recorder is what the store's
// admission gate later reconstructs, generation stamped by the append.
func TestStoreBackedConformanceRecorder(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ceiling, ok := domain.ProvableCapabilities(domain.BackendFreshVMReadOnlyVolumeHandoff)
	if !ok {
		t.Fatal("fresh-vm class has no registered ceiling")
	}
	record, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend:             domain.BackendFreshVMReadOnlyVolumeHandoff,
		Outcome:             domain.ConformancePassed,
		ConfigurationDigest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		Capabilities:        ceiling,
		ProvedAt:            time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("NewBackendConformance: %v", err)
	}

	var recorder ward.ConformanceRecorder = storeConformanceRecorder{st: st}
	if err := recorder.RecordBackendConformance(ctx, record); err != nil {
		t.Fatalf("RecordBackendConformance through the adapter: %v", err)
	}

	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		got, found, err := tx.LatestBackendConformance(ctx, record.Backend)
		if err != nil {
			return err
		}
		if !found {
			t.Fatal("recorded conformance not found")
		}
		if got.Generation != 1 || got.Outcome != domain.ConformancePassed ||
			got.ConfigurationDigest != record.ConfigurationDigest {
			t.Errorf("reconstructed record = %+v, want generation 1, passed", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("Read: %v", err)
	}
}
