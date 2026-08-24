// Package wardstore composes ward's persistence ports with the authoritative
// SQLite store. It is deliberately separate from both packages: ward owns the
// runner contract, store owns durable state, and this boundary owns only the
// transaction wrappers and vocabulary mapping between them.
package wardstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
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
	if err := strictjson.Decode(body, &value, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
		if errors.Is(err, strictjson.ErrTrailingData) {
			return value, errors.New("decode Codex review journal: trailing JSON value")
		}
		return value, fmt.Errorf("decode Codex review journal: %w", err)
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
	bodyDigest := contentaddr.Sum(record.Body)
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
	bodyDigest := contentaddr.Sum(authority)
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
	Journal    *Journal
	Leaser     *Leaser
	AuthState  *AuthState
	Enrollment *Enrollment
}

// Journal backs ward's journal and atomic leased-open interfaces.
type Journal struct {
	store *store.Store
}

// Leaser backs ward's identity binding and mutation-lease interface.
type Leaser struct {
	store *store.Store
}

// AuthState backs ward's durable, identity-scoped Codex re-enrollment marker.
type AuthState struct {
	store *store.Store
	now   func() time.Time
}

// Enrollment backs ward's Codex enrollment journal and verified projection
// port. It owns the transaction that creates an initial identity and marker
// before acquiring the exact mutation lease.
type Enrollment struct {
	store     *store.Store
	authState *AuthState
}

// New constructs the production ward persistence adapters.
func New(st *store.Store) (*Adapters, error) {
	if st == nil {
		return nil, errors.New("ward store adapters: nil store")
	}
	authState := &AuthState{store: st, now: time.Now}
	return &Adapters{
		Journal:    &Journal{store: st},
		Leaser:     &Leaser{store: st},
		AuthState:  authState,
		Enrollment: &Enrollment{store: st, authState: authState},
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

// GetIdentity reconstructs the current trusted identity declaration.
func (a *Leaser) GetIdentity(
	ctx context.Context, id domain.AuthIdentityID,
) (domain.AuthIdentity, error) {
	var identity domain.AuthIdentity
	err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		identity, err = tx.GetAuthIdentity(ctx, id)
		return err
	})
	if err != nil {
		return domain.AuthIdentity{}, fmt.Errorf("auth identity %q: %w", id, err)
	}
	return identity, nil
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
		volume = identity.Interim.AuthStoreVolume
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("auth-store volume for identity %q: %w", id, err)
	}
	return volume, nil
}

func codexAuthReenrollmentItem(
	id domain.AuthIdentityID,
	occurrence int,
	projectID domain.ProjectID,
	version int,
	status domain.ItemStatus,
	binding *domain.CodexReenrollmentRecoveryBinding,
) (domain.AttentionItem, error) {
	return store.NewCodexReenrollmentMarker(id, occurrence, projectID, version, status, binding)
}

func codexAuthReenrollmentItemAt(
	id domain.AuthIdentityID,
	occurrence int,
	projectID domain.ProjectID,
	version int,
	status domain.ItemStatus,
	binding *domain.CodexReenrollmentRecoveryBinding,
	createdAt time.Time,
) (domain.AttentionItem, error) {
	item, err := codexAuthReenrollmentItem(id, occurrence, projectID, version, status, binding)
	if err != nil {
		return domain.AttentionItem{}, err
	}
	createdAt = createdAt.UTC()
	item.CreatedAt = &createdAt
	if err := item.Validate(); err != nil {
		return domain.AttentionItem{}, err
	}
	return item, nil
}

func validateCodexAuthReenrollmentItem(
	item domain.AttentionItem, id domain.AuthIdentityID,
) (int, error) {
	return store.CodexReenrollmentMarkerOccurrence(item, id)
}

type codexAuthReenrollmentOccurrences struct {
	latest           domain.AttentionItem
	latestOccurrence int
	open             []domain.AttentionItem
}

func scanCodexAuthReenrollmentOccurrences(
	items []store.Snapshotted[domain.AttentionItem], id domain.AuthIdentityID,
) (codexAuthReenrollmentOccurrences, error) {
	var occurrences codexAuthReenrollmentOccurrences
	for _, snapshot := range items {
		item := snapshot.Value
		occurrence, err := validateCodexAuthReenrollmentItem(item, id)
		if err != nil {
			return codexAuthReenrollmentOccurrences{}, err
		}
		if occurrence == 0 {
			continue
		}
		if occurrence > occurrences.latestOccurrence {
			occurrences.latest = item
			occurrences.latestOccurrence = occurrence
		}
		if item.Status == domain.StatusOpen {
			occurrences.open = append(occurrences.open, item)
		}
	}
	return occurrences, nil
}

