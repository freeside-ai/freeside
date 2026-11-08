// What outlives the retired attended fake publication lane (#2790): the
// fake-lane outbox kinds and their codecs, the backup extractors that keep
// old fake-lane rows inside checkpoint closure, and the one-time converge
// that gives each pre-2026-08-02 fake-lane run a resolved policy row.
//
// The converge rewrites stored runs, so it re-checks before it writes: it
// rewrites a run only when the stored run is exactly the legacy v1 shape the
// task implies and no policy row exists, and refuses any other shape as
// corruption (fakePublicationPolicyState). A later contract unit replaces
// it with a store migration and deletes this file's converge.

package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/fakepublication"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	fakePublicationTaskKind               = fakepublication.TaskKind
	fakePublicationInvocationOwnerKind    = "engine.fake_publication_invocation_owner"
	fakePublicationInvocationOwnerVersion = "freeside.engine.fake-publication-invocation-owner/v1"
	fakePublicationTaskVersion            = fakepublication.TaskVersion
	fakePublicationStageName              = "fake_candidate_publication"
)

// FakePublicationTaskKind identifies the durable outbox payload whose recipe
// blob must remain in backup closure for replay after restore.
const FakePublicationTaskKind = fakePublicationTaskKind

// FakePublicationInvocationOwnerKind identifies a completed durable
// invocation-owner claim.
const FakePublicationInvocationOwnerKind = fakePublicationInvocationOwnerKind

type fakePublicationTask fakepublication.Task

type fakePublicationInvocationOwner struct {
	Version       string              `json:"version"`
	InvocationID  domain.InvocationID `json:"invocation_id"`
	Role          string              `json:"role"`
	BindingDigest domain.Digest       `json:"binding_digest"`
	RunID         domain.RunID        `json:"run_id"`
}

type fakePublicationPolicyRecovery struct {
	store *store.Store
	mu    sync.Mutex
	epoch string
}

// ConvergeLegacyFakePublicationPolicies completes the epoch's one-time
// dispatched-task policy upgrade. The daemon calls it synchronously before
// starting the scheduler; reconciliation retains the same gate for restores
// and callers that do not use the daemon composition root.
func (e *Engine) ConvergeLegacyFakePublicationPolicies(ctx context.Context) error {
	if e.fakePublicationPolicy == nil {
		return nil
	}
	return e.fakePublicationPolicy.converge(ctx)
}

func (r *fakePublicationPolicyRecovery) converge(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	var recoveryEpoch string
	var owned []store.QueueEntry
	if err := r.store.Read(ctx, func(tx *store.ReadTx) error {
		state, err := tx.ServerState(ctx)
		if err != nil {
			return err
		}
		recoveryEpoch = state.SyncEpoch
		if r.epoch == recoveryEpoch {
			return nil
		}
		pending, err := tx.ListPendingOutbox(ctx, fakePublicationTaskKind)
		if err != nil {
			return err
		}
		dispatched, err := tx.ListDispatchedOutbox(ctx, fakePublicationTaskKind)
		if err != nil {
			return err
		}
		owned = append(pending, dispatched...)
		return nil
	}); err != nil {
		return err
	}

	if r.epoch == recoveryEpoch {
		return nil
	}
	var recoveryErr error
	// Both pending and completed v1 tasks can own watches: publication arms them
	// before marking the task dispatched, so a crash can persist either status.
	// Converge all durable owners once per store epoch before processing new
	// work. A restart or in-place restore gets a fresh pass, while steady-state
	// reconciliation does not rescan unbounded history.
	for _, entry := range owned {
		task, err := decodeBoundFakePublicationTask(entry)
		if err == nil {
			err = ensureFakePublicationPolicy(ctx, r.store, task)
		}
		if err != nil {
			recoveryErr = errors.Join(
				recoveryErr,
				fmt.Errorf("task %q policy convergence: %w", entry.IdempotencyKey, err),
			)
		}
	}
	if recoveryErr != nil {
		return recoveryErr
	}
	r.epoch = recoveryEpoch
	return nil
}

type fakePublicationRunReader interface {
	GetRun(context.Context, domain.RunID) (domain.Run, error)
	GetResolvedPolicy(context.Context, domain.RunID) (domain.ResolvedPolicy, error)
}

