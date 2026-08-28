package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"unicode/utf8"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// The statements below are deliberately spelled out per entity, as constants:
// no SQL is ever assembled at runtime. Two write shapes, chosen by the domain
// contract: current-state aggregates (Run, Conversation, AttentionItem,
// AttentionDelivery) upsert, keeping the extracted key columns in sync with
// the body, incrementing entity_version, and stamping the enclosing
// transaction's as_of_revision (§5.14); write-once records (Artifact,
// AgentInvocation, Finding, Classification, ResolvedPolicy) go through
// putImmutable, since the domain corrects them with new versions or
// identities, never in place. An updating Put on a current-state aggregate
// still guards what the domain fixes at creation: identity bindings never
// change, and recorded history (a run's stages and attempts, a
// conversation's messages) only appends. Each Get selects the extracted
// columns alongside the body and cross-checks them; the synchronized
// aggregates funnel that reconstruction through one scan function per entity
// (see scanner), which their collection Lists reuse.

const putRunSQL = `
INSERT INTO runs (
    id, project_id, policy_digest, campaign_id, attempt_number,
    attempt_reason, parent_run_id, entity_version, as_of_revision, body
)
VALUES (?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    project_id     = excluded.project_id,
    policy_digest  = excluded.policy_digest,
    campaign_id    = excluded.campaign_id,
    attempt_number = excluded.attempt_number,
    attempt_reason = excluded.attempt_reason,
    parent_run_id  = excluded.parent_run_id,
    entity_version = runs.entity_version + 1,
    as_of_revision = excluded.as_of_revision,
    body           = excluded.body`

func (tx *WriteTx) PutRun(ctx context.Context, run domain.Run) error {
	if err := tx.authenticateRunProductionLineage(ctx, run); err != nil {
		return fmt.Errorf("put run %q: %w", run.ID, err)
	}
	body, err := encode(run)
	if err != nil {
		return fmt.Errorf("put run %q: %w", run.ID, err)
	}
	existing, err := tx.existingBody(ctx, `SELECT body FROM runs WHERE id = ?`, run.ID)
	if err != nil {
		return fmt.Errorf("put run %q: %w", run.ID, err)
	}
	if existing != nil {
		old, err := decode[domain.Run](existing)
		if err != nil {
			return fmt.Errorf("put run %q: %w", run.ID, err)
		}
		if err := domain.ValidateRunTransition(old, run); err != nil {
			return fmt.Errorf("put run %q: %w", run.ID, mapTransition(err))
		}
	}
	if _, err := tx.tx.ExecContext(ctx, putRunSQL,
		run.ID, run.ProjectID, run.PolicyDigest, nullableString(string(run.CampaignID)),
		nullableInt(run.AttemptNumber), nullableString(run.AttemptReason),
		nullableString(string(run.ParentRunID)), tx.asOfRevision, body); err != nil {
		return fmt.Errorf("put run %q: %w", run.ID, err)
	}
	return nil
}

// MigrateLegacyTrustProfileRunPolicy atomically translates the exact legacy
// fake-publication representation into an authenticated ResolvedPolicy
// binding, including every schedule already bound to that run. The old run
// digest must be preserved as the one trust-profile key's value and
// provenance, every other run field must remain byte-for-byte unchanged, the
// current row must still equal legacy, and no policy may already exist.
// Ordinary run and schedule updates retain immutable policy bindings.
func (tx *WriteTx) MigrateLegacyTrustProfileRunPolicy(
	ctx context.Context,
	legacy, updated domain.Run,
	policy domain.ResolvedPolicy,
) error {
	if legacy.PolicyDigest == updated.PolicyDigest {
		return fmt.Errorf("migrate run policy %q: digest did not change: %w",
			legacy.ID, domain.ErrImmutableTransition)
	}
	expected := legacy
	expected.PolicyDigest = updated.PolicyDigest
	expectedBody, err := encode(expected)
	if err != nil {
		return fmt.Errorf("migrate run policy %q expected: %w", legacy.ID, err)
	}
	updatedBody, err := encode(updated)
	if err != nil {
		return fmt.Errorf("migrate run policy %q updated: %w", legacy.ID, err)
	}
	if expectedBody != updatedBody ||
		policy.RunID != updated.ID || policy.Digest != updated.PolicyDigest {
		return fmt.Errorf("migrate run policy %q changes more than its authenticated binding: %w",
			legacy.ID, domain.ErrImmutableTransition)
	}
	if err := policy.Validate(); err != nil {
		return fmt.Errorf("migrate run policy %q policy: %w", legacy.ID, err)
	}
	if len(policy.Keys) != 1 || policy.Keys[0].Key != "trust_profile_digest" ||
		policy.Keys[0].Value != string(legacy.PolicyDigest) ||
		policy.Keys[0].Provenance.Source != domain.ProvenanceOverride ||
		policy.Keys[0].Provenance.Digest != legacy.PolicyDigest {
		return fmt.Errorf("migrate run policy %q does not preserve the legacy trust profile: %w",
			legacy.ID, domain.ErrImmutableTransition)
	}
	legacyBody, err := encode(legacy)
	if err != nil {
		return fmt.Errorf("migrate run policy %q legacy: %w", legacy.ID, err)
	}
	current, err := tx.GetRun(ctx, legacy.ID)
	if err != nil {
		return fmt.Errorf("migrate run policy %q current: %w", legacy.ID, err)
	}
	currentBody, err := encode(current)
	if err != nil {
		return fmt.Errorf("migrate run policy %q current: %w", legacy.ID, err)
	}
	if currentBody != legacyBody {
		return fmt.Errorf("migrate run policy %q current run differs from legacy: %w",
			legacy.ID, domain.ErrImmutableTransition)
	}
	if _, err := tx.GetResolvedPolicy(ctx, legacy.ID); err == nil {
		return fmt.Errorf("migrate run policy %q already has a resolved policy: %w",
			legacy.ID, ErrImmutableConflict)
	} else if !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("migrate run policy %q existing policy: %w", legacy.ID, err)
	}

	result, err := tx.tx.ExecContext(ctx, `
UPDATE runs
SET policy_digest = ?, entity_version = entity_version + 1,
    as_of_revision = ?, body = ?
WHERE id = ? AND policy_digest = ?`,
		updated.PolicyDigest, tx.asOfRevision, updatedBody,
		legacy.ID, legacy.PolicyDigest,
	)
	if err != nil {
		return fmt.Errorf("migrate run policy %q: %w", legacy.ID, err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("migrate run policy %q rows affected: %w", legacy.ID, err)
	}
	if changed != 1 {
		return fmt.Errorf("migrate run policy %q changed %d rows: %w",
			legacy.ID, changed, domain.ErrImmutableTransition)
	}
	if err := tx.migrateLegacyRunPolicySchedules(ctx, legacy, updated); err != nil {
		return err
	}
	return tx.PutResolvedPolicy(ctx, policy)
}

