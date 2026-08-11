package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/elaborate"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const (
	KindElaborationInvocationRequested = "elaboration_invocation_requested"
	kindElaborationTerminal            = "elaboration_stage_terminal"
	// KindElaborationImplementationClaim reserves the future implementation
	// run identity before approval and remains dispatched for durable replay.
	KindElaborationImplementationClaim = "elaboration_implementation_claim"
	elaborationStageName               = "elaboration"
	elaborationRequestVersion          = "freeside.elaboration-request/v1"
	maxElaborationContractBytes        = strictjson.Limit(1 << 20)
)

var (
	ErrElaborationIterationsExhausted = errors.New("elaboration iteration budget exhausted")
	ErrSpecApprovalRequired           = errors.New("current specification is not approved")
)

type elaborationWorkflow struct {
	fetcher *elaborate.Fetcher
	blobs   *signet.BlobStore
	now     func() time.Time
}

type ElaborationConfig struct {
	Fetcher *elaborate.Fetcher
	Blobs   *signet.BlobStore
	Now     func() time.Time
}

func WithElaboration(cfg ElaborationConfig) Option {
	return func(e *Engine) error {
		if cfg.Fetcher == nil || cfg.Blobs == nil || cfg.Now == nil {
			return errors.New("with elaboration: fetcher, blob store, and clock are required")
		}
		e.elaboration = &elaborationWorkflow{fetcher: cfg.Fetcher, blobs: cfg.Blobs, now: cfg.Now}
		return nil
	}
}

func elaborationStageID(runID domain.RunID) domain.StageID {
	return domain.StageID("elaborate-" + string(runID))
}

func elaborationInvocationID(runID domain.RunID, iteration int) domain.InvocationID {
	return domain.InvocationID(fmt.Sprintf("inv-elaborate-%s-%d", runID, iteration))
}

func elaborationImplementationClaimKey(runID domain.RunID) string {
	return "claim-elaboration-implementation-" + string(runID)
}

// ElaborationRunSpec keeps the pre-approval run separate from the immutable
// implementation run. The latter does not exist until its current spec wins
// a digest-bound approval.
type ElaborationRunSpec struct {
	ElaborationRunID    domain.RunID
	ImplementationRunID domain.RunID
	ProjectID           domain.ProjectID
	SourceArtifactID    domain.ArtifactID
	PolicyArtifactID    domain.ArtifactID
	ResolvedPolicy      domain.ResolvedPolicy
	Publication         ProductionPublication
	WorkUnit            *domain.WorkUnitDeclarationInput
}

type elaborationRequest struct {
	Version             string                           `json:"version"`
	ElaborationRunID    domain.RunID                     `json:"elaboration_run_id"`
	ImplementationRunID domain.RunID                     `json:"implementation_run_id"`
	ProjectID           domain.ProjectID                 `json:"project_id"`
	InvocationID        domain.InvocationID              `json:"invocation_id"`
	Iteration           int                              `json:"iteration"`
	InputArtifactIDs    []domain.ArtifactID              `json:"input_artifact_ids"`
	PriorSpecArtifactID *domain.ArtifactID               `json:"prior_spec_artifact_id,omitempty"`
	FeedbackArtifactIDs []domain.ArtifactID              `json:"feedback_artifact_ids"`
	PolicyArtifactID    domain.ArtifactID                `json:"policy_artifact_id"`
	Publication         ProductionPublication            `json:"publication"`
	WorkUnit            *domain.WorkUnitDeclarationInput `json:"work_unit,omitempty"`
}

type elaborationTerminal struct {
	InvocationID        domain.InvocationID `json:"invocation_id"`
	Iteration           int                 `json:"iteration"`
	Status              exec.Status         `json:"status"`
	ResearchArtifactIDs []domain.ArtifactID `json:"research_artifact_ids"`
	SpecArtifactID      *domain.ArtifactID  `json:"spec_artifact_id,omitempty"`
	ApprovalItemID      *domain.ItemID      `json:"approval_item_id,omitempty"`
}

