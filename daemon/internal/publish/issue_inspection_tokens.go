package publish

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// IssueInspectionTokenSource mints one repository-scoped, issue-read token
// from an exact durable installation-authority binding, the issue-read sibling
// of AuthorityInspectionTokenSource. Prelaunch reads the run's target issue
// with it before spend, while no publication janitor runs: IssueReadTokenSource
// resolves through the janitor gate and so cannot serve that read, and running
// a janitor pass would write janitor state. It has no cache because prelaunch
// mints once per run; every mint passes the ordinary grant validation and the
// minter's audit recorder.
type IssueInspectionTokenSource struct {
	minter         *Minter
	authority      InstallationAuthoritySource
	registrationID int64
	repositoryID   int64
}

// NewIssueInspectionTokenSource wires the narrow prelaunch issue-read source.
func NewIssueInspectionTokenSource(
	minter *Minter,
	authority InstallationAuthoritySource,
	registrationID, repositoryID int64,
) *IssueInspectionTokenSource {
	return &IssueInspectionTokenSource{
		minter: minter, authority: authority,
		registrationID: registrationID, repositoryID: repositoryID,
	}
}

// Format renders the source without its minter, whose keystore holds the
// App's private key; fmt prints values behind unexported fields raw.
func (*IssueInspectionTokenSource) Format(f fmt.State, _ rune) {
	io.WriteString(f, "publish.IssueInspectionTokenSource") //nolint:errcheck,gosec // fmt.State writes cannot be usefully handled
}

// Token revalidates the selected App and durable trusted installation on every
// call, through the same resolution AuthorityInspectionTokenSource uses, then
// mints only IssueInspectionPermissions. Pending installation envelopes never
// authorize this source.
func (s *IssueInspectionTokenSource) Token(
	ctx context.Context,
	repo string,
) (InstallationToken, error) {
	if s == nil {
		return InstallationToken{}, errors.New("issue inspection token: nil or invalid dependency")
	}
	binding, parsed, err := resolveDurableBinding(
		ctx, "issue inspection token", s.minter, s.authority, s.registrationID, s.repositoryID, repo,
	)
	if err != nil {
		return InstallationToken{}, err
	}
	return s.minter.mintResolved(
		ctx, binding, parsed, s.repositoryID,
		IssueInspectionPermissions, issueInspectionPermissionScopes,
	)
}
