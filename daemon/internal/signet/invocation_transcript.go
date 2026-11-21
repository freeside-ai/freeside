package signet

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

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
	// bytes are gone, and no running writer to read; either way nothing is
	// served.
	InvocationTranscriptUnavailable InvocationTranscriptAvailability = "unavailable"
	// InvocationTranscriptLive is a running writer's scanned records, read
	// while it runs (plan §5.7, "Live view of a running writer"). They are
	// never stored and never evidence.
	InvocationTranscriptLive InvocationTranscriptAvailability = "live"
	// InvocationTranscriptWithheld means a credential rule matched the
	// running writer's transcript; nothing more is served while it runs.
	InvocationTranscriptWithheld InvocationTranscriptAvailability = "withheld"
)

// AllInvocationTranscriptAvailabilities registers every availability.
var AllInvocationTranscriptAvailabilities = []InvocationTranscriptAvailability{
	InvocationTranscriptAvailable, InvocationTranscriptUnavailable,
	InvocationTranscriptLive, InvocationTranscriptWithheld,
}

func (a InvocationTranscriptAvailability) valid() bool {
	switch a {
	case InvocationTranscriptAvailable, InvocationTranscriptUnavailable,
		InvocationTranscriptLive, InvocationTranscriptWithheld:
		return true
	default:
		return false
	}
}

// InvocationTranscript is one writer invocation's transcript and the prompt
// it received. All of it is the agent's context, never verified evidence.
// For an ended writer, Transcript references the retained transcript, whose
// bytes the client fetches by digest; it is set only when Availability is
// available. For a running writer, Live carries its scanned records; it is
// set only when Availability is live.
type InvocationTranscript struct {
	RunID        domain.RunID                     `json:"run_id"`
	InvocationID domain.InvocationID              `json:"invocation_id"`
	Availability InvocationTranscriptAvailability `json:"availability"`
	Transcript   *domain.AgentClaim               `json:"transcript"`
	Live         *LiveTranscript                  `json:"live"`
	Prompt       *string                          `json:"prompt"`
}

// LiveTranscript is a running writer's complete, scanned records in the
// transcript's byte range [Start, End), concatenated, each ending in a
// newline. Records the scan withheld for size or encoding are left out and
// counted in UnreadableRecords. OmittedBefore says records between the
// requested offset and Start left the daemon's bounded window.
type LiveTranscript struct {
	Start             int64  `json:"start"`
	End               int64  `json:"end"`
	Records           []byte `json:"records"`
	OmittedBefore     bool   `json:"omitted_before"`
	UnreadableRecords int    `json:"unreadable_records"`
}

// ErrLiveTranscriptOffset means a live read's offset isn't 0 or an end an
// earlier answer carried. The request is malformed, not the transcript.
var ErrLiveTranscriptOffset = errors.New("live transcript offset is not a record boundary")

// GetInvocationTranscript serves the transcript recorded for one invocation
// of one run. The invocation's recorded admission binds it to the run: an
// admission naming another run reads as not found, so a device can't reach
// another run's transcript by pairing its run ID with a foreign invocation.
// The admission is read without current policy, so a policy change never
// hides history. A claim set that disagrees with the invocation fails the
// read and serves nothing.
//
// With no claim, it asks the bound live reader for a running writer's
// records after byte offset after, but only for an invocation the run
// records as a writer (recordedWriter). A claim always wins, so an ended
// writer reads as it did before the live view.
func (s *Service) GetInvocationTranscript(
	ctx context.Context, runID domain.RunID, invocationID domain.InvocationID, after int64,
) (InvocationTranscript, error) {
	var claim *domain.AgentClaim
	var writer bool
	err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		run, err := tx.GetRun(ctx, runID)
		if err != nil {
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
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return err
		default:
			if claim, err = transcriptClaim(invocationID, claims); err != nil {
				return err
			}
		}
		writer = recordedWriter(run, invocationID)
		return nil
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
	if claim == nil && writer {
		if out.Availability, out.Live, err = s.liveTranscript(ctx, invocationID, after); err != nil {
			return InvocationTranscript{}, err
		}
	}
	out.Prompt, err = s.invocationPrompt(ctx, invocationID)
	return out, err
}

// recordedWriter reports whether the run records the invocation as an
// attempt of an implementation stage, the only invocations the live view
// reads. That covers a remediation round too: the engine records its stage
// under the implementation role and the remediator as its attempt when it
// admits it, in the same transaction as the admission this read already
// requires. A reviewer, a verifier, and a specifier are never read live.
func recordedWriter(run domain.Run, id domain.InvocationID) bool {
	for _, stage := range run.Stages {
		if role, err := domain.CanonicalStageRole(stage.Name); err != nil || role != domain.StageNameImplementation {
			continue
		}
		for _, attempt := range stage.Attempts {
			if attempt.InvocationID == id {
				return true
			}
		}
	}
	return false
}

// liveTranscript asks the bound reader for a running writer's records. With
// no reader bound, or no handoff running for the invocation, it reads as
// unavailable.
func (s *Service) liveTranscript(
	ctx context.Context, id domain.InvocationID, after int64,
) (InvocationTranscriptAvailability, *LiveTranscript, error) {
	if s.liveTranscripts == nil {
		return InvocationTranscriptUnavailable, nil, nil
	}
	reader := s.liveTranscripts()
	if reader == nil {
		return InvocationTranscriptUnavailable, nil, nil
	}
	availability, live, err := reader.LiveTranscript(ctx, id, after)
	if err != nil {
		return "", nil, err
	}
	switch availability {
	case InvocationTranscriptLive:
		if live == nil {
			return "", nil, errors.New("live transcript reader answered live without records")
		}
		if live.Records == nil {
			live.Records = []byte{}
		}
		return availability, live, nil
	case InvocationTranscriptWithheld, InvocationTranscriptUnavailable:
		return availability, nil, nil
	case InvocationTranscriptAvailable:
	}
	return "", nil, fmt.Errorf("live transcript reader answered %q", availability)
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
	var after int64
	if raw, given := r.URL.Query()["after"]; given {
		parsed, err := strconv.ParseInt(raw[0], 10, 64)
		if err != nil || parsed < 0 || len(raw) > 1 {
			writeJSON(w, http.StatusBadRequest, errorResponse{Message: "after must be a non-negative integer"})
			return
		}
		after = parsed
	}
	transcript, err := h.service.GetInvocationTranscript(r.Context(),
		domain.RunID(r.PathValue("run_id")), domain.InvocationID(r.PathValue("invocation_id")), after)
	if errors.Is(err, ErrLiveTranscriptOffset) {
		writeJSON(w, http.StatusBadRequest, errorResponse{Message: "after is not a record boundary of the live transcript"})
		return
	}
	if err != nil {
		writeReadError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, transcript)
}