func SubmitElaborationRun(ctx context.Context, st *store.Store, spec ElaborationRunSpec) error {
	if st == nil || spec.ElaborationRunID == "" || spec.ImplementationRunID == "" ||
		spec.ElaborationRunID == spec.ImplementationRunID || spec.ProjectID == "" ||
		spec.SourceArtifactID == "" || spec.PolicyArtifactID == "" {
		return errors.New("submit elaboration run: distinct run IDs, project, source, and policy are required")
	}
	if spec.ResolvedPolicy.RunID != spec.ElaborationRunID {
		return fmt.Errorf("submit elaboration run: policy run %q differs from %q: %w",
			spec.ResolvedPolicy.RunID, spec.ElaborationRunID, domain.ErrParentKeyMismatch)
	}
	if _, err := elaborate.ParsePolicy(spec.ResolvedPolicy); err != nil {
		return fmt.Errorf("submit elaboration run: %w", err)
	}
	if err := spec.Publication.Validate(); err != nil {
		return fmt.Errorf("submit elaboration run: %w", err)
	}
	if spec.WorkUnit != nil {
		if _, err := domain.NewWorkUnitDeclaration(
			*spec.WorkUnit, spec.ImplementationRunID, spec.ProjectID, time.Unix(1, 0)); err != nil {
			return fmt.Errorf("submit elaboration run work unit: %w", err)
		}
	}
	invocationID := elaborationInvocationID(spec.ElaborationRunID, 1)
	request := elaborationRequest{
		Version: elaborationRequestVersion, ElaborationRunID: spec.ElaborationRunID,
		ImplementationRunID: spec.ImplementationRunID, ProjectID: spec.ProjectID,
		InvocationID: invocationID, Iteration: 1,
		InputArtifactIDs: []domain.ArtifactID{spec.SourceArtifactID},
		PolicyArtifactID: spec.PolicyArtifactID, Publication: spec.Publication,
		WorkUnit: cloneElaborationWorkUnit(spec.WorkUnit),
	}
	payload, err := encodeElaborationRequest(request)
	if err != nil {
		return err
	}
	invocation, err := domain.NewAgentInvocation(invocationID, request.InputArtifactIDs, nil, 0)
	if err != nil {
		return err
	}
	return st.Write(ctx, func(tx *store.WriteTx) error {
		source, err := tx.GetArtifact(ctx, spec.SourceArtifactID)
		if err != nil {
			return err
		}
		if source.Type != domain.ArtifactKindSpecification {
			return fmt.Errorf("elaboration source %q has type %q: %w", source.ID, source.Type, domain.ErrParentKeyMismatch)
		}
		policyArtifact, err := tx.GetArtifact(ctx, spec.PolicyArtifactID)
		if err != nil {
			return err
		}
		if policyArtifact.Type != domain.ArtifactKindPolicy || policyArtifact.Digest != spec.ResolvedPolicy.Digest {
			return fmt.Errorf("elaboration policy artifact disagrees with resolved policy: %w", domain.ErrParentKeyMismatch)
		}
		want := domain.Run{
			ID: spec.ElaborationRunID, ProjectID: spec.ProjectID,
			SpecDigest: source.Digest, PolicyDigest: spec.ResolvedPolicy.Digest,
			Stages: []domain.Stage{{
				ID: elaborationStageID(spec.ElaborationRunID), RunID: spec.ElaborationRunID,
				Name: elaborationStageName, Attempts: []domain.Attempt{},
			}},
		}
		if existing, err := tx.GetRun(ctx, want.ID); err == nil {
			stored, markerErr := tx.GetOutbox(ctx, string(invocationID))
			claim, claimErr := tx.GetOutbox(ctx,
				elaborationImplementationClaimKey(spec.ImplementationRunID))
			storedPolicy, policyErr := tx.GetResolvedPolicy(ctx, want.ID)
			storedInvocation, invocationErr := tx.GetAgentInvocation(ctx, invocationID)
			if existing.ProjectID != want.ProjectID || existing.SpecDigest != want.SpecDigest ||
				existing.PolicyDigest != want.PolicyDigest ||
				markerErr != nil || stored.Kind != KindElaborationInvocationRequested || !bytes.Equal(stored.Payload, payload) ||
				claimErr != nil || claim.Kind != KindElaborationImplementationClaim ||
				!claim.Dispatched() || !bytes.Equal(claim.Payload, payload) ||
				policyErr != nil || storedPolicy.Digest != spec.ResolvedPolicy.Digest ||
				!slices.Equal(storedPolicy.Keys, spec.ResolvedPolicy.Keys) || invocationErr != nil ||
				storedInvocation.ConversationID != nil ||
				!slices.Equal(storedInvocation.InputIDs, invocation.InputIDs) ||
				storedInvocation.ThroughSequence != invocation.ThroughSequence {
				return fmt.Errorf("stored elaboration run disagrees: %w", domain.ErrImmutableTransition)
			}
			if _, ok := findElaborationStage(existing); !ok {
				return fmt.Errorf("stored elaboration stage disagrees: %w", domain.ErrImmutableTransition)
			}
			if len(existing.Stages) != 1 {
				return fmt.Errorf("stored elaboration run has foreign stages: %w", domain.ErrImmutableTransition)
			}
			return nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if _, err := tx.GetRun(ctx, spec.ImplementationRunID); err == nil {
			return fmt.Errorf("implementation run %q already exists: %w",
				spec.ImplementationRunID, domain.ErrImmutableTransition)
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if err := tx.PutRun(ctx, want); err != nil {
			return err
		}
		if err := tx.PutResolvedPolicy(ctx, spec.ResolvedPolicy); err != nil {
			return err
		}
		if err := tx.PutAgentInvocation(ctx, invocation); err != nil {
			return err
		}
		entry, inserted, err := tx.EnqueueOutbox(ctx, string(invocationID), KindElaborationInvocationRequested, payload)
		if err != nil {
			return err
		}
		if !inserted || entry.Kind != KindElaborationInvocationRequested || !bytes.Equal(entry.Payload, payload) {
			return fmt.Errorf("create elaboration marker: %w", domain.ErrImmutableTransition)
		}
		claim, claimed, err := tx.EnqueueOutbox(ctx,
			elaborationImplementationClaimKey(spec.ImplementationRunID),
			KindElaborationImplementationClaim, payload)
		if err != nil {
			return err
		}
		if !claimed || claim.Kind != KindElaborationImplementationClaim || !bytes.Equal(claim.Payload, payload) {
			return fmt.Errorf("claim implementation run %q: %w",
				spec.ImplementationRunID, domain.ErrImmutableTransition)
		}
		if err := tx.MarkOutboxDispatched(ctx, claim.IdempotencyKey); err != nil {
			return err
		}
		return nil
	})
}

func cloneElaborationWorkUnit(in *domain.WorkUnitDeclarationInput) *domain.WorkUnitDeclarationInput {
	if in == nil {
		return nil
	}
	out := *in
	if in.BoundIssue != nil {
		issue := *in.BoundIssue
		out.BoundIssue = &issue
	}
	out.DependsOnIssues = slices.Clone(in.DependsOnIssues)
	out.DeclaredPaths = slices.Clone(in.DeclaredPaths)
	return &out
}

func (r elaborationRequest) validate() error {
	if r.Version != elaborationRequestVersion || r.ElaborationRunID == "" ||
		r.ImplementationRunID == "" || r.ElaborationRunID == r.ImplementationRunID ||
		r.ProjectID == "" || r.InvocationID == "" || r.Iteration < 1 ||
		r.PolicyArtifactID == "" || len(r.InputArtifactIDs) == 0 ||
		r.InvocationID != elaborationInvocationID(r.ElaborationRunID, r.Iteration) {
		return fmt.Errorf("invalid elaboration request identity: %w", domain.ErrParentKeyMismatch)
	}
	if err := r.Publication.Validate(); err != nil {
		return err
	}
	if r.WorkUnit != nil {
		if _, err := domain.NewWorkUnitDeclaration(
			*r.WorkUnit, r.ImplementationRunID, r.ProjectID, time.Unix(1, 0)); err != nil {
			return err
		}
	}
	seen := make(map[domain.ArtifactID]struct{}, len(r.InputArtifactIDs))
	for _, id := range r.InputArtifactIDs {
		if id == "" {
			return domain.ErrEmptyID
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("duplicate elaboration input %q: %w", id, domain.ErrDuplicate)
		}
		seen[id] = struct{}{}
	}
	if r.PriorSpecArtifactID != nil {
		if *r.PriorSpecArtifactID == "" {
			return domain.ErrEmptyID
		}
		if _, ok := seen[*r.PriorSpecArtifactID]; !ok {
			return fmt.Errorf("prior specification is not an invocation input: %w", domain.ErrParentKeyMismatch)
		}
	}
	feedback := make(map[domain.ArtifactID]struct{}, len(r.FeedbackArtifactIDs))
	for _, id := range r.FeedbackArtifactIDs {
		if id == "" || !strings.HasPrefix(string(id), "spec-feedback-") {
			return fmt.Errorf("invalid feedback artifact %q: %w", id, domain.ErrParentKeyMismatch)
		}
		if _, ok := seen[id]; !ok {
			return fmt.Errorf("feedback artifact is not an invocation input: %w", domain.ErrParentKeyMismatch)
		}
		if _, duplicate := feedback[id]; duplicate {
			return domain.ErrDuplicate
		}
		feedback[id] = struct{}{}
	}
	return nil
}

func encodeElaborationRequest(request elaborationRequest) ([]byte, error) {
	if err := request.validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode elaboration request: %w", err)
	}
	// Enforce the decoder's aggregate limit at encode time so an oversized
	// but otherwise-valid request (for example a work unit with many declared
	// paths) fails fast at submission instead of persisting a durable row that
	// decodeElaborationPayload later rejects on every reconcile pass, halting
	// dispatch for unrelated runs.
	if strictjson.Limit(len(body)) > maxElaborationContractBytes {
		return nil, fmt.Errorf("encoded elaboration request is %d bytes, over the %d-byte limit: %w",
			len(body), int64(maxElaborationContractBytes), domain.ErrClaimTextTooLarge)
	}
	return body, nil
}

func decodeElaborationRequest(entry store.QueueEntry) (elaborationRequest, error) {
	if entry.Kind != KindElaborationInvocationRequested {
		return elaborationRequest{}, fmt.Errorf("elaboration intent kind %q: %w", entry.Kind, domain.ErrParentKeyMismatch)
	}
	request, err := decodeElaborationPayload(entry.Payload)
	if err != nil {
		return elaborationRequest{}, err
	}
	if string(request.InvocationID) != entry.IdempotencyKey {
		return elaborationRequest{}, fmt.Errorf("elaboration request is not key-bound: %w", domain.ErrParentKeyMismatch)
	}
	return request, nil
}

func decodeElaborationPayload(payload []byte) (elaborationRequest, error) {
	var request elaborationRequest
	if err := strictjson.Decode(payload, &request, strictjson.RejectInvalidUTF8, maxElaborationContractBytes); err != nil {
		return elaborationRequest{}, fmt.Errorf("decode elaboration request: %w", err)
	}
	if err := request.validate(); err != nil {
		return elaborationRequest{}, err
	}
	canonical, err := encodeElaborationRequest(request)
	if err != nil {
		return elaborationRequest{}, err
	}
	if !bytes.Equal(canonical, payload) {
		return elaborationRequest{}, fmt.Errorf("elaboration request is not canonical: %w", domain.ErrParentKeyMismatch)
	}
	return request, nil
}

// ElaborationInvocationBackupPayloadDigests authenticates a dispatch marker
// for backup closure. Its artifact references are store IDs, not raw digests.
func ElaborationInvocationBackupPayloadDigests(entry store.QueueEntry) ([]domain.Digest, error) {
	if _, err := decodeElaborationRequest(entry); err != nil {
		return nil, err
	}
	return nil, nil
}

// ElaborationImplementationClaimBackupPayloadDigests authenticates the
// dispatched reservation marker for backup closure.
func ElaborationImplementationClaimBackupPayloadDigests(entry store.QueueEntry) ([]domain.Digest, error) {
	if entry.Kind != KindElaborationImplementationClaim || !entry.Dispatched() {
		return nil, domain.ErrParentKeyMismatch
	}
	request, err := decodeElaborationPayload(entry.Payload)
	if err != nil {
		return nil, err
	}
	if entry.IdempotencyKey != elaborationImplementationClaimKey(request.ImplementationRunID) {
		return nil, domain.ErrParentKeyMismatch
	}
	return nil, nil
}

func authenticateElaborationRoot(
	ctx context.Context, tx *store.ReadTx, request elaborationRequest,
) error {
	rootEntry, err := tx.GetOutbox(ctx, string(elaborationInvocationID(request.ElaborationRunID, 1)))
	if err != nil {
		return err
	}
	root, err := decodeElaborationRequest(rootEntry)
	if err != nil {
		return err
	}
	claim, err := tx.GetOutbox(ctx, elaborationImplementationClaimKey(request.ImplementationRunID))
	if err != nil {
		return err
	}
	if root.Iteration != 1 || len(root.InputArtifactIDs) != 1 || root.PriorSpecArtifactID != nil ||
		len(root.FeedbackArtifactIDs) != 0 || claim.Kind != KindElaborationImplementationClaim ||
		!claim.Dispatched() || !bytes.Equal(claim.Payload, rootEntry.Payload) ||
		request.ElaborationRunID != root.ElaborationRunID ||
		request.ImplementationRunID != root.ImplementationRunID ||
		request.ProjectID != root.ProjectID || request.PolicyArtifactID != root.PolicyArtifactID ||
		request.Publication != root.Publication || !sameElaborationWorkUnit(request.WorkUnit, root.WorkUnit) ||
		len(request.InputArtifactIDs) == 0 || request.InputArtifactIDs[0] != root.InputArtifactIDs[0] {
		return fmt.Errorf("elaboration request disagrees with initial claim: %w", domain.ErrParentKeyMismatch)
	}
	return nil
}

func sameElaborationWorkUnit(left, right *domain.WorkUnitDeclarationInput) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	if left.CompletionCriterion != right.CompletionCriterion ||
		left.ContractSerialized != right.ContractSerialized ||
		!slices.Equal(left.DependsOnIssues, right.DependsOnIssues) ||
		!slices.Equal(left.DeclaredPaths, right.DeclaredPaths) {
		return false
	}
	if left.BoundIssue == nil || right.BoundIssue == nil {
		return left.BoundIssue == nil && right.BoundIssue == nil
	}
	return *left.BoundIssue == *right.BoundIssue
}