// scanRunSnapshot reconstructs one runs row (see the scanner doc for the
// shared gate sequence). Errors are returned unwrapped; callers add the
// entity/key context.
func (tx *ReadTx) scanRunSnapshot(ctx context.Context, sc scanner) (domain.Run, Snapshot, error) {
	var (
		id           string
		projectID    string
		policyDigest string
		campaignID   sql.NullString
		attempt      sql.NullInt64
		reason       sql.NullString
		parentRunID  sql.NullString
		snap         Snapshot
		body         []byte
	)
	if err := sc.Scan(
		&id, &projectID, &policyDigest, &campaignID, &attempt, &reason,
		&parentRunID, &snap.EntityVersion, &snap.AsOfRevision, &body,
	); err != nil {
		return domain.Run{}, Snapshot{}, err
	}
	run, err := decode[domain.Run](body)
	if err != nil {
		return domain.Run{}, Snapshot{}, err
	}
	if run.ID != domain.RunID(id) || run.ProjectID != domain.ProjectID(projectID) ||
		run.PolicyDigest != domain.Digest(policyDigest) ||
		!optionalStringEqual(campaignID, string(run.CampaignID)) ||
		!optionalIntEqual(attempt, run.AttemptNumber) ||
		!optionalStringEqual(reason, run.AttemptReason) ||
		!optionalStringEqual(parentRunID, string(run.ParentRunID)) ||
		snap.EntityVersion < 1 || snap.AsOfRevision < 1 {
		return domain.Run{}, Snapshot{}, errRowInconsistent
	}
	if err := tx.authenticateRunProductionLineage(ctx, run); err != nil {
		return domain.Run{}, Snapshot{}, err
	}
	return run, snap, nil
}

func (tx *ReadTx) GetRun(ctx context.Context, id domain.RunID) (domain.Run, error) {
	snapshot, err := tx.GetRunSnapshot(ctx, id)
	return snapshot.Value, err
}

// GetRunSnapshot reconstructs one run with the sync metadata read from the
// same row. It shares scanRunSnapshot with GetRun and ListRuns so every read
// re-runs the identical returned-object trust gate.
func (tx *ReadTx) GetRunSnapshot(ctx context.Context, id domain.RunID) (Snapshotted[domain.Run], error) {
	run, snapshot, err := tx.scanRunSnapshot(ctx, tx.tx.QueryRowContext(ctx,
		`SELECT id, project_id, policy_digest, campaign_id, attempt_number, attempt_reason,
                parent_run_id, entity_version, as_of_revision, body
         FROM runs WHERE id = ?`, id))
	if err != nil {
		return Snapshotted[domain.Run]{}, fmt.Errorf("get run %q: %w", id, notFoundOr(err))
	}
	return Snapshotted[domain.Run]{Value: run, Snapshot: snapshot}, nil
}

const putConversationSQL = `
INSERT INTO conversations (id, entity_version, as_of_revision, body)
VALUES (?, 1, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    entity_version = conversations.entity_version + 1,
    as_of_revision = excluded.as_of_revision,
    body           = excluded.body`

func (tx *WriteTx) PutConversation(ctx context.Context, conversation domain.Conversation) error {
	body, err := encode(conversation)
	if err != nil {
		return fmt.Errorf("put conversation %q: %w", conversation.ID, err)
	}
	existing, err := tx.existingBody(ctx, `SELECT body FROM conversations WHERE id = ?`, conversation.ID)
	if err != nil {
		return fmt.Errorf("put conversation %q: %w", conversation.ID, err)
	}
	if existing != nil {
		old, err := decode[domain.Conversation](existing)
		if err != nil {
			return fmt.Errorf("put conversation %q: %w", conversation.ID, err)
		}
		if err := domain.ValidateConversationTransition(old, conversation); err != nil {
			return fmt.Errorf("put conversation %q: %w", conversation.ID, mapTransition(err))
		}
	}
	if _, err := tx.tx.ExecContext(ctx, putConversationSQL, conversation.ID, tx.asOfRevision, body); err != nil {
		return fmt.Errorf("put conversation %q: %w", conversation.ID, err)
	}
	return nil
}

// scanConversationSnapshot reconstructs one conversations row (see the
// scanner doc for the shared gate sequence).
func (tx *ReadTx) scanConversationSnapshot(sc scanner) (domain.Conversation, Snapshot, error) {
	var (
		id   string
		snap Snapshot
		body []byte
	)
	if err := sc.Scan(&id, &snap.EntityVersion, &snap.AsOfRevision, &body); err != nil {
		return domain.Conversation{}, Snapshot{}, err
	}
	conversation, err := decode[domain.Conversation](body)
	if err != nil {
		return domain.Conversation{}, Snapshot{}, err
	}
	if conversation.ID != domain.ConversationID(id) ||
		snap.EntityVersion < 1 || snap.AsOfRevision < 1 {
		return domain.Conversation{}, Snapshot{}, errRowInconsistent
	}
	return conversation, snap, nil
}

func (tx *ReadTx) GetConversation(ctx context.Context, id domain.ConversationID) (domain.Conversation, error) {
	conversation, _, err := tx.scanConversationSnapshot(tx.tx.QueryRowContext(ctx,
		`SELECT id, entity_version, as_of_revision, body FROM conversations WHERE id = ?`, id))
	if err != nil {
		return domain.Conversation{}, fmt.Errorf("get conversation %q: %w", id, notFoundOr(err))
	}
	return conversation, nil
}

const putAgentInvocationSQL = `
INSERT INTO agent_invocations (id, entity_version, as_of_revision, body)
VALUES (?, 1, ?, ?)
ON CONFLICT (id) DO NOTHING`

func (tx *WriteTx) PutAgentInvocation(ctx context.Context, invocation domain.AgentInvocation) error {
	body, err := encode(invocation)
	if err != nil {
		return fmt.Errorf("put agent invocation %q: %w", invocation.ID, err)
	}
	if err := tx.putImmutable(ctx, putAgentInvocationSQL,
		[]any{invocation.ID, tx.asOfRevision, body},
		`SELECT body FROM agent_invocations WHERE id = ?`, []any{invocation.ID}, body); err != nil {
		return fmt.Errorf("put agent invocation %q: %w", invocation.ID, err)
	}
	return nil
}

func (tx *ReadTx) GetAgentInvocation(ctx context.Context, id domain.InvocationID) (domain.AgentInvocation, error) {
	var body []byte
	err := tx.tx.QueryRowContext(ctx,
		`SELECT body FROM agent_invocations WHERE id = ?`, id).Scan(&body)
	if err != nil {
		return domain.AgentInvocation{}, fmt.Errorf("get agent invocation %q: %w", id, notFoundOr(err))
	}
	invocation, err := decode[domain.AgentInvocation](body)
	if err != nil {
		return domain.AgentInvocation{}, fmt.Errorf("get agent invocation %q: %w", id, err)
	}
	if invocation.ID != id {
		return domain.AgentInvocation{}, fmt.Errorf("get agent invocation %q: %w", id, errRowInconsistent)
	}
	return invocation, nil
}

