package ward

import (
	"errors"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// DecodeRetainedCodexReview authenticates the opaque journal at the read-only
// client boundary. Codex is the production reviewer; Claude shadow records
// cannot pass its provider-specific gate or the caller's primary binding.
// This does not authorize execution, cleanup, readiness, or publication.
func DecodeRetainedCodexReview(binding domain.ReviewRequestRecord, requestBody []byte, requestDigest, state string, body []byte, bodyDigest string) (CodexReviewSourceOutcome, error) {
	var outcome CodexReviewSourceOutcome
	if state != "ready" && state != "collected" {
		return outcome, domain.ErrInvalidReviewCompletionEvidence
	}
	authority := append([]byte(state+"\x00"), body...)
	if contentaddr.Sum(authority) != bodyDigest || contentaddr.Sum(requestBody) != requestDigest {
		return outcome, domain.ErrInvalidReviewCompletionEvidence
	}
	var request exec.ReviewRequest
	if err := strictjson.Decode(requestBody, &request, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
		return outcome, err
	}
	if err := request.Validate(); err != nil {
		return outcome, err
	}
	if request.RunID != binding.RunID || request.Round != binding.Round || request.BaseSHA != binding.BaseSHA || request.HeadSHA != binding.HeadSHA {
		return outcome, domain.ErrParentKeyMismatch
	}
	if err := strictjson.Decode(body, &outcome, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
		return outcome, err
	}
	if err := outcome.Validate(); err != nil {
		return outcome, err
	}
	if outcome.InvocationID != binding.InvocationID {
		return outcome, domain.ErrParentKeyMismatch
	}
	if outcome.Result != nil && (outcome.Result.BaseSHA != binding.BaseSHA || outcome.Result.HeadSHA != binding.HeadSHA) {
		return outcome, domain.ErrParentKeyMismatch
	}
	if err := outcome.verifyCompletionEvidence(codexReviewProvider{}); err != nil {
		return outcome, errors.Join(domain.ErrInvalidReviewCompletionEvidence, err)
	}
	return outcome, nil
}

// DecodeRetainedCodexReviewRequestFacts authenticates request facts for the exact
// candidate. It also recovers the original request time when an in-flight
// provider journal predates the typed review request table; it grants no
// execution authority. The caller must select the row by binding.InvocationID.
// Legacy instruction validation remains the execution source's responsibility.
func DecodeRetainedCodexReviewRequestFacts(binding domain.ReviewRequestRecord, body []byte, digest string) (domain.ReviewRequestRecord, error) {
	var request exec.ReviewRequest
	if contentaddr.Sum(body) != digest {
		return domain.ReviewRequestRecord{}, domain.ErrInvalidReviewCompletionEvidence
	}
	if err := strictjson.Decode(body, &request, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
		return domain.ReviewRequestRecord{}, err
	}
	facts := domain.ReviewRequestRecord{
		InvocationID: binding.InvocationID, RunID: request.RunID, Round: request.Round,
		BaseSHA: request.BaseSHA, HeadSHA: request.HeadSHA, RequestedAt: request.RequestedAt,
	}
	if err := facts.Validate(); err != nil {
		return domain.ReviewRequestRecord{}, err
	}
	if request.RunID != binding.RunID || request.Round != binding.Round || request.BaseSHA != binding.BaseSHA || request.HeadSHA != binding.HeadSHA {
		return domain.ReviewRequestRecord{}, domain.ErrParentKeyMismatch
	}
	return facts, nil
}
