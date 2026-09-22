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
	subjectHandle, err := proposalSubjectHandle(proposal)
	if err != nil {
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
		proposal.ResolvedPolicyRunID, proposal.ResolvedPolicyDigest, subjectHandle,
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
	handle, err := proposalSubjectHandle(instance.Proposal)
	if err != nil {
		return domain.ProposalInstance{}, "", errRowInconsistent
	}
	if string(instance.ID) != instanceID || derivedKey != admissionKey ||
		string(instance.ProposalBatchID) != batchID || string(instance.Proposal.Kind) != effectKind ||
		string(instance.Proposal.Digest) != contentDigest ||
		string(instance.Proposal.ResolvedPolicyRunID) != policyRunID ||
		string(instance.Proposal.ResolvedPolicyDigest) != policyDigest ||
		string(handle) != subjectHandle ||
		!instance.CreatedAt.Equal(storedCreatedAt) {
		return domain.ProposalInstance{}, "", errRowInconsistent
	}
	return instance, admissionKey, nil
}

// proposalSubjectHandle returns the opaque work-unit handle for either
// registry kind. The switch dispatches behaviour and so omits default; the
// trailing return guards an unregistered kind.
func proposalSubjectHandle(proposal domain.EffectProposal) (domain.OpaqueSubjectHandle, error) {
	switch proposal.Kind {
	case domain.EffectTaskProposal:
		if proposal.TaskProposal == nil {
			return "", domain.ErrEffectProposalInconsistent
		}
		return proposal.TaskProposal.SubjectHandle, nil
	case domain.EffectSourceIssueClosure:
		if proposal.ClosureProposal == nil {
			return "", domain.ErrEffectProposalInconsistent
		}
		return proposal.ClosureProposal.SubjectHandle, nil
	}
	return "", domain.ErrInvalidEffectKind
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

// BindProposalItem writes the immutable anchor from an attention item to its
// proposal instance and rendered digest. A task_proposal item carries no
// prospective merge (merge must be nil); an effect_proposal (closure) item
// requires one, and its candidate head must equal the item's PRHeadSHA so the
// approval binding and the command binding check judge the same head. The bind
// is idempotent: a repeat with the same instance, digest, and merge is a no-op,
// and any divergence is an immutable conflict.
func (tx *WriteTx) BindProposalItem(
	ctx context.Context,
	itemID domain.ItemID,
	instanceID domain.ProposalInstanceID,
	digest domain.Digest,
	merge *domain.ProspectiveMerge,
) error {
	item, err := tx.GetAttentionItem(ctx, itemID)
	if err != nil {
		return fmt.Errorf("bind proposal item %q: %w", itemID, err)
	}
	if len(item.ArtifactDigests) != 1 || item.ArtifactDigests[0] != digest {
		return fmt.Errorf("bind proposal item %q: %w", itemID, errRowInconsistent)
	}
	switch item.Type {
	case domain.AttentionTaskProposal:
		if merge != nil {
			return fmt.Errorf("bind proposal item %q: task proposal carries a merge: %w", itemID, errRowInconsistent)
		}
	case domain.AttentionEffectProposal:
		if merge == nil {
			return fmt.Errorf("bind proposal item %q: effect proposal lacks a merge: %w", itemID, errRowInconsistent)
		}
		if err := merge.Validate(); err != nil {
			return fmt.Errorf("bind proposal item %q merge: %w", itemID, errRowInconsistent)
		}
		if item.PRHeadSHA != merge.CandidateHeadSHA {
			return fmt.Errorf("bind proposal item %q: candidate head differs from item head: %w", itemID, errRowInconsistent)
		}
	default:
		return fmt.Errorf("bind proposal item %q: type %q: %w", itemID, item.Type, errRowInconsistent)
	}
	var publicationIdentity, candidateHeadSHA, baseRef, baseSHA any
	if merge != nil {
		publicationIdentity, candidateHeadSHA = string(merge.PublicationIdentity), merge.CandidateHeadSHA
		baseRef, baseSHA = merge.BaseRef, merge.BaseSHA
	}
	res, err := tx.tx.ExecContext(ctx, `INSERT INTO effect_proposal_items
		(item_id, instance_id, content_digest,
		 publication_identity, candidate_head_sha, base_ref, base_sha)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (item_id) DO NOTHING`,
		itemID, instanceID, digest, publicationIdentity, candidateHeadSHA, baseRef, baseSHA)
	if err != nil {
		return fmt.Errorf("bind proposal item %q: %w", itemID, err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if inserted == 0 {
		storedInstance, storedDigest, storedMerge, err := tx.proposalItemBinding(ctx, itemID)
		if err != nil {
			return err
		}
		if storedInstance != instanceID || storedDigest != digest || !sameProspectiveMerge(storedMerge, merge) {
			return fmt.Errorf("bind proposal item %q: %w", itemID, ErrImmutableConflict)
		}
	}
	return nil
}

// sameProspectiveMerge reports whether two optional merges are equal, treating
// two absent merges as equal (both task proposals).
func sameProspectiveMerge(a, b *domain.ProspectiveMerge) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// ProposalForItem returns the instance and exact rendered proposal revision,
// re-gated against the declaration and policy resolved from its opaque handle.
func (tx *ReadTx) ProposalForItem(
	ctx context.Context,
	itemID domain.ItemID,
) (domain.ProposalInstance, domain.EffectProposal, error) {
	instanceID, renderedDigest, _, err := tx.proposalItemBinding(ctx, itemID)
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
	prior := instance.Proposal
	if proposal.Digest != digest || proposal.Kind != prior.Kind {
		return domain.EffectProposal{}, domain.EffectProposal{}, errRowInconsistent
	}
	expectedAction, err := revisionActionForKind(prior.Kind)
	if err != nil {
		return domain.EffectProposal{}, domain.EffectProposal{}, err
	}
	command, err := tx.GetCommand(ctx, commandID)
	if err != nil {
		return domain.EffectProposal{}, domain.EffectProposal{}, err
	}
	boundInstance, boundDigest, _, err := tx.proposalItemBinding(ctx, command.ItemID)
	if err != nil {
		return domain.EffectProposal{}, domain.EffectProposal{}, err
	}
	supersedes := domain.Digest(supersedesValue)
	if command.Action != expectedAction || boundInstance != instance.ID ||
		boundDigest != supersedes || supersedes != prior.Digest ||
		!slices.Equal(command.ArtifactDigests, []domain.Digest{supersedes}) {
		return domain.EffectProposal{}, domain.EffectProposal{}, errRowInconsistent
	}
	handle, err := proposalSubjectHandle(prior)
	if err != nil {
		return domain.EffectProposal{}, domain.EffectProposal{}, err
	}
	declaration, policy, err := tx.ResolveProposalSubject(ctx, handle)
	if err != nil {
		return domain.EffectProposal{}, domain.EffectProposal{}, err
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
	// Rebuild the expected revised proposal from the command-authored bounded
	// delta and current durable inputs, then require the stored revision body to
	// reproduce it exactly. A closure revision's rebuild draws its target,
	// provenance, and origin from the daemon's current closable-source
	// determination, so a decoded resolves flag is the only client-authored bit.
	expected, err := tx.expectedRevision(ctx, prior, declaration, policy, command.Message)
	if err != nil || expected.Digest != proposal.Digest {
		return domain.EffectProposal{}, domain.EffectProposal{}, errRowInconsistent
	}
	return proposal, prior, nil
}

// expectedRevision recomputes the revised proposal one revise command should
// produce, from the command's canonical bounded delta and current durable
// state. The switch dispatches on the prior kind and so omits default; the
// trailing return guards an unregistered kind.
func (tx *ReadTx) expectedRevision(
	ctx context.Context,
	prior domain.EffectProposal,
	declaration domain.WorkUnitDeclaration,
	policy domain.ResolvedPolicy,
	message string,
) (domain.EffectProposal, error) {
	switch prior.Kind {
	case domain.EffectTaskProposal:
		var revision struct {
			Intent            domain.TaskProposalIntent `json:"intent"`
			ExpectedCostUnits int                       `json:"expected_cost_units"`
			Scope             domain.TaskProposalScope  `json:"scope"`
		}
		if err := decodeCanonicalRevision(message, &revision); err != nil {
			return domain.EffectProposal{}, err
		}
		if err := domain.GateTaskProposalScope(revision.Scope, declaration); err != nil {
			return domain.EffectProposal{}, errRowInconsistent
		}
		return domain.NewEffectProposal(domain.EffectTaskProposal, domain.TaskProposalParameters{
			SubjectHandle: prior.TaskProposal.SubjectHandle, Intent: revision.Intent,
			ExpectedCostUnits: revision.ExpectedCostUnits, Scope: revision.Scope,
		}, policy)
	case domain.EffectSourceIssueClosure:
		var revision struct {
			Resolves bool `json:"resolves"`
		}
		if err := decodeCanonicalRevision(message, &revision); err != nil {
			return domain.EffectProposal{}, err
		}
		closable, err := tx.closableSource(ctx, declaration)
		if err != nil {
			return domain.EffectProposal{}, err
		}
		return domain.NewEffectProposal(domain.EffectSourceIssueClosure, domain.SourceIssueClosureInput{
			SubjectHandle:  prior.ClosureProposal.SubjectHandle,
			Source:         closable,
			ProposedTarget: prior.ClosureProposal.Target,
			Origin:         prior.ClosureProposal.Origin,
			Resolves:       revision.Resolves,
		}, policy)
	}
	return domain.EffectProposal{}, domain.ErrInvalidEffectKind
}

// ReviseClosureProposal rebuilds the revised source-issue-closure proposal a
// approve_with_changes command produces, from the prior proposal and the
// command's canonical {resolves} delta, drawing target, provenance, and origin
// from the daemon's current closable-source determination. It is the closure
// counterpart of the task path's inline revise construction; the caller checks
// that the digest actually changed (an unchanged resolves is rejected there).
func (tx *ReadTx) ReviseClosureProposal(
	ctx context.Context,
	prior domain.EffectProposal,
	message string,
) (domain.EffectProposal, error) {
	if prior.Kind != domain.EffectSourceIssueClosure || prior.ClosureProposal == nil {
		return domain.EffectProposal{}, domain.ErrEffectProposalInconsistent
	}
	declaration, policy, err := tx.ResolveProposalSubject(ctx, prior.ClosureProposal.SubjectHandle)
	if err != nil {
		return domain.EffectProposal{}, err
	}
	return tx.expectedRevision(ctx, prior, declaration, policy, message)
}

// ProspectiveMergeForItem returns the stored prospective merge of an effect
// proposal (closure) item, or nil for a task-proposal item. It fails closed on
// a partial merge (via proposalItemBinding).
func (tx *ReadTx) ProspectiveMergeForItem(
	ctx context.Context,
	itemID domain.ItemID,
) (*domain.ProspectiveMerge, error) {
	_, _, merge, err := tx.proposalItemBinding(ctx, itemID)
	if err != nil {
		return nil, err
	}
	return merge, nil
}

// decodeCanonicalRevision strictly decodes a revise command's message into the
// kind's bounded delta and rejects any message that is not the delta's exact
// canonical JSON, so a self-consistent but non-canonical body is not authority.
func decodeCanonicalRevision(message string, into any) error {
	if err := strictjson.Decode(
		[]byte(message), into, strictjson.RejectInvalidUTF8, domain.MaxEffectProposalBytes,
	); err != nil {
		return errRowInconsistent
	}
	canonical, err := json.Marshal(into)
	if err != nil || string(canonical) != message {
		return errRowInconsistent
	}
	return nil
}

func (tx *ReadTx) proposalItemBinding(
	ctx context.Context,
	itemID domain.ItemID,
) (domain.ProposalInstanceID, domain.Digest, *domain.ProspectiveMerge, error) {
	var instanceID, renderedDigest string
	var publicationIdentity, candidateHeadSHA, baseRef, baseSHA sql.NullString
	if err := tx.tx.QueryRowContext(ctx, `SELECT instance_id, content_digest,
		publication_identity, candidate_head_sha, base_ref, base_sha
		FROM effect_proposal_items WHERE item_id = ?`, itemID).Scan(
		&instanceID, &renderedDigest,
		&publicationIdentity, &candidateHeadSHA, &baseRef, &baseSHA); err != nil {
		return "", "", nil, fmt.Errorf("proposal for item %q: %w", itemID, notFoundOr(err))
	}
	if instanceID == "" || renderedDigest == "" {
		return "", "", nil, fmt.Errorf("proposal for item %q: %w", itemID, errRowInconsistent)
	}
	merge, err := scanProspectiveMerge(publicationIdentity, candidateHeadSHA, baseRef, baseSHA)
	if err != nil {
		return "", "", nil, fmt.Errorf("proposal for item %q: %w", itemID, err)
	}
	return domain.ProposalInstanceID(instanceID), domain.Digest(renderedDigest), merge, nil
}

// scanProspectiveMerge reconstructs the closure item's prospective merge from
// its four nullable columns. All four are set together for a closure item and
// all null for a task-proposal item; a partially populated row is a corrupt
// binding and fails closed. The reconstructed merge is validated so a blank
// column that slipped past the CHECK cannot become a trusted binding value.
func scanProspectiveMerge(
	publicationIdentity, candidateHeadSHA, baseRef, baseSHA sql.NullString,
) (*domain.ProspectiveMerge, error) {
	set := 0
	for _, v := range []sql.NullString{publicationIdentity, candidateHeadSHA, baseRef, baseSHA} {
		if v.Valid {
			set++
		}
	}
	switch set {
	case 0:
		return nil, nil
	case 4:
		merge := domain.ProspectiveMerge{
			PublicationIdentity: domain.Digest(publicationIdentity.String),
			CandidateHeadSHA:    candidateHeadSHA.String,
			BaseRef:             baseRef.String,
			BaseSHA:             baseSHA.String,
		}
		if err := merge.Validate(); err != nil {
			return nil, fmt.Errorf("%w: %w", errRowInconsistent, err)
		}
		return &merge, nil
	default:
		return nil, errRowInconsistent
	}
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
// then run the closed effect registry gate plus each kind's re-gate against
// state the store derives from current rows, never from the proposal body.
// The switch dispatches behaviour and so omits default; the trailing return
// guards an unregistered kind.
func (tx *ReadTx) gateProposalSubject(
	ctx context.Context,
	proposal domain.EffectProposal,
) (domain.WorkUnitDeclaration, error) {
	handle, err := proposalSubjectHandle(proposal)
	if err != nil {
		return domain.WorkUnitDeclaration{}, err
	}
	declaration, policy, err := tx.ResolveProposalSubject(ctx, handle)
	if err != nil {
		return domain.WorkUnitDeclaration{}, err
	}
	switch proposal.Kind {
	case domain.EffectTaskProposal:
		if err := domain.GateTaskProposalScope(proposal.TaskProposal.Scope, declaration); err != nil {
			return domain.WorkUnitDeclaration{}, err
		}
		if err := domain.GateEffectProposal(proposal, policy); err != nil {
			return domain.WorkUnitDeclaration{}, err
		}
		return declaration, nil
	case domain.EffectSourceIssueClosure:
		if err := domain.GateEffectProposal(proposal, policy); err != nil {
			return domain.WorkUnitDeclaration{}, err
		}
		closable, err := tx.closableSource(ctx, declaration)
		if err != nil {
			return domain.WorkUnitDeclaration{}, err
		}
		if err := domain.GateSourceIssueClosure(proposal, closable); err != nil {
			return domain.WorkUnitDeclaration{}, err
		}
		return declaration, nil
	}
	return domain.WorkUnitDeclaration{}, domain.ErrInvalidEffectKind
}

// closableSource derives, from current durable rows, whether the work unit
// behind a declaration has a closable source, which issue it is, and the
// provenance it earns. A daemon-bound issue subject is the verified target;
// any other task source leaves only a same-repository recommended target whose
// exact issue number the client chose and the daemon cannot re-derive (#1417).
// The derived fact, not the decoded proposal body, is the sole authority the
// closure gate re-checks against, and every cross-row identity is verified so
// a tampered join cannot redirect the target.
func (tx *ReadTx) closableSource(
	ctx context.Context,
	declaration domain.WorkUnitDeclaration,
) (domain.ClosableSource, error) {
	project, err := tx.GetProject(ctx, declaration.ProjectID)
	if err != nil {
		return domain.ClosableSource{}, err
	}
	run, err := tx.GetRun(ctx, declaration.RunID)
	if err != nil {
		return domain.ClosableSource{}, err
	}
	if run.ProjectID != project.ID || run.TaskID == "" {
		return domain.ClosableSource{}, errRowInconsistent
	}
	task, err := tx.GetTask(ctx, run.TaskID)
	if err != nil {
		return domain.ClosableSource{}, err
	}
	if task.ProjectID != project.ID {
		return domain.ClosableSource{}, errRowInconsistent
	}
	if task.Source != nil && task.Source.Kind == domain.SpecificationSourceIssueSubject && task.Source.IssueSubject != nil {
		subject := task.Source.IssueSubject
		// A daemon-bound issue subject is verified only when it names the
		// project's own repository. Label intake binds occurrences in that
		// repository, so a verified target is always same-repository, and a
		// cross-repository source yields no proposal (plan §5.13). Re-anchor the
		// verified target to project.RepositoryID here, alongside the run and
		// task ProjectID joins above, so a tampered task-source row cannot label
		// a foreign-repository close as verified and mislead an approver.
		if subject.Repo != project.Repo || subject.RepositoryID != project.RepositoryID {
			return domain.ClosableSource{}, errRowInconsistent
		}
		return domain.ClosableSource{
			Present: true, Provenance: domain.ClosureProvenanceVerified,
			Repo: subject.Repo, RepositoryID: subject.RepositoryID, IssueNumber: subject.IssueNumber,
		}, nil
	}
	return domain.ClosableSource{
		Present: true, Provenance: domain.ClosureProvenanceRecommended,
		Repo: project.Repo, RepositoryID: project.RepositoryID,
	}, nil
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
	// A revision keeps the effect kind; the authoring action is the kind's
	// revise action (start_with_changes for a task, approve_with_changes for a
	// closure).
	if prior.Kind != revised.Kind {
		return fmt.Errorf("put proposal revision command %q kind: %w", commandID, domain.ErrTransitionCommandMismatch)
	}
	expectedAction, err := revisionActionForKind(revised.Kind)
	if err != nil {
		return fmt.Errorf("put proposal revision command %q: %w", commandID, err)
	}
	command, err := tx.GetCommand(ctx, commandID)
	if err != nil {
		return fmt.Errorf("put proposal revision command %q: %w", commandID, err)
	}
	boundInstance, boundDigest, _, err := tx.proposalItemBinding(ctx, command.ItemID)
	if err != nil {
		return fmt.Errorf("put proposal revision command %q: %w", commandID, err)
	}
	if command.Action != expectedAction || boundInstance != instance.ID || boundDigest != prior.Digest {
		return fmt.Errorf("put proposal revision command %q: %w", commandID, domain.ErrTransitionCommandMismatch)
	}
	_, storedPrior, err := tx.ProposalForItem(ctx, command.ItemID)
	if err != nil {
		return fmt.Errorf("put proposal revision command %q prior: %w", commandID, err)
	}
	priorHandle, err := proposalSubjectHandle(storedPrior)
	if err != nil {
		return fmt.Errorf("put proposal revision command %q prior subject: %w", commandID, err)
	}
	revisedHandle, err := proposalSubjectHandle(revised)
	if err != nil {
		return fmt.Errorf("put proposal revision command %q subject: %w", commandID, err)
	}
	if storedPrior.Digest != prior.Digest || storedPrior.Kind != revised.Kind || revisedHandle != priorHandle {
		return fmt.Errorf("put proposal revision command %q subject: %w", commandID, domain.ErrTransitionCommandMismatch)
	}
	item, err := tx.GetAttentionItem(ctx, command.ItemID)
	if err != nil {
		return err
	}
	// gateProposalSubject re-derives the declaration and current policy and runs
	// the kind's full gate against live rows (task scope + effect gate, or the
	// closable-source re-gate for a closure), so the revised body is never its
	// own authority.
	declaration, err := tx.gateProposalSubject(ctx, revised)
	if err != nil {
		return fmt.Errorf("put proposal revision command %q gate: %w", commandID, err)
	}
	if declaration.ProjectID != item.ProjectID {
		return fmt.Errorf("put proposal revision command %q project: %w", commandID, domain.ErrTransitionCommandMismatch)
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

// proposalInstanceKind reads the effect kind of a stored instance. It is the
// authority for the action-family gate, taken from the durable row rather than
// any decoded proposal body.
func (tx *ReadTx) proposalInstanceKind(
	ctx context.Context,
	instanceID domain.ProposalInstanceID,
) (domain.EffectKind, error) {
	var kind string
	if err := tx.tx.QueryRowContext(ctx,
		`SELECT effect_kind FROM effect_proposal_instances WHERE instance_id = ?`,
		instanceID).Scan(&kind); err != nil {
		return "", notFoundOr(err)
	}
	return domain.EffectKind(kind), nil
}

// gateDecisionActionKind rejects a decision action that does not belong to the
// instance's effect kind. start and start_with_changes decide task proposals;
// approve and approve_with_changes decide effect (closure) proposals; decline
// terminates either. This is a predicate over the proposal-decision subset of
// the Action union, so it uses default rather than exhaustive dispatch.
func gateDecisionActionKind(action domain.Action, kind domain.EffectKind) error {
	switch action {
	case domain.ActionStart, domain.ActionStartWithChanges:
		if kind != domain.EffectTaskProposal {
			return domain.ErrTransitionCommandMismatch
		}
		return nil
	case domain.ActionApprove, domain.ActionApproveWithChanges:
		if kind != domain.EffectSourceIssueClosure {
			return domain.ErrTransitionCommandMismatch
		}
		return nil
	case domain.ActionDecline:
		return nil
	default:
		return domain.ErrTransitionCommandMismatch
	}
}

// revisionActionForKind returns the decision action that authors a revision for
// the given effect kind. The switch dispatches behaviour and so omits default,
// forcing a new registry member to declare its revise action.
func revisionActionForKind(kind domain.EffectKind) (domain.Action, error) {
	switch kind {
	case domain.EffectTaskProposal:
		return domain.ActionStartWithChanges, nil
	case domain.EffectSourceIssueClosure:
		return domain.ActionApproveWithChanges, nil
	}
	return "", domain.ErrInvalidEffectKind
}

// RecordProposalDecision appends the terminal ledger row for one effect
// identity. selectedDigest is required for start, start_with_changes, approve,
// and approve_with_changes, and absent for decline.
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
	boundInstance, boundDigest, _, err := tx.proposalItemBinding(ctx, command.ItemID)
	if err != nil {
		return fmt.Errorf("record proposal decision command %q: %w", commandID, err)
	}
	if command.Action != action || boundInstance != instanceID {
		return fmt.Errorf("record proposal decision command %q: %w", commandID, domain.ErrTransitionCommandMismatch)
	}
	kind, err := tx.proposalInstanceKind(ctx, instanceID)
	if err != nil {
		return fmt.Errorf("record proposal decision command %q kind: %w", commandID, err)
	}
	// The action family must match the effect kind: start/start_with_changes
	// decide a task proposal, approve/approve_with_changes an effect (closure)
	// proposal. Enforcing it here keeps a start from landing on a closure
	// instance (or an approve on a task instance) even if a caller mis-routes.
	if err := gateDecisionActionKind(action, kind); err != nil {
		return fmt.Errorf("record proposal decision command %q: %w", commandID, err)
	}
	// A "digest" decision (start/approve) binds the item's current rendered
	// digest; a "revision" decision (start_with_changes/approve_with_changes)
	// binds a revision row authored by this command; decline binds none.
	requireRevisionRow := func() error {
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
		return nil
	}
	switch action {
	case domain.ActionStart, domain.ActionApprove:
		if selectedDigest == nil || *selectedDigest != boundDigest {
			return fmt.Errorf("record proposal decision command %q selected digest: %w", commandID, domain.ErrTransitionCommandMismatch)
		}
	case domain.ActionStartWithChanges, domain.ActionApproveWithChanges:
		if err := requireRevisionRow(); err != nil {
			return err
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

// ClosureApprovalForInstance rebuilds the source-issue-closure approval binding
// an approve or approve_with_changes decision established, from the durable
// decision row, its authoring command, the decided item, and that item's stored
// prospective merge. It returns nil for an undecided or declined instance (the
// caller reads "no approval" as no close). It fails closed: a non-closure
// instance, an absent or partial merge, a candidate head that disagrees with the
// item's PRHeadSHA, an approved digest that no authenticated proposal or
// revision backs, or a malformed ClosureApproval all yield an error, never a
// silently weakened approval. #1419 passes the result to
// ClosureApproval.AuthorizesClose with the merge it observes now.
func (tx *ReadTx) ClosureApprovalForInstance(
	ctx context.Context,
	instanceID domain.ProposalInstanceID,
) (*domain.ClosureApproval, error) {
	var commandID, action string
	var selectedDigest sql.NullString
	err := tx.tx.QueryRowContext(ctx, `SELECT command_id, action, selected_digest
		FROM effect_proposal_decisions
		WHERE instance_id = ? AND action IN ('approve', 'approve_with_changes')`,
		instanceID).Scan(&commandID, &action, &selectedDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("closure approval for instance %q: %w", instanceID, err)
	}
	if !selectedDigest.Valid || selectedDigest.String == "" {
		return nil, fmt.Errorf("closure approval for instance %q: %w", instanceID, errRowInconsistent)
	}
	kind, err := tx.proposalInstanceKind(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("closure approval for instance %q: %w", instanceID, err)
	}
	if kind != domain.EffectSourceIssueClosure {
		return nil, fmt.Errorf("closure approval for instance %q: %w", instanceID, errRowInconsistent)
	}
	command, err := tx.GetCommand(ctx, commandID)
	if err != nil {
		return nil, fmt.Errorf("closure approval for instance %q: %w", instanceID, err)
	}
	// Re-gate the authoring command's action against the ledger row, exactly as
	// the write path (RecordProposalDecision) does. A durable row whose action
	// was corrupted to approve (from a decline, say) still names its original
	// command; without this check that decline command would pass the remaining
	// binding checks and mint a closure-authorizing approval.
	if command.Action != domain.Action(action) {
		return nil, fmt.Errorf("closure approval for instance %q: %w", instanceID, errRowInconsistent)
	}
	boundInstance, boundDigest, merge, err := tx.proposalItemBinding(ctx, command.ItemID)
	if err != nil {
		return nil, fmt.Errorf("closure approval for instance %q: %w", instanceID, err)
	}
	if boundInstance != instanceID || merge == nil {
		return nil, fmt.Errorf("closure approval for instance %q: %w", instanceID, errRowInconsistent)
	}
	// Re-tie the approved digest to an authenticated proposal: an approve binds
	// the item's rendered digest; an approve_with_changes binds a revision row
	// this command authored. A tampered selected_digest fails here.
	approved := domain.Digest(selectedDigest.String)
	switch domain.Action(action) {
	case domain.ActionApprove:
		if approved != boundDigest {
			return nil, fmt.Errorf("closure approval for instance %q: %w", instanceID, errRowInconsistent)
		}
	case domain.ActionApproveWithChanges:
		var matched int
		if err := tx.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM effect_proposal_revisions
			WHERE instance_id = ? AND content_digest = ? AND supersedes_digest = ? AND command_id = ?`,
			instanceID, approved, boundDigest, commandID).Scan(&matched); err != nil {
			return nil, fmt.Errorf("closure approval for instance %q revision: %w", instanceID, err)
		}
		if matched != 1 {
			return nil, fmt.Errorf("closure approval for instance %q revision: %w", instanceID, errRowInconsistent)
		}
	default:
		return nil, fmt.Errorf("closure approval for instance %q action %q: %w", instanceID, action, errRowInconsistent)
	}
	item, err := tx.GetAttentionItem(ctx, command.ItemID)
	if err != nil {
		return nil, fmt.Errorf("closure approval for instance %q: %w", instanceID, err)
	}
	if item.PRHeadSHA != merge.CandidateHeadSHA {
		return nil, fmt.Errorf("closure approval for instance %q: candidate head differs from item head: %w",
			instanceID, errRowInconsistent)
	}
	approval := domain.ClosureApproval{
		ProposalDigest:      approved,
		PublicationIdentity: merge.PublicationIdentity,
		CandidateHeadSHA:    merge.CandidateHeadSHA,
		BaseRef:             merge.BaseRef,
		BaseSHA:             merge.BaseSHA,
		Actor:               domain.ClosureApprovalActorHuman,
	}
	if err := approval.Validate(); err != nil {
		return nil, fmt.Errorf("closure approval for instance %q: %w: %w", instanceID, errRowInconsistent, err)
	}
	return &approval, nil
}

// OpenEffectItemForInstance returns the single open effect_proposal item bound
// to an instance and its stored merge, for OpenEffectProposalItem's idempotency
// and supersession. found is false when the instance has no open effect item.
// More than one open item for one instance is a corrupt state and fails closed,
// because OpenEffectProposalItem supersedes the prior item before opening a new
// one.
func (tx *ReadTx) OpenEffectItemForInstance(
	ctx context.Context,
	instanceID domain.ProposalInstanceID,
) (domain.AttentionItem, *domain.ProspectiveMerge, bool, error) {
	rows, err := tx.tx.QueryContext(ctx,
		`SELECT item_id FROM effect_proposal_items WHERE instance_id = ?`, instanceID)
	if err != nil {
		return domain.AttentionItem{}, nil, false, fmt.Errorf("open effect item for instance %q: %w", instanceID, err)
	}
	// Collect the ids and close the cursor before loading each item: the store
	// runs on one connection, so a per-row GetAttentionItem while rows is open
	// would contend with the cursor.
	var itemIDs []domain.ItemID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return domain.AttentionItem{}, nil, false, err
		}
		itemIDs = append(itemIDs, domain.ItemID(id))
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return domain.AttentionItem{}, nil, false, err
	}
	if err := rows.Close(); err != nil {
		return domain.AttentionItem{}, nil, false, err
	}
	var open domain.AttentionItem
	found := false
	for _, id := range itemIDs {
		item, err := tx.GetAttentionItem(ctx, id)
		if err != nil {
			return domain.AttentionItem{}, nil, false, err
		}
		if item.Type != domain.AttentionEffectProposal || item.Status != domain.StatusOpen {
			continue
		}
		if found {
			return domain.AttentionItem{}, nil, false,
				fmt.Errorf("open effect item for instance %q: %w", instanceID, errRowInconsistent)
		}
		open = item
		found = true
	}
	if !found {
		return domain.AttentionItem{}, nil, false, nil
	}
	_, _, merge, err := tx.proposalItemBinding(ctx, open.ID)
	if err != nil {
		return domain.AttentionItem{}, nil, false, err
	}
	return open, merge, true, nil
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
	boundInstance, boundDigest, _, err := tx.proposalItemBinding(ctx, command.ItemID)
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
	instanceID, _, _, err := tx.proposalItemBinding(ctx, itemID)
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
		boundInstance, boundDigest, _, err := tx.proposalItemBinding(ctx, command.ItemID)
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