// NeedsCodexAuthReenrollment authenticates the newest occurrence for id. The
// item is advisory globally; this identity-specific predicate is the refusal.
func (a *AuthState) NeedsCodexAuthReenrollment(
	ctx context.Context, id domain.AuthIdentityID,
) (bool, error) {
	var needs bool
	err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		occurrences, err := scanCodexAuthReenrollmentOccurrences(items, id)
		if err != nil {
			return err
		}
		if len(occurrences.open) > 1 {
			return errors.New("codex auth identity has multiple open re-enrollment items")
		}
		if occurrences.latestOccurrence == 0 {
			needs = false
			return nil
		}
		latestItem := occurrences.latest
		if len(occurrences.open) > 0 || latestItem.Status != domain.StatusResolved ||
			latestItem.CodexReenrollmentRecoveryBinding == nil {
			needs = true
			return nil
		}
		latestJournal, found, err := tx.LatestCodexReenrollmentJournal(ctx, id)
		if err != nil {
			return err
		}
		if !found {
			needs = true
			return nil
		}
		binding, err := latestJournal.RecoveryBinding()
		if err != nil {
			if errors.Is(err, store.ErrCodexReenrollmentNotVerified) {
				needs = true
				return nil
			}
			return err
		}
		if latestJournal.MarkerItemID != latestItem.ID ||
			*latestItem.CodexReenrollmentRecoveryBinding != binding {
			needs = true
			return nil
		}
		transition, found, err := tx.LatestCodexReenrollmentRecoveryTransition(ctx, id)
		if err != nil {
			return err
		}
		if !found {
			needs = true
			return nil
		}
		carrier, err := tx.CodexReenrollmentRecoveryCarrier(ctx, transition)
		if err != nil {
			return err
		}
		needs = carrier != latestItem.ID || transition.Binding() != binding
		return nil
	})
	return needs, err
}

// MarkCodexAuthNeedsReenrollment converges on an unbound occurrence. A later
// revocation supersedes a marker that already carries, or has completed, a
// verified operation so the older authority cannot clear the new failure.
func (a *AuthState) MarkCodexAuthNeedsReenrollment(
	ctx context.Context, runID domain.RunID, id domain.AuthIdentityID,
) error {
	return a.store.Write(ctx, func(tx *store.WriteTx) error {
		run, err := tx.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		occurrences, err := scanCodexAuthReenrollmentOccurrences(items, id)
		if err != nil {
			return err
		}
		if len(occurrences.open) > 1 {
			return errors.New("codex auth identity has multiple open re-enrollment items")
		}
		if len(occurrences.open) == 1 {
			current := occurrences.open[0]
			if current.ID != occurrences.latest.ID {
				return errors.New("codex auth identity open re-enrollment item is not its latest occurrence")
			}
			supersede := current.CodexReenrollmentRecoveryBinding != nil
			if !supersede {
				latest, found, err := tx.LatestCodexReenrollmentJournal(ctx, id)
				if err != nil {
					return err
				}
				supersede = found && latest.MarkerItemID == current.ID && latest.Terminal != nil &&
					latest.Terminal.Outcome == store.CodexReenrollmentVerified
			}
			if !supersede {
				return nil
			}
			current.Status = domain.StatusSuperseded
			current.ItemVersion++
			if err := tx.PutAttentionItem(ctx, current); err != nil {
				return err
			}
		}
		nextOccurrence, err := store.NextCodexReenrollmentMarkerOccurrence(occurrences.latestOccurrence)
		if err != nil {
			return err
		}
		item, err := codexAuthReenrollmentItemAt(
			id, nextOccurrence, run.ProjectID, 1, domain.StatusOpen, nil, a.now(),
		)
		if err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, item)
	})
}

// ProjectVerifiedCodexReenrollment attaches the latest verified operation to
// the exact open revoked-identity marker and exposes its resolving action. The
// synchronized item update and journal re-read share one SQLite transaction.
func (a *AuthState) ProjectVerifiedCodexReenrollment(
	ctx context.Context, id domain.AuthIdentityID,
) (domain.AttentionItem, error) {
	var projected domain.AttentionItem
	err := a.store.Write(ctx, func(tx *store.WriteTx) error {
		latest, found, err := tx.LatestCodexReenrollmentJournal(ctx, id)
		if err != nil {
			return err
		}
		if !found {
			return store.ErrNotFound
		}
		binding, err := latest.RecoveryBinding()
		if err != nil {
			return err
		}
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		occurrences, err := scanCodexAuthReenrollmentOccurrences(items, id)
		if err != nil {
			return err
		}
		if len(occurrences.open) != 1 {
			return fmt.Errorf("project codex re-enrollment binding: found %d open markers", len(occurrences.open))
		}
		item := occurrences.open[0]
		if item.ID != occurrences.latest.ID {
			return domain.ErrCodexReenrollmentMarkerMismatch
		}
		if item.ID != latest.MarkerItemID {
			return domain.ErrCodexReenrollmentMarkerMismatch
		}
		if item.CodexReenrollmentRecoveryBinding != nil {
			if *item.CodexReenrollmentRecoveryBinding == binding && item.Offers(domain.ActionResolveReenrollment) {
				projected = item
				return nil
			}
			return domain.ErrCodexReenrollmentBindingMismatch
		}
		item.CodexReenrollmentRecoveryBinding = &binding
		item.RequestedDecision = append(item.RequestedDecision, domain.ActionResolveReenrollment)
		item.ItemVersion++
		if err := tx.PutAttentionItem(ctx, item); err != nil {
			return err
		}
		projected = item
		return nil
	})
	return projected, err
}