const putArtifactSQL = `
INSERT INTO artifacts (id, digest, entity_version, as_of_revision, body)
VALUES (?, ?, 1, ?, ?)
ON CONFLICT (id) DO NOTHING`

func (tx *WriteTx) PutArtifact(ctx context.Context, artifact domain.Artifact) error {
	body, err := encode(artifact)
	if err != nil {
		return fmt.Errorf("put artifact %q: %w", artifact.ID, err)
	}
	// Re-derive publish_eligibility against policy before persisting: encode's
	// Validate is policy-free, so a caller bypassing NewArtifact could otherwise
	// persist a forged publish_eligible under an unapproved recipe (plan §5.15
	// rule 2). Runs before the write, so an idempotent replay is gated too.
	if err := domain.ValidatePublishEligibility(artifact, tx.approvedRecipes); err != nil {
		return fmt.Errorf("put artifact %q: %w", artifact.ID, err)
	}
	if err := tx.gateEffectProposalArtifact(ctx, artifact); err != nil {
		return fmt.Errorf("put artifact %q: %w", artifact.ID, err)
	}
	if err := tx.putImmutable(ctx, putArtifactSQL,
		[]any{artifact.ID, artifact.Digest, tx.asOfRevision, body},
		`SELECT body FROM artifacts WHERE id = ?`, []any{artifact.ID}, body); err != nil {
		return fmt.Errorf("put artifact %q: %w", artifact.ID, err)
	}
	return nil
}

func (tx *ReadTx) GetArtifact(ctx context.Context, id domain.ArtifactID) (domain.Artifact, error) {
	var (
		digest string
		body   []byte
	)
	err := tx.tx.QueryRowContext(ctx,
		`SELECT digest, body FROM artifacts WHERE id = ?`, id).Scan(&digest, &body)
	if err != nil {
		return domain.Artifact{}, fmt.Errorf("get artifact %q: %w", id, notFoundOr(err))
	}
	artifact, err := decode[domain.Artifact](body)
	if err != nil {
		return domain.Artifact{}, fmt.Errorf("get artifact %q: %w", id, err)
	}
	if artifact.ID != id || artifact.Digest != domain.Digest(digest) {
		return domain.Artifact{}, fmt.Errorf("get artifact %q: %w", id, errRowInconsistent)
	}
	// Reconstruction re-runs the policy gate: decode's Validate cannot check
	// recipe approval, so a row whose publish_eligible disagrees with the
	// current approved-recipe set (a forged row, or one written under a policy
	// that no longer approves the recipe) fails closed rather than leaking as
	// valid evidence.
	if err := domain.ValidatePublishEligibility(artifact, tx.approvedRecipes); err != nil {
		return domain.Artifact{}, fmt.Errorf("get artifact %q: %w", id, err)
	}
	if err := tx.gateEffectProposalArtifact(ctx, artifact); err != nil {
		return domain.Artifact{}, fmt.Errorf("get artifact %q: %w", id, err)
	}
	return artifact, nil
}

const putAttentionItemSQL = `
INSERT INTO attention_items (id, project_id, conversation_id, item_type, status, health_posture, subject_run_id, readiness_summary, yield_history, entity_version, as_of_revision, body)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    project_id      = excluded.project_id,
    conversation_id = excluded.conversation_id,
    item_type       = excluded.item_type,
    status          = excluded.status,
    health_posture  = excluded.health_posture,
    subject_run_id  = excluded.subject_run_id,
    entity_version  = attention_items.entity_version + 1,
    as_of_revision  = excluded.as_of_revision,
    body            = excluded.body`

func (tx *WriteTx) PutAttentionItem(ctx context.Context, item domain.AttentionItem) error {
	body, err := encode(item)
	if err != nil {
		return fmt.Errorf("put attention item %q: %w", item.ID, err)
	}
	// Gate the embedded evidence against policy before persisting: encode's
	// Validate enforces only the producer-class half of the evidence rule, so a
	// caller bypassing NewAttentionItem could otherwise persist an evidence
	// artifact under an unapproved recipe (plan §5.15 rule 2). Runs before the
	// write, so an idempotent replay is gated too.
	if err := tx.gateEvidence(ctx, item); err != nil {
		return fmt.Errorf("put attention item %q: %w", item.ID, err)
	}
	if err := tx.gateFindingAdjudicationItem(ctx, item); err != nil {
		return fmt.Errorf("put attention item %q finding adjudication binding: %w", item.ID, err)
	}
	var readinessSummary *string
	if item.Readiness != nil {
		encoded, err := encode(*item.Readiness)
		if err != nil {
			return fmt.Errorf("put attention item %q readiness summary: %w", item.ID, err)
		}
		readinessSummary = &encoded
	}
	var yieldHistory *string
	if item.YieldHistory != nil {
		encoded, err := encode(*item.YieldHistory)
		if err != nil {
			return fmt.Errorf("put attention item %q yield history: %w", item.ID, err)
		}
		yieldHistory = &encoded
	}
	existing, err := tx.existingBody(ctx, `SELECT body FROM attention_items WHERE id = ?`, item.ID)
	if err != nil {
		return fmt.Errorf("put attention item %q: %w", item.ID, err)
	}
	if existing != nil {
		// A byte-identical replay (a retried command) converges without a
		// write, so it causes no entity_version churn.
		if string(existing) == body {
			return tx.putAttentionItemPRReference(ctx, item)
		}
		old, err := decode[domain.AttentionItem](existing)
		if err != nil {
			return fmt.Errorf("put attention item %q: %w", item.ID, err)
		}
		// A row persisted under an older encoding can carry this item's exact
		// content in different bytes: a since-added optional member (e.g.
		// commit_plan_notice, #222) renders as an explicit null the stored
		// body predates. Re-encode the decoded row so the idempotence compare
		// is canonical content, not raw bytes, and an unchanged replay against
		// such a row still converges without a version-advance demand; the
		// row itself is rewritten only by a real transition.
		oldCanonical, err := encode(old)
		if err != nil {
			return fmt.Errorf("put attention item %q: %w", item.ID, err)
		}
		if oldCanonical == body {
			return tx.putAttentionItemPRReference(ctx, item)
		}
		if err := domain.ValidateAttentionItemTransition(old, item); err != nil {
			return fmt.Errorf("put attention item %q: %w", item.ID, mapTransition(err))
		}
	}
	if _, err := tx.tx.ExecContext(ctx, putAttentionItemSQL,
		item.ID, item.ProjectID, item.ConversationID, item.Type, item.Status,
		item.Posture, item.Subject.RunID, readinessSummary, yieldHistory, tx.asOfRevision, body); err != nil {
		return fmt.Errorf("put attention item %q: %w", item.ID, err)
	}
	if err := tx.putAttentionItemPRReference(ctx, item); err != nil {
		return fmt.Errorf("put attention item %q pr reference: %w", item.ID, err)
	}
	if existing == nil && item.Type == domain.AttentionReadyForFinalReview && item.Status == domain.StatusOpen {
		tx.readyItemCreated = true
	}
	return nil
}

