package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ErrSpecificationNotRetryable refuses a specification retry whose newest
// invocation has not ended failed, canceled, or lost with its failure card
// still open, or already has a pending retry for another reason.
var ErrSpecificationNotRetryable = errors.New("specification invocation is not retryable")

// SpecificationRetrySpec names one deliberate retry of a failed
// specification invocation, before its specification is approved (#2315).
type SpecificationRetrySpec struct {
	SpecificationRunID domain.RunID
	Reason             string
}

// SpecificationRetry reports the iteration a retry added, or the pending one
// a repeated command with the same reason found (Created false).
type SpecificationRetry struct {
	Created             bool
	Run                 domain.Run
	StageID             domain.StageID
	InvocationID        domain.InvocationID
	RetriedInvocationID domain.InvocationID
	SourceArtifactID    domain.ArtifactID
	PolicyArtifactID    domain.ArtifactID
	PublicationDigest   domain.Digest
	CampaignID          domain.CampaignID
	Reason              string
}

// SpecificationRetryFact is one retry iteration of a specification run, for
// the task record.
type SpecificationRetryFact struct {
	InvocationID        domain.InvocationID
	RetriedInvocationID domain.InvocationID
	Reason              string
}

// RetrySpecificationRun adds one iteration to a specification run whose
// newest invocation ended failed, canceled, or lost. The iteration repeats
// that invocation's inputs and records the operator's reason; no run,
// campaign, or task is created. The same write supersedes the failure card,
// which together with the failed terminal is what lets the chain check
// accept the new iteration. Dispatch runs the ordinary delivery check and
// execution admission, so none runs here.
func RetrySpecificationRun(
	ctx context.Context, st *store.Store, spec SpecificationRetrySpec,
) (SpecificationRetry, error) {
	if st == nil {
		return SpecificationRetry{}, errors.New("retry specification run: nil store")
	}
	if spec.SpecificationRunID == "" {
		return SpecificationRetry{}, errors.New("retry specification run: a specification run is required")
	}
	if spec.Reason == "" || spec.Reason != strings.TrimSpace(spec.Reason) {
		return SpecificationRetry{}, errors.New("retry specification run: a non-empty trimmed reason is required")
	}
	var out SpecificationRetry
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		run, err := tx.GetRun(ctx, spec.SpecificationRunID)
		if err != nil {
			return err
		}
		stage, ok := findSpecificationStage(run)
		if !ok || len(run.Stages) != 1 {
			return fmt.Errorf("run %q is not a specification run: %w", run.ID, ErrSpecificationNotRetryable)
		}
		if err := requireTaskExecutionOpen(ctx, &tx.ReadTx, run.ID); err != nil {
			return err
		}
		requests, err := specificationRequests(ctx, &tx.ReadTx, run.ID)
		if err != nil {
			return err
		}
		if len(requests) == 0 {
			return fmt.Errorf("specification run %q has not started: %w", run.ID, ErrSpecificationNotRetryable)
		}
		latest := requests[len(requests)-1]
		verified, err := verifySpecificationChain(ctx, &tx.ReadTx, latest)
		if err != nil {
			return err
		}
		out = SpecificationRetry{
			Run: run, StageID: stage.ID,
			SourceArtifactID: latest.InputArtifactIDs[0], PolicyArtifactID: latest.PolicyArtifactID,
			PublicationDigest: latest.PublicationDigest, CampaignID: latest.CampaignID,
			Reason: spec.Reason,
		}
		terminal, ended, err := specificationTerminalFor(ctx, &tx.ReadTx, latest)
		if err != nil {
			return err
		}
		if !ended {
			// A repeated command finds its own pending retry; anything else
			// pending is not this command's to replace.
			if latest.RetryReason == spec.Reason {
				out.InvocationID = latest.InvocationID
				out.RetriedInvocationID = requests[len(requests)-2].InvocationID
				return errReplay // Roll back, so a replay writes nothing.
			}
			return fmt.Errorf("specification invocation %q has not ended: %w",
				latest.InvocationID, ErrSpecificationNotRetryable)
		}
		if !retryableSpecificationStatus(terminal.Status) {
			return fmt.Errorf("specification invocation %q ended %q: %w",
				latest.InvocationID, terminal.Status, ErrSpecificationNotRetryable)
		}
		card, err := tx.GetAttentionItem(ctx, specificationFailureItemID(latest.InvocationID))
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("specification invocation %q has no failure card: %w",
				latest.InvocationID, ErrSpecificationNotRetryable)
		}
		if err != nil {
			return err
		}
		if !specificationFailureCardBinds(card, run) {
			return fmt.Errorf("specification failure card %q binding disagrees: %w", card.ID, domain.ErrParentKeyMismatch)
		}
		if card.Status != domain.StatusOpen {
			return fmt.Errorf("specification failure card %q is %q: %w", card.ID, card.Status, ErrSpecificationNotRetryable)
		}
		next := nextSpecificationRetryRequest(latest, spec.Reason)
		if next.Iteration-next.firstIteration()+1 > verified.settings.MaxIterations {
			return fmt.Errorf("specification iteration %d exceeds the policy maximum %d: %w",
				next.Iteration, verified.settings.MaxIterations, ErrSpecificationIterationsExhausted)
		}
		payload, err := encodeSpecificationRequest(next)
		if err != nil {
			return err
		}
		invocation, err := domain.NewAgentInvocation(next.InvocationID, next.InputArtifactIDs, nil, 0)
		if err != nil {
			return err
		}
		if err := tx.PutAgentInvocation(ctx, invocation); err != nil {
			return err
		}
		stored, inserted, err := tx.EnqueueOutbox(ctx, string(next.InvocationID), KindSpecificationInvocationRequested, payload)
		if err != nil {
			return err
		}
		if !inserted && (stored.Kind != KindSpecificationInvocationRequested || !bytes.Equal(stored.Payload, payload)) {
			return fmt.Errorf("specification retry %q disagrees: %w", next.InvocationID, domain.ErrImmutableTransition)
		}
		card.Reason += fmt.Sprintf(
			" Superseded by specification iteration %d (retry reason: %s); no recovery choice is needed.",
			next.Iteration, spec.Reason)
		card.ItemVersion++
		// As for a superseded attempt's card, the conclusion leaves out the
		// operator's reason, which the item's reason already carries.
		card, err = card.ConcludeByDaemon(domain.ConcludedBySupersession, time.Now().UTC(),
			fmt.Sprintf("Superseded by specification iteration %d; no recovery choice is needed.", next.Iteration), nil)
		if err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, card); err != nil {
			return err
		}
		// Refuse to commit an iteration dispatch would quarantine.
		if _, err := verifySpecificationChain(ctx, &tx.ReadTx, next); err != nil {
			return err
		}
		out.Created = true
		out.InvocationID = next.InvocationID
		out.RetriedInvocationID = latest.InvocationID
		return nil
	})
	if err != nil && !errors.Is(err, errReplay) {
		return SpecificationRetry{}, fmt.Errorf("retry specification run %q: %w", spec.SpecificationRunID, err)
	}
	return out, nil
}

