package signet

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ErrInvalidSyncSnapshot marks state that cannot be served as a canonical
// client snapshot. Store reconstruction checks the lower bound that is valid
// in both reads and writes; signet adds the pure-read upper bound against the
// ServerState read in the same transaction.
var ErrInvalidSyncSnapshot = errors.New("invalid sync snapshot")

// ServerRevision is the revision heartbeat payload. A changed SyncEpoch
// invalidates every client cache; a higher Revision tells a client that it
// missed one or more invalidations and must refetch or bootstrap.
type ServerRevision struct {
	SyncEpoch string `json:"sync_epoch"`
	Revision  int64  `json:"revision"`
}

// AttentionItemSnapshot is an AttentionItem with its store-stamped sync
// metadata, matching api/openapi.yaml.
type AttentionItemSnapshot struct {
	AsOfRevision  int64                `json:"as_of_revision"`
	EntityVersion int64                `json:"entity_version"`
	Item          domain.AttentionItem `json:"item"`
}

// AttentionDeliverySnapshot is an AttentionDelivery with its store-stamped
// sync metadata, matching api/openapi.yaml.
type AttentionDeliverySnapshot struct {
	AsOfRevision  int64                    `json:"as_of_revision"`
	EntityVersion int64                    `json:"entity_version"`
	Delivery      domain.AttentionDelivery `json:"delivery"`
}

// RunSnapshot is a Run with its store-stamped sync metadata, matching
// api/openapi.yaml.
type RunSnapshot struct {
	AsOfRevision  int64      `json:"as_of_revision"`
	EntityVersion int64      `json:"entity_version"`
	Run           domain.Run `json:"run"`
}

// ConversationSnapshot is a whole Conversation with its store-stamped sync
// metadata, matching the Phase 1 whole-snapshot contract.
type ConversationSnapshot struct {
	AsOfRevision  int64               `json:"as_of_revision"`
	EntityVersion int64               `json:"entity_version"`
	Conversation  domain.Conversation `json:"conversation"`
}

// BootstrapSnapshot is one canonical view of all synchronized resources.
// Service.Bootstrap constructs every field inside one Store.Read callback, so
// Revision is the upper bound for every resource's AsOfRevision and no write
// can tear the collections apart.
type BootstrapSnapshot struct {
	SyncEpoch           string                      `json:"sync_epoch"`
	Revision            int64                       `json:"revision"`
	AttentionItems      []AttentionItemSnapshot     `json:"attention_items"`
	AttentionDeliveries []AttentionDeliverySnapshot `json:"attention_deliveries"`
	Runs                []RunSnapshot               `json:"runs"`
	Conversations       []ConversationSnapshot      `json:"conversations"`
}

// Bootstrap returns the one response that advances a client's
// last_full_snapshot_revision. All other reads below are partial resource
// fetches and deliberately carry no whole-cache revision cursor.
func (s *Service) Bootstrap(ctx context.Context) (BootstrapSnapshot, error) {
	var out BootstrapSnapshot
	err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		state, err := tx.ServerState(ctx)
		if err != nil {
			return err
		}
		if err := validateServerState(state); err != nil {
			return err
		}
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		deliveries, err := tx.ListAttentionDeliveries(ctx)
		if err != nil {
			return err
		}
		runs, err := tx.ListRuns(ctx)
		if err != nil {
			return err
		}
		conversations, err := tx.ListConversations(ctx)
		if err != nil {
			return err
		}

		out = BootstrapSnapshot{
			SyncEpoch: state.SyncEpoch, Revision: state.Revision,
			AttentionItems:      make([]AttentionItemSnapshot, 0, len(items)),
			AttentionDeliveries: make([]AttentionDeliverySnapshot, 0, len(deliveries)),
			Runs:                make([]RunSnapshot, 0, len(runs)),
			Conversations:       make([]ConversationSnapshot, 0, len(conversations)),
		}
		for _, item := range items {
			if err := validateSnapshot(state, item.Snapshot); err != nil {
				return fmt.Errorf("attention item %q: %w", item.Value.ID, err)
			}
			out.AttentionItems = append(out.AttentionItems, itemSnapshot(item.Value, item.Snapshot))
		}
		for _, delivery := range deliveries {
			if err := validateSnapshot(state, delivery.Snapshot); err != nil {
				return fmt.Errorf("attention delivery %q/%q/%s/%d: %w",
					delivery.Value.ItemID, delivery.Value.DeviceID, delivery.Value.Channel, delivery.Value.Attempt, err)
			}
			out.AttentionDeliveries = append(out.AttentionDeliveries,
				deliverySnapshot(delivery.Value, delivery.Snapshot))
		}
		for _, run := range runs {
			if err := validateSnapshot(state, run.Snapshot); err != nil {
				return fmt.Errorf("run %q: %w", run.Value.ID, err)
			}
			out.Runs = append(out.Runs, runSnapshot(run.Value, run.Snapshot))
		}
		for _, conversation := range conversations {
			if err := validateSnapshot(state, conversation.Snapshot); err != nil {
				return fmt.Errorf("conversation %q: %w", conversation.Value.ID, err)
			}
			out.Conversations = append(out.Conversations,
				conversationSnapshot(conversation.Value, conversation.Snapshot))
		}
		return nil
	})
	if err != nil {
		return BootstrapSnapshot{}, fmt.Errorf("bootstrap sync: %w", err)
	}
	return out, nil
}

