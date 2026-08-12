package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

const maxProposalInstanceBytes = domain.MaxEffectProposalBytes + 16<<10

const allocateProposalInstanceSQL = `
INSERT INTO effect_proposal_instances
    (instance_id, admission_key, proposal_batch_id, effect_kind, content_digest,
     resolved_policy_run_id, resolved_policy_digest, subject_handle, created_at, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (admission_key) DO UPDATE SET admission_key = excluded.admission_key
RETURNING instance_id, admission_key, proposal_batch_id, effect_kind,
          content_digest, resolved_policy_run_id, resolved_policy_digest,
          subject_handle, created_at, body`

// AllocateProposalInstance atomically chooses the daemon-generated effect
// identity under the typed occurrence key. A retry returns the surviving row;
// changed content under one occurrence key is an immutable conflict.
func (tx *WriteTx) AllocateProposalInstance(
	ctx context.Context,
	admission domain.ProposalAdmissionKey,
	batchID domain.ProposalBatchID,
	proposal domain.EffectProposal,
	createdAt time.Time,
) (domain.ProposalInstance, bool, error) {
	if batchID == "" {
		return domain.ProposalInstance{}, false, fmt.Errorf("allocate proposal instance batch: %w", domain.ErrEmptyID)
	}
	if err := proposal.Validate(); err != nil {
		return domain.ProposalInstance{}, false, fmt.Errorf("allocate proposal instance: %w", err)
	}
	if _, err := tx.gateProposalSubject(ctx, proposal); err != nil {
		return domain.ProposalInstance{}, false, fmt.Errorf("allocate proposal instance gate: %w", err)
	}
	admissionKey, err := admission.String()
	if err != nil {
		return domain.ProposalInstance{}, false, fmt.Errorf("allocate proposal instance admission: %w", err)
	}
	instanceID, err := randomProposalInstanceID()
	if err != nil {
		return domain.ProposalInstance{}, false, err
	}
	want := domain.ProposalInstance{
		ID: instanceID, Admission: admission, ProposalBatchID: batchID,
		Proposal: proposal, CreatedAt: createdAt.UTC(),
	}
	body, err := encode(want)
	if err != nil {
		return domain.ProposalInstance{}, false, fmt.Errorf("allocate proposal instance: %w", err)
	}
	returned, returnedKey, err := scanProposalInstance(tx.tx.QueryRowContext(ctx,
		allocateProposalInstanceSQL,
		want.ID, admissionKey, batchID, proposal.Kind, proposal.Digest,
		proposal.ResolvedPolicyRunID, proposal.ResolvedPolicyDigest, proposal.RunProposal.SubjectHandle,
		formatTime(want.CreatedAt), body,
	))
	if err != nil {
		return domain.ProposalInstance{}, false, fmt.Errorf("allocate proposal instance: %w", err)
	}
	inserted := returned.ID == want.ID
	if returnedKey != admissionKey || returned.Admission != admission ||
		returned.ProposalBatchID != batchID || returned.Proposal.Digest != proposal.Digest {
		return domain.ProposalInstance{}, false, fmt.Errorf("allocate proposal instance %q: %w", admissionKey, ErrImmutableConflict)
	}
	if _, err := tx.gateProposalSubject(ctx, returned.Proposal); err != nil {
		return domain.ProposalInstance{}, false, fmt.Errorf("allocate proposal instance stored gate: %w", err)
	}
	return returned, inserted, nil
}

func randomProposalInstanceID() (domain.ProposalInstanceID, error) {
	var body [16]byte
	if _, err := rand.Read(body[:]); err != nil {
		return "", fmt.Errorf("generate proposal instance id: %w", err)
	}
	return domain.ProposalInstanceID("proposal-" + hex.EncodeToString(body[:])), nil
}

// gateEffectProposalArtifact prevents the registry's compiled metadata recipe
// from becoming generic publish authority. A carrier using that recipe must
// reproduce exactly what EvidenceArtifact derives from a durable instance or
// revision; every other artifact continues through the ordinary recipe gate.
func (tx *ReadTx) gateEffectProposalArtifact(ctx context.Context, artifact domain.Artifact) error {
	if artifact.Provenance.VerificationRecipeDigest == nil ||
		*artifact.Provenance.VerificationRecipeDigest != domain.EffectProposalRecipeDigest {
		return nil
	}
	const invocationPrefix = "effect-proposal/"
	invocationID := string(artifact.Provenance.ProducerInvocationID)
	if !strings.HasPrefix(invocationID, invocationPrefix) {
		return domain.ErrEffectProposalInconsistent
	}
	instanceID := domain.ProposalInstanceID(strings.TrimPrefix(invocationID, invocationPrefix))
	if instanceID == "" {
		return domain.ErrEffectProposalInconsistent
	}
	row := tx.tx.QueryRowContext(ctx, `SELECT instance_id, admission_key,
		proposal_batch_id, effect_kind, content_digest, resolved_policy_run_id,
		resolved_policy_digest, subject_handle, created_at, body
		FROM effect_proposal_instances WHERE instance_id = ?`, instanceID)
	instance, _, err := scanProposalInstance(row)
	if err != nil {
		return fmt.Errorf("proposal artifact instance %q: %w", instanceID, notFoundOr(err))
	}
	if artifact.Digest != instance.Proposal.Digest {
		proposal, _, err := tx.authenticatedProposalRevision(ctx, instance, artifact.Digest)
		if err != nil {
			return fmt.Errorf("proposal artifact revision %q/%q: %w", instanceID, artifact.Digest, err)
		}
		instance.Proposal = proposal
	}
	if _, err := tx.gateProposalSubject(ctx, instance.Proposal); err != nil {
		return fmt.Errorf("proposal artifact %q gate: %w", artifact.Digest, err)
	}
	expected, err := instance.EvidenceArtifact()
	if err != nil {
		return err
	}
	if !sameProposalArtifact(artifact, expected) {
		return domain.ErrEffectProposalInconsistent
	}
	return nil
}

