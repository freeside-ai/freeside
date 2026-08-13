package domain

import (
	"fmt"
	"strings"
)

// Project is the durable, write-once authority binding a daemon-assigned
// project identity to the one repository it operates on (issue #740). It exists
// so a label-intake admission can verify that an implementation run's project
// belongs to the occurrence's own repository: a Run carries only a ProjectID and
// a ProjectImage carries a RepositoryID but no ProjectID, so before this record
// no store query could answer project -> repository, and a run for one
// repository's project could bind an occurrence in another repository sharing an
// issue number.
//
// The record is immutable for the project's life: no state machine, no update
// path. That durability is what lets the read boundary require it — for an
// authentic binding the projects row can never legitimately be absent, so a
// missing row is corruption to fail closed on, not a transient availability the
// boundary must tolerate (unlike a policy artifact, whose current absence is a
// start-time concern; issue #720 round 11, owner-ratified).
//
// RepositoryID is the forge's canonical numeric identity and is authoritative
// for equality; Repo is the human-facing name, which can be transferred or
// reused. The pair matches the repository identity BaseRevision, ProjectImage,
// and IntakeOccurrence already carry.
type Project struct {
	ID           ProjectID `json:"id"`
	Repo         string    `json:"repo"`
	RepositoryID int64     `json:"repository_id"`
}

// NewProject builds a validated project authority record.
func NewProject(id ProjectID, repo string, repositoryID int64) (Project, error) {
	p := Project{ID: id, Repo: repo, RepositoryID: repositoryID}
	if err := p.Validate(); err != nil {
		return Project{}, err
	}
	return p, nil
}

// Validate is the reconstruction backstop for a project authority record. It
// reuses the repository-name pattern the project-image provenance validates
// (projectRepositoryPattern) and the positive repository-id rule, so a project
// and the images built for it agree on what a repository identity is.
func (p Project) Validate() error {
	if p.ID == "" {
		return fmt.Errorf("project id: %w", ErrEmptyID)
	}
	if !projectRepositoryPattern.MatchString(p.Repo) || strings.Contains(p.Repo, "..") {
		return fmt.Errorf("project repo %q: %w", p.Repo, ErrProjectInvalid)
	}
	if p.RepositoryID <= 0 {
		return fmt.Errorf("project repository_id %d: %w", p.RepositoryID, ErrNonPositive)
	}
	return nil
}
