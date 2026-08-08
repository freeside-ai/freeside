// Package wardstore composes ward's persistence ports with the authoritative
// SQLite store. It is deliberately separate from both packages: ward owns the
// runner contract, store owns durable state, and this boundary owns only the
// transaction wrappers and vocabulary mapping between them.
package wardstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func marshalCodexReview(value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal Codex review journal: %w", err)
	}
	return body, nil
}

func decodeCodexReview[T any](body []byte) (T, error) {
	var value T
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, fmt.Errorf("decode Codex review journal: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return value, errors.New("decode Codex review journal: trailing JSON value")
	}
	return value, nil
}

func legacyCodexReviewRequest(body []byte, request exec.ReviewRequest) bool {
	if request.Instructions.CompositionVersion != "" ||
		request.Instructions.HostDigest != nil ||
		len(request.Instructions.RepositorySources) != 0 ||
		request.Instructions.ResultDigest != "" {
		return false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return false
	}
	for field := range fields {
		if strings.EqualFold(field, "instructions") {
			return false
		}
	}
	return true
}

func classifyCodexReviewMutation(err error) error {
	if errors.Is(err, store.ErrImmutableConflict) {
		return errors.Join(ward.ErrConformance, err)
	}
	return err
}

func verifyCodexReviewBody(record store.CodexReviewOpaqueRecord) error {
	bodyDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(record.Body))
	if record.BodyDigest != bodyDigest {
		return fmt.Errorf(
			"persisted body digest %q does not match %q", record.BodyDigest, bodyDigest)
	}
	return nil
}

func verifyCodexReviewStateBody(record store.CodexReviewOpaqueRecord) error {
	authority := make([]byte, 0, len(record.State)+1+len(record.Body))
	authority = append(authority, record.State...)
	authority = append(authority, 0)
	authority = append(authority, record.Body...)
	bodyDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(authority))
	if record.BodyDigest != bodyDigest {
		return fmt.Errorf(
			"persisted state/body digest %q does not match %q", record.BodyDigest, bodyDigest)
	}
	return nil
}

// Adapters groups the two production ports backed by one open Store. They are
// separate Go types because AuthStoreLeaser.Get and HandoffJournal.Get
// intentionally use the same verb for different records.
type Adapters struct {
	Journal *Journal
	Leaser  *Leaser
}

// Journal backs ward's journal and atomic leased-open interfaces.
type Journal struct {
	store *store.Store
}

// Leaser backs ward's identity binding and mutation-lease interface.
type Leaser struct {
	store *store.Store
}

// New constructs the production ward persistence adapters.
func New(st *store.Store) (*Adapters, error) {
	if st == nil {
		return nil, errors.New("ward store adapters: nil store")
	}
	return &Adapters{
		Journal: &Journal{store: st},
		Leaser:  &Leaser{store: st},
	}, nil
}

func (a *Journal) PutCodexReviewWorkspaceBinding(
	ctx context.Context, binding ward.CodexReviewWorkspaceBinding,
) error {
	body, err := marshalCodexReview(binding)
	if err != nil {
		return err
	}
	var existing store.CodexReviewOpaqueRecord
	err = a.store.Read(ctx, func(tx *store.ReadTx) error {
		var readErr error
		existing, readErr = tx.GetCodexReviewWorkspace(ctx, binding.SourceRunID)
		return readErr
	})
	if errors.Is(err, store.ErrNotFound) {
		return classifyCodexReviewMutation(a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.RecordCodexReviewWorkspace(ctx, binding.SourceRunID, binding.Volume, body)
		}))
	}
	if err != nil {
		return err
	}
	if err := verifyCodexReviewBody(existing); err != nil {
		return errors.Join(ward.ErrConformance, err)
	}
	prior, err := decodeCodexReview[ward.CodexReviewWorkspaceBinding](existing.Body)
	if err != nil {
		return errors.Join(ward.ErrConformance, err)
	}
	if prior == binding {
		return nil
	}
	if prior.SourceRunID != binding.SourceRunID || prior.Volume != binding.Volume ||
		prior.OwnershipToken != binding.OwnershipToken || prior.CreationFingerprint != "" ||
		binding.CreationFingerprint == "" {
		return errors.Join(ward.ErrConformance, store.ErrImmutableConflict)
	}
	return classifyCodexReviewMutation(a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.FinalizeCodexReviewWorkspace(ctx, binding.SourceRunID, existing.Body, body)
	}))
}

