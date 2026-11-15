package daemoncompose

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/claude"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func recordTipObservation(t *testing.T, st *store.Store, observation domain.RepositoryTipObservation) {
	t.Helper()
	if err := st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		return tx.RecordRepositoryTipObservation(t.Context(), observation)
	}); err != nil {
		t.Fatal(err)
	}
}

// The derived base is the tip the tip watch stored for the configured
// repository and branch, read on every attempt, and an attempt with no such
// tip holds as input_unavailable instead of starting anywhere.
func TestClaudeAdmissionDerivationReadsTheStoredTip(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, t.TempDir()+"/freesided.db", store.Options{})
	t.Cleanup(func() { _ = st.Close() })
	cfg := ClaudeDriverConfig{Repo: "owner/repo", RepositoryID: 7, BaseRef: "main"}
	derive := claudeAdmissionDerivation(st, cfg)

	if _, _, err := derive(t.Context(), "inv-1"); !errors.Is(err, exec.ErrInputUnavailable) {
		t.Fatalf("no stored tip: err = %v, want ErrInputUnavailable", err)
	}

	observed := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	recordTipObservation(t, st, domain.RepositoryTipObservation{
		RepositoryID: 7, Repository: "owner/repo", Branch: "release",
		TipSHA: "1111111111111111111111111111111111111111", ObservedAt: observed,
	})
	if _, _, err := derive(t.Context(), "inv-1"); !errors.Is(err, exec.ErrInputUnavailable) {
		t.Fatalf("tip of another branch: err = %v, want ErrInputUnavailable", err)
	}

	for _, sha := range []string{"2222222222222222222222222222222222222222", "3333333333333333333333333333333333333333"} {
		recordTipObservation(t, st, domain.RepositoryTipObservation{
			RepositoryID: 7, Repository: "owner/repo", Branch: "main", TipSHA: sha, ObservedAt: observed,
		})
		workspace, base, err := derive(t.Context(), "inv-1")
		if err != nil {
			t.Fatal(err)
		}
		want := domain.BaseRevision{Repo: "owner/repo", RepositoryID: 7, BaseRef: "main", BaseSHA: sha}
		if base != want || workspace != claude.WorkspaceFor("inv-1") {
			t.Fatalf("derived %q at %+v, want %q at %+v", workspace, base, claude.WorkspaceFor("inv-1"), want)
		}
	}
}

// Start reads the tip and stores it, replacing a stale stored tip and keeping
// the tip upkeep last handled. A failed read falls back to a stored tip, and
// fails the start only when none is stored.
func TestStartupRepositoryTip(t *testing.T) {
	t.Parallel()
	cfg := ClaudeDriverConfig{Repo: "owner/repo", RepositoryID: 7, BaseRef: "main"}
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	stale, remote := "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	logger := slog.New(slog.DiscardHandler)
	newStore := func(t *testing.T) *store.Store {
		st := storetest.Open(t, t.TempDir()+"/freesided.db", store.Options{})
		t.Cleanup(func() { _ = st.Close() })
		return st
	}
	type read struct{ repo, branch string }
	observer := func(sha string, err error) (func(context.Context, string, string) (string, error), *[]read) {
		var reads []read
		return func(_ context.Context, repo, branch string) (string, error) {
			reads = append(reads, read{repo, branch})
			return sha, err
		}, &reads
	}
	stored := func(t *testing.T, st *store.Store) domain.RepositoryTipObservation {
		var observation domain.RepositoryTipObservation
		if err := st.Read(t.Context(), func(tx *store.ReadTx) error {
			var err error
			observation, _, err = tx.GetRepositoryTipObservation(t.Context(), 7)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return observation
	}
	want := domain.BaseRevision{Repo: "owner/repo", RepositoryID: 7, BaseRef: "main", BaseSHA: remote}

	for name, branch := range map[string]string{
		"a stale stored tip is replaced": "main",
		// A row for another branch is not this start's tip.
		"a missing tip is stored": "release",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st := newStore(t)
			recordTipObservation(t, st, domain.RepositoryTipObservation{
				RepositoryID: 7, Repository: "owner/repo", Branch: branch, TipSHA: stale, ObservedAt: now,
				UpkeepTipSHA: stale,
			})
			observe, reads := observer(remote, nil)
			tip, err := startupRepositoryTip(t.Context(), st, cfg, observe, now, logger)
			if err != nil || tip != want || len(*reads) != 1 || (*reads)[0] != (read{"owner/repo", "main"}) {
				t.Fatalf("start = (%+v, %v) after reads %v, want %+v after one read of owner/repo@main", tip, err, *reads, want)
			}
			// Upkeep's mark stays, so upkeep still handles the new tip.
			if got := stored(t, st); got.Branch != "main" || got.TipSHA != remote || got.UpkeepTipSHA != stale {
				t.Fatalf("stored %+v, want main at %s with upkeep still at %s", got, remote, stale)
			}
			if derived, err := storedRepositoryTip(t.Context(), st, cfg); err != nil || derived != want {
				t.Fatalf("admission derives %+v, %v, want the tip start stored", derived, err)
			}
		})
	}

	t.Run("a failed read starts at the stored tip", func(t *testing.T) {
		t.Parallel()
		st := newStore(t)
		recordTipObservation(t, st, domain.RepositoryTipObservation{
			RepositoryID: 7, Repository: "owner/repo", Branch: "main", TipSHA: stale, ObservedAt: now,
		})
		observe, _ := observer("", errors.New("ref read failed"))
		tip, err := startupRepositoryTip(t.Context(), st, cfg, observe, now, logger)
		if err != nil || tip.BaseSHA != stale {
			t.Fatalf("start = (%+v, %v), want the stored tip %s", tip, err, stale)
		}
	})

	t.Run("a failed read with no stored tip fails the start", func(t *testing.T) {
		t.Parallel()
		st := newStore(t)
		failure := errors.New("ref read failed")
		observe, _ := observer("", failure)
		if _, err := startupRepositoryTip(t.Context(), st, cfg, observe, now, logger); !errors.Is(err, failure) {
			t.Fatalf("start = %v, want the read failure", err)
		}
		if _, err := storedRepositoryTip(t.Context(), st, cfg); !errors.Is(err, exec.ErrInputUnavailable) {
			t.Fatalf("after a failed start the stored tip = %v, want none", err)
		}
	})
}
