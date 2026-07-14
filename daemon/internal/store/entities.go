package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// ErrNotFound is returned (wrapped, with the entity and id) by every Get
// whose row does not exist.
var ErrNotFound = errors.New("not found")

// errRowInconsistent marks a row whose JSON body disagrees with its extracted
// key columns: the store's foreign keys and lookups act on the columns, so a
// divergent body would be trusted domain data with unenforced keys. Every Get
// cross-checks the two and fails loudly instead of returning it.
var errRowInconsistent = errors.New("stored row body inconsistent with its key columns")

// ErrImmutableConflict is returned (wrapped, with the entity and id) when a
// write-once entity is re-put with different content under an existing key.
// The domain contract makes these values immutable (a correction is a new
// value with a new version or identity, never an in-place edit), so the store
// tolerates only byte-identical replays: a retry converges, a rewrite fails.
var ErrImmutableConflict = errors.New("immutable row already exists with different content")

// ErrStaleWrite is returned (wrapped, with the entity and id) when an update
// to a current-state aggregate does not move its state forward: an attention
// item whose item_version is not beyond the stored one, or a delivery whose
// lifecycle status regresses. Retries replaying the identical bytes converge
// silently; a genuinely stale body must fail rather than roll back state
// (§5.14 optimistic concurrency).
var ErrStaleWrite = errors.New("write is stale: stored state is newer")

// validator is implemented by every persisted domain type. Puts validate
// before writing and Gets validate after reading, so a corrupt row fails
// loudly at the boundary instead of leaking an invalid value into the daemon.
type validator interface{ Validate() error }

// encode validates v and returns its canonical JSON body, as a string so it
// binds as TEXT (a []byte binds as BLOB, which a STRICT TEXT column rejects).
func encode(v validator) (string, error) {
	if err := v.Validate(); err != nil {
		return "", err
	}
	body, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

// decode unmarshals a stored body and re-validates it: Validate is the
// deserialization backstop for values that bypassed their constructor.
func decode[T validator](body []byte) (T, error) {
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		return v, err
	}
	if err := v.Validate(); err != nil {
		return v, fmt.Errorf("stored row invalid: %w", err)
	}
	return v, nil
}

// putImmutable inserts a write-once row (INSERT ... ON CONFLICT DO NOTHING),
// tolerating only a byte-identical replay of an existing key: canonical
// json.Marshal is deterministic, so a retried Put of the same value converges
// on the original row (no entity_version churn, nothing new for sync to
// observe), while a same-key write with different content fails with
// ErrImmutableConflict.
func (tx *WriteTx) putImmutable(ctx context.Context, insertSQL string, insertArgs []any, selectBodySQL string, keyArgs []any, body string) error {
	res, err := tx.tx.ExecContext(ctx, insertSQL, insertArgs...)
	if err != nil {
		return err
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if inserted > 0 {
		return nil
	}
	var existing string
	if err := tx.tx.QueryRowContext(ctx, selectBodySQL, keyArgs...).Scan(&existing); err != nil {
		return err
	}
	if existing != body {
		return ErrImmutableConflict
	}
	return nil
}

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
// columns alongside the body and cross-checks them.

// existingBody fetches the current body for an aggregate's key, or nil when
// the row does not exist. The query must be a constant from this file.
func (tx *WriteTx) existingBody(ctx context.Context, selectSQL string, keyArgs ...any) ([]byte, error) {
	var body []byte
	err := tx.tx.QueryRowContext(ctx, selectSQL, keyArgs...).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return body, nil
}

// jsonEqual compares two values by their canonical JSON, the same byte form
// the store persists.
func jsonEqual(a, b any) (bool, error) {
	ab, err := json.Marshal(a)
	if err != nil {
		return false, err
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false, err
	}
	return string(ab) == string(bb), nil
}

const putRunSQL = `
INSERT INTO runs (id, project_id, policy_digest, entity_version, as_of_revision, body)
VALUES (?, ?, ?, 1, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    project_id     = excluded.project_id,
    policy_digest  = excluded.policy_digest,
    entity_version = runs.entity_version + 1,
    as_of_revision = excluded.as_of_revision,
    body           = excluded.body`

// stagesExtend reports whether new preserves old's recorded execution
// history: every existing stage keeps its identity and name, every existing
// attempt is unchanged, and growth is append-only.
func stagesExtend(old, updated []domain.Stage) bool {
	if len(updated) < len(old) {
		return false
	}
	for i, os := range old {
		ns := updated[i]
		if ns.ID != os.ID || ns.RunID != os.RunID || ns.Name != os.Name {
			return false
		}
		if len(ns.Attempts) < len(os.Attempts) {
			return false
		}
		for j, oa := range os.Attempts {
			if ns.Attempts[j] != oa {
				return false
			}
		}
	}
	return true
}

func (tx *WriteTx) PutRun(ctx context.Context, run domain.Run) error {
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
		// Project, approved spec, and resolved policy are fixed at run
		// creation (§5.3 binds a run to its spec and policy digests), and
		// stages/attempts are recorded history: an update may only append.
		if run.ProjectID != old.ProjectID || run.SpecDigest != old.SpecDigest ||
			run.PolicyDigest != old.PolicyDigest || !stagesExtend(old.Stages, run.Stages) {
			return fmt.Errorf("put run %q: fixed bindings or recorded history would change: %w", run.ID, ErrImmutableConflict)
		}
	}
	if _, err := tx.tx.ExecContext(ctx, putRunSQL, run.ID, run.ProjectID, run.PolicyDigest, tx.asOfRevision, body); err != nil {
		return fmt.Errorf("put run %q: %w", run.ID, err)
	}
	return nil
}

