package signet

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// ErrInvalidSyncSnapshot marks state that cannot be served as a canonical
// client snapshot. Store reconstruction checks the lower bound that is valid
// in both reads and writes; signet adds the pure-read upper bound against the
// ServerState read in the same transaction.
var ErrInvalidSyncSnapshot = errors.New("invalid sync snapshot")

var errNoProposalSnoozeRelease = errors.New("no proposal snooze release pending")

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

// RunProposalFactsSnapshot is the authenticated, bounded review projection
// for one run-proposal card. The opaque subject handle and policy identities
// remain server-side; this tuple proves the facts match the rendered item and
// its digest-bound proposal revision.
type RunProposalFactsSnapshot struct {
	AsOfRevision      int64                     `json:"as_of_revision"`
	EntityVersion     int64                     `json:"entity_version"`
	ItemVersion       int                       `json:"item_version"`
	ProposalDigest    domain.Digest             `json:"proposal_digest"`
	Supersedes        *RunProposalRevisionFacts `json:"supersedes"`
	Intent            domain.RunProposalIntent  `json:"intent"`
	ExpectedCostUnits int                       `json:"expected_cost_units"`
	Scope             domain.RunProposalScope   `json:"scope"`
}

// RunProposalRevisionFacts is one bounded side of a revision comparison.
// It contains no opaque handle or policy authority.
type RunProposalRevisionFacts struct {
	ProposalDigest    domain.Digest            `json:"proposal_digest"`
	Intent            domain.RunProposalIntent `json:"intent"`
	ExpectedCostUnits int                      `json:"expected_cost_units"`
	Scope             domain.RunProposalScope  `json:"scope"`
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

// ScheduleSnapshot is a §5.16 Schedule with its store-stamped sync metadata,
// matching api/openapi.yaml. The synced aggregate carries the binding,
// cadence or deadline, and terminal resolution; the daemon-internal tick
// bookkeeping (timers, occurrences) deliberately never rides sync
// (migration 0025).
type ScheduleSnapshot struct {
	AsOfRevision  int64           `json:"as_of_revision"`
	EntityVersion int64           `json:"entity_version"`
	Schedule      domain.Schedule `json:"schedule"`
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
	Schedules           []ScheduleSnapshot          `json:"schedules"`
}

// Bootstrap returns the one response that advances a client's
// last_full_snapshot_revision. All other reads below are partial resource
// fetches and deliberately carry no whole-cache revision cursor.
func (s *Service) Bootstrap(ctx context.Context) (BootstrapSnapshot, error) {
	now := s.now().UTC()
	if err := s.convergeProposalSnoozes(ctx, now); err != nil {
		return BootstrapSnapshot{}, fmt.Errorf("bootstrap proposal snoozes: %w", err)
	}
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
		schedules, err := tx.ListSchedules(ctx)
		if err != nil {
			return err
		}

		out = BootstrapSnapshot{
			SyncEpoch: state.SyncEpoch, Revision: state.Revision,
			AttentionItems:      make([]AttentionItemSnapshot, 0, len(items)),
			AttentionDeliveries: make([]AttentionDeliverySnapshot, 0, len(deliveries)),
			Runs:                make([]RunSnapshot, 0, len(runs)),
			Conversations:       make([]ConversationSnapshot, 0, len(conversations)),
			Schedules:           make([]ScheduleSnapshot, 0, len(schedules)),
		}
		snoozedItems := make(map[domain.ItemID]bool)
		for _, item := range items {
			if err := validateSnapshot(state, item.Snapshot); err != nil {
				return fmt.Errorf("attention item %q: %w", item.Value.ID, err)
			}
			snoozed, err := proposalSnoozed(ctx, tx, item.Value, now)
			if err != nil {
				return fmt.Errorf("attention item %q snooze: %w", item.Value.ID, err)
			}
			if snoozed {
				snoozedItems[item.Value.ID] = true
				continue
			}
			out.AttentionItems = append(out.AttentionItems, itemSnapshot(item.Value, item.Snapshot))
		}
		for _, delivery := range deliveries {
			if snoozedItems[delivery.Value.ItemID] {
				continue
			}
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
		for _, schedule := range schedules {
			if err := validateSnapshot(state, schedule.Snapshot); err != nil {
				return fmt.Errorf("schedule %q: %w", schedule.Value.ID, err)
			}
			out.Schedules = append(out.Schedules,
				scheduleSnapshot(schedule.Value, schedule.Snapshot))
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
	if err := s.convergeProposalSnoozes(ctx, s.now().UTC()); err != nil {
		return ServerRevision{}, fmt.Errorf("sync revision proposal snoozes: %w", err)
	}
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
	now := s.now().UTC()
	if err := s.convergeProposalSnoozes(ctx, now); err != nil {
		return nil, fmt.Errorf("list attention items proposal snoozes: %w", err)
	}
	var out []AttentionItemSnapshot
	err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		state, err := tx.ServerState(ctx)
		if err != nil {
			return err
		}
		values, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		out = make([]AttentionItemSnapshot, 0, len(values))
		for _, value := range values {
			if err := validateSnapshot(state, value.Snapshot); err != nil {
				return fmt.Errorf("item %q: %w", value.Value.ID, err)
			}
			snoozed, err := proposalSnoozed(ctx, tx, value.Value, now)
			if err != nil {
				return fmt.Errorf("item %q snooze: %w", value.Value.ID, err)
			}
			if !snoozed {
				out = append(out, itemSnapshot(value.Value, value.Snapshot))
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list attention items: %w", err)
	}
	return out, nil
}

// GetAttentionItem returns one current canonical item snapshot.
func (s *Service) GetAttentionItem(ctx context.Context, id domain.ItemID) (AttentionItemSnapshot, error) {
	now := s.now().UTC()
	if err := s.convergeProposalSnoozes(ctx, now); err != nil {
		return AttentionItemSnapshot{}, fmt.Errorf("get attention item %q proposal snoozes: %w", id, err)
	}
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
		snoozed, err := proposalSnoozed(ctx, tx, item, now)
		if err != nil {
			return err
		}
		if snoozed {
			return ErrProposalSnoozed
		}
		out = itemSnapshot(item, snapshot)
		return nil
	})
	if err != nil {
		return AttentionItemSnapshot{}, fmt.Errorf("get attention item %q: %w", id, err)
	}
	return out, nil
}

// GetRunProposalFacts returns only store-authenticated proposal facts whose
// item/entity/digest tuple can be matched to the decision card. It follows the
// same active-snooze visibility rule as the item reads.
func (s *Service) GetRunProposalFacts(ctx context.Context, id domain.ItemID) (RunProposalFactsSnapshot, error) {
	now := s.now().UTC()
	if err := s.convergeProposalSnoozes(ctx, now); err != nil {
		return RunProposalFactsSnapshot{}, fmt.Errorf("get run proposal facts %q snoozes: %w", id, err)
	}
	var out RunProposalFactsSnapshot
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
		if item.Type != domain.AttentionRunProposal {
			return store.ErrNotFound
		}
		snoozed, err := proposalSnoozed(ctx, tx, item, now)
		if err != nil {
			return err
		}
		if snoozed {
			return ErrProposalSnoozed
		}
		_, proposal, superseded, err := tx.ProposalForItemWithRevisionContext(ctx, id)
		if err != nil {
			return err
		}
		if proposal.RunProposal == nil || len(item.ArtifactDigests) != 1 ||
			item.ArtifactDigests[0] != proposal.Digest {
			return ErrInvalidSyncSnapshot
		}
		var supersedes *RunProposalRevisionFacts
		if superseded != nil {
			supersedes = &RunProposalRevisionFacts{
				ProposalDigest: superseded.Digest, Intent: superseded.RunProposal.Intent,
				ExpectedCostUnits: superseded.RunProposal.ExpectedCostUnits,
				Scope:             superseded.RunProposal.Scope,
			}
		}
		out = RunProposalFactsSnapshot{
			AsOfRevision: snapshot.AsOfRevision, EntityVersion: snapshot.EntityVersion,
			ItemVersion: item.ItemVersion, ProposalDigest: proposal.Digest,
			Supersedes: supersedes, Intent: proposal.RunProposal.Intent,
			ExpectedCostUnits: proposal.RunProposal.ExpectedCostUnits,
			Scope:             proposal.RunProposal.Scope,
		}
		return nil
	})
	if err != nil {
		return RunProposalFactsSnapshot{}, fmt.Errorf("get run proposal facts %q: %w", id, err)
	}
	return out, nil
}

