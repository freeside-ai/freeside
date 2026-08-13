package domain_test

import (
	"errors"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func projectFixture(t *testing.T) domain.Project {
	t.Helper()
	project, err := domain.NewProject("project-alpha", "owner/repo", 1278475858)
	if err != nil {
		t.Fatalf("NewProject: %v", err)
	}
	return project
}

func TestProjectValidation(t *testing.T) {
	base := projectFixture(t)
	cases := []struct {
		name   string
		mutate func(*domain.Project)
		want   error
	}{
		{"empty id", func(p *domain.Project) { p.ID = "" }, domain.ErrEmptyID},
		{"empty repository", func(p *domain.Project) { p.Repo = "" }, domain.ErrProjectInvalid},
		{"option-shaped owner", func(p *domain.Project) { p.Repo = "-owner/repo" }, domain.ErrProjectInvalid},
		{"unqualified repository", func(p *domain.Project) { p.Repo = "repo" }, domain.ErrProjectInvalid},
		{"traversing repository", func(p *domain.Project) { p.Repo = "owner/../repo" }, domain.ErrProjectInvalid},
		{"missing repository id", func(p *domain.Project) { p.RepositoryID = 0 }, domain.ErrNonPositive},
		{"negative repository id", func(p *domain.Project) { p.RepositoryID = -1 }, domain.ErrNonPositive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := base
			tc.mutate(&got)
			if err := got.Validate(); !errors.Is(err, tc.want) {
				t.Fatalf("Validate() = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestNewProjectValidatesFixture(t *testing.T) {
	project := projectFixture(t)
	if err := project.Validate(); err != nil {
		t.Fatalf("fixture is not valid: %v", err)
	}
	if _, err := domain.NewProject("", "owner/repo", 1); !errors.Is(err, domain.ErrEmptyID) {
		t.Fatalf("NewProject with empty id = %v, want ErrEmptyID", err)
	}
}