func (a *Journal) PutCodexReviewRequest(
	ctx context.Context, id string, request exec.ReviewRequest,
) error {
	if err := request.Validate(); err != nil {
		return err
	}
	body, err := marshalCodexReview(request)
	if err != nil {
		return err
	}
	return classifyCodexReviewMutation(a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordCodexReviewRequest(ctx, id, body)
	}))
}

func (a *Journal) GetCodexReviewRequest(
	ctx context.Context, id string,
) (exec.ReviewRequest, error) {
	var record store.CodexReviewOpaqueRecord
	err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		record, err = tx.GetCodexReviewRequest(ctx, id)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return exec.ReviewRequest{}, exec.ErrUnknownInvocation
	}
	if err != nil {
		return exec.ReviewRequest{}, err
	}
	if err := verifyCodexReviewBody(record); err != nil {
		return exec.ReviewRequest{}, errors.Join(ward.ErrCodexReviewRequestRejected, err)
	}
	request, err := decodeCodexReview[exec.ReviewRequest](record.Body)
	if err != nil {
		return exec.ReviewRequest{}, errors.Join(ward.ErrCodexReviewRequestRejected, err)
	}
	if err := request.Validate(); err != nil {
		if legacyCodexReviewRequest(record.Body, request) {
			return exec.ReviewRequest{}, errors.Join(
				ward.ErrCodexReviewRequestRejected, exec.ErrLegacyReviewRequest, err)
		}
		return exec.ReviewRequest{}, errors.Join(ward.ErrCodexReviewRequestRejected, err)
	}
	return request, nil
}

func (a *Journal) PutCodexReviewOutcome(
	ctx context.Context, id string, outcome ward.CodexReviewSourceOutcome,
) error {
	if err := outcome.Validate(); err != nil {
		return err
	}
	if outcome.InvocationID != domain.InvocationID(id) {
		return domain.ErrParentKeyMismatch
	}
	body, err := marshalCodexReview(outcome)
	if err != nil {
		return err
	}
	return classifyCodexReviewMutation(a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordCodexReviewOutcome(ctx, id, body)
	}))
}

func (a *Journal) GetCodexReviewOutcome(
	ctx context.Context, id string,
) (ward.CodexReviewSourceOutcome, bool, error) {
	var record store.CodexReviewOpaqueRecord
	err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		record, err = tx.GetCodexReviewOutcome(ctx, id)
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ward.CodexReviewSourceOutcome{}, false, ward.ErrCodexReviewOutcomeNotFound
		}
		return ward.CodexReviewSourceOutcome{}, false, err
	}
	if err := verifyCodexReviewStateBody(record); err != nil {
		return ward.CodexReviewSourceOutcome{}, false, errors.Join(
			ward.ErrCodexReviewOutcomeRejected, err)
	}
	outcome, err := decodeCodexReview[ward.CodexReviewSourceOutcome](record.Body)
	if err != nil {
		return ward.CodexReviewSourceOutcome{}, false,
			errors.Join(ward.ErrCodexReviewOutcomeRejected, err)
	}
	if err := outcome.Validate(); err != nil {
		return ward.CodexReviewSourceOutcome{}, false,
			errors.Join(ward.ErrCodexReviewOutcomeRejected, err)
	}
	if outcome.InvocationID != domain.InvocationID(id) {
		return ward.CodexReviewSourceOutcome{}, false,
			errors.Join(ward.ErrCodexReviewOutcomeRejected, domain.ErrParentKeyMismatch)
	}
	return outcome, record.State == "ready", nil
}

func (a *Journal) ListCodexReviewOutcomeIDs(ctx context.Context) ([]string, error) {
	var ids []string
	if err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		ids, err = tx.ListCodexReviewOutcomeIDs(ctx)
		return err
	}); err != nil {
		return nil, err
	}
	return ids, nil
}

func (a *Journal) MarkCodexReviewOutcomeReady(ctx context.Context, id string) error {
	return classifyCodexReviewMutation(a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkCodexReviewOutcomeReady(ctx, id)
	}))
}

