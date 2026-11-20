package engine

import (
	"context"
	"encoding/json"
	"net/url"
	"strconv"
	"strings"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// implementationPlanMarker is the first line of a planning session's current
// implementation-plan comment (docs/coordination.md, Stages).
const implementationPlanMarker = "<!-- freeside-implementation-plan:v1 -->"

// planCommentAuthorAssociations are the GitHub author associations whose
// marker counts, the same set the claim protocol counts.
var planCommentAuthorAssociations = map[string]bool{
	"OWNER": true, "MEMBER": true, "COLLABORATOR": true,
}

// issueCommentsFirstPage reports the repository ID and issue number a
// research fetch read when its final URL is the first page of one issue's
// comments. The research fetcher rewrites every in-scope issue URL to the
// repository-ID form, so that is the only spelling accepted. The query must
// be exactly per_page=100, GitHub's largest page: a smaller page, a later
// page, or a filter such as since could hide a second marked comment from
// the exactly-one check.
func issueCommentsFirstPage(finalURL string) (int64, int, bool) {
	parsed, err := url.Parse(finalURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "api.github.com" ||
		parsed.RawPath != "" || parsed.Fragment != "" || parsed.User != nil ||
		parsed.RawQuery != "per_page=100" {
		return 0, 0, false
	}
	segments := strings.Split(parsed.Path, "/")
	if len(segments) != 6 || segments[0] != "" || segments[1] != "repositories" ||
		segments[3] != "issues" || segments[5] != "comments" {
		return 0, 0, false
	}
	repositoryID, err := strconv.ParseInt(segments[2], 10, 64)
	if err != nil || repositoryID <= 0 || strconv.FormatInt(repositoryID, 10) != segments[2] {
		return 0, 0, false
	}
	issue, err := strconv.Atoi(segments[4])
	if err != nil || issue <= 0 || strconv.Itoa(issue) != segments[4] {
		return 0, 0, false
	}
	return repositoryID, issue, true
}

// currentPlanCommentID returns the ID of the one current implementation-plan
// comment on a research fetch of the bound issue's first comments page: a
// comment whose first line is the marker and whose author is the
// repository's owner, a member, or a collaborator. Any other fetch, a failed
// or undecodable one, or a page with no such comment or several, returns
// false. The comment stays untrusted issue text; this decides only whether
// the specifier is told the issue is planned.
func currentPlanCommentID(source specify.ResearchSource, body string, issue domain.IssueSubjectRef) (int64, bool) {
	if source.Status != 200 || source.Failure != "" {
		return 0, false
	}
	repositoryID, number, ok := issueCommentsFirstPage(source.FinalURL)
	if !ok || repositoryID != issue.RepositoryID || number != issue.IssueNumber {
		return 0, false
	}
	// GitHub sends many more fields per comment; only these three decide.
	var comments []struct {
		ID                int64  `json:"id"`
		AuthorAssociation string `json:"author_association"`
		Body              string `json:"body"`
	}
	if err := json.Unmarshal([]byte(body), &comments); err != nil {
		return 0, false
	}
	var found int64
	matches := 0
	for _, comment := range comments {
		firstLine, _, _ := strings.Cut(comment.Body, "\n")
		if comment.ID <= 0 || !planCommentAuthorAssociations[comment.AuthorAssociation] ||
			strings.TrimSuffix(firstLine, "\r") != implementationPlanMarker {
			continue
		}
		found = comment.ID
		matches++
	}
	if matches != 1 {
		return 0, false
	}
	return found, true
}

// specificationPlanCommentID returns the plan comment a research artifact
// names for its specification run's bound issue, or 0 when it names none.
// The artifact's producer is the specifier invocation that requested it,
// which leads to the run and its task. Only a fetch of an issue's first
// comments page reads the store, so every other block renders as before.
func (e *Engine) specificationPlanCommentID(
	ctx context.Context, artifact domain.Artifact, evidence specify.ResearchEvidence,
) (int64, error) {
	if _, _, ok := issueCommentsFirstPage(evidence.Source.FinalURL); !ok {
		return 0, nil
	}
	runID, ok := domain.SpecificationRunIDFromInvocationID(artifact.Provenance.ProducerInvocationID)
	if !ok {
		return 0, nil
	}
	var issue *domain.IssueSubjectRef
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		run, err := tx.GetRun(ctx, runID)
		if err != nil || run.TaskID == "" {
			return err
		}
		task, err := tx.GetTask(ctx, run.TaskID)
		if err != nil {
			return err
		}
		issue = task.Issue()
		return nil
	}); err != nil {
		return 0, err
	}
	if issue == nil {
		return 0, nil
	}
	if id, ok := currentPlanCommentID(evidence.Source, evidence.Body, *issue); ok {
		return id, nil
	}
	return 0, nil
}