func (tx *ReadTx) gateFindingAdjudicationItem(
	ctx context.Context, item domain.AttentionItem,
) error {
	if item.Type != domain.AttentionFindingAdjudication {
		return nil
	}
	binding := item.FindingAdjudication
	artifact, err := tx.GetFindingAdjudication(ctx, binding.AdjudicationDigest)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return domain.ErrParentKeyMismatch
		}
		return err
	}
	if artifact.RunID != binding.RunID || artifact.Round != binding.Round {
		return domain.ErrParentKeyMismatch
	}
	record, err := tx.reviewRecordForRound(ctx, binding.RunID, binding.Round)
	if err != nil {
		return err
	}
	if item.PRHeadSHA != record.HeadSHA {
		return domain.ErrParentKeyMismatch
	}
	if len(binding.Proposals) != len(artifact.Entries) {
		return domain.ErrParentKeyMismatch
	}
	entries := make(map[domain.FindingID]domain.FindingAdjudicationEntry, len(artifact.Entries))
	for _, entry := range artifact.Entries {
		entries[entry.FindingID] = entry
	}
	for _, proposal := range binding.Proposals {
		entry, ok := entries[proposal.FindingID]
		// OfferedAlternatives is compared here so an offered route or consequence
		// introduced or rewritten only in the item payload fails closed against
		// the digest-bound artifact (#893). OfferedAlternative is a comparable
		// struct, so sameSlice compares element-wise on route and consequence,
		// order-sensitive, with the same nil-versus-empty parity as the other
		// list fields. This single gate covers PutAttentionItem and every
		// snapshot reconstruction, so restart and raw-row tampering reject too.
		if !ok || proposal.Producer != entry.Producer ||
			proposal.GoalRelationship != entry.GoalRelationship ||
			!sameOptionalComparable(proposal.Compatibility, entry.Compatibility) ||
			proposal.Route != entry.Route ||
			proposal.Rationale != entry.Rationale ||
			!sameSlice(proposal.CitedRules, entry.CitedRules) ||
			!sameSlice(proposal.Assumptions, entry.Assumptions) ||
			!sameSlice(proposal.OpenQuestions, entry.OpenQuestions) ||
			!sameOptionalComparable(proposal.Confidence, entry.Confidence) ||
			!sameSlice(proposal.OfferedAlternatives, entry.OfferedAlternatives) {
			return domain.ErrParentKeyMismatch
		}
	}
	return nil
}

func sameOptionalComparable[T comparable](left, right *T) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func sameSlice[T comparable](left, right []T) bool {
	return (left == nil) == (right == nil) && slices.Equal(left, right)
}

func (tx *ReadTx) GetAttentionItem(ctx context.Context, id domain.ItemID) (domain.AttentionItem, error) {
	item, _, err := tx.GetAttentionItemSnapshot(ctx, id)
	return item, err
}

// GetAttentionItemSnapshot returns the item together with its persisted sync
// metadata, for the command-acceptance boundary (#91): the binding fields
// inside the body cannot distinguish a stale expected_entity_version when the
// domain content matches, so acceptance needs the store's own version counter.
func (tx *ReadTx) GetAttentionItemSnapshot(ctx context.Context, id domain.ItemID) (domain.AttentionItem, Snapshot, error) {
	item, snap, err := tx.scanAttentionItemSnapshot(ctx, tx.tx.QueryRowContext(ctx,
		`SELECT id, project_id, conversation_id, item_type, status, health_posture, subject_run_id, readiness_summary, yield_history, entity_version, as_of_revision, body FROM attention_items WHERE id = ?`, id))
	if err != nil {
		return domain.AttentionItem{}, Snapshot{}, fmt.Errorf("get attention item %q: %w", id, notFoundOr(err))
	}
	return item, snap, nil
}

// GetAttentionItemRecord authenticates immutable item history without
// re-applying mutable current recipe approval. It is for terminal workflow
// recovery only: any path that presents an item's evidence as currently
// trusted must use GetAttentionItem or GetAttentionItemSnapshot.
func (tx *ReadTx) GetAttentionItemRecord(
	ctx context.Context,
	id domain.ItemID,
) (domain.AttentionItem, error) {
	item, _, err := scanAttentionItemRecord(tx.tx.QueryRowContext(ctx,
		`SELECT id, project_id, conversation_id, item_type, status, health_posture, subject_run_id, readiness_summary, yield_history, entity_version, as_of_revision, body FROM attention_items WHERE id = ?`, id))
	if err != nil {
		return domain.AttentionItem{}, fmt.Errorf("get attention item record %q: %w", id, notFoundOr(err))
	}
	if item.ID != id {
		return domain.AttentionItem{}, fmt.Errorf("get attention item record %q: %w", id, errRowInconsistent)
	}
	return item, nil
}

// scanAttentionItemSnapshot reconstructs one attention_items row (see the
// scanner doc for the shared gate sequence), including the evidence policy
// re-gate.
func (tx *ReadTx) scanAttentionItemSnapshot(ctx context.Context, sc scanner) (domain.AttentionItem, Snapshot, error) {
	item, snap, err := scanAttentionItemRecord(sc)
	if err != nil {
		return domain.AttentionItem{}, Snapshot{}, err
	}
	// Reconstruction re-runs the evidence gate: decode's Validate cannot check
	// recipe approval, so an item carrying evidence under a now-unapproved (or
	// forged) recipe fails closed rather than reconstructing as valid.
	if err := tx.gateEvidence(ctx, item); err != nil {
		return domain.AttentionItem{}, Snapshot{}, err
	}
	if err := tx.gateFindingAdjudicationItem(ctx, item); err != nil {
		return domain.AttentionItem{}, Snapshot{}, err
	}
	if err := tx.gateReadyItemPRReference(ctx, item); err != nil {
		return domain.AttentionItem{}, Snapshot{}, err
	}
	return item, snap, nil
}