func findElaborationStage(run domain.Run) (domain.Stage, bool) {
	for _, stage := range run.Stages {
		if stage.ID == elaborationStageID(run.ID) && stage.Name == elaborationStageName {
			return stage, true
		}
	}
	return domain.Stage{}, false
}

func (e *Engine) loadElaborationBinding(ctx context.Context, entry store.QueueEntry) (elaborationRequest, invocationBinding, error) {
	request, err := decodeElaborationRequest(entry)
	if err != nil {
		return elaborationRequest{}, invocationBinding{}, err
	}
	var binding invocationBinding
	err = e.store.Read(ctx, func(tx *store.ReadTx) error {
		if err := authenticateElaborationRoot(ctx, tx, request); err != nil {
			return err
		}
		var err error
		binding.run, err = tx.GetRun(ctx, request.ElaborationRunID)
		if err != nil {
			return err
		}
		binding.invocation, err = tx.GetAgentInvocation(ctx, request.InvocationID)
		if err != nil {
			return err
		}
		if binding.invocation.ConversationID != nil || !slices.Equal(binding.invocation.InputIDs, request.InputArtifactIDs) {
			return fmt.Errorf("elaboration invocation inputs disagree: %w", domain.ErrParentKeyMismatch)
		}
		if binding.run.ProjectID != request.ProjectID {
			return fmt.Errorf("elaboration run project disagrees: %w", domain.ErrParentKeyMismatch)
		}
		policy, err := tx.GetResolvedPolicy(ctx, request.ElaborationRunID)
		if err != nil {
			return err
		}
		if binding.run.PolicyDigest != policy.Digest {
			return fmt.Errorf("elaboration run policy disagrees: %w", domain.ErrParentKeyMismatch)
		}
		policyArtifact, err := tx.GetArtifact(ctx, request.PolicyArtifactID)
		if err != nil {
			return err
		}
		if policyArtifact.Type != domain.ArtifactKindPolicy || policyArtifact.Digest != policy.Digest {
			return fmt.Errorf("elaboration policy artifact disagrees: %w", domain.ErrParentKeyMismatch)
		}
		// Every non-source input must have been produced by one of this run's
		// own prior elaboration invocations. Research fetches, prior
		// specifications, and revision feedback each stamp the producing
		// elaboration invocation, so re-binding the decoded input to that
		// producer set rejects a retargeted request that adopts a foreign
		// run's artifact by type alone, before its bytes reach the elaborator
		// or the approved specification. The reconcile-side consumption gate
		// re-binds the terminal's own identities; this is the dispatch-side
		// counterpart for the accumulated inputs.
		settings, err := elaborate.ParsePolicy(policy)
		if err != nil {
			return err
		}
		// Bound the decoded iteration before using it as an allocation
		// capacity and loop count: validate() only requires Iteration >= 1,
		// so a retargeted request with a huge iteration would otherwise
		// force an unbounded map allocation and loop here.
		if request.Iteration > settings.MaxIterations {
			return fmt.Errorf("elaboration iteration %d exceeds the policy maximum %d: %w",
				request.Iteration, settings.MaxIterations, domain.ErrParentKeyMismatch)
		}
		validProducers := make(map[domain.InvocationID]struct{}, request.Iteration)
		for iteration := 1; iteration < request.Iteration; iteration++ {
			validProducers[elaborationInvocationID(binding.run.ID, iteration)] = struct{}{}
		}
		for index, id := range request.InputArtifactIDs {
			artifact, err := tx.GetArtifact(ctx, id)
			if err != nil {
				return err
			}
			if index == 0 {
				if artifact.Type != domain.ArtifactKindSpecification || artifact.Digest != binding.run.SpecDigest {
					return fmt.Errorf("elaboration source artifact disagrees: %w", domain.ErrParentKeyMismatch)
				}
				continue
			}
			if _, ok := validProducers[artifact.Provenance.ProducerInvocationID]; !ok {
				return fmt.Errorf("elaboration input %q producer %q is not a prior invocation of run %q: %w",
					artifact.ID, artifact.Provenance.ProducerInvocationID, binding.run.ID, domain.ErrParentKeyMismatch)
			}
			if request.PriorSpecArtifactID != nil && id == *request.PriorSpecArtifactID {
				if artifact.Type != domain.ArtifactKindSpecification {
					return fmt.Errorf("prior specification %q has type %q: %w",
						artifact.ID, artifact.Type, domain.ErrParentKeyMismatch)
				}
				continue
			}
			if artifact.Type != domain.ArtifactKindResearch {
				return fmt.Errorf("elaboration input %q has type %q: %w",
					artifact.ID, artifact.Type, domain.ErrParentKeyMismatch)
			}
			if slices.Contains(request.FeedbackArtifactIDs, id) &&
				(artifact.Provenance.ProducerClass != domain.ProducerDaemon ||
					artifact.Provenance.HeadBinding != domain.HeadIndependent) {
				return fmt.Errorf("feedback artifact %q has invalid provenance: %w",
					artifact.ID, domain.ErrParentKeyMismatch)
			}
		}
		return nil
	})
	if err != nil {
		return elaborationRequest{}, invocationBinding{}, err
	}
	if _, ok := findElaborationStage(binding.run); !ok || len(binding.run.Stages) != 1 {
		return elaborationRequest{}, invocationBinding{}, fmt.Errorf("elaboration stage missing: %w", domain.ErrParentKeyMismatch)
	}
	return request, binding, nil
}