func (a *Journal) GetCodexReviewWorkspaceBinding(
	ctx context.Context, sourceRunID string,
) (ward.CodexReviewWorkspaceBinding, error) {
	var record store.CodexReviewOpaqueRecord
	err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		record, err = tx.GetCodexReviewWorkspace(ctx, sourceRunID)
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ward.CodexReviewWorkspaceBinding{}, ward.ErrCodexReviewWorkspaceNotFound
		}
		return ward.CodexReviewWorkspaceBinding{}, err
	}
	if err := verifyCodexReviewBody(record); err != nil {
		return ward.CodexReviewWorkspaceBinding{}, errors.Join(ward.ErrConformance, err)
	}
	binding, err := decodeCodexReview[ward.CodexReviewWorkspaceBinding](record.Body)
	if err != nil {
		return ward.CodexReviewWorkspaceBinding{}, errors.Join(ward.ErrConformance, err)
	}
	if binding.SourceRunID != sourceRunID || binding.Volume != record.Key {
		return ward.CodexReviewWorkspaceBinding{},
			errors.Join(ward.ErrConformance, domain.ErrParentKeyMismatch)
	}
	return binding, nil
}

func (a *Journal) ListCodexReviewWorkspaceIDs(ctx context.Context) ([]string, error) {
	var ids []string
	if err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		ids, err = tx.ListCodexReviewWorkspaceIDs(ctx)
		return err
	}); err != nil {
		return nil, err
	}
	return ids, nil
}

func (a *Journal) DeleteCodexReviewWorkspaceBinding(
	ctx context.Context, binding ward.CodexReviewWorkspaceBinding,
) error {
	body, err := marshalCodexReview(binding)
	if err != nil {
		return err
	}
	return classifyCodexReviewMutation(a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.DeleteCodexReviewWorkspace(ctx, binding.SourceRunID, binding.Volume, body)
	}))
}

func (a *Journal) BeginCodexReviewIntent(
	ctx context.Context, intent ward.CodexReviewLaunchIntent,
) error {
	body, err := marshalCodexReview(intent)
	if err != nil {
		return err
	}
	return classifyCodexReviewMutation(a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.BeginCodexReviewIntent(ctx, intent.RunID, string(intent.State), body)
	}))
}

func (a *Journal) GetCodexReviewIntent(
	ctx context.Context, runID string,
) (ward.CodexReviewLaunchIntent, error) {
	var record store.CodexReviewOpaqueRecord
	err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		record, err = tx.GetCodexReviewIntent(ctx, runID)
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return ward.CodexReviewLaunchIntent{}, ward.ErrCodexReviewIntentNotFound
	}
	if err != nil {
		return ward.CodexReviewLaunchIntent{}, err
	}
	if err := verifyCodexReviewStateBody(record); err != nil {
		return ward.CodexReviewLaunchIntent{}, errors.Join(ward.ErrConformance, err)
	}
	intent, err := decodeCodexReview[ward.CodexReviewLaunchIntent](record.Body)
	if err != nil {
		return ward.CodexReviewLaunchIntent{}, errors.Join(ward.ErrConformance, err)
	}
	if string(intent.State) != record.State || intent.RunID != runID {
		return ward.CodexReviewLaunchIntent{}, errors.Join(ward.ErrConformance, domain.ErrParentKeyMismatch)
	}
	return intent, nil
}

func (a *Journal) ListCodexReviewIntentIDs(ctx context.Context) ([]string, error) {
	var ids []string
	if err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		ids, err = tx.ListCodexReviewIntentIDs(ctx)
		return err
	}); err != nil {
		return nil, err
	}
	return ids, nil
}

func (a *Journal) updateCodexReviewIntent(
	ctx context.Context,
	runID string,
	requireOutcomeAbsent bool,
	mutate func(*ward.CodexReviewLaunchIntent) error,
) error {
	return classifyCodexReviewMutation(a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		record, err := tx.GetCodexReviewIntent(ctx, runID)
		if err != nil {
			return err
		}
		if err := verifyCodexReviewStateBody(record); err != nil {
			return errors.Join(ward.ErrConformance, err)
		}
		intent, err := decodeCodexReview[ward.CodexReviewLaunchIntent](record.Body)
		if err != nil {
			return errors.Join(ward.ErrConformance, err)
		}
		if intent.RunID != runID || string(intent.State) != record.State {
			return errors.Join(ward.ErrConformance, domain.ErrParentKeyMismatch)
		}
		if requireOutcomeAbsent && intent.State == ward.CodexReviewIntentPreparing {
			if _, outcomeErr := tx.GetCodexReviewOutcome(ctx, runID); outcomeErr == nil {
				return errors.Join(ward.ErrConformance, store.ErrImmutableConflict)
			} else if !errors.Is(outcomeErr, store.ErrNotFound) {
				return outcomeErr
			}
		}
		before := append([]byte(nil), record.Body...)
		beforeState := intent.State
		if err := mutate(&intent); err != nil {
			if errors.Is(err, store.ErrImmutableConflict) ||
				errors.Is(err, domain.ErrParentKeyMismatch) {
				return errors.Join(ward.ErrConformance, err)
			}
			return err
		}
		after, err := marshalCodexReview(intent)
		if err != nil {
			return err
		}
		return tx.UpdateCodexReviewIntent(ctx, runID, string(beforeState), string(intent.State), before, after)
	}))
}