// Revision returns the cheap periodic heartbeat. Only Bootstrap advances the
// client's full-snapshot cursor; this value exists to reveal a revision gap.
func (s *Service) Revision(ctx context.Context) (ServerRevision, error) {
	state, err := s.store.ServerState(ctx)
	if err != nil {
		return ServerRevision{}, fmt.Errorf("sync revision: %w", err)
	}
	if err := validateServerState(state); err != nil {
		return ServerRevision{}, fmt.Errorf("sync revision: %w", err)
	}
	return ServerRevision{SyncEpoch: state.SyncEpoch, Revision: state.Revision}, nil
}

// ListAttentionItems returns a partial resource fetch. Its snapshots are
// validated against one same-transaction ServerState, but that state is not
// included in the result and therefore cannot be mistaken for a full-cache
// cursor.
func (s *Service) ListAttentionItems(ctx context.Context) ([]AttentionItemSnapshot, error) {
	state, values, err := readSnapshots(ctx, s.store, (*store.ReadTx).ListAttentionItems)
	if err != nil {
		return nil, fmt.Errorf("list attention items: %w", err)
	}
	out := make([]AttentionItemSnapshot, 0, len(values))
	for _, value := range values {
		if err := validateSnapshot(state, value.Snapshot); err != nil {
			return nil, fmt.Errorf("list attention items: item %q: %w", value.Value.ID, err)
		}
		out = append(out, itemSnapshot(value.Value, value.Snapshot))
	}
	return out, nil
}

// GetAttentionItem returns one current canonical item snapshot.
func (s *Service) GetAttentionItem(ctx context.Context, id domain.ItemID) (AttentionItemSnapshot, error) {
	var out AttentionItemSnapshot
	err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		state, err := tx.ServerState(ctx)
		if err != nil {
			return err
		}
		item, snapshot, err := tx.GetAttentionItemSnapshot(ctx, id)
		if err != nil {
			return err
		}
		if err := validateSnapshot(state, snapshot); err != nil {
			return err
		}
		out = itemSnapshot(item, snapshot)
		return nil
	})
	if err != nil {
		return AttentionItemSnapshot{}, fmt.Errorf("get attention item %q: %w", id, err)
	}
	return out, nil
}

// ListAttentionItemDeliveries returns every delivery attempt for one item in
// the store's deterministic composite-key order. A missing parent is a
// not-found result rather than an indistinguishable empty delivery history.
func (s *Service) ListAttentionItemDeliveries(ctx context.Context, id domain.ItemID) ([]AttentionDeliverySnapshot, error) {
	var out []AttentionDeliverySnapshot
	err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		state, err := tx.ServerState(ctx)
		if err != nil {
			return err
		}
		_, itemState, err := tx.GetAttentionItemSnapshot(ctx, id)
		if err != nil {
			return err
		}
		if err := validateSnapshot(state, itemState); err != nil {
			return err
		}
		values, err := tx.ListAttentionDeliveries(ctx)
		if err != nil {
			return err
		}
		out = make([]AttentionDeliverySnapshot, 0)
		for _, value := range values {
			if value.Value.ItemID != id {
				continue
			}
			if err := validateSnapshot(state, value.Snapshot); err != nil {
				return err
			}
			out = append(out, deliverySnapshot(value.Value, value.Snapshot))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list attention item %q deliveries: %w", id, err)
	}
	return out, nil
}

// ListRuns returns the current run aggregates as partial resource snapshots.
func (s *Service) ListRuns(ctx context.Context) ([]RunSnapshot, error) {
	state, values, err := readSnapshots(ctx, s.store, (*store.ReadTx).ListRuns)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	out := make([]RunSnapshot, 0, len(values))
	for _, value := range values {
		if err := validateSnapshot(state, value.Snapshot); err != nil {
			return nil, fmt.Errorf("list runs: run %q: %w", value.Value.ID, err)
		}
		out = append(out, runSnapshot(value.Value, value.Snapshot))
	}
	return out, nil
}

// GetRun returns one current run aggregate snapshot.
func (s *Service) GetRun(ctx context.Context, id domain.RunID) (RunSnapshot, error) {
	state, values, err := readSnapshots(ctx, s.store, (*store.ReadTx).ListRuns)
	if err != nil {
		return RunSnapshot{}, fmt.Errorf("get run %q: %w", id, err)
	}
	for _, value := range values {
		if value.Value.ID != id {
			continue
		}
		if err := validateSnapshot(state, value.Snapshot); err != nil {
			return RunSnapshot{}, fmt.Errorf("get run %q: %w", id, err)
		}
		return runSnapshot(value.Value, value.Snapshot), nil
	}
	return RunSnapshot{}, fmt.Errorf("get run %q: %w", id, store.ErrNotFound)
}