func (tx *ReadTx) GetRun(ctx context.Context, id domain.RunID) (domain.Run, error) {
	var (
		projectID    string
		policyDigest string
		body         []byte
	)
	err := tx.tx.QueryRowContext(ctx,
		`SELECT project_id, policy_digest, body FROM runs WHERE id = ?`, id).
		Scan(&projectID, &policyDigest, &body)
	if err != nil {
		return domain.Run{}, fmt.Errorf("get run %q: %w", id, notFoundOr(err))
	}
	run, err := decode[domain.Run](body)
	if err != nil {
		return domain.Run{}, fmt.Errorf("get run %q: %w", id, err)
	}
	if run.ID != id || run.ProjectID != domain.ProjectID(projectID) ||
		run.PolicyDigest != domain.Digest(policyDigest) {
		return domain.Run{}, fmt.Errorf("get run %q: %w", id, errRowInconsistent)
	}
	return run, nil
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
		// Messages are immutable and corrections are new messages (§5.14):
		// an update must carry every stored message unchanged and may only
		// append.
		if len(conversation.Messages) < len(old.Messages) {
			return fmt.Errorf("put conversation %q: stored messages would be dropped: %w", conversation.ID, ErrImmutableConflict)
		}
		same, err := jsonEqual(old.Messages, conversation.Messages[:len(old.Messages)])
		if err != nil {
			return fmt.Errorf("put conversation %q: %w", conversation.ID, err)
		}
		if !same {
			return fmt.Errorf("put conversation %q: stored messages would be rewritten: %w", conversation.ID, ErrImmutableConflict)
		}
	}
	if _, err := tx.tx.ExecContext(ctx, putConversationSQL, conversation.ID, tx.asOfRevision, body); err != nil {
		return fmt.Errorf("put conversation %q: %w", conversation.ID, err)
	}
	return nil
}

