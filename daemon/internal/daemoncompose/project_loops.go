package daemoncompose

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// repositoryLoop is one repository's inputs to the loops the daemon runs
// per repository (#3210): image upkeep, its tip trigger, and its host-fault
// card. One repository's loop never waits on another's.
type repositoryLoop struct {
	Repo         string
	RepositoryID int64
	BaseRef      string
	// Project is the enabled project that names the repository, empty for a
	// composition from flags.
	Project domain.ProjectID
	// Projects are the projects upkeep files adoption cards under: the
	// label-intake projects on the repository in configuration order, then
	// its manual-submission projects in ProjectID order.
	Projects []domain.ProjectID
	// Manual are the repository's manual-submission projects; the first in
	// ProjectID order takes the repository's image repair tasks
	// (ImageRepairProject).
	Manual map[domain.ProjectID]engine.ManualInitiator
	// Policy is the policy upkeep bounds a dependency change on the
	// repository with (repositoryUpkeepPolicy).
	Policy domain.ResolvedPolicy
}

// cardProject is the project the repository's image-upkeep host-fault card
// is filed under, so each repository's healthy pass clears only its own
// card: the enabled project, else the first project on the repository, else
// the scheduler's system project for a composition that names none.
func (l repositoryLoop) cardProject() domain.ProjectID {
	switch {
	case l.Project != "":
		return l.Project
	case len(l.Projects) != 0:
		return l.Projects[0]
	default:
		return SchedulerSystemProjectID
	}
}

// loopRepository is a repository the daemon serves and the branch its loops
// read the tip of.
type loopRepository struct {
	Repo         string
	RepositoryID int64
	BaseRef      string
	Project      domain.ProjectID
}

// boundManualProject is a manual-submission project and the repository its
// projects binding names; RepositoryID is zero for a project with no
// binding yet.
type boundManualProject struct {
	ID           domain.ProjectID
	RepositoryID int64
	Initiator    engine.ManualInitiator
}

// repositoryLoops turns the served repositories and the configured projects
// into one loop entry per repository, in the order repositories names them.
// A repository named twice must name the same branch. A label-intake or
// manual project joins the entry for its repository, and one on a
// repository the daemon does not serve joins none. A manual project with no
// binding yet joins the first entry, as every configured project did when
// the daemon served one repository; its later binding must name that
// repository, or the store refuses its daemon-origin work.
func repositoryLoops(
	repositories []loopRepository, intake []IntakeInitiator, manual []boundManualProject,
) ([]repositoryLoop, error) {
	var loops []repositoryLoop
	index := map[int64]int{}
	for _, repository := range repositories {
		at, seen := index[repository.RepositoryID]
		if !seen {
			index[repository.RepositoryID] = len(loops)
			loops = append(loops, repositoryLoop{
				Repo: repository.Repo, RepositoryID: repository.RepositoryID, BaseRef: repository.BaseRef,
				Project: repository.Project, Manual: map[domain.ProjectID]engine.ManualInitiator{},
			})
			continue
		}
		if loops[at].Repo != repository.Repo || loops[at].BaseRef != repository.BaseRef {
			return nil, fmt.Errorf("repository %d is served as %s@%s and %s@%s; one repository has one branch",
				repository.RepositoryID, loops[at].Repo, loops[at].BaseRef, repository.Repo, repository.BaseRef)
		}
		if loops[at].Project == "" {
			loops[at].Project = repository.Project
		}
	}
	for _, initiator := range intake {
		if at, ok := index[initiator.RepositoryID]; ok && !slices.Contains(loops[at].Projects, initiator.ProjectID) {
			loops[at].Projects = append(loops[at].Projects, initiator.ProjectID)
		}
	}
	manual = slices.SortedFunc(slices.Values(manual), func(a, b boundManualProject) int {
		return cmp.Compare(a.ID, b.ID)
	})
	for _, project := range manual {
		at, ok := index[project.RepositoryID]
		if project.RepositoryID == 0 && len(loops) != 0 {
			at, ok = 0, true
		}
		if !ok {
			continue
		}
		loops[at].Manual[project.ID] = project.Initiator
		if !slices.Contains(loops[at].Projects, project.ID) {
			loops[at].Projects = append(loops[at].Projects, project.ID)
		}
	}
	for at := range loops {
		policy, err := repositoryUpkeepPolicy(loops[at].Manual)
		if err != nil {
			return nil, fmt.Errorf("repository %s: %w", loops[at].Repo, err)
		}
		loops[at].Policy = policy
	}
	return loops, nil
}

// tipWatches are the served repositories' tip watches, one per repository
// in loop order.
func tipWatches(loops []repositoryLoop) []repositoryTipWatch {
	watches := make([]repositoryTipWatch, 0, len(loops))
	for _, loop := range loops {
		watches = append(watches, repositoryTipWatch{
			Repo: loop.Repo, RepositoryID: loop.RepositoryID, Branch: loop.BaseRef,
		})
	}
	return watches
}

// bindManualProjects reads the repository each manual-submission project's
// projects binding names, so repositoryLoops can group the projects by
// repository. A project with no binding yet keeps a zero repository.
func bindManualProjects(
	ctx context.Context, st *store.Store, projects map[domain.ProjectID]engine.ManualInitiator,
) ([]boundManualProject, error) {
	bound := make([]boundManualProject, 0, len(projects))
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		for _, id := range slices.Sorted(maps.Keys(projects)) {
			project, err := tx.GetProject(ctx, id)
			switch {
			case errors.Is(err, store.ErrNotFound):
				bound = append(bound, boundManualProject{ID: id, Initiator: projects[id]})
			case err != nil:
				return err
			default:
				bound = append(bound, boundManualProject{
					ID: id, RepositoryID: project.RepositoryID, Initiator: projects[id],
				})
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the manual-submission projects' repositories: %w", err)
	}
	return bound, nil
}

// repositoryUpkeepPolicy is the policy image upkeep bounds a dependency
// change on one repository with: the registry set that repository's
// manual-submission projects declare. Upkeep keeps one image per
// repository, so projects on one repository that declare different
// registry sets leave no single answer, and the daemon refuses to start
// rather than pick one; projects on different repositories may differ. No
// declaration is a policy with no registry set, under which upkeep refuses
// any lockfile change.
//
// The result is not a run's policy and is never persisted, so it carries
// only keys: the gate and the room sizes read nothing else, and
// NewResolvedPolicy refuses the empty key set a config without a
// declaration yields.
func repositoryUpkeepPolicy(projects map[domain.ProjectID]engine.ManualInitiator) (domain.ResolvedPolicy, error) {
	var keys []domain.PolicyKey
	for _, projectID := range slices.Sorted(maps.Keys(projects)) {
		for _, key := range projects[projectID].PolicyKeys {
			if key.Key != domain.RegistrySetPolicyKey {
				continue
			}
			if len(keys) != 0 && keys[0].Value != key.Value {
				return domain.ResolvedPolicy{}, fmt.Errorf(
					"manual submission config project %q declares a %s other than another project's on the same repository; image upkeep keeps one image per repository and needs one",
					projectID, domain.RegistrySetPolicyKey)
			}
			keys = []domain.PolicyKey{key}
		}
	}
	return domain.ResolvedPolicy{Keys: keys}, nil
}