// Begin creates an initial identity and marker when needed, or authenticates
// the current unbound marker for an existing identity, then opens the #684
// journal under the exact lease in the same synchronized transaction.
func (a *Enrollment) Begin(
	ctx context.Context,
	identity domain.AuthIdentity,
	projectID domain.ProjectID,
	holder domain.InvocationID,
	now, expiresAt time.Time,
) (domain.AuthStoreMutationLease, error) {
	var lease domain.AuthStoreMutationLease
	err := a.store.Write(ctx, func(tx *store.WriteTx) error {
		stored, err := tx.GetAuthIdentity(ctx, identity.ID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			if err := tx.RecordAuthIdentity(ctx, identity, now); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			// The fixed-bindings predicate, not whole-record equality: the
			// operator fields (enabled, cost owner, budget) and the set-once
			// account binding may lawfully differ from what enrollment
			// constructs, and comparing them here would refuse re-enrollment
			// of an identity the operator has since characterized.
			if !stored.SameFixedBindings(identity) {
				return fmt.Errorf("existing Codex auth identity has incompatible fixed bindings: %w",
					domain.ErrImmutableTransition)
			}
		}

		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		occurrences, err := scanCodexAuthReenrollmentOccurrences(items, identity.ID)
		if err != nil {
			return err
		}
		if len(occurrences.open) > 1 {
			return errors.New("codex auth identity has multiple open re-enrollment items")
		}
		var marker domain.AttentionItem
		if len(occurrences.open) == 1 {
			marker = occurrences.open[0]
			if marker.ID != occurrences.latest.ID {
				return domain.ErrCodexReenrollmentMarkerMismatch
			}
			// A valid, explicit replacement enrollment has already authenticated
			// fresh operator input before reaching this port. It supersedes a
			// projected marker whose bound store no longer serves that authority,
			// rather than letting stale verified evidence block repair forever.
			if marker.CodexReenrollmentRecoveryBinding != nil ||
				marker.Offers(domain.ActionResolveReenrollment) {
				marker.Status = domain.StatusSuperseded
				marker.ItemVersion++
				if err := tx.PutAttentionItem(ctx, marker); err != nil {
					return err
				}
				marker = domain.AttentionItem{}
			}
		}
		if marker.ID == "" {
			next, err := store.NextCodexReenrollmentMarkerOccurrence(occurrences.latestOccurrence)
			if err != nil {
				return err
			}
			marker, err = codexAuthReenrollmentItemAt(
				identity.ID, next, projectID, 1, domain.StatusOpen, nil, now,
			)
			if err != nil {
				return err
			}
			if err := tx.PutAttentionItem(ctx, marker); err != nil {
				return err
			}
		}
		_, lease, err = tx.BeginCodexReenrollmentJournal(
			ctx, identity.ID, marker.ID, holder, now, expiresAt,
		)
		return err
	})
	return lease, err
}

// Fail records one credential-free terminal class. If an ordinary terminal
// proves impossible because the lease ended, retry with #684's constrained
// lease_lost outcome; the store accepts it only for this original holder and
// exact expired, released, or superseded fence.
func (a *Enrollment) Fail(
	ctx context.Context,
	id domain.AuthIdentityID,
	holder domain.InvocationID,
	fence int64,
	class ward.CodexAuthEnrollmentFailure,
	at time.Time,
) error {
	failure, err := codexEnrollmentFailureClass(class)
	if err != nil {
		return err
	}
	record := func(value store.CodexReenrollmentFailureClass) error {
		return a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.FailCodexReenrollment(ctx, id, holder, fence, value, at)
		})
	}
	err = record(failure)
	if !errors.Is(err, store.ErrCodexReenrollmentLeaseMismatch) {
		return err
	}
	return record(store.CodexReenrollmentLeaseLost)
}

