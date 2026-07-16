package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

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

// ErrStaleCommand is returned when a new command's pinned bindings no longer
// describe the live attention item: the item advanced (its version, PR head, or
// rendered digest set changed) after the command was prepared, so the
// submission is stale (§5.14 test 2). It is carried by a *StaleCommandError,
// which also holds the current item as the canonical replacement state; match
// the class with errors.Is and extract the replacement with errors.As.
var ErrStaleCommand = errors.New("command bindings no longer match the item")

// ErrActionNotOffered is returned when a command's action is a valid enum value
// but not one the live item offered in its requested_decision (plan §4: the
// offered set is item-specific, "approve" is not universal). Rejecting it keeps
// the durable record faithful to the choices rendered to the user: a client
// cannot record an action the item never presented.
var ErrActionNotOffered = errors.New("command action is not offered by the item")

// StaleCommandError reports a stale command submission and carries the current
// attention item as the replacement the caller must re-render and re-decide
// against (plan §4 lifecycle, §5.14 test 2). Idempotent replays are handled
// before this check, so a StaleCommandError only ever names a genuinely new
// command_id whose bound inputs drifted, never a retry of a committed one.
type StaleCommandError struct {
	CommandID   string
	Replacement domain.AttentionItem
}

func (e *StaleCommandError) Error() string {
	return fmt.Sprintf("command %q is stale: item %q is at version %d",
		e.CommandID, e.Replacement.ID, e.Replacement.ItemVersion)
}

// Is lets errors.Is(err, ErrStaleCommand) match the class while errors.As
// recovers the replacement item.
func (e *StaleCommandError) Is(target error) bool { return target == ErrStaleCommand }

// ErrClosedItem is returned when a genuinely new command targets an attention
// item whose status is no longer open (issue #55). Unlike ErrStaleCommand it
// does not depend on the command's bound version: the item's lifecycle has
// concluded, so no rebind-and-retry can ever succeed. It is carried by a
// *ClosedItemError; match the class with errors.Is and extract the canonical
// item with errors.As.
var ErrClosedItem = errors.New("item is no longer open for decisions")

// ClosedItemError reports a new command against a non-open item and carries
// the current attention item as the canonical state the caller should render
// (plan §4 lifecycle). Idempotent replays are handled before this check, so a
// ClosedItemError only ever names a genuinely new command_id, never a retry of
// a committed one (§5.14 test 4).
type ClosedItemError struct {
	CommandID string
	Item      domain.AttentionItem
}

func (e *ClosedItemError) Error() string {
	return fmt.Sprintf("command %q rejected: item %q is %s at version %d",
		e.CommandID, e.Item.ID, e.Item.Status, e.Item.ItemVersion)
}

// Is lets errors.Is(err, ErrClosedItem) match the class while errors.As
// recovers the canonical item.
func (e *ClosedItemError) Is(target error) bool { return target == ErrClosedItem }

// validator is implemented by every persisted domain type. Puts validate
// before writing and Gets validate after reading, so a corrupt row fails
// loudly at the boundary instead of leaking an invalid value into the daemon.
type validator interface{ Validate() error }

// mapTransition translates a domain transition-validator failure into the
// store's own boundary error. The domain validators own the transition rules
// (one definition every writer reuses); the store owns how a rejection surfaces
// at its edge. Double-wrapping keeps the store sentinel matchable by errors.Is
// while preserving the domain detail in the chain, so callers keep matching
// ErrImmutableConflict / ErrStaleWrite unchanged.
func mapTransition(err error) error {
	switch {
	case errors.Is(err, domain.ErrImmutableTransition):
		return fmt.Errorf("%w: %w", ErrImmutableConflict, err)
	case errors.Is(err, domain.ErrStaleTransition):
		return fmt.Errorf("%w: %w", ErrStaleWrite, err)
	default:
		return err
	}
}

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

// scanner is the shared surface of *sql.Row and *sql.Rows: one reconstruction
// function per entity (scan, decode, cross-check the extracted columns,
// range-check the store-stamped metadata, re-run the policy gate) serves both
// the single-entity Get and the collection List, so a gate added to one path
// cannot be missed on the other.
type scanner interface{ Scan(dest ...any) error }