func (a *Journal) MarkCodexReviewIntentResource(
	ctx context.Context, runID string, resource ward.CodexReviewIntentResource,
) error {
	return a.updateCodexReviewIntent(ctx, runID, false, func(intent *ward.CodexReviewLaunchIntent) error {
		if intent.State != ward.CodexReviewIntentPreparing {
			return store.ErrImmutableConflict
		}
		for i := range intent.Resources {
			if intent.Resources[i].Name == resource.Name {
				intent.Resources[i] = resource
				return nil
			}
		}
		return domain.ErrParentKeyMismatch
	})
}

func (a *Journal) transitionCodexReviewIntent(
	ctx context.Context, runID string,
	from []ward.CodexReviewIntentState, to ward.CodexReviewIntentState,
	requireOutcomeAbsent bool,
) error {
	return a.updateCodexReviewIntent(ctx, runID, requireOutcomeAbsent, func(intent *ward.CodexReviewLaunchIntent) error {
		for _, allowed := range from {
			if intent.State == allowed {
				intent.State = to
				return nil
			}
		}
		if intent.State == to {
			return nil
		}
		return store.ErrImmutableConflict
	})
}

func (a *Journal) MarkCodexReviewIntentPrepared(ctx context.Context, runID string) error {
	return a.transitionCodexReviewIntent(ctx, runID,
		[]ward.CodexReviewIntentState{ward.CodexReviewIntentPreparing}, ward.CodexReviewIntentPrepared, true)
}

func (a *Journal) MarkCodexReviewIntentStarting(ctx context.Context, runID string) error {
	return a.transitionCodexReviewIntent(ctx, runID,
		[]ward.CodexReviewIntentState{ward.CodexReviewIntentPrepared}, ward.CodexReviewIntentStarting, false)
}

func (a *Journal) MarkCodexReviewIntentStarted(ctx context.Context, runID string) error {
	return a.transitionCodexReviewIntent(ctx, runID,
		[]ward.CodexReviewIntentState{ward.CodexReviewIntentStarting}, ward.CodexReviewIntentStarted, false)
}

func (a *Journal) CloseCodexReviewIntent(ctx context.Context, runID string) error {
	return a.transitionCodexReviewIntent(ctx, runID,
		[]ward.CodexReviewIntentState{
			ward.CodexReviewIntentPreparing, ward.CodexReviewIntentPrepared,
			ward.CodexReviewIntentStarting, ward.CodexReviewIntentStarted,
		}, ward.CodexReviewIntentClosed, false)
}

func (a *Journal) PutCodexReviewBinding(
	ctx context.Context, binding ward.CodexReviewJournalBinding,
) error {
	body, err := marshalCodexReview(binding)
	if err != nil {
		return err
	}
	return classifyCodexReviewMutation(a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.RecordCodexReviewBinding(ctx, binding.RunID, body)
	}))
}

func (a *Journal) GetCodexReviewBinding(
	ctx context.Context, runID string,
) (ward.CodexReviewJournalBinding, error) {
	var record store.CodexReviewOpaqueRecord
	err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		record, err = tx.GetCodexReviewBinding(ctx, runID)
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ward.CodexReviewJournalBinding{}, ward.ErrCodexReviewBindingNotFound
		}
		return ward.CodexReviewJournalBinding{}, err
	}
	if err := verifyCodexReviewBody(record); err != nil {
		return ward.CodexReviewJournalBinding{}, errors.Join(ward.ErrConformance, err)
	}
	binding, err := decodeCodexReview[ward.CodexReviewJournalBinding](record.Body)
	if err != nil {
		return ward.CodexReviewJournalBinding{}, errors.Join(ward.ErrConformance, err)
	}
	if binding.RunID != runID {
		return ward.CodexReviewJournalBinding{},
			errors.Join(ward.ErrConformance, domain.ErrParentKeyMismatch)
	}
	return binding, nil
}