// GetConversation returns one whole Phase 1 conversation snapshot.
func (s *Service) GetConversation(ctx context.Context, id domain.ConversationID) (ConversationSnapshot, error) {
	state, values, err := readSnapshots(ctx, s.store, (*store.ReadTx).ListConversations)
	if err != nil {
		return ConversationSnapshot{}, fmt.Errorf("get conversation %q: %w", id, err)
	}
	for _, value := range values {
		if value.Value.ID != id {
			continue
		}
		if err := validateSnapshot(state, value.Snapshot); err != nil {
			return ConversationSnapshot{}, fmt.Errorf("get conversation %q: %w", id, err)
		}
		return conversationSnapshot(value.Value, value.Snapshot), nil
	}
	return ConversationSnapshot{}, fmt.Errorf("get conversation %q: %w", id, store.ErrNotFound)
}

func readSnapshots[T any](ctx context.Context, st *store.Store, list func(*store.ReadTx, context.Context) ([]store.Snapshotted[T], error)) (store.ServerState, []store.Snapshotted[T], error) {
	var (
		state  store.ServerState
		values []store.Snapshotted[T]
	)
	err := st.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		state, err = tx.ServerState(ctx)
		if err != nil {
			return err
		}
		if err := validateServerState(state); err != nil {
			return err
		}
		values, err = list(tx, ctx)
		return err
	})
	return state, values, err
}

func validateServerState(state store.ServerState) error {
	if state.SyncEpoch == "" || state.Revision < 0 {
		return fmt.Errorf("server state epoch %q revision %d: %w",
			state.SyncEpoch, state.Revision, ErrInvalidSyncSnapshot)
	}
	return nil
}

func validateSnapshot(state store.ServerState, snapshot store.Snapshot) error {
	if err := validateServerState(state); err != nil {
		return err
	}
	if snapshot.EntityVersion < 1 || snapshot.AsOfRevision < 1 || snapshot.AsOfRevision > state.Revision {
		return fmt.Errorf("entity_version %d as_of_revision %d exceeds server revision %d: %w",
			snapshot.EntityVersion, snapshot.AsOfRevision, state.Revision, ErrInvalidSyncSnapshot)
	}
	return nil
}

func itemSnapshot(item domain.AttentionItem, snapshot store.Snapshot) AttentionItemSnapshot {
	return AttentionItemSnapshot{
		AsOfRevision: snapshot.AsOfRevision, EntityVersion: snapshot.EntityVersion, Item: normalizeAttentionItem(item),
	}
}

func deliverySnapshot(delivery domain.AttentionDelivery, snapshot store.Snapshot) AttentionDeliverySnapshot {
	return AttentionDeliverySnapshot{
		AsOfRevision: snapshot.AsOfRevision, EntityVersion: snapshot.EntityVersion, Delivery: delivery,
	}
}

func runSnapshot(run domain.Run, snapshot store.Snapshot) RunSnapshot {
	return RunSnapshot{
		AsOfRevision: snapshot.AsOfRevision, EntityVersion: snapshot.EntityVersion, Run: normalizeRun(run),
	}
}

func conversationSnapshot(conversation domain.Conversation, snapshot store.Snapshot) ConversationSnapshot {
	return ConversationSnapshot{
		AsOfRevision: snapshot.AsOfRevision, EntityVersion: snapshot.EntityVersion, Conversation: normalizeConversation(conversation),
	}
}

// The OpenAPI domain mirrors make every slice a required, non-null array.
// Domain validation permits nil for empty optional collections, so the wire
// projection replaces those nils without mutating the store-returned value.
func normalizeAttentionItem(item domain.AttentionItem) domain.AttentionItem {
	item.RequestedDecision = nonNilSlice(item.RequestedDecision)
	item.EvidenceSnapshot = nonNilSlice(item.EvidenceSnapshot)
	item.AgentClaims = nonNilSlice(item.AgentClaims)
	item.ArtifactDigests = nonNilSlice(item.ArtifactDigests)
	return item
}

func normalizeRun(run domain.Run) domain.Run {
	run.Stages = nonNilSlice(run.Stages)
	if len(run.Stages) == 0 {
		return run
	}
	run.Stages = slices.Clone(run.Stages)
	for idx := range run.Stages {
		run.Stages[idx].Attempts = nonNilSlice(run.Stages[idx].Attempts)
	}
	return run
}

func normalizeConversation(conversation domain.Conversation) domain.Conversation {
	conversation.Messages = nonNilSlice(conversation.Messages)
	if len(conversation.Messages) == 0 {
		return conversation
	}
	conversation.Messages = slices.Clone(conversation.Messages)
	for idx := range conversation.Messages {
		conversation.Messages[idx].Attachments = nonNilSlice(conversation.Messages[idx].Attachments)
	}
	return conversation
}

func normalizeCommandResult(result CommandResult) CommandResult {
	result.Record.ArtifactDigests = nonNilSlice(result.Record.ArtifactDigests)
	return result
}

func nonNilSlice[T any](values []T) []T {
	if values == nil {
		return []T{}
	}
	return values
}
