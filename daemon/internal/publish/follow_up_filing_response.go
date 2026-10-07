package publish

import (
	"net/http"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// classifyFollowUpFilingCreate maps what one dispatched create request
// observed onto the ledger's response classes (plan §5.17). The rejection
// classes are an allowlist of responses in which GitHub refused the request
// before creating anything; a status class alone is not that evidence, so
// every dispatched outcome outside the list is unproven:
//
//   - success: 201 whose body decoded to an issue.
//   - definite rejection: 400, 401, 404, 410, 422, and 403 without a
//     rate-limit header.
//   - transient rejection: 429, and 403 with x-ratelimit-remaining: 0 or a
//     retry-after header.
//   - unproven: a transport error or timeout, any 5xx, any other status,
//     and a 201 whose body did not decode to an issue.
//
// The caller still validates a success response's issue as it would any
// candidate before ledgering it.
func classifyFollowUpFilingCreate(result issueCreateResult, sendErr error) domain.FollowUpFilingResponseClass {
	return classifyForgeCreate(result.Status, result.Header, result.Issue != nil, sendErr)
}

// classifyForgeCreate shares the proven-rejection table across non-idempotent
// forge creates. A decoded object still requires the caller's identity checks.
func classifyForgeCreate(status int, header http.Header, decoded bool, sendErr error) domain.FollowUpFilingResponseClass {
	if sendErr != nil {
		return domain.FollowUpFilingResponseUnproven
	}
	switch status {
	case http.StatusCreated:
		if decoded {
			return domain.FollowUpFilingResponseSuccess
		}
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusNotFound,
		http.StatusGone, http.StatusUnprocessableEntity:
		return domain.FollowUpFilingResponseDefiniteRejection
	case http.StatusTooManyRequests:
		return domain.FollowUpFilingResponseTransientRejection
	case http.StatusForbidden:
		if header.Get("X-RateLimit-Remaining") == "0" || header.Get("Retry-After") != "" {
			return domain.FollowUpFilingResponseTransientRejection
		}
		return domain.FollowUpFilingResponseDefiniteRejection
	}
	return domain.FollowUpFilingResponseUnproven
}