// AuthStoreVolume returns the trusted identity-to-volume binding.
func (a *Leaser) AuthStoreVolume(
	ctx context.Context, id domain.AuthIdentityID,
) (string, error) {
	var volume string
	err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		identity, err := tx.GetAuthIdentity(ctx, id)
		if err != nil {
			return err
		}
		volume = identity.AuthStoreVolume
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("auth-store volume for identity %q: %w", id, err)
	}
	return volume, nil
}

// Acquire opens or converges on one mutation window.
func (a *Leaser) Acquire(
	ctx context.Context,
	id domain.AuthIdentityID,
	holder domain.InvocationID,
	now, expiresAt time.Time,
) (domain.AuthStoreMutationLease, error) {
	var lease domain.AuthStoreMutationLease
	err := a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		lease, err = tx.AcquireAuthStoreMutationLease(ctx, id, holder, now, expiresAt)
		return err
	})
	if err != nil {
		return domain.AuthStoreMutationLease{}, err
	}
	return lease, nil
}

// Get reconstructs the identity's current mutation-window row.
func (a *Leaser) Get(
	ctx context.Context, id domain.AuthIdentityID,
) (domain.AuthStoreMutationLease, error) {
	var lease domain.AuthStoreMutationLease
	err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		lease, err = tx.GetAuthStoreMutationLease(ctx, id)
		return err
	})
	if err != nil {
		return domain.AuthStoreMutationLease{}, err
	}
	return lease, nil
}

// Release ends the exact held window. Store refusals that mean the recorded
// window already ended map to ward's convergence sentinel.
func (a *Leaser) Release(
	ctx context.Context,
	id domain.AuthIdentityID,
	holder domain.InvocationID,
	fence int64,
	releasedAt time.Time,
) error {
	err := a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.ReleaseAuthStoreMutationLease(ctx, id, holder, fence, releasedAt)
	})
	if errors.Is(err, store.ErrLeaseNotHeld) || errors.Is(err, store.ErrLeaseWindowRegresses) {
		return fmt.Errorf("%w: %w", ward.ErrLeaseWindowEnded, err)
	}
	return err
}

// Begin opens an unleased journal record.
func (a *Journal) Begin(ctx context.Context, rec ward.HandoffJournalRecord) error {
	if err := rec.Validate(); err != nil {
		return err
	}
	return a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.BeginHandoffJournal(ctx, toStoreRecord(rec))
	})
}

// BeginLeased atomically acquires the mutation window and opens the journal
// record carrying its exact reference.
func (a *Journal) BeginLeased(
	ctx context.Context,
	rec ward.HandoffJournalRecord,
	claim ward.AuthStoreLeaseClaim,
	now, expiresAt time.Time,
) (domain.AuthStoreMutationLease, error) {
	if err := rec.Validate(); err != nil {
		return domain.AuthStoreMutationLease{}, err
	}
	var lease domain.AuthStoreMutationLease
	err := a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		lease, err = tx.BeginLeasedHandoffJournal(
			ctx, toStoreRecord(rec), claim.AuthIdentityID, claim.Holder, now, expiresAt,
		)
		return err
	})
	if err != nil {
		return domain.AuthStoreMutationLease{}, err
	}
	return lease, nil
}

// Get reconstructs one journal record and re-runs both store and ward gates.
func (a *Journal) Get(
	ctx context.Context, runID string,
) (ward.HandoffJournalRecord, error) {
	var rec store.HandoffJournalRecord
	err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		rec, err = tx.GetHandoffJournal(ctx, runID)
		return err
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ward.HandoffJournalRecord{}, fmt.Errorf(
				"%w: %w", ward.ErrJournalRecordNotFound, err)
		}
		return ward.HandoffJournalRecord{}, err
	}
	converted := fromStoreRecord(rec)
	if err := converted.Validate(); err != nil {
		return ward.HandoffJournalRecord{}, err
	}
	return converted, nil
}

// MarkSeedObserved commits the pre-writer base proof.
func (a *Journal) MarkSeedObserved(ctx context.Context, runID, observedBaseSHA string) error {
	return a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkHandoffSeedObserved(ctx, runID, observedBaseSHA)
	})
}

