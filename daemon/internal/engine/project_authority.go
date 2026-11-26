package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ProjectAuthority is what a project's current settings grant its tasks: the
// repository a task is admitted, started, imported, and published against,
// the paths it may change, the recipe its images must be built from, the
// hand-back label, and the writer class. The path, repository, review, and
// hand-back gates read it per task from the task's project (#3209), so an
// edit to a project's settings reaches the task's next gate without a
// restart.
type ProjectAuthority struct {
	ProjectID domain.ProjectID
	// SettingsRevision is the project settings revision the values came
	// from, which an admission records; 0 for a fixed authority composed
	// from a settings file or flags.
	SettingsRevision int
	Repo             string
	RepositoryID     int64
	BaseRef          string
	AllowedPaths     []string
	ApprovedRecipe   domain.Digest
	// HandBackLabel is empty when the project names none, which turns
	// hand-back rework off for its pull requests.
	HandBackLabel string
	// WriterClass is the project's writer_class setting, nil when unset
	// (domain.ResolveWriterClass).
	WriterClass *domain.RunnerBackendClass
}

// ProjectAuthoritySource reads a project's current authority inside the
// caller's transaction, so a gate judges the same state as the rest of its
// check. A source refuses, joined with ErrProjectSettingsRefused, a project
// whose settings it can't read or that fail current policy; it never returns
// a partial authority.
type ProjectAuthoritySource func(ctx context.Context, tx *store.ReadTx, project domain.ProjectID) (ProjectAuthority, error)

// ErrProjectSettingsRefused marks a project whose current settings grant its
// tasks nothing: it has no settings record, the record fails the settings
// gate, or it names a repository other than the project's binding. It is a
// configuration verdict, so the gate holds the work instead of failing it,
// and a corrected record releases it.
var ErrProjectSettingsRefused = errors.New("the project's settings grant its tasks no authority")

// FixedProjectAuthority returns a source that answers every project with
// authority: the one project a composition from a settings file or from
// flags serves, so those compositions keep behaving as they did before the
// gates read per project. The installed daemon reads its projects' stored
// settings instead.
func FixedProjectAuthority(authority ProjectAuthority) ProjectAuthoritySource {
	authority = authority.clone()
	return func(context.Context, *store.ReadTx, domain.ProjectID) (ProjectAuthority, error) {
		return authority.clone(), nil
	}
}

func (a ProjectAuthority) clone() ProjectAuthority {
	a.AllowedPaths = slices.Clone(a.AllowedPaths)
	if a.WriterClass != nil {
		class := *a.WriterClass
		a.WriterClass = &class
	}
	return a
}

// projectAuthority reads project's authority through the engine's source in
// its own read. Without a source it returns the zero authority, whose unset
// writer class resolves to the cordon: a composition without one admits
// through its static environment alone, as before.
func (e *Engine) projectAuthority(ctx context.Context, project domain.ProjectID) (ProjectAuthority, error) {
	if e.projectAuthoritySource == nil {
		return ProjectAuthority{}, nil
	}
	var authority ProjectAuthority
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		authority, err = e.projectAuthoritySource(ctx, tx, project)
		return err
	}); err != nil {
		return ProjectAuthority{}, fmt.Errorf("project %q authority: %w", project, err)
	}
	return authority, nil
}

// sameRepository reports whether base names the project's repository,
// repository ID, and base ref.
func (a ProjectAuthority) sameRepository(base domain.BaseRevision) bool {
	return base.Repo == a.Repo && base.RepositoryID == a.RepositoryID && base.BaseRef == a.BaseRef
}

// WithProjectAuthority installs the source the admission and publication
// gates read a task's project authority from.
func WithProjectAuthority(source ProjectAuthoritySource) Option {
	return func(e *Engine) error {
		if source == nil {
			return errors.New("engine project authority: nil source")
		}
		e.projectAuthoritySource = source
		return nil
	}
}
