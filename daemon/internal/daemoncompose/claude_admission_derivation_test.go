package daemoncompose

import (
	"errors"
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
