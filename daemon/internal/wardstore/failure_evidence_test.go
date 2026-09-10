package wardstore_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
	"github.com/freeside-ai/freeside/daemon/internal/wardstore"
)

func TestFailureEvidenceDispositionSurvivesStoreReopen(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		ctx := context.Background()
		path := filepath.Join(t.TempDir(), "freeside.db")
		st, err := store.Open(ctx, path, store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		adapters, err := wardstore.New(st)
		if err != nil {
			t.Fatal(err)
		}
		rec := ward.HandoffJournalRecord{RunID: "failed-run", OwnershipToken: strings.Repeat("01", 16), SpecDigest: strings.Repeat("ab", 32), OpenedAt: time.Now().UTC()}
		if err := adapters.Journal.Begin(ctx, rec); err != nil {
			t.Fatal(err)
		}
		digest := ""
		if !unavailable {
			digest = contentaddr.Sum([]byte("diagnostic"))
		}
		if err := adapters.Journal.MarkFailureEvidence(ctx, rec.RunID, digest, unavailable); err == nil {
			t.Fatal("evidence accepted before failed-writer proof")
		}
		if err := adapters.Journal.MarkWriterFailed(ctx, rec.RunID, 1); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if err := adapters.Journal.MarkFailureEvidence(ctx, rec.RunID, digest, unavailable); err != nil {
				t.Fatal(err)
			}
		}
		if err := adapters.Journal.MarkFailureEvidence(ctx, rec.RunID, contentaddr.Sum([]byte("changed")), false); err == nil {
			t.Fatal("replaced immutable disposition")
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		st, err = store.Open(ctx, path, store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		adapters, err = wardstore.New(st)
		if err != nil {
			t.Fatal(err)
		}
		got, err := adapters.Journal.Get(ctx, rec.RunID)
		if err != nil || got.FailureEvidenceDigest != digest || got.FailureEvidenceUnavailable != unavailable || got.WriterFailureStatus == nil || *got.WriterFailureStatus != 1 || got.ExportDir != "" {
			t.Fatalf("reopened failure proof: %+v, %v", got, err)
		}
		if err := adapters.Journal.Close(ctx, rec.RunID, ward.HandoffFailed); err != nil {
			t.Fatal(err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