func (e *Engine) ownsElaborationRun(ctx context.Context, run domain.Run) (bool, error) {
	if e.elaboration == nil {
		return false, nil
	}
	var entry store.QueueEntry
	err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, string(elaborationInvocationID(run.ID, 1)))
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	request, err := decodeElaborationRequest(entry)
	return err == nil && request.ElaborationRunID == run.ID, err
}

func (e *Engine) acceptElaborationAttempt(ctx context.Context, run domain.Run, attempt domain.Attempt) (bool, error) {
	if attempt.ID != attemptIDFor(attempt.InvocationID) || attempt.StageID != elaborationStageID(run.ID) {
		return false, fmt.Errorf("elaboration attempt binding disagrees: %w", domain.ErrParentKeyMismatch)
	}
	var entry store.QueueEntry
	var resolved domain.ResolvedPolicy
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		entry, err = tx.GetOutbox(ctx, string(attempt.InvocationID))
		if err != nil {
			return err
		}
		resolved, err = tx.GetResolvedPolicy(ctx, run.ID)
		if err != nil {
			return err
		}
		return nil
	}); err != nil {
		return false, err
	}
	request, err := decodeElaborationRequest(entry)
	if err != nil {
		return false, err
	}
	settings, err := elaborate.ParsePolicy(resolved)
	if err != nil {
		return false, err
	}
	if err := e.cancelExpiredElaboration(ctx, attempt, settings.StageActiveTime); err != nil {
		return false, err
	}
	result, ready, err := e.collectTerminal(ctx, run.ID, attempt)
	if err != nil {
		if errors.Is(err, ErrInvocationLost) {
			return false, e.recordElaborationFailure(ctx, run, request, exec.StatusFailed, err.Error())
		}
		return false, err
	}
	if !ready {
		return false, nil
	}
	if result.Status != exec.StatusCompleted {
		return false, e.recordElaborationFailure(ctx, run, request, result.Status, result.Summary)
	}
	if err := e.requireElaborationAdmissible(ctx, request.InvocationID); err != nil {
		if MutableAdmissionPolicyRefusal(err) {
			return false, nil
		}
		return false, err
	}
	output, err := e.readElaborationOutput(ctx, request.InvocationID, result)
	if err != nil {
		return false, e.recordElaborationFailure(ctx, run, request, exec.StatusFailed, err.Error())
	}
	if len(output.FetchRequests) > 0 {
		if request.Iteration >= settings.MaxIterations {
			return false, e.recordElaborationFailure(ctx, run, request, exec.StatusFailed, ErrElaborationIterationsExhausted.Error())
		}
		return e.acceptResearchRequests(ctx, run, request, output.FetchRequests, settings)
	}
	return e.acceptSpecification(ctx, run, request, *output.Specification, settings)
}

func (e *Engine) readElaborationOutput(
	ctx context.Context, invocationID domain.InvocationID, result exec.StageResult,
) (elaborate.Output, error) {
	stream, err := e.driver.Stream(ctx, invocationID)
	if err != nil {
		return elaborate.Output{}, fmt.Errorf("open elaborator transcript stream: %w", err)
	}
	output, streamErr := elaborate.DecodeTranscript(stream)
	closeErr := stream.Close()
	if streamErr == nil {
		return output, closeErr
	}
	if !errors.Is(streamErr, elaborate.ErrTranscriptResultMissing) {
		return elaborate.Output{}, errors.Join(streamErr, closeErr)
	}
	if closeErr != nil {
		return elaborate.Output{}, closeErr
	}
	if len(result.Artifacts) != 1 {
		return elaborate.Output{}, fmt.Errorf(
			"elaborator result names %d artifacts, want one transcript: %w",
			len(result.Artifacts), elaborate.ErrTranscriptResultMissing)
	}
	digest := result.Artifacts[0]
	reader, err := e.elaboration.blobs.OpenContext(ctx, digest)
	if err != nil {
		return elaborate.Output{}, fmt.Errorf("open persisted elaborator transcript %s: %w", digest, err)
	}
	hasher := sha256.New()
	output, decodeErr := elaborate.DecodeTranscript(io.TeeReader(reader, hasher))
	closeErr = reader.Close()
	if decodeErr != nil || closeErr != nil {
		return elaborate.Output{}, errors.Join(decodeErr, closeErr)
	}
	if got := domain.Digest(contentaddr.Format(hasher.Sum(nil))); got != digest {
		return elaborate.Output{}, fmt.Errorf(
			"persisted elaborator transcript hashes to %s, result names %s: %w",
			got, digest, signet.ErrDigestMismatch)
	}
	return output, nil
}

