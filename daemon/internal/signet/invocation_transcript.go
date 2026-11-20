package signet

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// transcriptClaimLabel is the label every writer's transcript claim carries,
// a successful import's and a failed invocation's retained one alike.
const transcriptClaimLabel = "agent-transcript"

// InvocationTranscriptAvailability says whether an invocation's transcript
// bytes can be fetched now. The zero value is invalid.
type InvocationTranscriptAvailability string

const (
	// InvocationTranscriptAvailable is a transcript claim whose bytes the
	// blob store holds.
	InvocationTranscriptAvailable InvocationTranscriptAvailability = "available"
	// InvocationTranscriptUnavailable is no transcript claim, or one whose
	// bytes are gone; either way no claim is served.
	InvocationTranscriptUnavailable InvocationTranscriptAvailability = "unavailable"
)

// AllInvocationTranscriptAvailabilities registers every availability.
var AllInvocationTranscriptAvailabilities = []InvocationTranscriptAvailability{
	InvocationTranscriptAvailable, InvocationTranscriptUnavailable,
}

func (a InvocationTranscriptAvailability) valid() bool {
	switch a {
	case InvocationTranscriptAvailable, InvocationTranscriptUnavailable:
		return true
	default:
		return false
	}
}

// InvocationTranscript is one ended writer invocation's transcript reference
// and the prompt it received. Both are the agent's context, never verified
// evidence; the client fetches the transcript's bytes by digest. Transcript
// is set only when Availability is available.
type InvocationTranscript struct {
	RunID        domain.RunID                     `json:"run_id"`
	InvocationID domain.InvocationID              `json:"invocation_id"`
	Availability InvocationTranscriptAvailability `json:"availability"`
	Transcript   *domain.AgentClaim               `json:"transcript"`
	Prompt       *string                          `json:"prompt"`
}

// GetInvocationTranscript serves the transcript recorded for one invocation
// of one run. The invocation's recorded admission binds it to the run: an
// admission naming another run reads as not found, so a device can't reach
// another run's transcript by pairing its run ID with a foreign invocation.
// The admission is read without current policy, so a policy change never
// hides history. A claim set that disagrees with the invocation fails the
// read and serves nothing.
func (s *Service) GetInvocationTranscript(
	ctx context.Context, runID domain.RunID, invocationID domain.InvocationID,
) (InvocationTranscript, error) {
	var claim *domain.AgentClaim
	err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetRun(ctx, runID); err != nil {
			return err
		}
		admission, err := tx.GetExecutionAdmissionRecord(ctx, invocationID)
		if err != nil {
			return err
		}
		if admission.RunID != runID {
			return fmt.Errorf("invocation %s is admitted for another run: %w", invocationID, store.ErrNotFound)
		}
		claims, err := tx.GetAgentClaims(ctx, invocationID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		claim, err = transcriptClaim(invocationID, claims)
		return err
	})
	if err != nil {
		return InvocationTranscript{}, err
	}
	out := InvocationTranscript{
		RunID: runID, InvocationID: invocationID, Availability: InvocationTranscriptUnavailable,
	}
	// A claim whose bytes are gone is served as no claim: the client has
	// nothing to fetch, and the stored availability may predate the loss.
	if claim != nil && blobAvailability(s.blobs, claim.Digest) == domain.EvidenceAvailable {
		claim.Metadata.Availability = domain.EvidenceAvailable
		out.Availability, out.Transcript = InvocationTranscriptAvailable, claim
	}
	out.Prompt, err = s.invocationPrompt(ctx, invocationID)
	return out, err
}

// transcriptClaim picks the invocation's one transcript claim, nil when it
// recorded none. It applies the checks the failure card applies to the same
// claim (failureTranscriptClaims in the engine): the claim names this
// invocation as its producer, is sensitive and head-independent, and carries
// no inline text.
func transcriptClaim(invocationID domain.InvocationID, claims []domain.AgentClaim) (*domain.AgentClaim, error) {
	var found *domain.AgentClaim
	for _, claim := range claims {
		if claim.Label != transcriptClaimLabel {
			continue
		}
		if claim.Provenance.ProducerInvocationID != invocationID ||
			claim.Provenance.SensitivityClass != domain.SensitivitySensitive ||
			claim.Provenance.HeadBinding != domain.HeadIndependent || claim.Text != nil {
			return nil, fmt.Errorf("transcript claim disagrees with invocation %s: %w",
				invocationID, domain.ErrParentKeyMismatch)
		}
		if found != nil {
			return nil, fmt.Errorf("invocation %s records more than one transcript claim: %w",
				invocationID, domain.ErrParentKeyMismatch)
		}
		found = &claim
	}
	return found, nil
}

// invocationPrompt reads the prompt through the bound reader. The prompt is
// display-only, so a reader error never fails the read: a refused or
// unreadable intent, or a failed lookup, serves a null prompt and is logged,
// and the transcript stays readable. The driver's gate decides what may be
// served; this layer only declines to serve what it can't.
func (s *Service) invocationPrompt(ctx context.Context, invocationID domain.InvocationID) (*string, error) {
	if s.invocationPrompts == nil {
		return nil, nil
	}
	reader := s.invocationPrompts()
	if reader == nil {
		return nil, nil
	}
	prompt, found, err := reader.InvocationPrompt(ctx, invocationID)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		s.logger.Warn("serve invocation transcript without its prompt",
			"invocation_id", invocationID, "error", err)
		return nil, nil
	}
	if !found {
		return nil, nil
	}
	return &prompt, nil
}

func (h httpHandler) getInvocationTranscript(w http.ResponseWriter, r *http.Request, _ domain.DeviceID) {
	transcript, err := h.service.GetInvocationTranscript(r.Context(),
		domain.RunID(r.PathValue("run_id")), domain.InvocationID(r.PathValue("invocation_id")))
	if err != nil {
		writeReadError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, transcript)
}