func codexEnrollmentFailureClass(
	class ward.CodexAuthEnrollmentFailure,
) (store.CodexReenrollmentFailureClass, error) {
	registered := false
	for _, candidate := range ward.AllCodexAuthEnrollmentFailures {
		if class == candidate {
			registered = true
			break
		}
	}
	if !registered {
		return "", fmt.Errorf("unknown Codex auth enrollment failure class %q", class)
	}
	switch class {
	case ward.CodexAuthEnrollmentReplacementFailed:
		return store.CodexReenrollmentAuthStoreReplacementFailed, nil
	case ward.CodexAuthEnrollmentVerificationFailed:
		return store.CodexReenrollmentVerificationFailed, nil
	}
	return "", fmt.Errorf("unknown Codex auth enrollment failure class %q", class)
}

// Verify records the exact digest and access-token expiry while this holder's
// lease is still live.
func (a *Enrollment) Verify(
	ctx context.Context,
	id domain.AuthIdentityID,
	holder domain.InvocationID,
	fence int64,
	digest domain.Digest,
	expiresAt, verifiedAt time.Time,
) error {
	return a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.VerifyCodexReenrollment(
			ctx, id, holder, fence, digest, expiresAt, verifiedAt,
		)
	})
}

// RecoverableVerified returns the latest verified coordinates only while
// their exact marker is still the sole open occurrence for this identity.
// The caller must independently re-verify the live auth-store bytes before
// projecting the resolving action.
func (a *Enrollment) RecoverableVerified(
	ctx context.Context,
	identity domain.AuthIdentity,
) (domain.CodexReenrollmentRecoveryBinding, bool, error) {
	var binding domain.CodexReenrollmentRecoveryBinding
	var found bool
	err := a.store.Read(ctx, func(tx *store.ReadTx) error {
		stored, err := tx.GetAuthIdentity(ctx, identity.ID)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if !stored.SameFixedBindings(identity) {
			return fmt.Errorf("existing Codex auth identity has incompatible fixed bindings: %w",
				domain.ErrImmutableTransition)
		}
		latest, exists, err := tx.LatestCodexReenrollmentJournal(ctx, identity.ID)
		if err != nil || !exists {
			return err
		}
		binding, err = latest.RecoveryBinding()
		if errors.Is(err, store.ErrCodexReenrollmentNotVerified) {
			return nil
		}
		if err != nil {
			return err
		}
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		occurrences, err := scanCodexAuthReenrollmentOccurrences(items, identity.ID)
		if err != nil {
			return err
		}
		if len(occurrences.open) == 1 && occurrences.open[0].ID == latest.MarkerItemID &&
			occurrences.open[0].ID == occurrences.latest.ID {
			found = true
		}
		return nil
	})
	return binding, found, err
}

// ProjectVerified exposes resolve_reenrollment only after the journal's
// verified terminal has committed.
func (a *Enrollment) ProjectVerified(
	ctx context.Context, id domain.AuthIdentityID,
) (domain.AttentionItem, error) {
	return a.authState.ProjectVerifiedCodexReenrollment(ctx, id)
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

// Renew extends the exact live mutation window.
func (a *Leaser) Renew(
	ctx context.Context,
	id domain.AuthIdentityID,
	holder domain.InvocationID,
	fence int64,
	now, expiresAt time.Time,
) (domain.AuthStoreMutationLease, error) {
	var lease domain.AuthStoreMutationLease
	err := a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		lease, err = tx.RenewAuthStoreMutationLease(ctx, id, holder, fence, now, expiresAt)
		return err
	})
	if err != nil {
		return domain.AuthStoreMutationLease{}, err
	}
	return lease, nil
}

// WithHeldLeaseMutation authenticates the exact live lease and keeps the
// store's immediate write transaction open through one short host-filesystem
// mutation. A successor therefore cannot acquire the expired generation in
// the interval between authentication and the syscall it authorizes.
func (a *Leaser) WithHeldLeaseMutation(
	ctx context.Context, expected domain.AuthStoreMutationLease,
	now func() time.Time, mutation func() error,
) error {
	if now == nil || mutation == nil {
		return errors.New("auth store lease mutation clock and callback are required")
	}
	return a.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
		lease, err := tx.GetAuthStoreMutationLease(ctx, expected.AuthIdentityID)
		if err != nil {
			return err
		}
		checkedAt := now()
		if expected.Validate() != nil || !expected.HeldAt(checkedAt) ||
			lease.AuthIdentityID != expected.AuthIdentityID || lease.Holder != expected.Holder ||
			lease.Fence != expected.Fence || !lease.AcquiredAt.Equal(expected.AcquiredAt) ||
			!lease.ExpiresAt.Equal(expected.ExpiresAt) || !lease.HeldAt(checkedAt) {
			return fmt.Errorf("%w: auth store mutation lease changed", ward.ErrLeaseWindowEnded)
		}
		return mutation()
	})
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
	_ ward.CodexAuthState      = (*AuthState)(nil)
	_ ward.HandoffJournal      = (*Journal)(nil)
	_ ward.LeasedHandoffOpener = (*Journal)(nil)
)
