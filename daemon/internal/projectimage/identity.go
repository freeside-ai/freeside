package projectimage

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const maxRepositoryIdentityBytes = 1 << 20

type repositoryResolver interface {
	Verify(context.Context, string, int64) error
}

type githubRepositoryResolver struct {
	client   *http.Client
	endpoint string
}

func (r githubRepositoryResolver) Verify(
	ctx context.Context,
	repository string,
	repositoryID int64,
) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimSuffix(r.endpoint, "/")+"/repos/"+repository, nil)
	if err != nil {
		return fmt.Errorf("construct repository identity request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", "freeside-project-image-builder")
	response, err := r.client.Do(request)
	if err != nil {
		return fmt.Errorf("resolve canonical repository identity: %w", err)
	}
	defer response.Body.Close() //nolint:errcheck // read/validation error is primary
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("resolve canonical repository identity: GitHub returned %s",
			response.Status)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxRepositoryIdentityBytes+1))
	if err != nil {
		return fmt.Errorf("read canonical repository identity: %w", err)
	}
	if len(body) > maxRepositoryIdentityBytes {
		return fmt.Errorf("canonical repository identity response exceeds %d bytes",
			maxRepositoryIdentityBytes)
	}
	var resolved struct {
		ID       int64  `json:"id"`
		FullName string `json:"full_name"`
	}
	if err := decodeStrictJSON(body, &resolved); err != nil {
		return fmt.Errorf("decode canonical repository identity: %w", err)
	}
	if resolved.ID != repositoryID || resolved.FullName != repository {
		return fmt.Errorf("repository %q id %d resolves to %q id %d: %w",
			repository, repositoryID, resolved.FullName, resolved.ID, ErrInvalidRequest)
	}
	return nil
}

type trustedRepositoryResolver struct{}

func (trustedRepositoryResolver) Verify(context.Context, string, int64) error { return nil }

func defaultRepositoryResolver() repositoryResolver {
	return githubRepositoryResolver{
		client:   &http.Client{Timeout: 15 * time.Second},
		endpoint: "https://api.github.com",
	}
}
