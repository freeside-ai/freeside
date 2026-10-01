package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/agentbaseline"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestRecordBaselineAdapterConformance(t *testing.T) {
	st, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	provedAt := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	if err := recordBaselineAdapterConformance(t.Context(), st, provedAt); err != nil {
		t.Fatal(err)
	}
	adapters, err := agentbaseline.Adapters()
	if err != nil {
		t.Fatal(err)
	}
	implementation, err := agentbaseline.Launch(domain.StageNameImplementation)
	if err != nil {
		t.Fatal(err)
	}
	for _, adapter := range adapters {
		var record domain.AdapterConformance
		var found bool
		if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
			var err error
			record, found, err = tx.LatestAdapterConformance(t.Context(), adapter.Digest)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if !found || record.Outcome != domain.ConformancePassed {
			t.Fatalf("adapter %s record = %+v, found %v", adapter.AdapterBuild, record, found)
		}
		err := domain.ValidateAdapterLaunchCoverage(record, adapter.Digest, implementation)
		if covered := err == nil; covered != (adapter.AdapterBuild == agentbaseline.ClaudeWardAdapterBuild) {
			t.Fatalf("implementation launch on %s: %v", adapter.AdapterBuild, err)
		}
	}
}
