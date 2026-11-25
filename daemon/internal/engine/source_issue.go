package engine

import (
	"context"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// managedSourceIssue returns the issue a submitted task's text names when,
// with surrounding whitespace trimmed, the text is exactly one canonical
// GitHub issue URL in the project's managed repository, and nil otherwise.
// The reference carries the project's repository name and ID, never the
// submitted spelling, and the owner/name comparison ignores case as the
// research fetcher's scope check does. A URL for another repository, or a
// project with no registered repository, names no issue. Both callers run
// before the specifier's admission, so they rely on daemon start binding the
// configured project (#3592); a daemon composed from flags binds none until
// its first admission. Submission and triage both call it, so they agree
// about which tasks are issue-backed while the row stands as it did at
// submission. A task submitted before any start bound its project (with no
// daemon running, or to a daemon started without a driver) records no
// issue, yet triage after a later start reads the issue it names.
func managedSourceIssue(ctx context.Context, tx *store.ReadTx, projectID domain.ProjectID, text []byte) (*domain.IssueSubjectRef, error) {
	ownerRepo, number, ok := parseSourceIssueURL(TaskSourceIssue(text))
	if !ok {
		return nil, nil
	}
	scope, err := researchScope(ctx, tx, projectID)
	if err != nil {
		return nil, err
	}
	if scope.RepositoryID <= 0 || !strings.EqualFold(ownerRepo, scope.Repo) {
		return nil, nil
	}
	return &domain.IssueSubjectRef{Repo: scope.Repo, RepositoryID: scope.RepositoryID, IssueNumber: number}, nil
}
