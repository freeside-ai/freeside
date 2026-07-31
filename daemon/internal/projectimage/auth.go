package projectimage

import (
	"context"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

func repositoryToken(
	ctx context.Context,
	tokens publish.TokenSource,
	repository string,
	repositoryID int64,
) (publish.InstallationToken, error) {
	token, err := tokens.Token(ctx, repository)
	if err != nil {
		return publish.InstallationToken{}, fmt.Errorf("resolve repository token: %w", err)
	}
	if token.Repo != repository ||
		token.RepositoryID != repositoryID ||
		token.RegistrationID <= 0 ||
		token.InstallationID <= 0 ||
		token.Token.Reveal() == "" ||
		token.Permissions != publish.WorkflowAuditPermissions {
		return publish.InstallationToken{}, fmt.Errorf(
			"repository token is not exact read-only authority for %q id %d: %w",
			repository, repositoryID, ErrInvalidRequest)
	}
	return token, nil
}