func sameProposalArtifact(got, want domain.Artifact) bool {
	if got.ID != want.ID || got.Type != want.Type || got.Digest != want.Digest ||
		got.PublishEligible != want.PublishEligible ||
		got.Provenance.ProducerClass != want.Provenance.ProducerClass ||
		got.Provenance.ProducerInvocationID != want.Provenance.ProducerInvocationID ||
		got.Provenance.HeadBinding != want.Provenance.HeadBinding ||
		got.Provenance.SourceHeadSHA != want.Provenance.SourceHeadSHA ||
		got.Provenance.SensitivityClass != want.Provenance.SensitivityClass {
		return false
	}
	return got.Provenance.VerificationRecipeDigest != nil &&
		want.Provenance.VerificationRecipeDigest != nil &&
		*got.Provenance.VerificationRecipeDigest == *want.Provenance.VerificationRecipeDigest
}

func scanProposalInstance(sc scanner) (domain.ProposalInstance, string, error) {
	var (
		instanceID, admissionKey, batchID, effectKind, contentDigest string
		policyRunID, policyDigest, subjectHandle, createdAt, body    string
	)
	if err := sc.Scan(&instanceID, &admissionKey, &batchID, &effectKind,
		&contentDigest, &policyRunID, &policyDigest, &subjectHandle, &createdAt, &body); err != nil {
		return domain.ProposalInstance{}, "", err
	}
	var instance domain.ProposalInstance
	if err := strictjson.Decode([]byte(body), &instance, strictjson.RejectInvalidUTF8, maxProposalInstanceBytes); err != nil {
		return domain.ProposalInstance{}, "", err
	}
	if err := instance.Validate(); err != nil {
		return domain.ProposalInstance{}, "", fmt.Errorf("stored proposal instance invalid: %w", err)
	}
	derivedKey, err := instance.Admission.String()
	if err != nil {
		return domain.ProposalInstance{}, "", err
	}
	storedCreatedAt, err := parseTime(createdAt)
	if err != nil {
		return domain.ProposalInstance{}, "", err
	}
	if string(instance.ID) != instanceID || derivedKey != admissionKey ||
		string(instance.ProposalBatchID) != batchID || string(instance.Proposal.Kind) != effectKind ||
		string(instance.Proposal.Digest) != contentDigest ||
		string(instance.Proposal.ResolvedPolicyRunID) != policyRunID ||
		string(instance.Proposal.ResolvedPolicyDigest) != policyDigest ||
		instance.Proposal.RunProposal == nil || string(instance.Proposal.RunProposal.SubjectHandle) != subjectHandle ||
		!instance.CreatedAt.Equal(storedCreatedAt) {
		return domain.ProposalInstance{}, "", errRowInconsistent
	}
	return instance, admissionKey, nil
}

// GetProposalInstance reconstructs one instance and re-runs the registry gate
// against the policy independently resolved from its opaque work-unit handle.
func (tx *ReadTx) GetProposalInstance(
	ctx context.Context,
	id domain.ProposalInstanceID,
) (domain.ProposalInstance, error) {
	row := tx.tx.QueryRowContext(ctx, `SELECT instance_id, admission_key,
		proposal_batch_id, effect_kind, content_digest, resolved_policy_run_id, resolved_policy_digest,
		subject_handle, created_at, body FROM effect_proposal_instances WHERE instance_id = ?`, id)
	instance, _, err := scanProposalInstance(row)
	if err != nil {
		return domain.ProposalInstance{}, fmt.Errorf("get proposal instance %q: %w", id, notFoundOr(err))
	}
	if _, err := tx.gateProposalSubject(ctx, instance.Proposal); err != nil {
		return domain.ProposalInstance{}, fmt.Errorf("get proposal instance %q gate: %w", id, err)
	}
	return instance, nil
}

