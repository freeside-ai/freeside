package store_test

import (
	"context"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestDisplayNamesForFallsBackToStableIdentifiers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := openStore(t, store.Options{})
	runID := domain.RunID("run-missing")

	var got *domain.DisplayNames
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		got, err = tx.DisplayNamesFor(ctx, "project-missing", domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(runID), RunID: &runID,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want := &domain.DisplayNames{
		Project: domain.DisplayName{
			Text: "project-missing", Source: domain.DisplayNameSourceIdentifier,
		},
		WorkUnit: domain.DisplayName{
			Text: "run-missing", Source: domain.DisplayNameSourceIdentifier,
		},
	}
	if got == nil || *got != *want {
		t.Fatalf("display names = %#v, want %#v", got, want)
	}
}