func scanAttentionItemRecord(sc scanner) (domain.AttentionItem, Snapshot, error) {
	var (
		id             string
		projectID      string
		conversationID sql.NullString
		itemType       string
		status         string
		healthPosture  sql.NullString
		subjectRunID   sql.NullString
		readinessBody  sql.NullString
		yieldBody      sql.NullString
		snap           Snapshot
		body           []byte
	)
	if err := sc.Scan(&id, &projectID, &conversationID, &itemType, &status, &healthPosture, &subjectRunID, &readinessBody, &yieldBody, &snap.EntityVersion, &snap.AsOfRevision, &body); err != nil {
		return domain.AttentionItem{}, Snapshot{}, err
	}
	item, err := decode[domain.AttentionItem](body)
	if err != nil {
		return domain.AttentionItem{}, Snapshot{}, err
	}
	// item_type and status are the admission gate's lookup keys (issue #321),
	// while health_posture independently binds the safety decision the gate
	// acts on (issue #625), and subject_run_id binds run-scoped selection
	// (issue #824). A column diverging from the canonical body is a forged or
	// corrupt row, not repairable skew.
	consistent := item.ID == domain.ItemID(id) && item.ProjectID == domain.ProjectID(projectID) &&
		item.Type == domain.AttentionType(itemType) && item.Status == domain.ItemStatus(status)
	if conversationID.Valid {
		consistent = consistent && item.ConversationID != nil &&
			*item.ConversationID == domain.ConversationID(conversationID.String)
	} else {
		consistent = consistent && item.ConversationID == nil
	}
	if healthPosture.Valid {
		consistent = consistent && item.Posture != nil &&
			*item.Posture == domain.HealthPosture(healthPosture.String)
	} else {
		consistent = consistent && item.Posture == nil
	}
	if subjectRunID.Valid {
		consistent = consistent && item.Subject.RunID != nil &&
			*item.Subject.RunID == domain.RunID(subjectRunID.String)
	} else {
		consistent = consistent && item.Subject.RunID == nil
	}
	if readinessBody.Valid {
		readiness, err := decode[domain.ReadinessSummary]([]byte(readinessBody.String))
		if err != nil {
			return domain.AttentionItem{}, Snapshot{}, err
		}
		consistent = consistent && item.Readiness != nil && *item.Readiness == readiness
	} else {
		consistent = consistent && item.Readiness == nil
	}
	if yieldBody.Valid {
		history, err := decode[domain.ReviewYieldHistory]([]byte(yieldBody.String))
		if err != nil {
			return domain.AttentionItem{}, Snapshot{}, err
		}
		encoded, err := encode(history)
		if err != nil {
			return domain.AttentionItem{}, Snapshot{}, err
		}
		consistent = consistent && item.YieldHistory != nil && yieldBody.String == encoded
		if consistent {
			itemEncoded, err := encode(*item.YieldHistory)
			if err != nil {
				return domain.AttentionItem{}, Snapshot{}, err
			}
			consistent = itemEncoded == encoded
		}
	} else {
		consistent = consistent && item.YieldHistory == nil
	}
	// The metadata is store-stamped, so anything outside the values the Puts
	// can produce (versions start at 1, revisions are client-visible and
	// positive) is a forged or corrupt row, refused like a diverging column.
	consistent = consistent && snap.EntityVersion >= 1 && snap.AsOfRevision >= 1
	if !consistent {
		return domain.AttentionItem{}, Snapshot{}, errRowInconsistent
	}
	return item, snap, nil
}

// gateEvidence re-runs the approved-recipe evidence gate over an item's
// snapshot at the persistence/reconstruction boundary, using the transaction's
// policy set. It is the store's enforcement of the recipe-approval half of the
// evidence rule that AttentionItem.Validate cannot check (it holds no policy).
func (tx *ReadTx) gateEvidence(ctx context.Context, item domain.AttentionItem) error {
	for _, a := range item.EvidenceSnapshot {
		if err := domain.EligibleForEvidenceSnapshot(a, tx.approvedRecipes); err != nil {
			return err
		}
		if err := tx.gateEffectProposalArtifact(ctx, a); err != nil {
			return err
		}
	}
	return nil
}

