package signet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// errInvocationIntentCorrupt reports a pending outbox row whose payload does
// not name the invocation its idempotency key committed: fail-closed
// evidence of corruption or a foreign writer, never dispatchable.
var errInvocationIntentCorrupt = errors.New("outbox intent payload disagrees with its idempotency key")

// DispatchPendingInvocations hands every committed-but-undispatched
// AgentInvocationRequested intent to the driver: the recovery half of the
// discuss transaction (plan §5.14 test 5 — a daemon death between the commit
// and the provider start must still produce exactly one invocation). It is
// not the Wave 2 engine's dispatch/acceptance loop: no polling, no result
// acceptance, just the idempotent drain of the outbox ledger, safe to call
// on startup and after any suspected loss.
//
// Effectively-once composes from two layers: the driver's durable
// per-invocation intent dedups a repeated Start (exec.ErrDuplicateStart is
// success here, §5.3), and the dispatched mark only bounds rescans. Marking
// happens after Start returns, in a separate non-revision-bumping
// transaction: a crash between the two re-dispatches on the next call and
// converges on the driver's dedup.
func (s *Service) DispatchPendingInvocations(ctx context.Context, driver exec.StageDriver) (int, error) {
	var pending []store.QueueEntry
	err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		entries, err := tx.ListPendingOutbox(ctx, kindAgentInvocationRequested)
		if err != nil {
			return err
		}
		pending = entries
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("dispatch invocations: %w", err)
	}

	dispatched := 0
	for _, entry := range pending {
		request, err := decodeBoundInvocationRequest(entry)
		if err != nil {
			return dispatched, fmt.Errorf("dispatch invocations: intent %q payload: %w", entry.IdempotencyKey, err)
		}
		// StartSpec's run/stage/input fields describe pipeline stages; a
		// conversation invocation has none yet. Their shape for agent turns
		// is deferred to the Wave 2 dispatch contract (the engine's unit),
		// so the spec is deliberately empty here and the fakes accept it.
		if err := driver.Start(ctx, request.InvocationID, exec.StartSpec{}); err != nil && !errors.Is(err, exec.ErrDuplicateStart) {
			return dispatched, fmt.Errorf("dispatch invocations: start %q: %w", request.InvocationID, err)
		}
		err = s.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.MarkOutboxDispatched(ctx, entry.IdempotencyKey)
		})
		if err != nil {
			return dispatched, fmt.Errorf("dispatch invocations: mark %q: %w", entry.IdempotencyKey, err)
		}
		dispatched++
	}
	return dispatched, nil
}

// AgentInvocationBackupPayloadDigests validates a durable discuss invocation.
// The request is self-contained and needs no external blobs.
func AgentInvocationBackupPayloadDigests(entry store.QueueEntry) ([]domain.Digest, error) {
	if _, err := decodeBoundInvocationRequest(entry); err != nil {
		return nil, err
	}
	return nil, nil
}

func decodeBoundInvocationRequest(entry store.QueueEntry) (invocationRequest, error) {
	var request invocationRequest
	if err := json.Unmarshal(entry.Payload, &request); err != nil {
		return invocationRequest{}, err
	}
	// Queue payloads are opaque to the store, so the decoded intent is a
	// reconstruction boundary: re-check it against the row's own kind and key
	// before acting or declaring it backup-valid.
	if entry.Kind != AgentInvocationRequestedKind ||
		request.InvocationID == "" ||
		string(request.InvocationID) != entry.IdempotencyKey {
		return invocationRequest{}, fmt.Errorf(
			"intent %q payload names invocation %q: %w",
			entry.IdempotencyKey, request.InvocationID, errInvocationIntentCorrupt)
	}
	return request, nil
}
