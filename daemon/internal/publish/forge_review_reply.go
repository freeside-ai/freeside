package publish

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// replyComment carries only the identity needed to reconcile a send. Comment
// text is deliberately absent: it cannot prove authorship or reach an error.
type replyComment struct {
	ID          int64
	AuthorID    int64
	InReplyToID int64
	CreatedAt   time.Time
}

type replyCommentWire struct {
	IssueURL string `json:"issue_url"`
	ID       int64  `json:"id"`
	User     struct {
		ID   int64  `json:"id"`
		Type string `json:"type"`
	} `json:"user"`
	InReplyToID int64     `json:"in_reply_to_id"`
	CreatedAt   time.Time `json:"created_at"`
}

func (w replyCommentWire) comment() (replyComment, error) {
	if w.ID <= 0 || w.User.ID <= 0 || w.User.Type == "" || w.CreatedAt.IsZero() || w.InReplyToID < 0 {
		return replyComment{}, errors.New("reply comment: incomplete identity")
	}
	return replyComment{w.ID, w.User.ID, w.InReplyToID, w.CreatedAt.UTC()}, nil
}

type replyCreateResult struct {
	Status  int
	Header  http.Header
	Comment *replyComment
}

// Build before marking dispatching: a construction error proves no send.
func (f *forge) newReviewReplyRequest(ctx context.Context, repo repoRef, number int, root int64, body string) (*http.Request, error) {
	path := fmt.Sprintf("/repos/%s/issues/%d/comments", repo.path(), number)
	if root > 0 {
		path = fmt.Sprintf("/repos/%s/pulls/%d/comments/%d/replies", repo.path(), number, root)
	}
	return f.newRequest(ctx, http.MethodPost, repo, path, "", struct {
		Body string `json:"body"`
	}{body})
}

func (f *forge) sendReviewReply(req *http.Request, repo repoRef, number int, root int64) (replyCreateResult, error) {
	resp, err := f.client.Do(req)
	if err != nil {
		// A transport may include credentials in its error. No raw error
		// from the send crosses the poster's boundary.
		return replyCreateResult{}, errors.New("review reply: transport failed")
	}
	defer drainAndClose(resp.Body)
	result := replyCreateResult{Status: resp.StatusCode, Header: resp.Header}
	if resp.StatusCode == http.StatusCreated {
		var wire replyCommentWire
		if decodeResponse(resp.Body, &wire) == nil {
			if root == 0 && wire.IssueURL != fmt.Sprintf("%s/repos/%s/issues/%d", f.baseURL, repo.path(), number) {
				return result, nil
			}
			if comment, err := wire.comment(); err == nil {
				result.Comment = &comment
			}
		}
	}
	return result, nil
}

func (f *forge) listConversationComments(ctx context.Context, repo repoRef, number int) ([]replyComment, error) {
	path := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100", repo.path(), number)
	wire, _, _, _, err := fetchConditionalList[replyCommentWire](ctx, f, repo, path, "")
	if err != nil {
		return nil, errors.New("review reply: comment listing failed")
	}
	comments := make([]replyComment, 0, len(wire))
	for _, w := range wire {
		if w.IssueURL != fmt.Sprintf("%s/repos/%s/issues/%d", f.baseURL, repo.path(), number) {
			return nil, errors.New("review reply: comment belongs to another resource")
		}
		c, err := w.comment()
		if err != nil {
			return nil, err
		}
		comments = append(comments, c)
	}
	return comments, nil
}
