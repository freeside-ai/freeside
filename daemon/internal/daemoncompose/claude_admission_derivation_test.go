package daemoncompose

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
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

// The derived base is the tip the tip watch stored for the task's project's
// repository and branch, read on every attempt. An attempt with no such tip
// holds as input_unavailable instead of starting anywhere, and one whose
// stored tip names another branch (a project's base_ref edit the tip watch
// hasn't followed) holds as a repository identity mismatch.
func TestClaudeAdmissionDerivationReadsTheStoredTip(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, t.TempDir()+"/freesided.db", store.Options{})
	t.Cleanup(func() { _ = st.Close() })
	project := engine.ProjectAuthority{Repo: "owner/repo", RepositoryID: 7, BaseRef: "main"}
	derive := func(ctx context.Context, id domain.InvocationID) (string, domain.BaseRevision, error) {
		return claudeAdmissionDerivation(st)(ctx, id, project)
	}

	if _, _, err := derive(t.Context(), "inv-1"); !errors.Is(err, exec.ErrInputUnavailable) {
		t.Fatalf("no stored tip: err = %v, want ErrInputUnavailable", err)
	}

	observed := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	recordTipObservation(t, st, domain.RepositoryTipObservation{
		RepositoryID: 7, Repository: "owner/repo", Branch: "release",
		TipSHA: "1111111111111111111111111111111111111111", ObservedAt: observed,
	})
	if _, _, err := derive(t.Context(), "inv-1"); !errors.Is(err, domain.ErrRepositoryIdentityMismatch) {
		t.Fatalf("tip of another branch: err = %v, want ErrRepositoryIdentityMismatch", err)
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

// Each task derives its base from its own project's repository: with two
// projects' tips stored, a task of one is never based on the other's.
func TestClaudeAdmissionDerivationReadsTheTasksProject(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, t.TempDir()+"/freesided.db", store.Options{})
	t.Cleanup(func() { _ = st.Close() })
	observed := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	projects := map[string]engine.ProjectAuthority{
		"1111111111111111111111111111111111111111": {ProjectID: "project-a", Repo: "owner/a", RepositoryID: 7, BaseRef: "main"},
		"2222222222222222222222222222222222222222": {ProjectID: "project-b", Repo: "owner/b", RepositoryID: 8, BaseRef: "trunk"},
	}
	for sha, project := range projects {
		recordTipObservation(t, st, domain.RepositoryTipObservation{
			RepositoryID: project.RepositoryID, Repository: project.Repo, Branch: project.BaseRef,
			TipSHA: sha, ObservedAt: observed,
		})
	}
	derive := claudeAdmissionDerivation(st)
	for sha, project := range projects {
		_, base, err := derive(t.Context(), "inv-1", project)
		want := domain.BaseRevision{Repo: project.Repo, RepositoryID: project.RepositoryID, BaseRef: project.BaseRef, BaseSHA: sha}
		if err != nil || base != want {
			t.Fatalf("%s derives (%+v, %v), want %+v", project.ProjectID, base, err, want)
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
			if derived, err := storedRepositoryTip(t.Context(), st, cfg.Repo, cfg.RepositoryID, cfg.BaseRef); err != nil || derived != want {
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
		if _, err := storedRepositoryTip(t.Context(), st, cfg.Repo, cfg.RepositoryID, cfg.BaseRef); !errors.Is(err, exec.ErrInputUnavailable) {
			t.Fatalf("after a failed start the stored tip = %v, want none", err)
		}
	})
}

// A start whose configuration names a project binds it to the configured
// repository, so submission and triage find it before any admission; a
// configuration composed from flags names no project and binds none.
func TestBindStartupProject(t *testing.T) {
	t.Parallel()
	for name, project := range map[string]domain.ProjectID{"named project": "project-1", "no project": ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			st := storetest.Open(t, t.TempDir()+"/freesided.db", store.Options{})
			t.Cleanup(func() { _ = st.Close() })
			cfg := ClaudeDriverConfig{Repo: "owner/repo", RepositoryID: 7, BaseRef: "main", ProjectID: project}
			if err := bindStartupProject(t.Context(), st, cfg); err != nil {
				t.Fatal(err)
			}
			var got domain.Project
			err := st.Read(t.Context(), func(tx *store.ReadTx) error {
				var err error
				got, err = tx.GetProject(t.Context(), "project-1")
				return err
			})
			if project == "" {
				if !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("no configured project: read = (%+v, %v), want no row", got, err)
				}
				return
			}
			if want := (domain.Project{ID: "project-1", Repo: "owner/repo", RepositoryID: 7}); err != nil || got != want {
				t.Fatalf("bound project = (%+v, %v), want %+v", got, err, want)
			}
		})
	}
}

