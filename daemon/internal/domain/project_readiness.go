package domain

import (
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/credentialpattern"
)

// ProjectReadinessCheck is one per-project check the daemon evaluates before
// it admits unattended work for the project. "Readiness" here means project
// readiness, never pull request readiness (ReadinessVerdict).
type ProjectReadinessCheck string

const (
	// ProjectReadinessTrustProfile: the project's active trust profile
	// passes the rule unattended admission applies.
	ProjectReadinessTrustProfile ProjectReadinessCheck = "trust_profile"
	// ProjectReadinessReviewConfiguration: the profile approves the review
	// configuration this daemon runs.
	ProjectReadinessReviewConfiguration ProjectReadinessCheck = "review_configuration"
	// ProjectReadinessRepositoryBase: the base branch resolves on GitHub and
	// the repository is the one the project's settings name.
	ProjectReadinessRepositoryBase ProjectReadinessCheck = "repository_base"
	// ProjectReadinessPublicationAuthority: exactly one registered App has
	// installation authority over the repository.
	ProjectReadinessPublicationAuthority ProjectReadinessCheck = "publication_authority"
	// ProjectReadinessWriterImage: the approved recipe's preparation
	// resolves, and an image recorded for the current base was built from
	// that recipe.
	ProjectReadinessWriterImage ProjectReadinessCheck = "writer_image"
)

// AllProjectReadinessChecks lists every check in the fixed order a record
// holds its results in; it is the single registration point.
var AllProjectReadinessChecks = []ProjectReadinessCheck{
	ProjectReadinessTrustProfile, ProjectReadinessReviewConfiguration, ProjectReadinessRepositoryBase,
	ProjectReadinessPublicationAuthority, ProjectReadinessWriterImage,
}

func (c ProjectReadinessCheck) valid() bool {
	switch c {
	case ProjectReadinessTrustProfile, ProjectReadinessReviewConfiguration, ProjectReadinessRepositoryBase,
		ProjectReadinessPublicationAuthority, ProjectReadinessWriterImage:
		return true
	default:
		return false
	}
}

// StoreLocal reports whether the check reads only the daemon's own store and
// configuration, so it can be re-run inside a store transaction before every
// unattended dispatch. The others call GitHub or read image state and run on
// the evaluator's schedule. The switch omits default, so a new check decides
// which it is.
func (c ProjectReadinessCheck) StoreLocal() bool {
	switch c {
	case ProjectReadinessTrustProfile, ProjectReadinessReviewConfiguration:
		return true
	case ProjectReadinessRepositoryBase, ProjectReadinessPublicationAuthority, ProjectReadinessWriterImage:
		return false
	}
	return false
}

// ProjectReadinessStatus is one check's result.
type ProjectReadinessStatus string

const (
	ProjectReadinessPassed ProjectReadinessStatus = "passed"
	ProjectReadinessFailed ProjectReadinessStatus = "failed"
	// ProjectReadinessUnavailable is a check that could not be evaluated and
	// has no earlier pass for the same project and revisions to keep. It is
	// not a pass.
	ProjectReadinessUnavailable ProjectReadinessStatus = "unavailable"
)

// AllProjectReadinessStatuses lists every valid ProjectReadinessStatus; it is
// the single registration point.
var AllProjectReadinessStatuses = []ProjectReadinessStatus{
	ProjectReadinessPassed, ProjectReadinessFailed, ProjectReadinessUnavailable,
}

func (s ProjectReadinessStatus) valid() bool {
	switch s {
	case ProjectReadinessPassed, ProjectReadinessFailed, ProjectReadinessUnavailable:
		return true
	default:
		return false
	}
}

// maxProjectReadinessDetail bounds a result's detail, which the card shows.
const maxProjectReadinessDetail = 1024

// ProjectReadinessResult is one check's outcome. Detail says why a check did
// not pass; it is empty on a pass. CheckedAt is when the outcome was last
// observed, which a kept pass carries over from the evaluation that saw it.
type ProjectReadinessResult struct {
	Check     ProjectReadinessCheck  `json:"check"`
	Status    ProjectReadinessStatus `json:"status"`
	Detail    string                 `json:"detail"`
	CheckedAt time.Time              `json:"checked_at"`
}