func (e *Engine) requireElaborationAdmissible(ctx context.Context, invocationID domain.InvocationID) error {
	return e.store.Read(ctx, func(tx *store.ReadTx) error {
		_, found, err := tx.LookupExecutionAdmission(ctx, invocationID)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("elaboration invocation %q has no admission: %w", invocationID, store.ErrNotFound)
		}
		return nil
	})
}

func (e *Engine) cancelExpiredElaboration(ctx context.Context, attempt domain.Attempt, limit time.Duration) error {
	var admission domain.ExecutionAdmission
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(ctx, attempt.InvocationID)
		return err
	}); err != nil {
		return err
	}
	if e.elaboration.now().Sub(admission.AdmittedAt) <= limit {
		return nil
	}
	inspection, err := e.driver.Inspect(ctx, attempt.InvocationID)
	if err != nil {
		return err
	}
	if inspection.Status == exec.StatusPending || inspection.Status == exec.StatusRunning {
		return e.driver.Cancel(ctx, attempt.InvocationID)
	}
	return nil
}

func (e *Engine) acceptResearchRequests(ctx context.Context, run domain.Run, request elaborationRequest, requests []elaborate.FetchRequest, settings elaborate.Policy) (bool, error) {
	inputs := slices.Clone(request.InputArtifactIDs)
	ids := make([]domain.ArtifactID, 0, len(requests))
	for index, fetchRequest := range requests {
		artifact, err := e.elaboration.fetcher.Fetch(ctx, request.InvocationID, index+1, fetchRequest,
			settings.ResearchAllowlist, settings.ResearchMaxBytes)
		if err != nil {
			if elaborate.IsResearchRequestFailure(err) {
				return false, e.recordElaborationFailure(ctx, run, request, exec.StatusFailed, err.Error())
			}
			return false, err
		}
		inputs = append(inputs, artifact.Artifact.ID)
		ids = append(ids, artifact.Artifact.ID)
	}
	next := request
	next.Iteration++
	next.InvocationID = elaborationInvocationID(request.ElaborationRunID, next.Iteration)
	next.InputArtifactIDs = inputs
	payload, err := encodeElaborationRequest(next)
	if err != nil {
		return false, err
	}
	invocation, err := domain.NewAgentInvocation(next.InvocationID, inputs, nil, 0)
	if err != nil {
		return false, err
	}
	terminal := elaborationTerminal{
		InvocationID: request.InvocationID, Iteration: request.Iteration,
		Status: exec.StatusCompleted, ResearchArtifactIDs: ids,
	}
	terminalBody, err := encodeElaborationTerminal(terminal)
	if err != nil {
		return false, err
	}
	err = e.store.Write(ctx, func(tx *store.WriteTx) error {
		if _, found, err := tx.LookupExecutionAdmission(ctx, request.InvocationID); err != nil {
			return err
		} else if !found {
			return store.ErrNotFound
		}
		stored, inserted, err := tx.RecordInbox(ctx, string(request.InvocationID), kindElaborationTerminal, terminalBody)
		if err != nil {
			return err
		}
		if !inserted && (stored.Kind != kindElaborationTerminal || !bytes.Equal(stored.Payload, terminalBody)) {
			return fmt.Errorf("elaboration terminal %q disagrees: %w",
				request.InvocationID, domain.ErrImmutableTransition)
		}
		if !inserted {
			return errReplay
		}
		if err := tx.PutAgentInvocation(ctx, invocation); err != nil {
			return err
		}
		stored, inserted, err = tx.EnqueueOutbox(ctx, string(next.InvocationID), KindElaborationInvocationRequested, payload)
		if err != nil {
			return err
		}
		if !inserted && (stored.Kind != KindElaborationInvocationRequested || !bytes.Equal(stored.Payload, payload)) {
			return fmt.Errorf("next elaboration request %q disagrees: %w",
				next.InvocationID, domain.ErrImmutableTransition)
		}
		return nil
	})
	if errors.Is(err, errReplay) {
		return false, nil
	}
	if MutableAdmissionPolicyRefusal(err) {
		return false, nil
	}
	return err == nil, err
}