// putImmutable inserts a write-once row (INSERT ... ON CONFLICT DO NOTHING),
// tolerating only a byte-identical replay of an existing key: canonical
// json.Marshal is deterministic, so a retried Put of the same value converges
// on the original row (no entity_version churn, nothing new for sync to
// observe), while a same-key write with different content fails with
// ErrImmutableConflict. On InternalTx so the non-synchronized write-once
// records (pairing codes) share it; the synchronized callers all hold a
// WriteTx, whose statements stamp its as_of_revision.
func (tx *InternalTx) putImmutable(ctx context.Context, insertSQL string, insertArgs []any, selectBodySQL string, keyArgs []any, body string) error {
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
// columns alongside the body and cross-checks them; the synchronized
// aggregates funnel that reconstruction through one scan function per entity
// (see scanner), which their collection Lists reuse.

// existingBody fetches the current body for an aggregate's key, or nil when
// the row does not exist. The query must be a constant from this file or
// pairing.go. On InternalTx for the same reason as putImmutable.
func (tx *InternalTx) existingBody(ctx context.Context, selectSQL string, keyArgs ...any) ([]byte, error) {
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

const putRunSQL = `
INSERT INTO runs (id, project_id, policy_digest, entity_version, as_of_revision, body)
VALUES (?, ?, ?, 1, ?, ?)
ON CONFLICT (id) DO UPDATE SET
    project_id     = excluded.project_id,
    policy_digest  = excluded.policy_digest,
    entity_version = runs.entity_version + 1,
    as_of_revision = excluded.as_of_revision,
    body           = excluded.body`

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
		if err := domain.ValidateRunTransition(old, run); err != nil {
			return fmt.Errorf("put run %q: %w", run.ID, mapTransition(err))
		}
	}
	if _, err := tx.tx.ExecContext(ctx, putRunSQL, run.ID, run.ProjectID, run.PolicyDigest, tx.asOfRevision, body); err != nil {
		return fmt.Errorf("put run %q: %w", run.ID, err)
	}
	return nil
}

// scanRunSnapshot reconstructs one runs row (see the scanner doc for the
// shared gate sequence). Errors are returned unwrapped; callers add the
// entity/key context.
func (tx *ReadTx) scanRunSnapshot(sc scanner) (domain.Run, Snapshot, error) {
	var (
		id           string
		projectID    string
		policyDigest string
		snap         Snapshot
		body         []byte
	)
	if err := sc.Scan(&id, &projectID, &policyDigest, &snap.EntityVersion, &snap.AsOfRevision, &body); err != nil {
		return domain.Run{}, Snapshot{}, err
	}
	run, err := decode[domain.Run](body)
	if err != nil {
		return domain.Run{}, Snapshot{}, err
	}
	if run.ID != domain.RunID(id) || run.ProjectID != domain.ProjectID(projectID) ||
		run.PolicyDigest != domain.Digest(policyDigest) ||
		snap.EntityVersion < 1 || snap.AsOfRevision < 1 {
		return domain.Run{}, Snapshot{}, errRowInconsistent
	}
	return run, snap, nil
}