// ProjectReadiness is the daemon's record of whether one project, under the
// settings revisions it was composed from, is ready to run unattended work.
// Settings is nil for a daemon composed from a settings file. The record is
// an observation, never authority: admission, import, verification, and
// publication keep every check they run, and the gate that reads it re-runs
// the store-local checks.
type ProjectReadiness struct {
	ProjectID   ProjectID                `json:"project_id"`
	Settings    *SettingsRevisions       `json:"settings"`
	Results     []ProjectReadinessResult `json:"results"`
	EvaluatedAt time.Time                `json:"evaluated_at"`
}

// Validate reports whether the record is well-formed: one result per check
// in AllProjectReadinessChecks order, a detail on every result that did not
// pass and none on a pass, UTC times, and no credential in any detail.
func (r ProjectReadiness) Validate() error {
	if r.ProjectID == "" {
		return fmt.Errorf("project readiness project_id: %w", ErrEmptyID)
	}
	if r.Settings != nil {
		if err := r.Settings.Validate(); err != nil {
			return fmt.Errorf("project %q readiness: %w", r.ProjectID, err)
		}
	}
	if err := validateUTC("project readiness evaluated_at", r.EvaluatedAt); err != nil {
		return fmt.Errorf("project %q: %w", r.ProjectID, err)
	}
	if len(r.Results) != len(AllProjectReadinessChecks) {
		return fmt.Errorf("project %q readiness holds %d results, want %d: %w",
			r.ProjectID, len(r.Results), len(AllProjectReadinessChecks), ErrProjectReadinessInconsistent)
	}
	for i, result := range r.Results {
		if result.Check != AllProjectReadinessChecks[i] {
			return fmt.Errorf("project %q readiness result %d is %q, want %q: %w",
				r.ProjectID, i, result.Check, AllProjectReadinessChecks[i], ErrProjectReadinessInconsistent)
		}
		if err := result.validate(); err != nil {
			return fmt.Errorf("project %q readiness %s: %w", r.ProjectID, result.Check, err)
		}
	}
	return nil
}

func (r ProjectReadinessResult) validate() error {
	if !r.Status.valid() {
		return fmt.Errorf("status %q: %w", r.Status, ErrProjectReadinessInconsistent)
	}
	if (r.Status == ProjectReadinessPassed) != (r.Detail == "") {
		return fmt.Errorf("status %q with detail %q: %w", r.Status, r.Detail, ErrProjectReadinessInconsistent)
	}
	if len(r.Detail) > maxProjectReadinessDetail {
		return fmt.Errorf("detail of %d bytes: %w", len(r.Detail), ErrProjectReadinessInconsistent)
	}
	for _, rule := range credentialpattern.All() {
		if rule.Pattern.MatchString(r.Detail) {
			return fmt.Errorf("detail carries %s: %w", rule.Name, ErrProjectReadinessInconsistent)
		}
	}
	return validateUTC("checked_at", r.CheckedAt)
}

func validateUTC(field string, at time.Time) error {
	if at.IsZero() {
		return fmt.Errorf("%s: %w", field, ErrMissingTimestamp)
	}
	if at.Location() != time.UTC {
		return fmt.Errorf("%s: %w", field, ErrTimestampNotUTC)
	}
	return nil
}

// Ready reports whether every check passed.
func (r ProjectReadiness) Ready() bool {
	return len(r.NotPassed()) == 0
}

// NotPassed returns the results that did not pass, in check order.
func (r ProjectReadiness) NotPassed() []ProjectReadinessResult {
	var out []ProjectReadinessResult
	for _, result := range r.Results {
		if result.Status != ProjectReadinessPassed {
			out = append(out, result)
		}
	}
	return out
}

// Result returns the record's result for one check.
func (r ProjectReadiness) Result(check ProjectReadinessCheck) (ProjectReadinessResult, bool) {
	for _, result := range r.Results {
		if result.Check == check {
			return result, true
		}
	}
	return ProjectReadinessResult{}, false
}

// SameSettings reports whether two optional revision pairs name the same
// settings; nil names a daemon composed from a settings file.
func SameSettings(a, b *SettingsRevisions) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
