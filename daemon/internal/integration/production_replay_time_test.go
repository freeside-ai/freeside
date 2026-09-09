package integration_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestProductionReplaySeparatesCommitAndCompletionTime(t *testing.T) {
	for _, completionDelay := range []time.Duration{0, time.Minute} {
		t.Run(completionDelay.String(), func(t *testing.T) {
			p := newProductionPublicationHarness(t, "")
			record := p.startExecutionExport(t, p.replay.HeadSHA)
			record.RecordedAt = record.RecordedAt.Add(completionDelay)
			p.now = record.RecordedAt.Add(time.Minute)
			for range 2 {
				if err := engine.RecordProductionExecutionExport(p.ctx, p.store, record, p.replay); err != nil {
					t.Fatal(err)
				}
			}
			checkpoint := filepath.Join(t.TempDir(), "completion.db")
			if err := p.store.Checkpoint(p.ctx, checkpoint); err != nil {
				t.Fatal(err)
			}
			if _, err := p.store.Restore(p.ctx, checkpoint); err != nil {
				t.Fatal(err)
			}
			p.workflow = p.newEngine(t, productionCrashSeams{}, true)
			if result, err := p.reconcileLanes(); err != nil || result.ReadyItemsCreated != 1 {
				t.Fatalf("publication after restore = %#v, %v", result, err)
			}
			p.assertReady(t)
			if err := p.store.Read(p.ctx, func(tx *store.ReadTx) error {
				got, err := tx.GetExecutionExportRecord(p.ctx, p.invocation)
				if err == nil && (!got.RecordedAt.Equal(record.RecordedAt) || got.HeadSHA != record.HeadSHA) {
					return errors.New("publication changed the export time or head")
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			pushes := p.transport.pushCount()
			if result, err := p.reconcileLanes(); err != nil || result.ResultsAccepted != 0 || result.PublicationTasksCompleted != 0 {
				t.Fatalf("terminal replay = %#v, %v", result, err)
			}
			if p.transport.pushCount() != pushes {
				t.Fatal("terminal replay repeated publication")
			}
		})
	}
}

func TestProductionReplayChangedCommitTimeStillRefusesPublication(t *testing.T) {
	p := newProductionPublicationHarness(t, "")
	record := p.startExecutionExport(t, p.replay.HeadSHA)
	record.RecordedAt = record.RecordedAt.Add(2 * time.Minute)
	p.now = record.RecordedAt.Add(time.Minute)
	// This date precedes completion but cannot reconstruct the original head.
	p.replay.ImportOptions.CommitDate = p.replay.ImportOptions.CommitDate.Add(time.Minute)
	if err := engine.RecordProductionExecutionExport(p.ctx, p.store, record, p.replay); err != nil {
		t.Fatal(err)
	}
	if _, err := p.reconcileLanes(); err == nil || !strings.Contains(err.Error(), "reconstructed execution export produced head") {
		t.Fatalf("changed replay date was not refused: %v", err)
	}
	if refs, prs := p.forge.counts(); refs != 0 || prs != 0 {
		t.Fatalf("changed replay date published: %d refs, %d PRs", refs, prs)
	}
	if p.room.runs != 0 {
		t.Fatal("verification ran before replay identity was established")
	}
}