func (s *Service) convergeProposalSnoozes(ctx context.Context, now time.Time) error {
	var pending bool
	if err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		pending, err = tx.ProposalSnoozeReleasePending(ctx, now)
		return err
	}); err != nil || !pending {
		return err
	}
	err := s.store.Write(ctx, func(tx *store.WriteTx) error {
		released, err := tx.ReleaseExpiredProposalSnoozes(ctx, now)
		if err != nil {
			return err
		}
		if !released {
			return errNoProposalSnoozeRelease
		}
		return nil
	})
	if errors.Is(err, errNoProposalSnoozeRelease) {
		return nil
	}
	return err
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

// ListSchedules returns the current §5.16 schedule aggregates as partial
// resource snapshots.
func (s *Service) ListSchedules(ctx context.Context) ([]ScheduleSnapshot, error) {
	state, values, err := readSnapshots(ctx, s.store, (*store.ReadTx).ListSchedules)
	if err != nil {
		return nil, fmt.Errorf("list schedules: %w", err)
	}
	out := make([]ScheduleSnapshot, 0, len(values))
	for _, value := range values {
		if err := validateSnapshot(state, value.Snapshot); err != nil {
			return nil, fmt.Errorf("list schedules: schedule %q: %w", value.Value.ID, err)
		}
		out = append(out, scheduleSnapshot(value.Value, value.Snapshot))
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

func scheduleSnapshot(schedule domain.Schedule, snapshot store.Snapshot) ScheduleSnapshot {
	return ScheduleSnapshot{
		AsOfRevision: snapshot.AsOfRevision, EntityVersion: snapshot.EntityVersion, Schedule: schedule,
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