func (e *Engine) acceptSpecification(ctx context.Context, run domain.Run, request elaborationRequest, specification elaborate.Specification, settings elaborate.Policy) (bool, error) {
	digest := domain.Digest(contentaddr.Sum([]byte(specification.Body)))
	if _, err := e.elaboration.blobs.Put(digest, strings.NewReader(specification.Body)); err != nil {
		return false, err
	}
	artifactID := domain.ArtifactID(fmt.Sprintf("spec-%s-%d", request.ImplementationRunID, request.Iteration))
	artifact, err := domain.NewArtifact(domain.ArtifactInput{
		ID: artifactID, Type: domain.ArtifactKindSpecification, Digest: digest,
		Provenance: domain.Provenance{
			ProducerClass:        domain.ProducerAgent,
			ProducerInvocationID: request.InvocationID, HeadBinding: domain.HeadIndependent,
			SensitivityClass: domain.SensitivityNormal,
		},
	}, nil)
	if err != nil {
		return false, err
	}
	itemID := domain.ItemID(fmt.Sprintf("spec-approval-%s-%d", request.ImplementationRunID, request.Iteration))
	reason, err := e.specApprovalReason(ctx, request, specification)
	if err != nil {
		return false, err
	}
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: itemID, ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &run.ID},
		Type:    domain.AttentionSpecApproval, Priority: domain.PriorityNormal, Reason: reason,
		RequestedDecision: []domain.Action{domain.ActionApprove, domain.ActionRequestChanges, domain.ActionStop},
		AgentClaims: []domain.AgentClaim{{
			Label: "Specification", Artifact: artifact.ID, Digest: artifact.Digest,
			Provenance: artifact.Provenance, Text: &domain.ClaimText{MediaType: domain.MediaTypeTextMarkdown, Content: specification.Body},
		}},
		ItemVersion: 1, InterruptionClass: domain.InterruptionPlannedGate, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		return false, err
	}
	terminal := elaborationTerminal{
		InvocationID: request.InvocationID, Iteration: request.Iteration,
		Status: exec.StatusCompleted, ResearchArtifactIDs: []domain.ArtifactID{}, SpecArtifactID: &artifactID,
	}
	if settings.SpecApproval {
		terminal.ApprovalItemID = &itemID
	}
	terminalBody, err := encodeElaborationTerminal(terminal)
	if err != nil {
		return false, err
	}
	err = e.store.Write(ctx, func(tx *store.WriteTx) error {
		if _, found, err := tx.LookupExecutionAdmission(ctx, request.InvocationID); err != nil {
			return err
		} else if !found {
			return store.ErrNotFound
		}
		stored, inserted, err := tx.RecordInbox(ctx, string(request.InvocationID), kindElaborationTerminal, terminalBody)
		if err != nil {
			return err
		}
		if !inserted && (stored.Kind != kindElaborationTerminal || !bytes.Equal(stored.Payload, terminalBody)) {
			return fmt.Errorf("elaboration terminal %q disagrees: %w",
				request.InvocationID, domain.ErrImmutableTransition)
		}
		if !inserted {
			return errReplay
		}
		if err := tx.PutArtifact(ctx, artifact); err != nil {
			return err
		}
		if settings.SpecApproval {
			if err := tx.PutAttentionItem(ctx, item); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, errReplay) {
		if !settings.SpecApproval {
			_, startErr := e.startApprovedImplementation(ctx, request, artifactID)
			return false, startErr
		}
		return false, nil
	}
	if MutableAdmissionPolicyRefusal(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !settings.SpecApproval {
		_, err := e.startApprovedImplementation(ctx, request, artifactID)
		return true, err
	}
	return true, nil
}

func (e *Engine) specApprovalReason(ctx context.Context, request elaborationRequest, specification elaborate.Specification) (string, error) {
	var builder strings.Builder
	builder.WriteString(specification.Summary)
	if request.PriorSpecArtifactID != nil {
		prior, err := e.readArtifactBody(ctx, *request.PriorSpecArtifactID)
		if err != nil {
			return "", err
		}
		builder.WriteString("\n\nDiff from last reviewed version:\n")
		builder.WriteString(lineDiff(prior, specification.Body))
	}
	if len(request.FeedbackArtifactIDs) > 0 {
		builder.WriteString("\n\nHuman revision comments:\n")
		for _, id := range request.FeedbackArtifactIDs {
			comment, err := e.readArtifactBody(ctx, id)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&builder, "- %s\n", comment)
		}
	}
	if len(specification.Addressals) > 0 {
		builder.WriteString("\n\nClaimed addressals:\n")
		for _, addressal := range specification.Addressals {
			fmt.Fprintf(&builder, "- %s: %s\n", addressal.Comment, addressal.Response)
		}
	}
	return strings.TrimSpace(builder.String()), nil
}

func (e *Engine) readArtifactBody(ctx context.Context, id domain.ArtifactID) (string, error) {
	var artifact domain.Artifact
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		artifact, err = tx.GetArtifact(ctx, id)
		return err
	}); err != nil {
		return "", err
	}
	reader, err := e.elaboration.blobs.Open(artifact.Digest)
	if err != nil {
		return "", err
	}
	defer func() { _ = reader.Close() }()
	body, err := io.ReadAll(io.LimitReader(reader, domain.MaxClaimTextBytes+1))
	if err != nil || len(body) > domain.MaxClaimTextBytes {
		return "", errors.New("artifact body cannot be read within its bound")
	}
	if domain.Digest(contentaddr.Sum(body)) != artifact.Digest {
		return "", errors.New("artifact body does not match its registered digest")
	}
	return string(body), nil
}

func lineDiff(before, after string) string {
	if before == after {
		return "(no textual change)"
	}
	beforeLines, afterLines := strings.Split(before, "\n"), strings.Split(after, "\n")
	var out strings.Builder
	for i, line := range beforeLines {
		if i >= len(afterLines) || line != afterLines[i] {
			fmt.Fprintf(&out, "- %s\n", line)
		}
	}
	for i, line := range afterLines {
		if i >= len(beforeLines) || line != beforeLines[i] {
			fmt.Fprintf(&out, "+ %s\n", line)
		}
	}
	return strings.TrimSpace(out.String())
}

func (e *Engine) recordElaborationFailure(ctx context.Context, run domain.Run, request elaborationRequest, status exec.Status, summary string) error {
	terminal := elaborationTerminal{
		InvocationID: request.InvocationID, Iteration: request.Iteration,
		Status: status, ResearchArtifactIDs: []domain.ArtifactID{},
	}
	body, err := encodeElaborationTerminal(terminal)
	if err != nil {
		return err
	}
	item, err := elaborationFailureItem(run,
		domain.ItemID("execution-failure-"+string(request.InvocationID)), status, summary)
	if err != nil {
		return err
	}
	return e.store.Write(ctx, func(tx *store.WriteTx) error {
		stored, inserted, err := tx.RecordInbox(ctx, string(request.InvocationID), kindElaborationTerminal, body)
		if err != nil {
			return err
		}
		if !inserted && (stored.Kind != kindElaborationTerminal || !bytes.Equal(stored.Payload, body)) {
			return fmt.Errorf("elaboration terminal %q disagrees: %w",
				request.InvocationID, domain.ErrImmutableTransition)
		}
		if !inserted {
			return nil
		}
		return tx.PutAttentionItem(ctx, item)
	})
}

func elaborationFailureItem(
	run domain.Run, id domain.ItemID, status exec.Status, summary string,
) (domain.AttentionItem, error) {
	runID := run.ID
	reason := fmt.Sprintf("Elaboration ended %q without an accepted specification.", status)
	if summary != "" {
		reason += " Driver summary: " + summary
	}
	return domain.NewAttentionItem(domain.AttentionItemInput{
		ID: id, ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &runID},
		Type:    domain.AttentionExecutionFailure, Priority: domain.PriorityHigh, Reason: reason,
		RequestedDecision: []domain.Action{domain.ActionStop}, ItemVersion: 1,
		InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
	}, nil)
}

func decodeElaborationTerminal(entry store.QueueEntry) (elaborationTerminal, error) {
	if entry.Kind != kindElaborationTerminal {
		return elaborationTerminal{}, fmt.Errorf("elaboration terminal kind %q: %w", entry.Kind, domain.ErrParentKeyMismatch)
	}
	var terminal elaborationTerminal
	if err := strictjson.Decode(entry.Payload, &terminal, strictjson.RejectInvalidUTF8, maxElaborationContractBytes); err != nil {
		return elaborationTerminal{}, err
	}
	if err := terminal.validate(); err != nil {
		return elaborationTerminal{}, err
	}
	canonical, err := encodeElaborationTerminal(terminal)
	if err != nil {
		return elaborationTerminal{}, err
	}
	if string(terminal.InvocationID) != entry.IdempotencyKey || !bytes.Equal(canonical, entry.Payload) {
		return elaborationTerminal{}, fmt.Errorf("invalid elaboration terminal: %w", domain.ErrParentKeyMismatch)
	}
	return terminal, nil
}

