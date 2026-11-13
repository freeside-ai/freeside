package publish

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
)

// MergeBase reads the merge base of headSHA with the target branch baseRef
// from the forge's compare API. A hand-back cycle reviews its adopted head
// against that commit (issue #3160).
//
// The answer is untrusted: the forge could name any commit. The cycle that
// adopts it fetches the base only as a commit reachable from the target
// branch and proves the head descends from it, so a wrong answer stops the
// cycle rather than narrowing what it reviews. Only the shape is checked
// here. The read is not cached; it runs once per hand-back.
func (r *Reconciler) MergeBase(ctx context.Context, repo, baseRef, headSHA string) (string, error) {
	ref, err := parseRepo(repo)
	if err != nil {
		return "", fmt.Errorf("merge base: %w", err)
	}
	if baseRef == "" || !validCommitSHA(headSHA) {
		return "", fmt.Errorf("merge base: invalid comparison %q...%q", baseRef, headSHA)
	}
	return r.forge.mergeBase(ctx, ref, baseRef, headSHA)
}

func (f *forge) mergeBase(ctx context.Context, repo repoRef, baseRef, headSHA string) (string, error) {
	// One commit per page keeps the commit list small; the response still
	// carries the changed files, under the forge response bound.
	path := "/repos/" + repo.path() + "/compare/" + url.PathEscape(baseRef) + "..." + headSHA + "?per_page=1"
	resp, err := f.do(ctx, http.MethodGet, repo, path, "", nil)
	if err != nil {
		return "", fmt.Errorf("merge base: %w", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("merge base: %w", &APIError{Status: resp.StatusCode, RequestPath: path})
	}
	var decoded struct {
		MergeBaseCommit struct {
			SHA string `json:"sha"`
		} `json:"merge_base_commit"`
	}
	if err := decodeResponse(resp.Body, &decoded); err != nil {
		return "", fmt.Errorf("merge base: decode response: %w", err)
	}
	if !validCommitSHA(decoded.MergeBaseCommit.SHA) {
		return "", errors.New("merge base: response names no commit")
	}
	return decoded.MergeBaseCommit.SHA, nil
}