// gateReadyItemPRReference re-anchors a ready item's client-visible pull
// request coordinates to the immutable store-owned reference. Production
// items additionally re-run the deeper first-party publication binding gate.
// The mutable synchronized body is data, never authority to retarget an
// operator action.
func (tx *ReadTx) gateReadyItemPRReference(ctx context.Context, item domain.AttentionItem) error {
	if item.Type != domain.AttentionReadyForFinalReview {
		return nil
	}
	anchored, err := tx.getAttentionItemPRReference(ctx, item.ID)
	if err != nil {
		return err
	}
	if item.PRReference == nil || *item.PRReference != anchored {
		return errRowInconsistent
	}
	binding, err := tx.GetReadyItemPRBinding(ctx, item.ID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if item.PRReference == nil || item.PRReference.Repo != binding.Repo ||
		item.PRReference.Number != binding.PRNumber {
		return errRowInconsistent
	}
	return nil
}

const putAttentionDeliverySQL = `
INSERT INTO attention_deliveries (item_id, device_id, channel, attempt, entity_version, as_of_revision, body)
VALUES (?, ?, ?, ?, 1, ?, ?)
ON CONFLICT (item_id, device_id, channel, attempt) DO UPDATE SET
    entity_version = attention_deliveries.entity_version + 1,
    as_of_revision = excluded.as_of_revision,
    body           = excluded.body`

func (tx *WriteTx) PutAttentionDelivery(ctx context.Context, delivery domain.AttentionDelivery) error {
	wrap := func(err error) error {
		return fmt.Errorf("put attention delivery %q/%q/%q/%d: %w",
			delivery.ItemID, delivery.DeviceID, delivery.Channel, delivery.Attempt, err)
	}
	body, err := encode(delivery)
	if err != nil {
		return wrap(err)
	}
	existing, err := tx.existingBody(ctx,
		`SELECT body FROM attention_deliveries WHERE item_id = ? AND device_id = ? AND channel = ? AND attempt = ?`,
		delivery.ItemID, delivery.DeviceID, delivery.Channel, delivery.Attempt)
	if err != nil {
		return wrap(err)
	}
	if existing != nil {
		// A byte-identical replay (a retried poll or outbox redelivery)
		// converges without a write.
		if string(existing) == body {
			return nil
		}
		old, err := decode[domain.AttentionDelivery](existing)
		if err != nil {
			return wrap(err)
		}
		if err := domain.ValidateAttentionDeliveryTransition(old, delivery); err != nil {
			return wrap(mapTransition(err))
		}
	}
	if _, err := tx.tx.ExecContext(ctx, putAttentionDeliverySQL,
		delivery.ItemID, delivery.DeviceID, delivery.Channel, delivery.Attempt,
		tx.asOfRevision, body); err != nil {
		return wrap(err)
	}
	return nil
}

// scanAttentionDeliverySnapshot reconstructs one attention_deliveries row
// (see the scanner doc for the shared gate sequence).
func (tx *ReadTx) scanAttentionDeliverySnapshot(sc scanner) (domain.AttentionDelivery, Snapshot, error) {
	var (
		itemID   string
		deviceID string
		channel  string
		attempt  int
		snap     Snapshot
		body     []byte
	)
	if err := sc.Scan(&itemID, &deviceID, &channel, &attempt, &snap.EntityVersion, &snap.AsOfRevision, &body); err != nil {
		return domain.AttentionDelivery{}, Snapshot{}, err
	}
	delivery, err := decode[domain.AttentionDelivery](body)
	if err != nil {
		return domain.AttentionDelivery{}, Snapshot{}, err
	}
	if delivery.ItemID != domain.ItemID(itemID) || delivery.DeviceID != domain.DeviceID(deviceID) ||
		delivery.Channel != channel || delivery.Attempt != attempt ||
		snap.EntityVersion < 1 || snap.AsOfRevision < 1 {
		return domain.AttentionDelivery{}, Snapshot{}, errRowInconsistent
	}
	return delivery, snap, nil
}

func (tx *ReadTx) GetAttentionDelivery(ctx context.Context, itemID domain.ItemID, deviceID domain.DeviceID, channel string, attempt int) (domain.AttentionDelivery, error) {
	delivery, _, err := tx.scanAttentionDeliverySnapshot(tx.tx.QueryRowContext(ctx,
		`SELECT item_id, device_id, channel, attempt, entity_version, as_of_revision, body FROM attention_deliveries WHERE item_id = ? AND device_id = ? AND channel = ? AND attempt = ?`,
		itemID, deviceID, channel, attempt))
	if err != nil {
		return domain.AttentionDelivery{}, fmt.Errorf("get attention delivery %q/%q/%q/%d: %w", itemID, deviceID, channel, attempt, notFoundOr(err))
	}
	return delivery, nil
}

const putFindingSQL = `
INSERT INTO findings (id, run_id, entity_version, as_of_revision, body)
VALUES (?, ?, 1, ?, ?)
ON CONFLICT (id) DO NOTHING`

func (tx *WriteTx) PutFinding(ctx context.Context, finding domain.Finding) error {
	body, err := encode(finding)
	if err != nil {
		return fmt.Errorf("put finding %q: %w", finding.ID, err)
	}
	if err := tx.putImmutable(ctx, putFindingSQL,
		[]any{finding.ID, finding.RunID, tx.asOfRevision, body},
		`SELECT body FROM findings WHERE id = ?`, []any{finding.ID}, body); err != nil {
		return fmt.Errorf("put finding %q: %w", finding.ID, err)
	}
	return nil
}

func (tx *ReadTx) GetFinding(ctx context.Context, id domain.FindingID) (domain.Finding, error) {
	var (
		runID string
		body  []byte
	)
	err := tx.tx.QueryRowContext(ctx,
		`SELECT run_id, body FROM findings WHERE id = ?`, id).Scan(&runID, &body)
	if err != nil {
		return domain.Finding{}, fmt.Errorf("get finding %q: %w", id, notFoundOr(err))
	}
	finding, err := decode[domain.Finding](body)
	if err != nil {
		return domain.Finding{}, fmt.Errorf("get finding %q: %w", id, err)
	}
	if finding.ID != id || finding.RunID != domain.RunID(runID) {
		return domain.Finding{}, fmt.Errorf("get finding %q: %w", id, errRowInconsistent)
	}
	return finding, nil
}

const putClassificationSQL = `
INSERT INTO classifications (finding_id, version, entity_version, as_of_revision, body)
VALUES (?, ?, 1, ?, ?)
ON CONFLICT (finding_id, version) DO NOTHING`

func (tx *WriteTx) PutClassification(ctx context.Context, classification domain.Classification) error {
	body, err := encode(classification)
	if err != nil {
		return fmt.Errorf("put classification %q v%d: %w", classification.FindingID, classification.Version, err)
	}
	if err := tx.putImmutable(ctx, putClassificationSQL,
		[]any{classification.FindingID, classification.Version, tx.asOfRevision, body},
		`SELECT body FROM classifications WHERE finding_id = ? AND version = ?`,
		[]any{classification.FindingID, classification.Version}, body); err != nil {
		return fmt.Errorf("put classification %q v%d: %w", classification.FindingID, classification.Version, err)
	}
	return nil
}

func (tx *ReadTx) GetClassification(ctx context.Context, findingID domain.FindingID, version int) (domain.Classification, error) {
	var body []byte
	err := tx.tx.QueryRowContext(ctx,
		`SELECT body FROM classifications WHERE finding_id = ? AND version = ?`, findingID, version).Scan(&body)
	if err != nil {
		return domain.Classification{}, fmt.Errorf("get classification %q v%d: %w", findingID, version, notFoundOr(err))
	}
	classification, err := decode[domain.Classification](body)
	if err != nil {
		return domain.Classification{}, fmt.Errorf("get classification %q v%d: %w", findingID, version, err)
	}
	if classification.FindingID != findingID || classification.Version != version {
		return domain.Classification{}, fmt.Errorf("get classification %q v%d: %w", findingID, version, errRowInconsistent)
	}
	return classification, nil
}

const putResolvedPolicySQL = `
INSERT INTO resolved_policies (run_id, digest, entity_version, as_of_revision, body)
VALUES (?, ?, 1, ?, ?)
ON CONFLICT (run_id) DO NOTHING`

func (tx *WriteTx) PutResolvedPolicy(ctx context.Context, policy domain.ResolvedPolicy) error {
	// encode calls policy.Validate, which recomputes the digest from the keys
	// and rejects a forged one, so body carries an authenticated content digest
	// (domain.ResolvedPolicy.ComputeDigest), not a caller label.
	body, err := encode(policy)
	if err != nil {
		return fmt.Errorf("put resolved policy %q: %w", policy.RunID, err)
	}
	// The run binds its resolved policy by digest (§5.3): a policy whose digest
	// disagrees with its run's policy_digest column is rejected. Because the
	// digest above is now authenticated, this transitively binds the run's
	// policy_digest to the verified content digest in this same transaction. A
	// missing run falls through to the foreign-key failure on insert.
	var runPolicyDigest string
	err = tx.tx.QueryRowContext(ctx,
		`SELECT policy_digest FROM runs WHERE id = ?`, policy.RunID).Scan(&runPolicyDigest)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("put resolved policy %q: %w", policy.RunID, err)
	}
	if err == nil && policy.Digest != domain.Digest(runPolicyDigest) {
		return fmt.Errorf("put resolved policy %q: digest %q does not match the run's policy_digest %q",
			policy.RunID, policy.Digest, runPolicyDigest)
	}
	if err := tx.putImmutable(ctx, putResolvedPolicySQL,
		[]any{policy.RunID, policy.Digest, tx.asOfRevision, body},
		`SELECT body FROM resolved_policies WHERE run_id = ?`, []any{policy.RunID}, body); err != nil {
		return fmt.Errorf("put resolved policy %q: %w", policy.RunID, err)
	}
	return nil
}