// SpecificationRetries lists a specification run's retry iterations, oldest
// first, after authenticating its whole chain. A run's first iteration cannot
// be a retry, so a run with at most one iteration has none and is not read
// further.
func SpecificationRetries(
	ctx context.Context, tx *store.ReadTx, runID domain.RunID,
) ([]SpecificationRetryFact, error) {
	facts := []SpecificationRetryFact{}
	key, present, err := tx.FirstSpecificationMarkerKey(ctx, runID)
	if err != nil || !present {
		return facts, err
	}
	first, err := strconv.Atoi(strings.TrimPrefix(key, domain.SpecificationInvocationIDPrefix(runID)))
	if err != nil {
		return nil, fmt.Errorf("specification marker key %q: %w", key, domain.ErrParentKeyMismatch)
	}
	if _, err := tx.GetOutbox(ctx, string(specificationInvocationID(runID, first+1))); errors.Is(err, store.ErrNotFound) {
		return facts, nil
	} else if err != nil {
		return nil, err
	}
	requests, err := specificationRequests(ctx, tx, runID)
	if err != nil {
		return nil, err
	}
	if _, err := verifySpecificationChain(ctx, tx, requests[len(requests)-1]); err != nil {
		return nil, err
	}
	for i, request := range requests {
		if request.RetryReason == "" {
			continue
		}
		facts = append(facts, SpecificationRetryFact{
			InvocationID: request.InvocationID, RetriedInvocationID: requests[i-1].InvocationID,
			Reason: request.RetryReason,
		})
	}
	return facts, nil
}