func (tx *ReadTx) GetRun(ctx context.Context, id domain.RunID) (domain.Run, error) {
	run, _, err := tx.scanRunSnapshot(tx.tx.QueryRowContext(ctx,
		`SELECT id, project_id, policy_digest, entity_version, as_of_revision, body FROM runs WHERE id = ?`, id))
	if err != nil {
		return domain.Run{}, fmt.Errorf("get run %q: %w", id, notFoundOr(err))
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
	// Gate the embedded evidence against policy before persisting: encode's
	// Validate enforces only the producer-class half of the evidence rule, so a
	// caller bypassing NewAttentionItem could otherwise persist an evidence
	// artifact under an unapproved recipe (plan §5.15 rule 2). Runs before the
	// write, so an idempotent replay is gated too.
	if err := tx.gateEvidence(item); err != nil {
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
		if err := domain.ValidateAttentionItemTransition(old, item); err != nil {
			return fmt.Errorf("put attention item %q: %w", item.ID, mapTransition(err))
		}
	}
	if _, err := tx.tx.ExecContext(ctx, putAttentionItemSQL,
		item.ID, item.ProjectID, item.ConversationID, tx.asOfRevision, body); err != nil {
		return fmt.Errorf("put attention item %q: %w", item.ID, err)
	}
	return nil
}

func (tx *ReadTx) GetAttentionItem(ctx context.Context, id domain.ItemID) (domain.AttentionItem, error) {
	item, _, err := tx.GetAttentionItemSnapshot(ctx, id)
	return item, err
}

// Snapshot is the persisted §5.14 sync metadata read alongside a row: the
// per-row EntityVersion a ClientCommand's expected_entity_version is checked
// against, and the AsOfRevision of the transaction that last wrote the row.
// Both are stamped by the store's own Puts, never by callers, and are
// range-checked at reconstruction like every other extracted column.
type Snapshot struct {
	EntityVersion int64
	AsOfRevision  int64
}

// GetAttentionItemSnapshot returns the item together with its persisted sync
// metadata, for the command-acceptance boundary (#91): the binding fields
// inside the body cannot distinguish a stale expected_entity_version when the
// domain content matches, so acceptance needs the store's own version counter.
func (tx *ReadTx) GetAttentionItemSnapshot(ctx context.Context, id domain.ItemID) (domain.AttentionItem, Snapshot, error) {
	item, snap, err := tx.scanAttentionItemSnapshot(tx.tx.QueryRowContext(ctx,
		`SELECT id, project_id, conversation_id, entity_version, as_of_revision, body FROM attention_items WHERE id = ?`, id))
	if err != nil {
		return domain.AttentionItem{}, Snapshot{}, fmt.Errorf("get attention item %q: %w", id, notFoundOr(err))
	}
	return item, snap, nil
}

// scanAttentionItemSnapshot reconstructs one attention_items row (see the
// scanner doc for the shared gate sequence), including the evidence policy
// re-gate.
func (tx *ReadTx) scanAttentionItemSnapshot(sc scanner) (domain.AttentionItem, Snapshot, error) {
	var (
		id             string
		projectID      string
		conversationID sql.NullString
		snap           Snapshot
		body           []byte
	)
	if err := sc.Scan(&id, &projectID, &conversationID, &snap.EntityVersion, &snap.AsOfRevision, &body); err != nil {
		return domain.AttentionItem{}, Snapshot{}, err
	}
	item, err := decode[domain.AttentionItem](body)
	if err != nil {
		return domain.AttentionItem{}, Snapshot{}, err
	}
	consistent := item.ID == domain.ItemID(id) && item.ProjectID == domain.ProjectID(projectID)
	if conversationID.Valid {
		consistent = consistent && item.ConversationID != nil &&
			*item.ConversationID == domain.ConversationID(conversationID.String)
	} else {
		consistent = consistent && item.ConversationID == nil
	}
	// The metadata is store-stamped, so anything outside the values the Puts
	// can produce (versions start at 1, revisions are client-visible and
	// positive) is a forged or corrupt row, refused like a diverging column.
	consistent = consistent && snap.EntityVersion >= 1 && snap.AsOfRevision >= 1
	if !consistent {
		return domain.AttentionItem{}, Snapshot{}, errRowInconsistent
	}
	// Reconstruction re-runs the evidence gate: decode's Validate cannot check
	// recipe approval, so an item carrying evidence under a now-unapproved (or
	// forged) recipe fails closed rather than reconstructing as valid.
	if err := tx.gateEvidence(item); err != nil {
		return domain.AttentionItem{}, Snapshot{}, err
	}
	return item, snap, nil
}

// gateEvidence re-runs the approved-recipe evidence gate over an item's
// snapshot at the persistence/reconstruction boundary, using the transaction's
// policy set. It is the store's enforcement of the recipe-approval half of the
// evidence rule that AttentionItem.Validate cannot check (it holds no policy).
func (tx *ReadTx) gateEvidence(item domain.AttentionItem) error {
	for _, a := range item.EvidenceSnapshot {
		if err := domain.EligibleForEvidenceSnapshot(a, tx.approvedRecipes); err != nil {
			return err
		}
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
INSERT INTO commands (command_id, item_id, item_version, pr_head_sha, device_id, action, entity_version, as_of_revision, body)
VALUES (?, ?, ?, ?, ?, ?, 1, ?, ?)`

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
	body, err := encode(command)
	if err != nil {
		return fmt.Errorf("put command %q: %w", command.CommandID, err)
	}
	existing, err := tx.existingBody(ctx, `SELECT body FROM commands WHERE command_id = ?`, command.CommandID)
	if err != nil {
		return fmt.Errorf("put command %q: %w", command.CommandID, err)
	}
	if existing != nil {
		if string(existing) == body {
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
	if _, err := tx.tx.ExecContext(ctx, putCommandSQL,
		command.CommandID, command.ItemID, command.ItemVersion, command.PRHeadSHA,
		command.DeviceID, string(command.Action), tx.asOfRevision, body); err != nil {
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
	var (
		itemID      string
		itemVersion int
		prHeadSHA   string
		deviceID    string
		action      string
		snap        Snapshot
		body        []byte
	)
	err := tx.tx.QueryRowContext(ctx,
		`SELECT item_id, item_version, pr_head_sha, device_id, action, entity_version, as_of_revision, body FROM commands WHERE command_id = ?`, commandID).
		Scan(&itemID, &itemVersion, &prHeadSHA, &deviceID, &action, &snap.EntityVersion, &snap.AsOfRevision, &body)
	if err != nil {
		return domain.Command{}, Snapshot{}, fmt.Errorf("get command %q: %w", commandID, notFoundOr(err))
	}
	command, err := decode[domain.Command](body)
	if err != nil {
		return domain.Command{}, Snapshot{}, fmt.Errorf("get command %q: %w", commandID, err)
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
		snap.EntityVersion != 1 || snap.AsOfRevision < 1 {
		return domain.Command{}, Snapshot{}, fmt.Errorf("get command %q: %w", commandID, errRowInconsistent)
	}
	return command, snap, nil
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

// notFoundOr maps sql.ErrNoRows to ErrNotFound and passes every other error
// through.
func notFoundOr(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