// fakePublicationPolicyState recognizes exactly two representations: the
// current authenticated policy body, or the pre-upgrade v1 run whose trust
// profile digest stood in for policy and which has no policy row. Anything
// else is corruption, never migration input.
func fakePublicationPolicyState(
	ctx context.Context,
	reader fakePublicationRunReader,
	task fakePublicationTask,
) (bool, error) {
	run, err := reader.GetRun(ctx, task.RunID)
	if err != nil {
		return false, fmt.Errorf("load publication run %q: %w", task.RunID, err)
	}
	expected := publicationRun(task)
	expected.TaskID = run.TaskID
	if reflect.DeepEqual(run, expected) {
		policy, err := reader.GetResolvedPolicy(ctx, task.RunID)
		if err != nil {
			return false, fmt.Errorf("load publication resolved policy %q: %w", task.RunID, err)
		}
		if !reflect.DeepEqual(policy, publicationPolicy(task)) {
			return false, fmt.Errorf(
				"publication resolved policy %q disagrees with task: %w",
				task.RunID, domain.ErrParentKeyMismatch,
			)
		}
		return false, nil
	}
	legacyRun := legacyPublicationRun(task)
	legacyRun.TaskID = run.TaskID
	if !reflect.DeepEqual(run, legacyRun) {
		return false, fmt.Errorf(
			"publication run %q disagrees with task: %w",
			task.RunID, domain.ErrParentKeyMismatch,
		)
	}
	if _, err := reader.GetResolvedPolicy(ctx, task.RunID); err == nil {
		return false, fmt.Errorf(
			"legacy publication run %q unexpectedly has a resolved policy: %w",
			task.RunID, domain.ErrParentKeyMismatch,
		)
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, fmt.Errorf("load legacy publication resolved policy %q: %w", task.RunID, err)
	}
	return true, nil
}

func convergeFakePublicationPolicyTx(
	ctx context.Context,
	tx *store.WriteTx,
	task fakePublicationTask,
) (bool, error) {
	legacy, err := fakePublicationPolicyState(ctx, tx, task)
	if err != nil || !legacy {
		return false, err
	}
	stored, err := tx.GetRun(ctx, task.RunID)
	if err != nil {
		return false, err
	}
	updated := publicationRun(task)
	updated.TaskID = stored.TaskID
	if err := tx.MigrateLegacyTrustProfileRunPolicy(
		ctx, stored, updated, publicationPolicy(task),
	); err != nil {
		return false, err
	}
	return true, nil
}

func ensureFakePublicationPolicy(
	ctx context.Context,
	st *store.Store,
	task fakePublicationTask,
) error {
	legacy := false
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		legacy, err = fakePublicationPolicyState(ctx, tx, task)
		return err
	}); err != nil || !legacy {
		return err
	}
	err := st.Write(ctx, func(tx *store.WriteTx) error {
		migrated, err := convergeFakePublicationPolicyTx(ctx, tx, task)
		if err != nil {
			return err
		}
		if !migrated {
			return errReplay
		}
		return nil
	})
	if errors.Is(err, errReplay) {
		return nil
	}
	return err
}

func publicationRun(task fakePublicationTask) domain.Run {
	stageID := domain.StageID("publication-" + string(task.RunID))
	return domain.Run{
		ID: task.RunID, ProjectID: task.ProjectID,
		SpecDigest:   digestBytes(mustEncodeFakePublicationTask(task)),
		PolicyDigest: publicationPolicy(task).Digest,
		Stages: []domain.Stage{{
			ID: stageID, RunID: task.RunID, Name: fakePublicationStageName,
			Attempts: []domain.Attempt{{
				ID:      domain.AttemptID("attempt-" + string(task.PublicationInvocationID)),
				StageID: stageID, Number: 1, InvocationID: task.PublicationInvocationID,
			}},
		}},
	}
}

func legacyPublicationRun(task fakePublicationTask) domain.Run {
	run := publicationRun(task)
	run.PolicyDigest = task.TrustProfileDigest
	return run
}