// specificationRequests reads a run's dispatch markers in iteration order,
// from its first iteration to its newest.
func specificationRequests(
	ctx context.Context, tx *store.ReadTx, runID domain.RunID,
) ([]specificationRequest, error) {
	key, present, err := tx.FirstSpecificationMarkerKey(ctx, runID)
	if err != nil || !present {
		return nil, err
	}
	var requests []specificationRequest
	for {
		entry, err := tx.GetOutbox(ctx, key)
		if errors.Is(err, store.ErrNotFound) && len(requests) > 0 {
			return requests, nil
		}
		if err != nil {
			return nil, err
		}
		request, err := decodeSpecificationRequest(entry)
		if err != nil {
			return nil, err
		}
		requests = append(requests, request)
		key = string(specificationInvocationID(runID, request.Iteration+1))
	}
}

// specificationTerminalFor reads request's terminal, if it has ended.
func specificationTerminalFor(
	ctx context.Context, tx *store.ReadTx, request specificationRequest,
) (specificationTerminal, bool, error) {
	entry, err := tx.GetInbox(ctx, string(request.InvocationID))
	if errors.Is(err, store.ErrNotFound) {
		return specificationTerminal{}, false, nil
	}
	if err != nil {
		return specificationTerminal{}, false, err
	}
	terminal, err := decodeSpecificationTerminal(entry)
	if err != nil {
		return specificationTerminal{}, false, err
	}
	if terminal.InvocationID != request.InvocationID || terminal.Iteration != request.Iteration {
		return specificationTerminal{}, false, fmt.Errorf(
			"specification terminal %q disagrees with its request: %w", request.InvocationID, domain.ErrParentKeyMismatch)
	}
	return terminal, true, nil
}

// retryableSpecificationStatus names the terminals an operator may retry:
// the invocation ended without output. A completed invocation recorded its
// output or a refusal after it, and a blocked one awaits its blocker.
func retryableSpecificationStatus(status exec.Status) bool {
	switch status {
	case exec.StatusFailed, exec.StatusCanceled, exec.StatusGone:
		return true
	case exec.StatusPending, exec.StatusRunning, exec.StatusCompleted, exec.StatusBlocked:
		return false
	}
	return false
}

// authenticateSpecificationRetryTransition accepts next as request's retry
// only when it repeats request with a reason and the failed invocation's
// card was concluded by supersession, which only RetrySpecificationRun does
// to a specification failure card.
func authenticateSpecificationRetryTransition(
	ctx context.Context, tx *store.ReadTx, run domain.Run, request, next specificationRequest,
) error {
	if next.RetryReason == "" ||
		!sameSpecificationRequest(next, nextSpecificationRetryRequest(request, next.RetryReason)) {
		return fmt.Errorf("specification request %q is not a retry of %q: %w",
			next.InvocationID, request.InvocationID, domain.ErrParentKeyMismatch)
	}
	card, err := tx.GetAttentionItem(ctx, specificationFailureItemID(request.InvocationID))
	if err != nil {
		return err
	}
	if !specificationFailureCardBinds(card, run) || card.Status != domain.StatusSuperseded ||
		card.Conclusion == nil || card.Conclusion.Cause != domain.ConcludedBySupersession {
		return fmt.Errorf("specification retry %q has no superseded failure card: %w",
			next.InvocationID, domain.ErrParentKeyMismatch)
	}
	return nil
}

func specificationFailureItemID(invocationID domain.InvocationID) domain.ItemID {
	return domain.ItemID("execution-failure-" + string(invocationID))
}

func specificationFailureCardBinds(card domain.AttentionItem, run domain.Run) bool {
	return card.Type == domain.AttentionExecutionFailure && card.ProjectID == run.ProjectID &&
		card.Subject.Type == domain.SubjectRun && card.Subject.ID == domain.SubjectID(run.ID) &&
		card.Subject.RunID != nil && *card.Subject.RunID == run.ID
}
