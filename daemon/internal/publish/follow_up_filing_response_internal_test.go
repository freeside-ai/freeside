package publish

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// TestClassifyFollowUpFilingCreate pins every row of the response list: the
// rejection classes are an allowlist, and everything else a dispatched
// request can observe is unproven.
func TestClassifyFollowUpFilingCreate(t *testing.T) {
	t.Parallel()
	const (
		success   = domain.FollowUpFilingResponseSuccess
		definite  = domain.FollowUpFilingResponseDefiniteRejection
		transient = domain.FollowUpFilingResponseTransientRejection
		unproven  = domain.FollowUpFilingResponseUnproven
	)
	issue := &filedIssue{Number: 12, AuthorID: 7, AuthorType: "Bot", CreatedAt: time.Unix(1, 0)}
	header := func(key, value string) http.Header { return http.Header{key: {value}} }
	cases := []struct {
		name   string
		result issueCreateResult
		err    error
		want   domain.FollowUpFilingResponseClass
	}{
		{"201 with an issue", issueCreateResult{Status: 201, Issue: issue}, nil, success},
		{"201 without an issue", issueCreateResult{Status: 201}, nil, unproven},
		{"400", issueCreateResult{Status: 400}, nil, definite},
		{"401", issueCreateResult{Status: 401}, nil, definite},
		{"403", issueCreateResult{Status: 403}, nil, definite},
		{"403 with quota left", issueCreateResult{Status: 403, Header: header("X-Ratelimit-Remaining", "12")}, nil, definite},
		{"403 rate limit exhausted", issueCreateResult{Status: 403, Header: header("X-Ratelimit-Remaining", "0")}, nil, transient},
		{"403 retry-after", issueCreateResult{Status: 403, Header: header("Retry-After", "30")}, nil, transient},
		{"404", issueCreateResult{Status: 404}, nil, definite},
		{"410", issueCreateResult{Status: 410}, nil, definite},
		{"422", issueCreateResult{Status: 422}, nil, definite},
		{"429", issueCreateResult{Status: 429}, nil, transient},
		{"200", issueCreateResult{Status: 200}, nil, unproven},
		{"200 with an issue", issueCreateResult{Status: 200, Issue: issue}, nil, unproven},
		{"202", issueCreateResult{Status: 202}, nil, unproven},
		{"204", issueCreateResult{Status: 204}, nil, unproven},
		{"301", issueCreateResult{Status: 301}, nil, unproven},
		{"405", issueCreateResult{Status: 405}, nil, unproven},
		{"408", issueCreateResult{Status: 408}, nil, unproven},
		{"409", issueCreateResult{Status: 409}, nil, unproven},
		{"451", issueCreateResult{Status: 451}, nil, unproven},
		{"500", issueCreateResult{Status: 500}, nil, unproven},
		{"502", issueCreateResult{Status: 502}, nil, unproven},
		{"503", issueCreateResult{Status: 503}, nil, unproven},
		{"503 retry-after", issueCreateResult{Status: 503, Header: header("Retry-After", "30")}, nil, unproven},
		{"504", issueCreateResult{Status: 504}, nil, unproven},
		{"transport error", issueCreateResult{}, errors.New("connection reset"), unproven},
		{"timeout", issueCreateResult{}, context.DeadlineExceeded, unproven},
		{"transport error beside a rejection", issueCreateResult{Status: 422}, errors.New("reset"), unproven},
	}
	for _, tc := range cases {
		if got := classifyFollowUpFilingCreate(tc.result, tc.err); got != tc.want {
			t.Errorf("%s: class = %q, want %q", tc.name, got, tc.want)
		}
	}
}