// MarkCredentialObserved commits the pre-writer credential digest.
func (a *Journal) MarkCredentialObserved(ctx context.Context, runID, preDigest string) error {
	return a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkHandoffCredentialObserved(ctx, runID, preDigest)
	})
}

// MarkStatePrepared commits the lifecycle-scoped state-volume binding.
func (a *Journal) MarkStatePrepared(
	ctx context.Context,
	runID string,
	state ward.HandoffJournalState,
) error {
	return a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkHandoffStatePrepared(ctx, runID, store.HandoffJournalState{
			ConfigRootFingerprint:     state.ConfigRootFingerprint,
			ContinuityFingerprint:     state.ContinuityFingerprint,
			SessionScratchFingerprint: state.SessionScratchFingerprint,
			ConfigRootTarget:          state.ConfigRootTarget,
			ContinuityTarget:          state.ContinuityTarget,
			SessionScratchTarget:      state.SessionScratchTarget,
			ConfigRootReadOnly:        state.ConfigRootReadOnly,
			ContinuityReadOnly:        state.ContinuityReadOnly,
			SessionScratchReadOnly:    state.SessionScratchReadOnly,
			ConfigRootDigest:          state.ConfigRootDigest,
			ContinuityDigest:          state.ContinuityDigest,
			SessionScratchDigest:      state.SessionScratchDigest,
		})
	})
}

// MarkInstructionsPrepared commits the explicit instruction-bundle binding.
func (a *Journal) MarkInstructionsPrepared(
	ctx context.Context,
	runID string,
	instructions ward.HandoffJournalInstructions,
) error {
	return a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkHandoffInstructionsPrepared(
			ctx,
			runID,
			store.HandoffJournalInstructions{
				CompositionVersion:       instructions.CompositionVersion,
				HostDigest:               instructions.HostDigest,
				RepositoryManifestDigest: instructions.RepositoryManifestDigest,
				BundleDigest:             instructions.BundleDigest,
			},
		)
	})
}

// MarkWriterComplete commits the writer-complete proof.
func (a *Journal) MarkWriterComplete(ctx context.Context, runID string) error {
	return a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkHandoffWriterComplete(ctx, runID)
	})
}

// MarkCancellationRequested commits daemon cancellation intent.
func (a *Journal) MarkCancellationRequested(ctx context.Context, runID string) error {
	return a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkHandoffCancellationRequested(ctx, runID)
	})
}

// MarkWriterFailed commits the authenticated nonzero launcher status.
func (a *Journal) MarkWriterFailed(ctx context.Context, runID string, status int) error {
	return a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkHandoffWriterFailed(ctx, runID, status)
	})
}

// MarkExportMaterialized commits the verified export's host location.
func (a *Journal) MarkExportMaterialized(ctx context.Context, runID, exportDir string) error {
	return a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.MarkHandoffExportMaterialized(ctx, runID, exportDir)
	})
}

// Close commits the terminal journal outcome.
func (a *Journal) Close(
	ctx context.Context, runID string, outcome ward.HandoffJournalOutcome,
) error {
	return a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.CloseHandoffJournal(ctx, runID, store.HandoffJournalOutcome(outcome))
	})
}

