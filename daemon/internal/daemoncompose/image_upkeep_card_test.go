package daemoncompose

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// Each repository's host-fault card is filed under a project on it, so one
// repository's healthy pass leaves another's fault open. The card a daemon
// filed under the system project before cards were per repository, and one
// filed under a project the daemon no longer serves, are concluded, since no
// repository's pass would conclude them now.
func TestImageUpkeepHostFaultCardsArePerRepository(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, t.TempDir()+"/freesided.db", store.Options{})
	t.Cleanup(func() { _ = st.Close() })
	attention := signet.NewService(st)
	cfg := ClaudeStartupConfig{Claude: &ClaudeDriverConfig{OperatingMode: domain.ModeAttendedDev}}
	for _, orphan := range []domain.ProjectID{SchedulerSystemProjectID, "project-gone"} {
		if err := (operations.Doctor{
			Store: st, Attention: attention, ProjectID: orphan, Mode: domain.ModeAttendedDev,
		}).ConvergeImageUpkeep(ctx, "an old fault"); err != nil {
			t.Fatal(err)
		}
	}
	var doctorLock sync.Mutex
	orphans := &orphanUpkeepCards{
		store: st, attention: attention, cfg: cfg, keep: []domain.ProjectID{"project-a", "project-b"},
	}
	compose := func(loop repositoryLoop) func(context.Context, string) error {
		return composeImageUpkeep(st, attention, nil, cfg, &ClaudeComposition{}, nil, loop, t.TempDir(),
			&doctorLock, orphans).Health
	}
	first := compose(repositoryLoop{Repo: "owner/a", RepositoryID: 1, Project: "project-a"})
	second := compose(repositoryLoop{Repo: "owner/b", RepositoryID: 2, Projects: []domain.ProjectID{"project-b"}})
	for _, health := range []func(context.Context, string) error{first, second} {
		if err := health(ctx, "the project-image build failed"); err != nil {
			t.Fatal(err)
		}
	}
	if err := first(ctx, ""); err != nil {
		t.Fatal(err)
	}
	snapshots, err := attention.ListAttentionItems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	open := map[domain.ProjectID]int{}
	for _, snapshot := range snapshots {
		if item := snapshot.Item; item.Type == domain.AttentionSystemHealth && item.Status == domain.StatusOpen {
			open[item.ProjectID]++
		}
	}
	if len(open) != 1 || open["project-b"] != 1 {
		t.Fatalf("open host-fault cards by project = %v, want only project-b's", open)
	}
}

// The scheduled pass runs each repository's upkeep at once, so a repository
// whose pass is stuck never holds back another's. Run in turn, the first
// repository's tip read would wait forever on the second's, and the bubble
// would report the deadlock.
func TestScheduledImageUpkeepRunsRepositoriesAtOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		secondRead := make(chan struct{})
		errFirst, errSecond := errors.New("first tip unread"), errors.New("second tip unread")
		upkeeps := imageUpkeeps{
			{loop: repositoryLoop{Repo: "owner/a"}, upkeep: &engine.ImageUpkeep{
				ReadTip: func(ctx context.Context) (string, error) {
					<-secondRead
					return "", errFirst
				},
			}},
			{loop: repositoryLoop{Repo: "owner/b"}, upkeep: &engine.ImageUpkeep{
				ReadTip: func(ctx context.Context) (string, error) {
					close(secondRead)
					return "", errSecond
				},
			}},
		}
		err := upkeeps.runScheduled(t.Context(), slog.New(slog.DiscardHandler))
		if !errors.Is(err, errFirst) || !errors.Is(err, errSecond) {
			t.Fatalf("scheduled pass error = %v, want both repositories' failures", err)
		}
	})
}