func (tx *ReadTx) GetResolvedPolicy(ctx context.Context, runID domain.RunID) (domain.ResolvedPolicy, error) {
	var (
		digest string
		body   []byte
	)
	err := tx.tx.QueryRowContext(ctx,
		`SELECT digest, body FROM resolved_policies WHERE run_id = ?`, runID).Scan(&digest, &body)
	if err != nil {
		return domain.ResolvedPolicy{}, fmt.Errorf("get resolved policy %q: %w", runID, notFoundOr(err))
	}
	policy, err := decode[domain.ResolvedPolicy](body)
	if err != nil {
		return domain.ResolvedPolicy{}, fmt.Errorf("get resolved policy %q: %w", runID, err)
	}
	if policy.RunID != runID || policy.Digest != domain.Digest(digest) {
		return domain.ResolvedPolicy{}, fmt.Errorf("get resolved policy %q: %w", runID, errRowInconsistent)
	}
	return policy, nil
}

const putCommandSQL = `
INSERT INTO commands (command_id, item_id, item_version, pr_head_sha, device_id, action, entity_version, as_of_revision, backup_binding_digest, body)
VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?, ?)`

type storedInlineClaim struct {
	Digest  domain.Digest `json:"digest"`
	Content string        `json:"content"`
}

type storedCommandEnvelope struct {
	Command      domain.Command      `json:"command"`
	InlineClaims []storedInlineClaim `json:"inline_claims"`
}

func encodeStoredCommand(
	command domain.Command, inlineOnly map[domain.Digest]string,
) (string, domain.Digest, error) {
	claims := make([]storedInlineClaim, 0, len(inlineOnly))
	for digest, content := range inlineOnly {
		claims = append(claims, storedInlineClaim{Digest: digest, Content: content})
	}
	sort.Slice(claims, func(i, j int) bool { return claims[i].Digest < claims[j].Digest })
	body, err := json.Marshal(storedCommandEnvelope{Command: command, InlineClaims: claims})
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(body)
	return string(body), domain.Digest(contentaddr.Format(sum[:])), nil
}