func (t elaborationTerminal) validate() error {
	if t.InvocationID == "" || t.Iteration < 1 || !t.Status.Terminal() || t.ResearchArtifactIDs == nil {
		return fmt.Errorf("invalid elaboration terminal identity: %w", domain.ErrParentKeyMismatch)
	}
	hasResearch := len(t.ResearchArtifactIDs) > 0
	hasSpec := t.SpecArtifactID != nil
	if t.Status == exec.StatusCompleted {
		if hasResearch == hasSpec {
			return fmt.Errorf("completed elaboration terminal must bind research or a specification: %w", domain.ErrParentKeyMismatch)
		}
	} else if hasResearch || hasSpec || t.ApprovalItemID != nil {
		return fmt.Errorf("unsuccessful elaboration terminal carries output: %w", domain.ErrParentKeyMismatch)
	}
	if t.ApprovalItemID != nil && !hasSpec {
		return fmt.Errorf("approval item has no specification: %w", domain.ErrParentKeyMismatch)
	}
	seen := make(map[domain.ArtifactID]struct{}, len(t.ResearchArtifactIDs))
	for _, id := range t.ResearchArtifactIDs {
		if id == "" {
			return domain.ErrEmptyID
		}
		if _, duplicate := seen[id]; duplicate {
			return domain.ErrDuplicate
		}
		seen[id] = struct{}{}
	}
	if t.SpecArtifactID != nil && *t.SpecArtifactID == "" {
		return domain.ErrEmptyID
	}
	if t.ApprovalItemID != nil && *t.ApprovalItemID == "" {
		return domain.ErrEmptyID
	}
	return nil
}

func encodeElaborationTerminal(terminal elaborationTerminal) ([]byte, error) {
	if err := terminal.validate(); err != nil {
		return nil, err
	}
	body, err := json.Marshal(terminal)
	if err != nil {
		return nil, fmt.Errorf("encode elaboration terminal: %w", err)
	}
	return body, nil
}

func (e *Engine) reconcileElaborationGates(ctx context.Context) (int, int, error) {
	var runs []store.Snapshotted[domain.Run]
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		runs, err = tx.ListRuns(ctx)
		return err
	}); err != nil {
		return 0, 0, err
	}
	started, blocked := 0, 0
	for _, snapshot := range runs {
		run := snapshot.Value
		owned, err := e.ownsElaborationRun(ctx, run)
		if err != nil {
			return started, blocked, err
		}
		if !owned {
			continue
		}
		stage, _ := findElaborationStage(run)
		if len(stage.Attempts) == 0 {
			continue
		}
		latest := stage.Attempts[len(stage.Attempts)-1]
		var terminalEntry, requestEntry store.QueueEntry
		var resolved domain.ResolvedPolicy
		err = e.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			terminalEntry, err = tx.GetInbox(ctx, string(latest.InvocationID))
			if err != nil {
				return err
			}
			requestEntry, err = tx.GetOutbox(ctx, string(latest.InvocationID))
			if err != nil {
				return err
			}
			resolved, err = tx.GetResolvedPolicy(ctx, run.ID)
			return err
		})
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return started, blocked, err
		}
		terminal, err := decodeElaborationTerminal(terminalEntry)
		if err != nil {
			return started, blocked, err
		}
		if terminal.ApprovalItemID == nil || terminal.SpecArtifactID == nil {
			continue
		}
		request, err := decodeElaborationRequest(requestEntry)
		if err != nil {
			return started, blocked, err
		}
		// Re-bind the reconstructed terminal to this run before trusting its
		// approval and specification identities. Both IDs are deterministic in
		// the implementation identity and iteration, so a corrupted or
		// retargeted terminal that named a foreign, already-approved item and
		// specification would otherwise satisfy the self-consistent digest
		// checks below and start this implementation from the foreign spec.
		expectedSpec := domain.ArtifactID(fmt.Sprintf("spec-%s-%d", request.ImplementationRunID, request.Iteration))
		expectedItem := domain.ItemID(fmt.Sprintf("spec-approval-%s-%d", request.ImplementationRunID, request.Iteration))
		if *terminal.SpecArtifactID != expectedSpec || *terminal.ApprovalItemID != expectedItem {
			return started, blocked, fmt.Errorf("elaboration terminal %q identity mismatch: %w",
				latest.InvocationID, domain.ErrParentKeyMismatch)
		}
		settings, err := elaborate.ParsePolicy(resolved)
		if err != nil {
			return started, blocked, err
		}
		var item domain.AttentionItem
		var commands []domain.Command
		err = e.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			item, err = tx.GetAttentionItem(ctx, *terminal.ApprovalItemID)
			if err != nil {
				return err
			}
			commands, err = tx.ListCommandsForItem(ctx, item.ID)
			return err
		})
		if err != nil {
			return started, blocked, err
		}
		if item.Type != domain.AttentionSpecApproval || item.Subject.Type != domain.SubjectRun ||
			item.Subject.RunID == nil || *item.Subject.RunID != run.ID {
			return started, blocked, fmt.Errorf("elaboration approval item %q is not a spec approval bound to run %q: %w",
				item.ID, run.ID, domain.ErrParentKeyMismatch)
		}
		commands, err = elaborationDecisionCommands(commands)
		if err != nil {
			return started, blocked, err
		}
		if item.Status != domain.StatusOpen {
			if err := e.supersedeElaborationBlockedItem(ctx, *terminal.ApprovalItemID); err != nil {
				return started, blocked, err
			}
		}
		switch item.Status {
		case domain.StatusOpen:
			if e.elaboration.now().Sub(terminalEntry.CreatedAt) >= settings.ApprovalWait {
				created, err := e.ensureElaborationBlockedItem(
					ctx, run, *terminal.ApprovalItemID, terminalEntry.CreatedAt)
				if err != nil {
					return started, blocked, err
				}
				blocked += boolCount(created)
			}
		case domain.StatusResolved:
			if len(commands) == 1 && commands[0].Action == domain.ActionStop {
				continue
			}
			var specArtifact domain.Artifact
			if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				specArtifact, err = tx.GetArtifact(ctx, *terminal.SpecArtifactID)
				return err
			}); err != nil {
				return started, blocked, err
			}
			// The spec must have been produced by this run's own elaboration
			// invocation, never adopted from a foreign run.
			if specArtifact.Provenance.ProducerInvocationID != latest.InvocationID {
				return started, blocked, fmt.Errorf("elaboration spec %q was not produced by invocation %q: %w",
					specArtifact.ID, latest.InvocationID, domain.ErrParentKeyMismatch)
			}
			digest := specArtifact.Digest
			if len(commands) != 1 || commands[0].Action != domain.ActionApprove ||
				!slices.Equal(commands[0].ArtifactDigests, item.ArtifactDigests) ||
				!slices.Equal(item.ArtifactDigests, []domain.Digest{digest}) {
				return started, blocked, fmt.Errorf("implementation run %q: %w", request.ImplementationRunID, ErrSpecApprovalRequired)
			}
			created, err := e.startApprovedImplementation(ctx, request, *terminal.SpecArtifactID)
			if err != nil {
				return started, blocked, err
			}
			started += boolCount(created)
		case domain.StatusSuperseded:
			if len(commands) != 1 || commands[0].Action != domain.ActionRequestChanges {
				return started, blocked, fmt.Errorf("superseded spec lacks request-changes command: %w", ErrSpecApprovalRequired)
			}
			if request.Iteration >= settings.MaxIterations {
				failure, itemErr := elaborationFailureItem(run,
					domain.ItemID("execution-failure-spec-revision-"+string(request.ImplementationRunID)),
					exec.StatusFailed, ErrElaborationIterationsExhausted.Error())
				if itemErr != nil {
					return started, blocked, itemErr
				}
				if err := e.store.Write(ctx, func(tx *store.WriteTx) error {
					if _, err := tx.GetAttentionItem(ctx, failure.ID); err == nil {
						return errReplay
					} else if !errors.Is(err, store.ErrNotFound) {
						return err
					}
					return tx.PutAttentionItem(ctx, failure)
				}); err != nil && !errors.Is(err, errReplay) {
					return started, blocked, err
				}
				continue
			}
			if err := e.enqueueSpecRevision(ctx, request, *terminal.SpecArtifactID, commands[0]); err != nil {
				return started, blocked, err
			}
		case domain.StatusDismissed, domain.StatusExpired:
		}
	}
	return started, blocked, nil
}

