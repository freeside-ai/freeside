package ward

import (
	"errors"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// DecodeRetainedCodexReview authenticates the opaque journal at the read-only
// client boundary, for whichever provider ran the round. It re-checks the
// outcome's completion evidence under the provider the invocation's journal
// binding names (retainedReviewProvider), never one the outcome row chooses,
// and holds a returned admission against the persisted request.
// journalBindingBody and journalBindingDigest are the invocation's journal
// binding row; an empty body means the round has none, which only a failure
// fence that carries no collection evidence and no result may lack. This does
// not authorize execution, cleanup, readiness, or publication.
func DecodeRetainedCodexReview(binding domain.ReviewRequestRecord, requestBody []byte, requestDigest, state string, body []byte, bodyDigest string, journalBindingBody []byte, journalBindingDigest string) (CodexReviewSourceOutcome, error) {
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
	// A request from before review admission is held to the rule it was
	// written under, so a round that failed then keeps its evidence. A round
	// that completed then does not: its result predates the snapshot a
	// result returns and no longer decodes.
	if err := exec.ValidateRetainedReviewRequest(requestBody, request); err != nil {
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
	var evidenceErr error
	if outcome.CollectionEvidence != "" || outcome.Result != nil {
		provider, err := retainedReviewProvider(outcome.InvocationID, journalBindingBody, journalBindingDigest)
		if err != nil {
			return outcome, err
		}
		evidenceErr = outcome.verifyCompletionEvidence(provider)
	}
	if err := errors.Join(evidenceErr, outcome.verifyReturnedAdmission(request)); err != nil {
		return outcome, errors.Join(domain.ErrInvalidReviewCompletionEvidence, err)
	}
	return outcome, nil
}

// retainedReviewProvider resolves the provider that ran a retained round from
// the invocation's journal binding, the record its source wrote before
// launch, so an outcome row never chooses the gate it is held to. It
// re-checks the binding's digest against its body, that the binding belongs
// to the invocation, and that exactly one registered provider owns its
// topology version, current or legacy. Recovery validates the whole binding
// (reviewProviderForStartedIntent) because it acts on resources; a read
// grants nothing, so it decodes only the two fields it needs and stays
// readable across later binding schema changes.
func retainedReviewProvider(invocationID domain.InvocationID, body []byte, digest string) (reviewProvider, error) {
	if len(body) == 0 || contentaddr.Sum(body) != digest {
		return nil, domain.ErrInvalidReviewCompletionEvidence
	}
	var binding struct {
		TopologyVersion string `json:"topology_version"`
		RunID           string `json:"run_id"`
	}
	if err := strictjson.DecodeAllowingUnknownFields(body, &binding, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
		return nil, errors.Join(domain.ErrInvalidReviewCompletionEvidence, err)
	}
	if binding.RunID != string(invocationID) {
		return nil, domain.ErrInvalidReviewCompletionEvidence
	}
	var matched reviewProvider
	for _, provider := range allReviewProviders() {
		if binding.TopologyVersion != provider.topologyVersion() &&
			!slices.Contains(provider.legacyTopologyVersions(), binding.TopologyVersion) {
			continue
		}
		if matched != nil {
			return nil, domain.ErrInvalidReviewCompletionEvidence
		}
		matched = provider
	}
	if matched == nil {
		return nil, domain.ErrInvalidReviewCompletionEvidence
	}
	return matched, nil
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