func decodeStoredCommand(
	body []byte,
) (domain.Command, map[domain.Digest]struct{}, domain.Digest, error) {
	var probe struct {
		Command json.RawMessage `json:"command"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return domain.Command{}, nil, "", err
	}
	if len(probe.Command) == 0 {
		command, err := decode[domain.Command](body)
		return command, map[domain.Digest]struct{}{}, "", err
	}

	var envelope storedCommandEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return domain.Command{}, nil, "", err
	}
	if err := envelope.Command.Validate(); err != nil {
		return domain.Command{}, nil, "", fmt.Errorf("stored row invalid: %w", err)
	}
	inline := make(map[domain.Digest]struct{}, len(envelope.InlineClaims))
	var previous domain.Digest
	for idx, claim := range envelope.InlineClaims {
		if claim.Digest == "" || claim.Content == "" ||
			!utf8.ValidString(claim.Content) ||
			len(claim.Content) > domain.MaxClaimTextBytes ||
			(domain.ClaimText{Content: claim.Content}).ComputeDigest() != claim.Digest ||
			!slices.Contains(envelope.Command.ArtifactDigests, claim.Digest) ||
			(idx > 0 && claim.Digest <= previous) {
			return domain.Command{}, nil, "", errRowInconsistent
		}
		inline[claim.Digest] = struct{}{}
		previous = claim.Digest
	}
	sum := sha256.Sum256(body)
	return envelope.Command, inline,
		domain.Digest(contentaddr.Format(sum[:])), nil
}

// PutCommand records one accepted client decision as a write-once, immutable
// row keyed by command_id (§5.14 ClientCommand; §5.9 effectively-once). Three
// checks, in this order:
//
//  1. Idempotency. A command_id already on record returns its original result
//     regardless of the item's current state, so a lost-response retry converges
//     rather than being re-judged as stale (§5.14 test 4). A different body under
//     that id is a conflict.
//  2. Openness. A genuinely new command commits only against an item whose
//     status is still open (issue #55). This runs before the binding check
//     because a closed item is the more fundamental rejection: a stale error
//     invites a rebind-and-retry that can never succeed once the lifecycle has
//     concluded. Version advance alone does not imply closure (and closure at
//     the current version defeats the binding check), so status is gated
//     explicitly.
//  3. Binding authority. For a genuinely new command, its pinned bindings (the
//     accepted item version, PR head, and rendered digest set) must still
//     describe the live item, or the submission is stale and the caller gets the
//     current item as the canonical replacement (§5.14 test 2). This closes the
//     stale-approval class (plan §3.1) at the persistence boundary: an approval
//     cannot commit against inputs that changed after it was prepared.
//
// It is client-visible, so it must run inside Write (which bumps revision and
// stamps as_of_revision, the row's recorded committed result).
func (tx *WriteTx) PutCommand(ctx context.Context, command domain.Command) error {
	commandBody, err := encode(command)
	if err != nil {
		return fmt.Errorf("put command %q: %w", command.CommandID, err)
	}
	existing, err := tx.existingBody(ctx, `SELECT body FROM commands WHERE command_id = ?`, command.CommandID)
	if err != nil {
		return fmt.Errorf("put command %q: %w", command.CommandID, err)
	}
	if existing != nil {
		stored, _, _, err := decodeStoredCommand(existing)
		if err != nil {
			return fmt.Errorf("put command %q: %w", command.CommandID, err)
		}
		storedBody, err := encode(stored)
		if err != nil {
			return fmt.Errorf("put command %q: %w", command.CommandID, err)
		}
		if storedBody == commandBody {
			return nil
		}
		return fmt.Errorf("put command %q: %w", command.CommandID, ErrImmutableConflict)
	}
	// GetAttentionItem returns a wrapped ErrNotFound when the bound item does not
	// exist (a command can only decide an item that is present), and re-runs the
	// evidence gate so the item compared against is itself well-formed.
	item, err := tx.GetAttentionItem(ctx, command.ItemID)
	if err != nil {
		return fmt.Errorf("put command %q: %w", command.CommandID, err)
	}
	if item.Status != domain.StatusOpen {
		return fmt.Errorf("put command %q: %w", command.CommandID,
			&ClosedItemError{CommandID: command.CommandID, Item: item})
	}
	if !command.BindsSameAs(item) {
		return fmt.Errorf("put command %q: %w", command.CommandID,
			&StaleCommandError{CommandID: command.CommandID, Replacement: item})
	}
	// The command binds the live item; its action must also be one the item
	// offered. A stale item is handled above (the client re-decides against the
	// replacement's offered set), so reaching here means the action was checked
	// against the exact version the decision was rendered from.
	if !item.Offers(command.Action) {
		return fmt.Errorf("put command %q: action %q not offered by item %q: %w",
			command.CommandID, command.Action, command.ItemID, ErrActionNotOffered)
	}
	externallyBacked := make(map[domain.Digest]struct{},
		len(item.EvidenceSnapshot)+len(item.AgentClaims))
	for _, artifact := range item.EvidenceSnapshot {
		externallyBacked[artifact.Digest] = struct{}{}
	}
	for _, claim := range item.AgentClaims {
		if claim.Text == nil {
			externallyBacked[claim.Digest] = struct{}{}
		}
	}
	inlineOnly := make(map[domain.Digest]string, len(item.AgentClaims))
	for _, claim := range item.AgentClaims {
		if claim.Text == nil {
			continue
		}
		if _, external := externallyBacked[claim.Digest]; external {
			continue
		}
		inlineOnly[claim.Digest] = claim.Text.Content
	}
	body, bindingDigest, err := encodeStoredCommand(command, inlineOnly)
	if err != nil {
		return fmt.Errorf("put command %q backup binding: %w", command.CommandID, err)
	}
	if _, err := tx.tx.ExecContext(ctx, putCommandSQL,
		command.CommandID, command.ItemID, command.ItemVersion, command.PRHeadSHA,
		command.DeviceID, string(command.Action), tx.asOfRevision, bindingDigest, body); err != nil {
		return fmt.Errorf("put command %q: %w", command.CommandID, err)
	}
	return nil
}

func (tx *ReadTx) GetCommand(ctx context.Context, commandID string) (domain.Command, error) {
	command, _, err := tx.GetCommandSnapshot(ctx, commandID)
	return command, err
}

// GetCommandSnapshot returns the command together with its persisted sync
// metadata. The row is write-once, so its AsOfRevision is the command's
// original committed result: the revision an idempotent retry must return
// unchanged (§5.14 test 4). Inside the accepting Write itself the row already
// carries the revision that transaction will commit as, so the fresh-accept
// and retry paths read the result the same way.
func (tx *ReadTx) GetCommandSnapshot(ctx context.Context, commandID string) (domain.Command, Snapshot, error) {
	command, _, snap, err := tx.getStoredCommandSnapshot(ctx, commandID)
	return command, snap, err
}

func (tx *ReadTx) getStoredCommandSnapshot(
	ctx context.Context, commandID string,
) (domain.Command, map[domain.Digest]struct{}, Snapshot, error) {
	var (
		itemID              string
		itemVersion         int
		prHeadSHA           string
		deviceID            string
		action              string
		backupBindingDigest domain.Digest
		snap                Snapshot
		body                []byte
	)
	err := tx.tx.QueryRowContext(ctx,
		`SELECT item_id, item_version, pr_head_sha, device_id, action, entity_version, as_of_revision, backup_binding_digest, body FROM commands WHERE command_id = ?`, commandID).
		Scan(&itemID, &itemVersion, &prHeadSHA, &deviceID, &action,
			&snap.EntityVersion, &snap.AsOfRevision, &backupBindingDigest, &body)
	if err != nil {
		return domain.Command{}, nil, Snapshot{},
			fmt.Errorf("get command %q: %w", commandID, notFoundOr(err))
	}
	command, inline, computedBindingDigest, err := decodeStoredCommand(body)
	if err != nil {
		return domain.Command{}, nil, Snapshot{}, fmt.Errorf("get command %q: %w", commandID, err)
	}
	// Every binding the store extracts into a column is cross-checked against the
	// body: a forged row whose JSON disagrees with its authoritative columns (the
	// bound version, head, action, device, or item) fails loudly instead of
	// returning a decision record the columns do not back. The metadata is held
	// to what PutCommand can produce: the row is written once at entity_version 1
	// and never updated, and its revision is client-visible, so anything else is
	// a forged or corrupt result.
	if command.CommandID != commandID ||
		command.ItemID != domain.ItemID(itemID) ||
		command.ItemVersion != itemVersion ||
		command.PRHeadSHA != prHeadSHA ||
		command.DeviceID != domain.DeviceID(deviceID) ||
		command.Action != domain.Action(action) ||
		computedBindingDigest != backupBindingDigest ||
		snap.EntityVersion != 1 || snap.AsOfRevision < 1 {
		return domain.Command{}, nil, Snapshot{},
			fmt.Errorf("get command %q: %w", commandID, errRowInconsistent)
	}
	return command, inline, snap, nil
}

const putDeviceSQL = `
INSERT INTO devices (id, status, entity_version, as_of_revision, body)
VALUES (?, ?, 1, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    status         = excluded.status,
    entity_version = devices.entity_version + 1,
    as_of_revision = excluded.as_of_revision,
    body           = excluded.body`

func (tx *WriteTx) PutDevice(ctx context.Context, device domain.Device) error {
	body, err := encode(device)
	if err != nil {
		return fmt.Errorf("put device %q: %w", device.ID, err)
	}
	existing, err := tx.existingBody(ctx, `SELECT body FROM devices WHERE id = ?`, device.ID)
	if err != nil {
		return fmt.Errorf("put device %q: %w", device.ID, err)
	}
	if existing != nil {
		// A byte-identical replay (a retried revocation) converges without a
		// write, so it causes no entity_version churn.
		if string(existing) == body {
			return nil
		}
		old, err := decode[domain.Device](existing)
		if err != nil {
			return fmt.Errorf("put device %q: %w", device.ID, err)
		}
		if err := domain.ValidateDeviceTransition(old, device); err != nil {
			return fmt.Errorf("put device %q: %w", device.ID, mapTransition(err))
		}
	}
	if _, err := tx.tx.ExecContext(ctx, putDeviceSQL,
		device.ID, string(device.Status), tx.asOfRevision, body); err != nil {
		return fmt.Errorf("put device %q: %w", device.ID, err)
	}
	return nil
}

func (tx *ReadTx) GetDevice(ctx context.Context, id domain.DeviceID) (domain.Device, error) {
	device, _, err := tx.GetDeviceSnapshot(ctx, id)
	return device, err
}

// GetDeviceSnapshot returns the device together with its persisted sync
// metadata (#106): the pairing and revocation responses render the device as
// a DeviceSnapshot, and deriving entity_version/as_of_revision outside the
// store would duplicate its private revision-stamping invariant.
func (tx *ReadTx) GetDeviceSnapshot(ctx context.Context, id domain.DeviceID) (domain.Device, Snapshot, error) {
	var (
		status string
		snap   Snapshot
		body   []byte
	)
	err := tx.tx.QueryRowContext(ctx,
		`SELECT status, entity_version, as_of_revision, body FROM devices WHERE id = ?`, id).
		Scan(&status, &snap.EntityVersion, &snap.AsOfRevision, &body)
	if err != nil {
		return domain.Device{}, Snapshot{}, fmt.Errorf("get device %q: %w", id, notFoundOr(err))
	}
	device, err := decode[domain.Device](body)
	if err != nil {
		return domain.Device{}, Snapshot{}, fmt.Errorf("get device %q: %w", id, err)
	}
	// Devices are mutable (revocation bumps entity_version), so the metadata
	// is held to the mutable-entity range PutDevice can produce: versions
	// start at 1, revisions are client-visible and positive.
	if device.ID != id || device.Status != domain.DeviceStatus(status) ||
		snap.EntityVersion < 1 || snap.AsOfRevision < 1 {
		return domain.Device{}, Snapshot{}, fmt.Errorf("get device %q: %w", id, errRowInconsistent)
	}
	return device, snap, nil
}