func toStoreRecord(rec ward.HandoffJournalRecord) store.HandoffJournalRecord {
	converted := store.HandoffJournalRecord{
		RunID:                 rec.RunID,
		OwnershipToken:        rec.OwnershipToken,
		SpecDigest:            rec.SpecDigest,
		ObservedBaseSHA:       rec.ObservedBaseSHA,
		CredentialPreDigest:   rec.CredentialPreDigest,
		WriterComplete:        rec.WriterComplete,
		CancellationRequested: rec.CancellationRequested,
		WriterFailureStatus:   rec.WriterFailureStatus,
		ExportDir:             rec.ExportDir,
		OpenedAt:              rec.OpenedAt.UTC(),
	}
	if rec.Lease != nil {
		converted.Lease = &store.HandoffJournalLease{
			AuthIdentityID: rec.Lease.AuthIdentityID,
			Holder:         rec.Lease.Holder,
			Fence:          rec.Lease.Fence,
			AcquiredAt:     rec.Lease.AcquiredAt.UTC(),
			ExpiresAt:      rec.Lease.ExpiresAt.UTC(),
		}
	}
	if rec.State != nil {
		converted.State = &store.HandoffJournalState{
			ConfigRootFingerprint:     rec.State.ConfigRootFingerprint,
			ContinuityFingerprint:     rec.State.ContinuityFingerprint,
			SessionScratchFingerprint: rec.State.SessionScratchFingerprint,
			ConfigRootTarget:          rec.State.ConfigRootTarget,
			ContinuityTarget:          rec.State.ContinuityTarget,
			SessionScratchTarget:      rec.State.SessionScratchTarget,
			ConfigRootReadOnly:        rec.State.ConfigRootReadOnly,
			ContinuityReadOnly:        rec.State.ContinuityReadOnly,
			SessionScratchReadOnly:    rec.State.SessionScratchReadOnly,
			ConfigRootDigest:          rec.State.ConfigRootDigest,
			ContinuityDigest:          rec.State.ContinuityDigest,
			SessionScratchDigest:      rec.State.SessionScratchDigest,
		}
	}
	if rec.Instructions != nil {
		converted.Instructions = &store.HandoffJournalInstructions{
			CompositionVersion:       rec.Instructions.CompositionVersion,
			HostDigest:               rec.Instructions.HostDigest,
			RepositoryManifestDigest: rec.Instructions.RepositoryManifestDigest,
			BundleDigest:             rec.Instructions.BundleDigest,
		}
	}
	if rec.Outcome != nil {
		outcome := store.HandoffJournalOutcome(*rec.Outcome)
		converted.Outcome = &outcome
	}
	return converted
}

func fromStoreRecord(rec store.HandoffJournalRecord) ward.HandoffJournalRecord {
	converted := ward.HandoffJournalRecord{
		RunID:                 rec.RunID,
		OwnershipToken:        rec.OwnershipToken,
		SpecDigest:            rec.SpecDigest,
		ObservedBaseSHA:       rec.ObservedBaseSHA,
		CredentialPreDigest:   rec.CredentialPreDigest,
		WriterComplete:        rec.WriterComplete,
		CancellationRequested: rec.CancellationRequested,
		WriterFailureStatus:   rec.WriterFailureStatus,
		ExportDir:             rec.ExportDir,
		OpenedAt:              rec.OpenedAt,
	}
	if rec.Lease != nil {
		converted.Lease = &ward.HandoffJournalLease{
			AuthIdentityID: rec.Lease.AuthIdentityID,
			Holder:         rec.Lease.Holder,
			Fence:          rec.Lease.Fence,
			AcquiredAt:     rec.Lease.AcquiredAt,
			ExpiresAt:      rec.Lease.ExpiresAt,
		}
	}
	if rec.State != nil {
		converted.State = &ward.HandoffJournalState{
			ConfigRootFingerprint:     rec.State.ConfigRootFingerprint,
			ContinuityFingerprint:     rec.State.ContinuityFingerprint,
			SessionScratchFingerprint: rec.State.SessionScratchFingerprint,
			ConfigRootTarget:          rec.State.ConfigRootTarget,
			ContinuityTarget:          rec.State.ContinuityTarget,
			SessionScratchTarget:      rec.State.SessionScratchTarget,
			ConfigRootReadOnly:        rec.State.ConfigRootReadOnly,
			ContinuityReadOnly:        rec.State.ContinuityReadOnly,
			SessionScratchReadOnly:    rec.State.SessionScratchReadOnly,
			ConfigRootDigest:          rec.State.ConfigRootDigest,
			ContinuityDigest:          rec.State.ContinuityDigest,
			SessionScratchDigest:      rec.State.SessionScratchDigest,
		}
	}
	if rec.Instructions != nil {
		converted.Instructions = &ward.HandoffJournalInstructions{
			CompositionVersion:       rec.Instructions.CompositionVersion,
			HostDigest:               rec.Instructions.HostDigest,
			RepositoryManifestDigest: rec.Instructions.RepositoryManifestDigest,
			BundleDigest:             rec.Instructions.BundleDigest,
		}
	}
	if rec.Outcome != nil {
		outcome := ward.HandoffJournalOutcome(*rec.Outcome)
		converted.Outcome = &outcome
	}
	return converted
}

var (
	_ ward.AuthStoreLeaser     = (*Leaser)(nil)
	_ ward.HandoffJournal      = (*Journal)(nil)
	_ ward.LeasedHandoffOpener = (*Journal)(nil)
)