func (tx *ReadTx) GetConversation(ctx context.Context, id domain.ConversationID) (domain.Conversation, error) {
	var body []byte
	err := tx.tx.QueryRowContext(ctx,
		`SELECT body FROM conversations WHERE id = ?`, id).Scan(&body)
	if err != nil {
		return domain.Conversation{}, fmt.Errorf("get conversation %q: %w", id, notFoundOr(err))
	}
	conversation, err := decode[domain.Conversation](body)
	if err != nil {
		return domain.Conversation{}, fmt.Errorf("get conversation %q: %w", id, err)
	}
	if conversation.ID != id {
		return domain.Conversation{}, fmt.Errorf("get conversation %q: %w", id, errRowInconsistent)
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
	return artifact, nil
}

const putAttentionItemSQL = `
INSERT INTO attention_items (id, project_id, conversation_id, entity_version, as_of_revision, body)
VALUES (?, ?, ?, 1, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    project_id      = excluded.project_id,
    conversation_id = excluded.conversation_id,
    entity_version  = attention_items.entity_version + 1,
    as_of_revision  = excluded.as_of_revision,
    body            = excluded.body`

func (tx *WriteTx) PutAttentionItem(ctx context.Context, item domain.AttentionItem) error {
	body, err := encode(item)
	if err != nil {
		return fmt.Errorf("put attention item %q: %w", item.ID, err)
	}
	existing, err := tx.existingBody(ctx, `SELECT body FROM attention_items WHERE id = ?`, item.ID)
	if err != nil {
		return fmt.Errorf("put attention item %q: %w", item.ID, err)
	}
	if existing != nil {
		// A byte-identical replay (a retried command) converges without a
		// write, so it causes no entity_version churn.
		if string(existing) == body {
			return nil
		}
		old, err := decode[domain.AttentionItem](existing)
		if err != nil {
			return fmt.Errorf("put attention item %q: %w", item.ID, err)
		}
		// What an item is about is fixed at creation: transitions bump
		// item_version and evolve status/evidence on the same identity, and
		// a different subject or type is a new (superseding) item, never a
		// retarget (§4, §5.14).
		sameSubject, err := jsonEqual(old.Subject, item.Subject)
		if err != nil {
			return fmt.Errorf("put attention item %q: %w", item.ID, err)
		}
		if item.ProjectID != old.ProjectID || item.Type != old.Type || !sameSubject {
			return fmt.Errorf("put attention item %q: fixed bindings would change: %w", item.ID, ErrImmutableConflict)
		}
		// A changed body must move the version forward, or a stale copy
		// could roll back a later transition (a resolved v2 overwritten by
		// an open v1).
		if item.ItemVersion <= old.ItemVersion {
			return fmt.Errorf("put attention item %q: item_version %d does not advance stored %d: %w",
				item.ID, item.ItemVersion, old.ItemVersion, ErrStaleWrite)
		}
	}
	if _, err := tx.tx.ExecContext(ctx, putAttentionItemSQL,
		item.ID, item.ProjectID, item.ConversationID, tx.asOfRevision, body); err != nil {
		return fmt.Errorf("put attention item %q: %w", item.ID, err)
	}
	return nil
}

func (tx *ReadTx) GetAttentionItem(ctx context.Context, id domain.ItemID) (domain.AttentionItem, error) {
	var (
		projectID      string
		conversationID sql.NullString
		body           []byte
	)
	err := tx.tx.QueryRowContext(ctx,
		`SELECT project_id, conversation_id, body FROM attention_items WHERE id = ?`, id).
		Scan(&projectID, &conversationID, &body)
	if err != nil {
		return domain.AttentionItem{}, fmt.Errorf("get attention item %q: %w", id, notFoundOr(err))
	}
	item, err := decode[domain.AttentionItem](body)
	if err != nil {
		return domain.AttentionItem{}, fmt.Errorf("get attention item %q: %w", id, err)
	}
	consistent := item.ID == id && item.ProjectID == domain.ProjectID(projectID)
	if conversationID.Valid {
		consistent = consistent && item.ConversationID != nil &&
			*item.ConversationID == domain.ConversationID(conversationID.String)
	} else {
		consistent = consistent && item.ConversationID == nil
	}
	if !consistent {
		return domain.AttentionItem{}, fmt.Errorf("get attention item %q: %w", id, errRowInconsistent)
	}
	return item, nil
}

const putAttentionDeliverySQL = `
INSERT INTO attention_deliveries (item_id, device_id, channel, attempt, entity_version, as_of_revision, body)
VALUES (?, ?, ?, ?, 1, ?, ?)
ON CONFLICT (item_id, device_id, channel, attempt) DO UPDATE SET
    entity_version = attention_deliveries.entity_version + 1,
    as_of_revision = excluded.as_of_revision,
    body           = excluded.body`

// deliveryRank orders the delivery lifecycle for the forward-only update
// guard. Behaviour-dispatch switch per the domain convention: no default, so
// the exhaustive linter forces a new status to be ranked; the trailing return
// covers the invalid zero value.
func deliveryRank(status domain.DeliveryStatus) int {
	switch status {
	case domain.DeliverySubmitted:
		return 1
	case domain.DeliveryChannelAccepted:
		return 2
	case domain.DeliveryOpened:
		return 3
	}
	return 0
}

// timesEqual compares an optional receipt pair, nil meaning not yet recorded.
func timesEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

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
		// The lifecycle only moves forward: a stale retry must not roll an
		// opened delivery back to submitted and drop the receipts that
		// timing aggregates depend on.
		if deliveryRank(delivery.Status) <= deliveryRank(old.Status) {
			return wrap(fmt.Errorf("delivery_status %q does not advance stored %q: %w",
				delivery.Status, old.Status, ErrStaleWrite))
		}
		// Advancing preserves the receipts already recorded.
		if !delivery.SubmittedAt.Equal(old.SubmittedAt) ||
			(old.ChannelAcceptedAt != nil && !timesEqual(delivery.ChannelAcceptedAt, old.ChannelAcceptedAt)) ||
			(old.OpenedAt != nil && !timesEqual(delivery.OpenedAt, old.OpenedAt)) {
			return wrap(fmt.Errorf("recorded receipts would change: %w", ErrImmutableConflict))
		}
	}
	if _, err := tx.tx.ExecContext(ctx, putAttentionDeliverySQL,
		delivery.ItemID, delivery.DeviceID, delivery.Channel, delivery.Attempt,
		tx.asOfRevision, body); err != nil {
		return wrap(err)
	}
	return nil
}

