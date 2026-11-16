package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func readinessRecord(settings *domain.SettingsRevisions, at time.Time, failed ...domain.ProjectReadinessCheck) domain.ProjectReadiness {
	r := domain.ProjectReadiness{ProjectID: "project-widgets", Settings: settings, EvaluatedAt: at}
	for _, check := range domain.AllProjectReadinessChecks {
		result := domain.ProjectReadinessResult{Check: check, Status: domain.ProjectReadinessPassed, CheckedAt: at}
		for _, f := range failed {
			if f == check {
				result.Status, result.Detail = domain.ProjectReadinessFailed, string(check)+" is broken"
			}
		}
		r.Results = append(r.Results, result)
	}
	return r
}

// TestProjectReadinessIsKeyedByProjectAndRevisions: a record reads back
// under its own key only, a later evaluation replaces it, and a key with no
// record wraps ErrNotFound.
func TestProjectReadinessIsKeyedByProjectAndRevisions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	at := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	stored := &domain.SettingsRevisions{Instance: 3, Project: 2}
	put := func(r domain.ProjectReadiness) {
		t.Helper()
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error { return tx.RecordProjectReadiness(ctx, r) }); err != nil {
			t.Fatal(err)
		}
	}
	read := func(settings *domain.SettingsRevisions) (domain.ProjectReadiness, error) {
		var r domain.ProjectReadiness
		err := st.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			r, err = tx.ProjectReadiness(ctx, "project-widgets", settings)
			return err
		})
		return r, err
	}

	put(readinessRecord(stored, at, domain.ProjectReadinessTrustProfile))
	put(readinessRecord(nil, at))
	got, err := read(stored)
	if err != nil || got.Ready() || got.NotPassed()[0].Check != domain.ProjectReadinessTrustProfile {
		t.Fatalf("stored-settings record = %+v, %v", got, err)
	}
	if got, err := read(nil); err != nil || !got.Ready() || got.Settings != nil {
		t.Fatalf("file-settings record = %+v, %v", got, err)
	}
	if _, err := read(&domain.SettingsRevisions{Instance: 4, Project: 2}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("record for other revisions = %v, want ErrNotFound", err)
	}

	put(readinessRecord(stored, at.Add(time.Minute)))
	if got, err := read(stored); err != nil || !got.Ready() || !got.EvaluatedAt.Equal(at.Add(time.Minute)) {
		t.Fatalf("replaced record = %+v, %v", got, err)
	}

	bad := readinessRecord(stored, at)
	bad.Results = bad.Results[1:]
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error { return tx.RecordProjectReadiness(ctx, bad) }); !errors.Is(err, domain.ErrProjectReadinessInconsistent) {
		t.Fatalf("put of an invalid record = %v, want ErrProjectReadinessInconsistent", err)
	}
}