// BindProposalItem anchors one rendered card to the exact proposal digest it
// exposes. The binding is immutable and cross-checked on every decision read.
func (tx *WriteTx) BindProposalItem(
	ctx context.Context,
	itemID domain.ItemID,
	instanceID domain.ProposalInstanceID,
	digest domain.Digest,
) error {
	item, err := tx.GetAttentionItem(ctx, itemID)
	if err != nil {
		return fmt.Errorf("bind proposal item %q: %w", itemID, err)
	}
	if item.Type != domain.AttentionRunProposal || len(item.ArtifactDigests) != 1 || item.ArtifactDigests[0] != digest {
		return fmt.Errorf("bind proposal item %q: %w", itemID, errRowInconsistent)
	}
	res, err := tx.tx.ExecContext(ctx, `INSERT INTO effect_proposal_items
		(item_id, instance_id, content_digest) VALUES (?, ?, ?)
		ON CONFLICT (item_id) DO NOTHING`, itemID, instanceID, digest)
	if err != nil {
		return fmt.Errorf("bind proposal item %q: %w", itemID, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if inserted == 0 {
		var storedInstance, storedDigest string
		if err := tx.tx.QueryRowContext(ctx, `SELECT instance_id, content_digest
			FROM effect_proposal_items WHERE item_id = ?`, itemID).Scan(&storedInstance, &storedDigest); err != nil {
			return err
		}
		if storedInstance != string(instanceID) || storedDigest != string(digest) {
			return fmt.Errorf("bind proposal item %q: %w", itemID, ErrImmutableConflict)
		}
	}
	return nil
}

// ProposalForItem returns the instance and exact rendered proposal revision,
// re-gated against the declaration and policy resolved from its opaque handle.
func (tx *ReadTx) ProposalForItem(
	ctx context.Context,
	itemID domain.ItemID,
) (domain.ProposalInstance, domain.EffectProposal, error) {
	instanceID, renderedDigest, err := tx.proposalItemBinding(ctx, itemID)
	if err != nil {
		return domain.ProposalInstance{}, domain.EffectProposal{}, err
	}
	row := tx.tx.QueryRowContext(ctx, `SELECT instance_id, admission_key,
		proposal_batch_id, effect_kind, content_digest, resolved_policy_run_id,
		resolved_policy_digest, subject_handle, created_at, body
		FROM effect_proposal_instances WHERE instance_id = ?`, instanceID)
	instance, _, err := scanProposalInstance(row)
	if err != nil {
		return domain.ProposalInstance{}, domain.EffectProposal{}, fmt.Errorf("proposal for item %q: %w", itemID, err)
	}
	proposal := instance.Proposal
	if renderedDigest != proposal.Digest {
		var body []byte
		if err := tx.tx.QueryRowContext(ctx, `SELECT body FROM effect_proposal_revisions
			WHERE instance_id = ? AND content_digest = ?`, instanceID, renderedDigest).Scan(&body); err != nil {
			return domain.ProposalInstance{}, domain.EffectProposal{}, fmt.Errorf("proposal for item %q revision: %w", itemID, notFoundOr(err))
		}
		proposal, err = domain.DecodeEffectProposal(body)
		if err != nil {
			return domain.ProposalInstance{}, domain.EffectProposal{}, fmt.Errorf("proposal for item %q revision: %w", itemID, err)
		}
	}
	declaration, err := tx.gateProposalSubject(ctx, proposal)
	if err != nil {
		return domain.ProposalInstance{}, domain.EffectProposal{}, fmt.Errorf("proposal for item %q gate: %w", itemID, err)
	}
	item, err := tx.GetAttentionItem(ctx, itemID)
	if err != nil {
		return domain.ProposalInstance{}, domain.EffectProposal{}, err
	}
	if item.ProjectID != declaration.ProjectID || len(item.ArtifactDigests) != 1 || item.ArtifactDigests[0] != proposal.Digest ||
		item.Subject.ID != domain.SubjectID(instance.ProposalBatchID) {
		return domain.ProposalInstance{}, domain.EffectProposal{}, errRowInconsistent
	}
	return instance, proposal, nil
}

// ProposalForItemWithRevisionContext returns the authenticated rendered
// proposal plus the exact digest it supersedes. The extra context is a read
// projection for operator review; it is reconstructed from the append-only
// revision row and its durable start_with_changes command, never trusted from
// the attention item or a client.
func (tx *ReadTx) ProposalForItemWithRevisionContext(
	ctx context.Context,
	itemID domain.ItemID,
) (domain.ProposalInstance, domain.EffectProposal, *domain.EffectProposal, error) {
	instance, proposal, err := tx.ProposalForItem(ctx, itemID)
	if err != nil {
		return domain.ProposalInstance{}, domain.EffectProposal{}, nil, err
	}
	if proposal.Digest == instance.Proposal.Digest {
		return instance, proposal, nil, nil
	}
	authenticated, prior, err := tx.authenticatedProposalRevision(ctx, instance, proposal.Digest)
	if err != nil {
		return domain.ProposalInstance{}, domain.EffectProposal{}, nil,
			fmt.Errorf("proposal revision context for item %q: %w", itemID, err)
	}
	if authenticated.Digest != proposal.Digest {
		return domain.ProposalInstance{}, domain.EffectProposal{}, nil,
			fmt.Errorf("proposal revision context for item %q: %w", itemID, errRowInconsistent)
	}
	return instance, proposal, &prior, nil
}

// authenticatedProposalRevision reconstructs one revision through the full
// append-only authority chain: row body, superseded digest, authoring command,
// original item binding, opaque subject, current policy, and command-authored
// bounded delta. Carrier and facts reads share this path so neither can treat
// a self-consistent revision body as its own authority.
func (tx *ReadTx) authenticatedProposalRevision(
	ctx context.Context,
	instance domain.ProposalInstance,
	digest domain.Digest,
) (domain.EffectProposal, domain.EffectProposal, error) {
	var body []byte
	var supersedesValue, commandID string
	if err := tx.tx.QueryRowContext(ctx, `SELECT body, supersedes_digest, command_id
		FROM effect_proposal_revisions WHERE instance_id = ? AND content_digest = ?`,
		instance.ID, digest).Scan(&body, &supersedesValue, &commandID); err != nil {
		return domain.EffectProposal{}, domain.EffectProposal{}, notFoundOr(err)
	}
	proposal, err := domain.DecodeEffectProposal(body)
	if err != nil {
		return domain.EffectProposal{}, domain.EffectProposal{}, err
	}
	if proposal.Digest != digest {
		return domain.EffectProposal{}, domain.EffectProposal{}, errRowInconsistent
	}
	command, err := tx.GetCommand(ctx, commandID)
	if err != nil {
		return domain.EffectProposal{}, domain.EffectProposal{}, err
	}
	boundInstance, boundDigest, err := tx.proposalItemBinding(ctx, command.ItemID)
	if err != nil {
		return domain.EffectProposal{}, domain.EffectProposal{}, err
	}
	supersedes := domain.Digest(supersedesValue)
	prior := instance.Proposal
	if command.Action != domain.ActionStartWithChanges || boundInstance != instance.ID ||
		boundDigest != supersedes || supersedes != prior.Digest ||
		!slices.Equal(command.ArtifactDigests, []domain.Digest{supersedes}) {
		return domain.EffectProposal{}, domain.EffectProposal{}, errRowInconsistent
	}
	var revision struct {
		Intent            domain.RunProposalIntent `json:"intent"`
		ExpectedCostUnits int                      `json:"expected_cost_units"`
		Scope             domain.RunProposalScope  `json:"scope"`
	}
	if err := strictjson.Decode(
		[]byte(command.Message), &revision, strictjson.RejectInvalidUTF8, domain.MaxEffectProposalBytes,
	); err != nil {
		return domain.EffectProposal{}, domain.EffectProposal{}, errRowInconsistent
	}
	canonical, err := json.Marshal(revision)
	if err != nil || string(canonical) != command.Message {
		return domain.EffectProposal{}, domain.EffectProposal{}, errRowInconsistent
	}
	declaration, policy, err := tx.ResolveProposalSubject(ctx, prior.RunProposal.SubjectHandle)
	if err != nil {
		return domain.EffectProposal{}, domain.EffectProposal{}, err
	}
	if err := domain.GateRunProposalScope(revision.Scope, declaration); err != nil {
		return domain.EffectProposal{}, domain.EffectProposal{}, errRowInconsistent
	}
	// The authoring item is historical authentication input, not evidence to
	// present as currently trusted. Re-entering its evidence gate here would
	// recurse if corruption rebound that item to this revision carrier.
	item, err := tx.GetAttentionItemRecord(ctx, command.ItemID)
	if err != nil {
		return domain.EffectProposal{}, domain.EffectProposal{}, err
	}
	itemTransitionValid := item.Status == domain.StatusOpen && item.ItemVersion == command.ItemVersion ||
		item.Status == domain.StatusSuperseded && item.ItemVersion > command.ItemVersion
	if declaration.ProjectID != item.ProjectID || !itemTransitionValid || item.PRHeadSHA != command.PRHeadSHA ||
		!slices.Equal(item.ArtifactDigests, command.ArtifactDigests) {
		return domain.EffectProposal{}, domain.EffectProposal{}, errRowInconsistent
	}
	expected, err := domain.NewEffectProposal(domain.EffectRunProposal, domain.RunProposalParameters{
		SubjectHandle: prior.RunProposal.SubjectHandle, Intent: revision.Intent,
		ExpectedCostUnits: revision.ExpectedCostUnits, Scope: revision.Scope,
	}, policy)
	if err != nil || expected.Digest != proposal.Digest {
		return domain.EffectProposal{}, domain.EffectProposal{}, errRowInconsistent
	}
	return proposal, prior, nil
}

func (tx *ReadTx) proposalItemBinding(
	ctx context.Context,
	itemID domain.ItemID,
) (domain.ProposalInstanceID, domain.Digest, error) {
	var instanceID, renderedDigest string
	if err := tx.tx.QueryRowContext(ctx, `SELECT instance_id, content_digest
		FROM effect_proposal_items WHERE item_id = ?`, itemID).Scan(&instanceID, &renderedDigest); err != nil {
		return "", "", fmt.Errorf("proposal for item %q: %w", itemID, notFoundOr(err))
	}
	if instanceID == "" || renderedDigest == "" {
		return "", "", fmt.Errorf("proposal for item %q: %w", itemID, errRowInconsistent)
	}
	return domain.ProposalInstanceID(instanceID), domain.Digest(renderedDigest), nil
}

// ResolveProposalSubject resolves an opaque handle through the daemon-owned
// work-unit registry. Policy comes from the resolved declaration rather than
// the proposal's claimed run binding, so decision-time re-gating is not
// tautological.
func (tx *ReadTx) ResolveProposalSubject(
	ctx context.Context,
	handle domain.OpaqueSubjectHandle,
) (domain.WorkUnitDeclaration, domain.ResolvedPolicy, error) {
	declaration, err := tx.GetWorkUnitDeclaration(ctx, domain.WorkUnitID(handle))
	if err != nil {
		return domain.WorkUnitDeclaration{}, domain.ResolvedPolicy{}, err
	}
	if domain.OpaqueSubjectHandle(declaration.ID) != handle {
		return domain.WorkUnitDeclaration{}, domain.ResolvedPolicy{}, errRowInconsistent
	}
	policy, err := tx.GetResolvedPolicy(ctx, declaration.RunID)
	if err != nil {
		return domain.WorkUnitDeclaration{}, domain.ResolvedPolicy{}, err
	}
	return declaration, policy, nil
}

// gateProposalSubject is the single store boundary for a proposal's opaque
// handle: resolve the durable declaration and current policy independently,
// bind declaration-derived scope, then run the closed effect registry gate.
func (tx *ReadTx) gateProposalSubject(
	ctx context.Context,
	proposal domain.EffectProposal,
) (domain.WorkUnitDeclaration, error) {
	declaration, policy, err := tx.ResolveProposalSubject(ctx, proposal.RunProposal.SubjectHandle)
	if err != nil {
		return domain.WorkUnitDeclaration{}, err
	}
	if err := domain.GateRunProposalScope(proposal.RunProposal.Scope, declaration); err != nil {
		return domain.WorkUnitDeclaration{}, err
	}
	if err := domain.GateEffectProposal(proposal, policy); err != nil {
		return domain.WorkUnitDeclaration{}, err
	}
	return declaration, nil
}

// PutProposalRevision records a command-authored revision under the same
// effect identity. The command and prior item must already be persisted in the
// enclosing decision transaction.
func (tx *WriteTx) PutProposalRevision(
	ctx context.Context,
	instance domain.ProposalInstance,
	prior domain.EffectProposal,
	revised domain.EffectProposal,
	commandID string,
	createdAt time.Time,
) error {
	if commandID == "" || revised.Digest == prior.Digest {
		return fmt.Errorf("put proposal revision: %w", domain.ErrEffectProposalInconsistent)
	}
	if err := revised.Validate(); err != nil {
		return fmt.Errorf("put proposal revision: %w", err)
	}
	command, err := tx.GetCommand(ctx, commandID)
	if err != nil {
		return fmt.Errorf("put proposal revision command %q: %w", commandID, err)
	}
	boundInstance, boundDigest, err := tx.proposalItemBinding(ctx, command.ItemID)
	if err != nil {
		return fmt.Errorf("put proposal revision command %q: %w", commandID, err)
	}
	if command.Action != domain.ActionStartWithChanges || boundInstance != instance.ID || boundDigest != prior.Digest {
		return fmt.Errorf("put proposal revision command %q: %w", commandID, domain.ErrTransitionCommandMismatch)
	}
	_, storedPrior, err := tx.ProposalForItem(ctx, command.ItemID)
	if err != nil {
		return fmt.Errorf("put proposal revision command %q prior: %w", commandID, err)
	}
	if storedPrior.Digest != prior.Digest ||
		revised.RunProposal.SubjectHandle != storedPrior.RunProposal.SubjectHandle {
		return fmt.Errorf("put proposal revision command %q subject: %w", commandID, domain.ErrTransitionCommandMismatch)
	}
	declaration, policy, err := tx.ResolveProposalSubject(ctx, revised.RunProposal.SubjectHandle)
	if err != nil {
		return err
	}
	item, err := tx.GetAttentionItem(ctx, command.ItemID)
	if err != nil {
		return err
	}
	if declaration.ProjectID != item.ProjectID {
		return fmt.Errorf("put proposal revision command %q project: %w", commandID, domain.ErrTransitionCommandMismatch)
	}
	if err := domain.GateRunProposalScope(revised.RunProposal.Scope, declaration); err != nil {
		return fmt.Errorf("put proposal revision command %q scope: %w", commandID, domain.ErrTransitionCommandMismatch)
	}
	if err := domain.GateEffectProposal(revised, policy); err != nil {
		return err
	}
	body, err := revised.Encode()
	if err != nil {
		return err
	}
	_, err = tx.tx.ExecContext(ctx, `INSERT INTO effect_proposal_revisions
		(instance_id, content_digest, supersedes_digest, command_id, created_at, body)
		VALUES (?, ?, ?, ?, ?, ?)`, instance.ID, revised.Digest, prior.Digest,
		commandID, formatTime(createdAt.UTC()), string(body))
	if err != nil {
		return fmt.Errorf("put proposal revision %q: %w", revised.Digest, err)
	}
	return nil
}

// RecordProposalDecision appends the terminal ledger row for one effect
// identity. selectedDigest is required for starts and absent for decline.
func (tx *WriteTx) RecordProposalDecision(
	ctx context.Context,
	instanceID domain.ProposalInstanceID,
	commandID string,
	action domain.Action,
	selectedDigest *domain.Digest,
	decidedAt time.Time,
) error {
	if commandID == "" || decidedAt.IsZero() {
		return fmt.Errorf("record proposal decision: %w", domain.ErrEmptyField)
	}
	command, err := tx.GetCommand(ctx, commandID)
	if err != nil {
		return fmt.Errorf("record proposal decision command %q: %w", commandID, err)
	}
	boundInstance, boundDigest, err := tx.proposalItemBinding(ctx, command.ItemID)
	if err != nil {
		return fmt.Errorf("record proposal decision command %q: %w", commandID, err)
	}
	if command.Action != action || boundInstance != instanceID {
		return fmt.Errorf("record proposal decision command %q: %w", commandID, domain.ErrTransitionCommandMismatch)
	}
	switch action {
	case domain.ActionStart:
		if selectedDigest == nil || *selectedDigest != boundDigest {
			return fmt.Errorf("record proposal decision command %q selected digest: %w", commandID, domain.ErrTransitionCommandMismatch)
		}
	case domain.ActionStartWithChanges:
		if selectedDigest == nil {
			return fmt.Errorf("record proposal decision command %q selected digest: %w", commandID, domain.ErrTransitionCommandMismatch)
		}
		var matched int
		if err := tx.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM effect_proposal_revisions
			WHERE instance_id = ? AND content_digest = ? AND supersedes_digest = ? AND command_id = ?`,
			instanceID, *selectedDigest, boundDigest, commandID).Scan(&matched); err != nil {
			return fmt.Errorf("record proposal decision command %q revision: %w", commandID, err)
		}
		if matched != 1 {
			return fmt.Errorf("record proposal decision command %q revision: %w", commandID, domain.ErrTransitionCommandMismatch)
		}
	case domain.ActionDecline:
		if selectedDigest != nil {
			return fmt.Errorf("record proposal decision command %q selected digest: %w", commandID, domain.ErrTransitionCommandMismatch)
		}
	default:
		return fmt.Errorf("record proposal decision command %q action %q: %w", commandID, action, domain.ErrTransitionCommandMismatch)
	}
	var selected any
	if selectedDigest != nil {
		selected = *selectedDigest
	}
	_, err = tx.tx.ExecContext(ctx, `INSERT INTO effect_proposal_decisions
		(instance_id, command_id, action, selected_digest, decided_at)
		VALUES (?, ?, ?, ?, ?)`, instanceID, commandID, action, selected, formatTime(decidedAt.UTC()))
	if err != nil {
		return fmt.Errorf("record proposal decision %q: %w", instanceID, err)
	}
	return nil
}

// RecordProposalSnooze appends a bounded deferral and leaves the proposal
// open. The item's version transition makes every pre-snooze command stale.
func (tx *WriteTx) RecordProposalSnooze(
	ctx context.Context,
	instanceID domain.ProposalInstanceID,
	commandID string,
	until, createdAt time.Time,
) error {
	if commandID == "" || !until.After(createdAt) || until.Location() != time.UTC || createdAt.Location() != time.UTC {
		return fmt.Errorf("record proposal snooze: invalid timing")
	}
	command, err := tx.GetCommand(ctx, commandID)
	if err != nil {
		return fmt.Errorf("record proposal snooze command %q: %w", commandID, err)
	}
	boundInstance, boundDigest, err := tx.proposalItemBinding(ctx, command.ItemID)
	if err != nil {
		return fmt.Errorf("record proposal snooze command %q: %w", commandID, err)
	}
	item, err := tx.GetAttentionItem(ctx, command.ItemID)
	if err != nil {
		return fmt.Errorf("record proposal snooze command %q item: %w", commandID, err)
	}
	if command.Action != domain.ActionSnooze || boundInstance != instanceID ||
		item.Status != domain.StatusOpen || !command.BindsSameAs(item) ||
		len(command.ArtifactDigests) != 1 || command.ArtifactDigests[0] != boundDigest {
		return fmt.Errorf("record proposal snooze command %q: %w", commandID, domain.ErrTransitionCommandMismatch)
	}
	if command.Message != formatTime(until) {
		return fmt.Errorf("record proposal snooze command %q timing: %w", commandID, domain.ErrTransitionCommandMismatch)
	}
	snoozes, err := tx.proposalSnoozes(ctx, command.ItemID)
	if err != nil {
		return fmt.Errorf("record proposal snooze %q prior: %w", instanceID, err)
	}
	if len(snoozes) > 0 && snoozes[len(snoozes)-1].releasedAt == nil {
		return fmt.Errorf("record proposal snooze %q: %w", instanceID, ErrImmutableConflict)
	}
	_, err = tx.tx.ExecContext(ctx, `INSERT INTO effect_proposal_snoozes
		(command_id, instance_id, snooze_until, created_at, released_at) VALUES (?, ?, ?, ?, NULL)`,
		commandID, instanceID, formatTime(until), formatTime(createdAt))
	if err != nil {
		return fmt.Errorf("record proposal snooze %q: %w", instanceID, err)
	}
	return nil
}

type proposalSnooze struct {
	command    domain.Command
	instanceID domain.ProposalInstanceID
	until      time.Time
	createdAt  time.Time
	releasedAt *time.Time
}

// proposalSnoozes reconstructs the complete deferral ledger for an instance.
// The row fragment is never authority on its own: every row must bind to a
// durable snooze command for that instance, preserve canonical daemon timing,
// and begin no earlier than the preceding deferral ends.
func (tx *ReadTx) proposalSnoozes(ctx context.Context, itemID domain.ItemID) ([]proposalSnooze, error) {
	instanceID, _, err := tx.proposalItemBinding(ctx, itemID)
	if err != nil {
		return nil, err
	}
	rows, err := tx.tx.QueryContext(ctx, `SELECT command_id, instance_id, snooze_until, created_at, released_at
		FROM effect_proposal_snoozes WHERE instance_id = ? ORDER BY rowid`, instanceID)
	if err != nil {
		return nil, fmt.Errorf("proposal snoozes for item %q: %w", itemID, err)
	}
	defer func() { _ = rows.Close() }()
	var snoozes []proposalSnooze
	for rows.Next() {
		var commandID, storedInstance, untilValue, createdValue string
		var releasedValue sql.NullString
		if err := rows.Scan(&commandID, &storedInstance, &untilValue, &createdValue, &releasedValue); err != nil {
			return nil, fmt.Errorf("proposal snooze for item %q: %w", itemID, err)
		}
		until, err := parseTime(untilValue)
		if err != nil || formatTime(until) != untilValue {
			return nil, fmt.Errorf("proposal snooze for item %q until: %w", itemID, errRowInconsistent)
		}
		createdAt, err := parseTime(createdValue)
		if err != nil || formatTime(createdAt) != createdValue || !until.After(createdAt) {
			return nil, fmt.Errorf("proposal snooze for item %q timing: %w", itemID, errRowInconsistent)
		}
		var releasedAt *time.Time
		if releasedValue.Valid {
			parsed, err := parseTime(releasedValue.String)
			if err != nil || formatTime(parsed) != releasedValue.String || parsed.Before(until) {
				return nil, fmt.Errorf("proposal snooze for item %q release: %w", itemID, errRowInconsistent)
			}
			releasedAt = &parsed
		}
		command, err := tx.GetCommand(ctx, commandID)
		if err != nil {
			return nil, fmt.Errorf("proposal snooze for item %q command %q: %w", itemID, commandID, err)
		}
		boundInstance, boundDigest, err := tx.proposalItemBinding(ctx, command.ItemID)
		if err != nil {
			return nil, fmt.Errorf("proposal snooze for item %q command %q: %w", itemID, commandID, err)
		}
		commandItem, err := tx.GetAttentionItem(ctx, command.ItemID)
		if err != nil {
			return nil, fmt.Errorf("proposal snooze for item %q command %q item: %w", itemID, commandID, err)
		}
		minimumVersion := command.ItemVersion + 1
		validPhase := commandItem.Status == domain.StatusOpen
		if releasedAt != nil {
			minimumVersion++
			validPhase = true
		}
		if command.Action != domain.ActionSnooze || command.CommandID != commandID ||
			storedInstance != string(instanceID) || boundInstance != instanceID ||
			len(command.ArtifactDigests) != 1 || command.ArtifactDigests[0] != boundDigest ||
			command.PRHeadSHA != commandItem.PRHeadSHA ||
			!slices.Equal(command.ArtifactDigests, commandItem.ArtifactDigests) ||
			commandItem.ItemVersion < minimumVersion || !validPhase ||
			command.Message != untilValue || (len(snoozes) > 0 && createdAt.Before(snoozes[len(snoozes)-1].until)) {
			return nil, fmt.Errorf("proposal snooze for item %q command %q: %w", itemID, commandID, errRowInconsistent)
		}
		snoozes = append(snoozes, proposalSnooze{
			command: command, instanceID: instanceID, until: until,
			createdAt: createdAt, releasedAt: releasedAt,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("proposal snoozes for item %q: %w", itemID, err)
	}
	return snoozes, nil
}

// ProposalSnoozeUntil returns the latest authenticated durable deferral for
// the proposal item. A missing row means the item has never been snoozed.
func (tx *ReadTx) ProposalSnoozeUntil(ctx context.Context, itemID domain.ItemID) (*time.Time, error) {
	snoozes, err := tx.proposalSnoozes(ctx, itemID)
	if err != nil || len(snoozes) == 0 {
		return nil, err
	}
	return &snoozes[len(snoozes)-1].until, nil
}

// ProposalSnoozed reports whether the latest durable deferral is still active
// at the daemon-owned instant.
func (tx *ReadTx) ProposalSnoozed(
	ctx context.Context,
	itemID domain.ItemID,
	now time.Time,
) (bool, error) {
	snoozes, err := tx.proposalSnoozes(ctx, itemID)
	if err != nil || len(snoozes) == 0 {
		return false, err
	}
	latest := snoozes[len(snoozes)-1]
	return now.Before(latest.until), nil
}

// ProposalSnoozeReleasePending reports whether expiry has changed visibility
// without yet producing the synchronized item-version transition.
func (tx *ReadTx) ProposalSnoozeReleasePending(ctx context.Context, now time.Time) (bool, error) {
	rows, err := tx.tx.QueryContext(ctx, `SELECT c.item_id FROM effect_proposal_snoozes s
		JOIN commands c ON c.command_id = s.command_id
		WHERE s.rowid = (
			SELECT MAX(latest.rowid) FROM effect_proposal_snoozes latest WHERE latest.instance_id = s.instance_id)
		ORDER BY c.item_id`)
	if err != nil {
		return false, err
	}
	var itemIDs []domain.ItemID
	for rows.Next() {
		var itemID string
		if err := rows.Scan(&itemID); err != nil {
			_ = rows.Close()
			return false, err
		}
		itemIDs = append(itemIDs, domain.ItemID(itemID))
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	for _, itemID := range itemIDs {
		snoozes, err := tx.proposalSnoozes(ctx, itemID)
		if err != nil {
			return false, err
		}
		latest := snoozes[len(snoozes)-1]
		if latest.releasedAt != nil && latest.releasedAt.After(now) {
			return false, fmt.Errorf("proposal snooze for item %q future release: %w", itemID, errRowInconsistent)
		}
		if latest.releasedAt == nil && !now.Before(latest.until) {
			return true, nil
		}
	}
	return false, nil
}

// ReleaseExpiredProposalSnoozes durably exposes each expired proposal. The
// item version is the sync-visible release marker; released_at authenticates
// that the same ledger row cannot advance it again.
func (tx *WriteTx) ReleaseExpiredProposalSnoozes(ctx context.Context, now time.Time) (bool, error) {
	rows, err := tx.tx.QueryContext(ctx, `SELECT c.item_id FROM effect_proposal_snoozes s
		JOIN commands c ON c.command_id = s.command_id
		WHERE s.rowid = (
			SELECT MAX(latest.rowid) FROM effect_proposal_snoozes latest WHERE latest.instance_id = s.instance_id)
		ORDER BY c.item_id`)
	if err != nil {
		return false, err
	}
	var itemIDs []domain.ItemID
	for rows.Next() {
		var itemID string
		if err := rows.Scan(&itemID); err != nil {
			_ = rows.Close()
			return false, err
		}
		itemIDs = append(itemIDs, domain.ItemID(itemID))
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	released := false
	for _, itemID := range itemIDs {
		snoozes, err := tx.proposalSnoozes(ctx, itemID)
		if err != nil {
			return false, err
		}
		latest := snoozes[len(snoozes)-1]
		if latest.releasedAt != nil && latest.releasedAt.After(now) {
			return false, fmt.Errorf("proposal snooze for item %q future release: %w", itemID, errRowInconsistent)
		}
		if latest.releasedAt != nil || now.Before(latest.until) {
			continue
		}
		item, err := tx.GetAttentionItem(ctx, latest.command.ItemID)
		if err != nil {
			return false, err
		}
		if item.Status == domain.StatusOpen {
			if item.ItemVersion < latest.command.ItemVersion+1 {
				return false, fmt.Errorf("release proposal snooze for item %q version: %w", item.ID, errRowInconsistent)
			}
			item.ItemVersion++
			if err := tx.PutAttentionItem(ctx, item); err != nil {
				return false, err
			}
		}
		result, err := tx.tx.ExecContext(ctx, `UPDATE effect_proposal_snoozes SET released_at = ?
			WHERE command_id = ? AND released_at IS NULL`, formatTime(now), latest.command.CommandID)
		if err != nil {
			return false, err
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			return false, fmt.Errorf("release proposal snooze for item %q: %w", item.ID, errRowInconsistent)
		}
		released = true
	}
	return released, nil
}

// ListProposalBatch returns one batch in stable instance-id order, re-gating
// every row through the same reconstruction path as GetProposalInstance.
func (tx *ReadTx) ListProposalBatch(
	ctx context.Context,
	batchID domain.ProposalBatchID,
) ([]domain.ProposalInstance, error) {
	rows, err := tx.tx.QueryContext(ctx, `SELECT instance_id, admission_key,
		proposal_batch_id, effect_kind, content_digest, resolved_policy_run_id, resolved_policy_digest,
		subject_handle, created_at, body FROM effect_proposal_instances
		WHERE proposal_batch_id = ? ORDER BY instance_id`, batchID)
	if err != nil {
		return nil, fmt.Errorf("list proposal batch %q: %w", batchID, err)
	}
	defer func() { _ = rows.Close() }()
	var instances []domain.ProposalInstance
	for rows.Next() {
		instance, _, err := scanProposalInstance(rows)
		if err != nil {
			return nil, fmt.Errorf("list proposal batch %q: %w", batchID, err)
		}
		if _, err := tx.gateProposalSubject(ctx, instance.Proposal); err != nil {
			return nil, fmt.Errorf("list proposal batch %q gate: %w", batchID, err)
		}
		instances = append(instances, instance)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, fmt.Errorf("list proposal batch %q: %w", batchID, err)
	}
	return slices.Clone(instances), nil
}

// marshalProposalInstance is used only by adversarial internal tests to
// produce canonical seed bodies before tampering extracted columns.
func marshalProposalInstance(instance domain.ProposalInstance) ([]byte, error) {
	return json.Marshal(instance)
}