func (tx *ReadTx) GetAttentionDelivery(ctx context.Context, itemID domain.ItemID, deviceID domain.DeviceID, channel string, attempt int) (domain.AttentionDelivery, error) {
	var body []byte
	err := tx.tx.QueryRowContext(ctx,
		`SELECT body FROM attention_deliveries WHERE item_id = ? AND device_id = ? AND channel = ? AND attempt = ?`,
		itemID, deviceID, channel, attempt).Scan(&body)
	if err != nil {
		return domain.AttentionDelivery{}, fmt.Errorf("get attention delivery %q/%q/%q/%d: %w", itemID, deviceID, channel, attempt, notFoundOr(err))
	}
	delivery, err := decode[domain.AttentionDelivery](body)
	if err != nil {
		return domain.AttentionDelivery{}, fmt.Errorf("get attention delivery %q/%q/%q/%d: %w", itemID, deviceID, channel, attempt, err)
	}
	if delivery.ItemID != itemID || delivery.DeviceID != deviceID ||
		delivery.Channel != channel || delivery.Attempt != attempt {
		return domain.AttentionDelivery{}, fmt.Errorf("get attention delivery %q/%q/%q/%d: %w", itemID, deviceID, channel, attempt, errRowInconsistent)
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
	body, err := encode(policy)
	if err != nil {
		return fmt.Errorf("put resolved policy %q: %w", policy.RunID, err)
	}
	// The run binds its resolved policy by digest (§5.3): a policy whose
	// digest disagrees with its run's policy_digest column is rejected. A
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

// notFoundOr maps sql.ErrNoRows to ErrNotFound and passes every other error
// through.
func notFoundOr(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