func elaborationDecisionCommands(commands []domain.Command) ([]domain.Command, error) {
	decisions := make([]domain.Command, 0, 1)
	for _, command := range commands {
		if command.Action == domain.ActionApprove ||
			command.Action == domain.ActionRequestChanges ||
			command.Action == domain.ActionStop {
			decisions = append(decisions, command)
			continue
		}
		return nil, fmt.Errorf("elaboration item command %q has action %q: %w",
			command.CommandID, command.Action, domain.ErrParentKeyMismatch)
	}
	return decisions, nil
}

func (e *Engine) startApprovedImplementation(ctx context.Context, request elaborationRequest, specArtifactID domain.ArtifactID) (bool, error) {
	var resolved domain.ResolvedPolicy
	alreadyExists := false
	if err := e.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		if err := authenticateElaborationRoot(ctx, tx, request); err != nil {
			return err
		}
		resolved, err = tx.GetResolvedPolicy(ctx, request.ElaborationRunID)
		if err != nil {
			return err
		}
		_, err = tx.GetRun(ctx, request.ImplementationRunID)
		alreadyExists = err == nil
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		return err
	}); err != nil {
		return false, err
	}
	implementationPolicy, err := domain.NewResolvedPolicy(request.ImplementationRunID, resolved.Keys)
	if err != nil {
		return false, err
	}
	_, err = SubmitProductionRun(ctx, e.store, ProductionRunSpec{
		RunID: request.ImplementationRunID, ProjectID: request.ProjectID,
		SpecArtifactID: specArtifactID, PolicyArtifactID: request.PolicyArtifactID,
		ResolvedPolicy: implementationPolicy, Publication: request.Publication,
		WorkUnit: cloneElaborationWorkUnit(request.WorkUnit),
	})
	return !alreadyExists && err == nil, err
}

func (e *Engine) enqueueSpecRevision(ctx context.Context, request elaborationRequest, priorSpec domain.ArtifactID, command domain.Command) error {
	if command.Message == "" {
		return errors.New("request_changes command has no feedback")
	}
	digest := domain.Digest(contentaddr.Sum([]byte(command.Message)))
	if _, err := e.elaboration.blobs.Put(digest, strings.NewReader(command.Message)); err != nil {
		return err
	}
	feedbackID := domain.ArtifactID("spec-feedback-" + command.CommandID)
	feedback, err := domain.NewArtifact(domain.ArtifactInput{
		ID: feedbackID, Type: domain.ArtifactKindResearch, Digest: digest,
		Provenance: domain.Provenance{
			ProducerClass:        domain.ProducerDaemon,
			ProducerInvocationID: request.InvocationID, HeadBinding: domain.HeadIndependent,
			SensitivityClass: domain.SensitivityNormal,
		},
	}, nil)
	if err != nil {
		return err
	}
	next := request
	next.Iteration++
	next.InvocationID = elaborationInvocationID(request.ElaborationRunID, next.Iteration)
	next.PriorSpecArtifactID = &priorSpec
	next.FeedbackArtifactIDs = append(slices.Clone(request.FeedbackArtifactIDs), feedbackID)
	// Retire the superseded prior specification before appending the new one.
	// loadElaborationBinding tolerates exactly one non-source Specification
	// input (the current PriorSpec), so a stale prior spec left in the inputs
	// binds as ErrParentKeyMismatch and the revision can never dispatch: a
	// second request_changes round would otherwise enqueue an invocation that
	// no reconcile pass can decode. Feedback stays research-typed and
	// accumulates.
	retained := slices.Clone(request.InputArtifactIDs)
	if request.PriorSpecArtifactID != nil {
		retained = slices.DeleteFunc(retained, func(id domain.ArtifactID) bool {
			return id == *request.PriorSpecArtifactID
		})
	}
	if !slices.Contains(retained, priorSpec) {
		retained = append(retained, priorSpec)
	}
	next.InputArtifactIDs = append(retained, feedbackID)
	payload, err := encodeElaborationRequest(next)
	if err != nil {
		return err
	}
	invocation, err := domain.NewAgentInvocation(next.InvocationID, next.InputArtifactIDs, nil, 0)
	if err != nil {
		return err
	}
	return e.store.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutArtifact(ctx, feedback); err != nil {
			return err
		}
		if err := tx.PutAgentInvocation(ctx, invocation); err != nil {
			return err
		}
		stored, inserted, err := tx.EnqueueOutbox(ctx, string(next.InvocationID), KindElaborationInvocationRequested, payload)
		if err != nil {
			return err
		}
		if !inserted && (stored.Kind != KindElaborationInvocationRequested || !bytes.Equal(stored.Payload, payload)) {
			return fmt.Errorf("revision elaboration request %q disagrees: %w",
				next.InvocationID, domain.ErrImmutableTransition)
		}
		return nil
	})
}

func (e *Engine) ensureElaborationBlockedItem(
	ctx context.Context, run domain.Run, approvalItemID domain.ItemID, waitingSince time.Time,
) (bool, error) {
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: domain.ItemID("blocked-" + string(approvalItemID)), ProjectID: run.ProjectID,
		Subject: domain.Subject{Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &run.ID},
		Type:    domain.AttentionBlocked, Priority: domain.PriorityNormal,
		Reason:            fmt.Sprintf("Specification approval has been waiting since %s.", waitingSince.UTC().Format(time.RFC3339)),
		RequestedDecision: []domain.Action{}, ItemVersion: 1,
		InterruptionClass: domain.InterruptionExceptional, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		return false, err
	}
	created := false
	err = e.store.Write(ctx, func(tx *store.WriteTx) error {
		if _, err := tx.GetAttentionItem(ctx, item.ID); err == nil {
			return errReplay
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		created = true
		return tx.PutAttentionItem(ctx, item)
	})
	if errors.Is(err, errReplay) {
		return false, nil
	}
	return created, err
}

func (e *Engine) supersedeElaborationBlockedItem(ctx context.Context, approvalItemID domain.ItemID) error {
	err := e.store.Write(ctx, func(tx *store.WriteTx) error {
		item, err := tx.GetAttentionItem(ctx, domain.ItemID("blocked-"+string(approvalItemID)))
		if errors.Is(err, store.ErrNotFound) {
			return errReplay
		}
		if err != nil {
			return err
		}
		if item.Status != domain.StatusOpen {
			return errReplay
		}
		item.Status = domain.StatusSuperseded
		item.ItemVersion++
		return tx.PutAttentionItem(ctx, item)
	})
	if errors.Is(err, errReplay) {
		return nil
	}
	return err
}