func publicationPolicy(task fakePublicationTask) domain.ResolvedPolicy {
	policy, err := domain.NewResolvedPolicy(task.RunID, []domain.PolicyKey{{
		Key:   "trust_profile_digest",
		Value: string(task.TrustProfileDigest),
		Provenance: domain.KeyProvenance{
			Source: domain.ProvenanceOverride,
			Digest: task.TrustProfileDigest,
		},
	}})
	if err != nil {
		panic(fmt.Sprintf("construct fake publication policy: %v", err))
	}
	return policy
}

func fakePublicationTaskKey(runID domain.RunID) string {
	return fakepublication.TaskKey(runID)
}

func fakePublicationInvocationOwnerKey(invocationID domain.InvocationID) string {
	return fakePublicationInvocationOwnerKind + "/" + string(invocationID)
}

func encodeFakePublicationTask(task fakePublicationTask) ([]byte, error) {
	return fakepublication.EncodeTask(fakepublication.Task(task))
}

func mustEncodeFakePublicationTask(task fakePublicationTask) []byte {
	payload, err := encodeFakePublicationTask(task)
	if err != nil {
		panic(fmt.Sprintf("engine fake publication task invariant: %v", err))
	}
	return payload
}

func decodeFakePublicationTask(payload []byte) (fakePublicationTask, error) {
	task, err := fakepublication.DecodeTask(payload)
	return fakePublicationTask(task), err
}

// FakePublicationBackupPayloadDigests validates a durable fake-publication
// entry and returns the externally stored blobs required to replay it.
func FakePublicationBackupPayloadDigests(entry store.QueueEntry) ([]domain.Digest, error) {
	task, err := decodeBoundFakePublicationTask(entry)
	if err != nil {
		return nil, err
	}
	return []domain.Digest{task.RecipeDigest}, nil
}

// FakePublicationInvocationOwnerBackupPayloadDigests validates a completed
// invocation-owner entry. The claim is self-contained and needs no blobs.
func FakePublicationInvocationOwnerBackupPayloadDigests(
	entry store.QueueEntry,
) ([]domain.Digest, error) {
	if entry.Kind != FakePublicationInvocationOwnerKind || !entry.Dispatched() {
		return nil, domain.ErrParentKeyMismatch
	}
	owner, err := decodeFakePublicationInvocationOwner(entry.Payload)
	if err != nil {
		return nil, err
	}
	if entry.IdempotencyKey != fakePublicationInvocationOwnerKey(owner.InvocationID) {
		return nil, domain.ErrParentKeyMismatch
	}
	return nil, nil
}

func decodeBoundFakePublicationTask(entry store.QueueEntry) (fakePublicationTask, error) {
	if entry.Kind != FakePublicationTaskKind {
		return fakePublicationTask{}, domain.ErrParentKeyMismatch
	}
	task, err := decodeFakePublicationTask(entry.Payload)
	if err != nil {
		return fakePublicationTask{}, err
	}
	if entry.IdempotencyKey != fakePublicationTaskKey(task.RunID) {
		return fakePublicationTask{}, fmt.Errorf("names run %q: %w",
			task.RunID, domain.ErrParentKeyMismatch)
	}
	return task, nil
}

func decodeFakePublicationInvocationOwner(
	payload []byte,
) (fakePublicationInvocationOwner, error) {
	var owner fakePublicationInvocationOwner
	if err := strictjson.Decode(payload, &owner, strictjson.TolerateInvalidUTF8, strictjson.NoLimit); err != nil {
		if errors.Is(err, strictjson.ErrTrailingData) {
			return fakePublicationInvocationOwner{}, errors.New("trailing data")
		}
		return fakePublicationInvocationOwner{}, err
	}
	if owner.Version != fakePublicationInvocationOwnerVersion {
		return fakePublicationInvocationOwner{}, fmt.Errorf(
			"unknown publication invocation owner version %q", owner.Version,
		)
	}
	if owner.InvocationID == "" || owner.RunID == "" {
		return fakePublicationInvocationOwner{}, domain.ErrEmptyID
	}
	if !validSHA256Digest(owner.BindingDigest) {
		return fakePublicationInvocationOwner{}, domain.ErrEmptyField
	}
	if owner.Role != "verification" && owner.Role != "publication" {
		return fakePublicationInvocationOwner{}, fmt.Errorf(
			"unknown publication invocation role %q", owner.Role,
		)
	}
	return owner, nil
}

func validSHA256Digest(digest domain.Digest) bool {
	return contentaddr.Valid(string(digest))
}