// A proof pass seeds at the stored tip of its image's own repository, so
// one repository's image is never proved on another's tree; an image with
// no record seeds at the configured repository's tip. Another repository's
// missing tip is input_unavailable and one stored under another name is an
// identity mismatch, as for the configured repository.
func TestConformanceBaseIsTheImagesRepositoryTip(t *testing.T) {
	t.Parallel()
	served := selectorImage(t, strings.Repeat("a", 40), "1", "d")
	other, err := domain.NewProjectImage(domain.ProjectImageInput{
		Repository: "owner/other", RepositoryID: 43, CommitSHA: strings.Repeat("e", 40),
		RecipeDigest: served.RecipeDigest, PreparationCommand: served.PreparationCommand,
		BaseImageRef: served.BaseImageRef,
		ImageRef:     domain.ImageRef("ghcr.io/owner/other@sha256:" + strings.Repeat("f", 64)),
		Environment:  served.Environment,
	})
	if err != nil {
		t.Fatal(err)
	}
	st := selectorStore(t, served, other)
	cfg := ClaudeDriverConfig{Repo: selectorRepo, RepositoryID: selectorRepositoryID, BaseRef: "main"}
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	recordTipObservation(t, st, domain.RepositoryTipObservation{
		RepositoryID: selectorRepositoryID, Repository: selectorRepo, Branch: "main", TipSHA: selectorBase, ObservedAt: now,
	})
	if _, err := conformanceBase(t.Context(), st, cfg, other.ImageRef); !errors.Is(err, exec.ErrInputUnavailable) {
		t.Fatalf("another repository with no stored tip = %v, want input_unavailable", err)
	}
	otherTip := strings.Repeat("9", 40)
	recordTipObservation(t, st, domain.RepositoryTipObservation{
		RepositoryID: 43, Repository: "owner/renamed", Branch: "trunk", TipSHA: otherTip, ObservedAt: now,
	})
	if _, err := conformanceBase(t.Context(), st, cfg, other.ImageRef); !errors.Is(err, domain.ErrRepositoryIdentityMismatch) {
		t.Fatalf("another repository's tip stored under another name = %v, want ErrRepositoryIdentityMismatch", err)
	}
	recordTipObservation(t, st, domain.RepositoryTipObservation{
		RepositoryID: 43, Repository: "owner/other", Branch: "trunk", TipSHA: otherTip, ObservedAt: now.Add(time.Minute),
	})
	for name, test := range map[string]struct {
		image domain.ImageRef
		want  domain.BaseRevision
	}{
		"the configured repository's image": {image: served.ImageRef, want: selectorRevision},
		"an image with no record":           {image: "ghcr.io/owner/agent:latest", want: selectorRevision},
		"another repository's image": {image: other.ImageRef, want: domain.BaseRevision{
			Repo: "owner/other", RepositoryID: 43, BaseRef: "trunk", BaseSHA: otherTip,
		}},
	} {
		if base, err := conformanceBase(t.Context(), st, cfg, test.image); err != nil || base != test.want {
			t.Fatalf("%s seeds at %+v, %v; want %+v", name, base, err, test.want)
		}
	}
}
