package signet

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/strictjson"
)

// ErrInvalidSyncSnapshot marks state that cannot be served as a canonical
// client snapshot. Store reconstruction checks the lower bound that is valid
// in both reads and writes; signet adds the pure-read upper bound against the
// ServerState read in the same transaction.
var ErrInvalidSyncSnapshot = errors.New("invalid sync snapshot")

// ErrRunObservationIntegrity marks a run whose observation projection cannot
// bind to its own durable authority (issue #767): a powerless observation row
// or milestone that retargets or lacks the run's attempt, admission, execution,
// dispatch, or publication records. A paced status that merely lags
// authenticated terminal authority is projected from that authority instead.
// The listing reads (Bootstrap, ListRuns) isolate an integrity-failing run by
// excluding it and logging the contradiction, so one damaged legacy run cannot
// fail the whole operator observation surface; the single-run reads (GetRun,
// GetRunTimeline) still surface it as this differentiated failure for the
// specifically-requested run rather than a false-empty. An infrastructure
// failure (a store or db read error) never carries this sentinel and still
// fails the whole read closed.
var ErrRunObservationIntegrity = errors.New("run observation projection integrity failure")

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
	AsOfRevision  int64 `json:"as_of_revision"`
	EntityVersion int64 `json:"entity_version"`
	Run           Run   `json:"run"`
}

// Run is the synchronized run aggregate plus its current daemon-derived
// progress pulse. The summary is presentation, never workflow authority.
type Run struct {
	ID              domain.RunID             `json:"id"`
	ProjectID       domain.ProjectID         `json:"project_id"`
	DisplayNames    *domain.DisplayNames     `json:"display_names"`
	CreatedAt       *time.Time               `json:"created_at"`
	LastActivityAt  *time.Time               `json:"last_activity_at"`
	SpecDigest      domain.Digest            `json:"spec_digest"`
	PolicyDigest    domain.Digest            `json:"policy_digest"`
	CampaignID      *domain.CampaignID       `json:"campaign_id"`
	AttemptNumber   *int                     `json:"attempt_number"`
	AttemptReason   *string                  `json:"attempt_reason"`
	ParentRunID     *domain.RunID            `json:"parent_run_id"`
	Stages          []domain.Stage           `json:"stages"`
	LatestMilestone *domain.RunMilestoneKind `json:"latest_milestone"`
	Outcome         domain.RunOutcome        `json:"outcome"`
	HoldReason      *domain.RunHoldReason    `json:"hold_reason"`
	// Lifecycle splits the runs list into active and finished (#1134); it
	// is derived from Outcome and SupersededBy, never stored.
	Lifecycle domain.RunLifecycle `json:"lifecycle"`
	// SupersededBy names the attempt that retried this run, or null.
	SupersededBy *domain.RunID `json:"superseded_by"`
	// Completion carries the merge facts once the work unit is done, read
	// from the re-gated completion row, or null.
	Completion *WorkUnitCompletionFacts `json:"completion"`
	// BillableCostSoFar is the run's render-only spend figure, computed the
	// same way the attention cards compute theirs, or null before any
	// billable observation.
	BillableCostSoFar *domain.CostSoFar `json:"billable_cost_so_far"`
}

// WorkUnitCompletionFacts is the wire projection of the work unit's
// completion record: the PR whose merge satisfied the criterion, the merge
// commit, the bound issue when the criterion names one, and the instant the
// daemon recorded it. The milestone that ends the timeline carries none of
// these; they are read from the store's re-gated completion row.
type WorkUnitCompletionFacts struct {
	PRNumber       int       `json:"pr_number"`
	MergeCommitSHA string    `json:"merge_commit_sha"`
	BoundIssue     *int      `json:"bound_issue"`
	RecordedAt     time.Time `json:"recorded_at"`
}

// RunTimeline is the typed daemon-observation projection read under one
// server revision. It deliberately carries no free agent-authored text.
type RunTimeline struct {
	AsOfRevision      int64                          `json:"as_of_revision"`
	AsOf              time.Time                      `json:"as_of"`
	RunID             domain.RunID                   `json:"run_id"`
	Milestones        []domain.RunMilestone          `json:"milestones"`
	Hold              *domain.RunHoldObservation     `json:"hold"`
	Invocations       []domain.InvocationObservation `json:"invocations"`
	Completion        *WorkUnitCompletionFacts       `json:"completion"`
	BillableCostSoFar *domain.CostSoFar              `json:"billable_cost_so_far"`
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
	var excluded []excludedRun
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
			out.AttentionItems = append(out.AttentionItems, itemSnapshot(item.Value, item.Snapshot, s.blobs))
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
			snapshot, err := projectRunSnapshot(ctx, tx, state, run.Value, run.Snapshot, items)
			if err != nil {
				if errors.Is(err, ErrRunObservationIntegrity) {
					excluded = append(excluded, excludedRun{
						id: run.Value.ID, projectID: run.Value.ProjectID, err: err,
					})
					continue
				}
				return err
			}
			out.Runs = append(out.Runs, snapshot)
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
	s.logExcludedRuns(excluded)
	s.convergeRunProjectionHealth(ctx, excluded)
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
				out = append(out, itemSnapshot(value.Value, value.Snapshot, s.blobs))
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
		out = itemSnapshot(item, snapshot, s.blobs)
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
	var out []RunSnapshot
	var excluded []excludedRun
	err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		state, err := tx.ServerState(ctx)
		if err != nil {
			return err
		}
		if err := validateServerState(state); err != nil {
			return err
		}
		values, err := tx.ListRuns(ctx)
		if err != nil {
			return err
		}
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		out = make([]RunSnapshot, 0, len(values))
		for _, value := range values {
			if err := validateSnapshot(state, value.Snapshot); err != nil {
				return fmt.Errorf("run %q: %w", value.Value.ID, err)
			}
			snapshot, err := projectRunSnapshot(ctx, tx, state, value.Value, value.Snapshot, items)
			if err != nil {
				if errors.Is(err, ErrRunObservationIntegrity) {
					excluded = append(excluded, excludedRun{
						id: value.Value.ID, projectID: value.Value.ProjectID, err: err,
					})
					continue
				}
				return err
			}
			out = append(out, snapshot)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	s.logExcludedRuns(excluded)
	s.convergeRunProjectionHealth(ctx, excluded)
	return out, nil
}

// GetRun returns one current run aggregate snapshot.
func (s *Service) GetRun(ctx context.Context, id domain.RunID) (RunSnapshot, error) {
	var out RunSnapshot
	err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		state, err := tx.ServerState(ctx)
		if err != nil {
			return err
		}
		if err := validateServerState(state); err != nil {
			return err
		}
		value, err := tx.GetRunSnapshot(ctx, id)
		if err != nil {
			return err
		}
		if err := validateSnapshot(state, value.Snapshot); err != nil {
			return err
		}
		observation, err := tx.ObserveRun(ctx, id)
		if err != nil {
			return err
		}
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		if err := authenticateRunObservation(ctx, tx, state, value.Value, observation, items); err != nil {
			return asRunObservationIntegrityError(err)
		}
		observation = withAuthoritativeInvocationStatuses(observation)
		conclusion, err := AuthenticatedRunConclusion(
			ctx, tx, value.Value, observation, publicationReadyMilestone(observation),
		)
		if err != nil {
			return asRunObservationIntegrityError(err)
		}
		facts, err := runProjectionFactsFor(ctx, tx, id, observation)
		if err != nil {
			return asRunObservationIntegrityError(err)
		}
		displayNames, err := tx.DisplayNamesFor(ctx, value.Value.ProjectID, domain.Subject{
			Type: domain.SubjectRun, ID: domain.SubjectID(value.Value.ID), RunID: &value.Value.ID,
		})
		if err != nil {
			return err
		}
		out = runSnapshot(value.Value, value.Snapshot, observation, conclusion, state.Revision, displayNames, facts)
		return nil
	})
	if err != nil {
		return RunSnapshot{}, fmt.Errorf("get run %q: %w", id, err)
	}
	return out, nil
}

// GetRunTimeline returns one run's observation timeline from a single store
// transaction, after proving the run itself exists under the current trust
// gate. The server revision is the upper bound for every returned fact.
func (s *Service) GetRunTimeline(ctx context.Context, id domain.RunID) (RunTimeline, error) {
	var out RunTimeline
	err := s.store.Read(ctx, func(tx *store.ReadTx) error {
		state, err := tx.ServerState(ctx)
		if err != nil {
			return err
		}
		if err := validateServerState(state); err != nil {
			return err
		}
		run, err := tx.GetRunSnapshot(ctx, id)
		if err != nil {
			return err
		}
		if err := validateSnapshot(state, run.Snapshot); err != nil {
			return err
		}
		observation, err := tx.ObserveRun(ctx, id)
		if err != nil {
			return err
		}
		items, err := tx.ListAttentionItems(ctx)
		if err != nil {
			return err
		}
		if err := authenticateRunObservation(ctx, tx, state, run.Value, observation, items); err != nil {
			return asRunObservationIntegrityError(err)
		}
		observation = withAuthoritativeInvocationStatuses(observation)
		facts, err := runProjectionFactsFor(ctx, tx, id, observation)
		if err != nil {
			return asRunObservationIntegrityError(err)
		}
		out = runTimeline(observation, state.Revision, time.Now().UTC(), facts)
		return nil
	})
	if err != nil {
		return RunTimeline{}, fmt.Errorf("get run %q timeline: %w", id, err)
	}
	return out, nil
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

func itemSnapshot(item domain.AttentionItem, snapshot store.Snapshot, blobs *BlobStore) AttentionItemSnapshot {
	normalized := projectEvidenceAvailability(normalizeAttentionItem(item), blobs)
	return AttentionItemSnapshot{
		AsOfRevision: snapshot.AsOfRevision, EntityVersion: snapshot.EntityVersion, Item: normalized,
	}
}

// projectEvidenceAvailability recomputes every evidence and claim reference's
// EvidenceMetadata.Availability from the blob store immediately before
// serialization (§5.15). Availability is a daemon read-time fact, never a
// trusted persisted value (the same re-gate discipline as publish_eligible):
// a digest whose bytes are in the store projects available, one held without
// bytes projects bytes_absent. A nil blob store (none configured) or a store
// that cannot confirm presence projects bytes_absent for everything, the
// truthful conservative state, since a fetch could not be served either way.
// The mutated slices are cloned first, so the store's decoded value is never
// touched (nonNilSlice returns the input slice unchanged when non-nil).
func projectEvidenceAvailability(item domain.AttentionItem, blobs *BlobStore) domain.AttentionItem {
	availability := func(digest domain.Digest) domain.EvidenceAvailability {
		if blobs == nil {
			return domain.EvidenceBytesAbsent
		}
		has, err := blobs.Has(digest)
		if err != nil || !has {
			return domain.EvidenceBytesAbsent
		}
		return domain.EvidenceAvailable
	}
	if len(item.EvidenceSnapshot) > 0 {
		evidence := slices.Clone(item.EvidenceSnapshot)
		for i := range evidence {
			evidence[i].Metadata.Availability = availability(evidence[i].Digest)
		}
		item.EvidenceSnapshot = evidence
	}
	if len(item.AgentClaims) > 0 {
		claims := slices.Clone(item.AgentClaims)
		for i := range claims {
			// An inline text claim carries its content in-band (ClaimText), so
			// the client renders it without a fetch: it is available regardless
			// of blob-store state. Only a referenced claim (an image, or text
			// carried out of line) is gated on the blob store.
			if claims[i].Text != nil {
				claims[i].Metadata.Availability = domain.EvidenceAvailable
			} else {
				claims[i].Metadata.Availability = availability(claims[i].Digest)
			}
		}
		item.AgentClaims = claims
	}
	return item
}

func deliverySnapshot(delivery domain.AttentionDelivery, snapshot store.Snapshot) AttentionDeliverySnapshot {
	return AttentionDeliverySnapshot{
		AsOfRevision: snapshot.AsOfRevision, EntityVersion: snapshot.EntityVersion, Delivery: delivery,
	}
}

func runSnapshot(
	run domain.Run,
	snapshot store.Snapshot,
	observation domain.RunObservation,
	conclusion domain.RunConclusion,
	asOfRevision int64,
	displayNames *domain.DisplayNames,
	facts runProjectionFacts,
) RunSnapshot {
	normalized := normalizeRun(run)
	projection := Run{
		ID: normalized.ID, ProjectID: normalized.ProjectID,
		DisplayNames: displayNames,
		SpecDigest:   normalized.SpecDigest, PolicyDigest: normalized.PolicyDigest,
		Stages:            normalized.Stages,
		Outcome:           conclusion.Outcome,
		Lifecycle:         domain.LifecycleOf(conclusion, facts.supersededBy != nil),
		SupersededBy:      facts.supersededBy,
		Completion:        facts.completion,
		BillableCostSoFar: facts.cost,
	}
	if normalized.CampaignID != "" {
		campaignID := normalized.CampaignID
		attemptNumber := normalized.AttemptNumber
		projection.CampaignID = &campaignID
		projection.AttemptNumber = &attemptNumber
		if normalized.AttemptReason != "" {
			reason := normalized.AttemptReason
			projection.AttemptReason = &reason
		}
		if normalized.ParentRunID != "" {
			parentRunID := normalized.ParentRunID
			projection.ParentRunID = &parentRunID
		}
	}
	if createdAt, ok := observation.SubmittedAt(); ok {
		projection.CreatedAt = &createdAt
	}
	if lastActivityAt, ok := observation.LastObservedAt(); ok {
		projection.LastActivityAt = &lastActivityAt
	}
	if len(observation.Milestones) > 0 {
		latest := observation.Milestones[len(observation.Milestones)-1].Kind
		projection.LatestMilestone = &latest
	}
	if observation.Hold != nil {
		reason := observation.Hold.Reason
		projection.HoldReason = &reason
	}
	return RunSnapshot{
		AsOfRevision: asOfRevision, EntityVersion: snapshot.EntityVersion, Run: projection,
	}
}

func runTimeline(
	observation domain.RunObservation, asOfRevision int64, asOf time.Time, facts runProjectionFacts,
) RunTimeline {
	return RunTimeline{
		AsOfRevision:      asOfRevision,
		AsOf:              asOf,
		RunID:             observation.RunID,
		Milestones:        nonNilSlice(observation.Milestones),
		Hold:              observation.Hold,
		Invocations:       nonNilSlice(observation.Invocations),
		Completion:        facts.completion,
		BillableCostSoFar: facts.cost,
	}
}

// runProjectionFacts are the store-read facts the run summary and timeline
// carry beyond the observation itself (#1134): the authenticated completion
// record's wire facts, the superseding attempt, and the spend figure.
type runProjectionFacts struct {
	completion   *WorkUnitCompletionFacts
	supersededBy *domain.RunID
	cost         *domain.CostSoFar
}

// runProjectionFactsFor reads the projection facts inside the same
// transaction as the observation. It runs after authenticateRunObservation,
// so a work_unit_completed milestone has already been bound to its re-gated
// completion row; the row is re-read here rather than threaded out of the
// authentication pass. A missing row at this point is the same integrity
// contradiction and fails closed the same way.
func runProjectionFactsFor(
	ctx context.Context, tx *store.ReadTx, runID domain.RunID, observation domain.RunObservation,
) (runProjectionFacts, error) {
	var facts runProjectionFacts
	for _, milestone := range observation.Milestones {
		if milestone.Kind != domain.MilestoneWorkUnitCompleted {
			continue
		}
		completion, err := authenticatedWorkUnitCompletion(ctx, tx, runID, milestone)
		if err != nil {
			return runProjectionFacts{}, err
		}
		facts.completion = &WorkUnitCompletionFacts{
			PRNumber: completion.PRNumber, MergeCommitSHA: completion.MergeCommitSHA,
			BoundIssue: completion.BoundIssue, RecordedAt: completion.RecordedAt,
		}
		break
	}
	successor, ok, err := tx.RunSuccessor(ctx, runID)
	if err != nil {
		return runProjectionFacts{}, err
	}
	if ok {
		facts.supersededBy = &successor
	}
	facts.cost, err = tx.BillableCostSoFar(ctx, runID)
	if err != nil {
		return runProjectionFacts{}, err
	}
	return facts, nil
}

// runAttemptBindings indexes a run's attempt invocations. The invocation
// bindings are a pure projection of the run, so both the observation pass and
// the conclusion can build them from the run alone.
func runAttemptBindings(run domain.Run) map[domain.InvocationID]runAttemptBinding {
	attempts := make(map[domain.InvocationID]runAttemptBinding)
	for _, stage := range run.Stages {
		for _, attempt := range stage.Attempts {
			attempts[attempt.InvocationID] = runAttemptBinding{
				stageID: stage.ID, attemptID: attempt.ID,
			}
		}
	}
	return attempts
}

// authenticatedCompletionMilestone binds one work_unit_completed milestone to
// both of the authorities a completion rests on: the run's own publication
// invocation, and the store's re-gated completion record. The observation
// pass and the conclusion both go through it, so the sync boundary and the
// direct-store observers cannot drift apart on which completions they accept.
func authenticatedCompletionMilestone(
	ctx context.Context,
	tx *store.ReadTx,
	run domain.Run,
	milestone domain.RunMilestone,
	attempts map[domain.InvocationID]runAttemptBinding,
) (domain.WorkUnitCompletion, error) {
	if err := authenticatePublicationInvocation(
		run.ID, *milestone.InvocationID, domain.ProductionPublicationInvocationID(run.ID), attempts,
	); err != nil {
		return domain.WorkUnitCompletion{}, fmt.Errorf("milestone %s: %w", milestone.Kind, err)
	}
	return authenticatedWorkUnitCompletion(ctx, tx, run.ID, milestone)
}

// authenticatedWorkUnitCompletion binds a work_unit_completed milestone to the
// store's completion record for the run's work unit. The milestone is a
// powerless mirror: the record is the authority, the store re-gates it
// against the declaration, binding, and fact timelines on read, and the
// milestone must restate the record's instant, so a milestone with no
// supported record behind it, or one recorded at another instant, fails
// closed as a parent-key mismatch. Every fail-closed verdict the store can
// give about one of those rows (store.IsRowVerdict) is that per-run
// contradiction, so one damaged row excludes its run rather than failing the
// whole listing read; anything else is infrastructure and propagates. The
// binding the record was derived from must also restate the run's ready
// resource, so the completion is bound to the pull request this run
// published rather than to any consistent set of rows.
func authenticatedWorkUnitCompletion(
	ctx context.Context, tx *store.ReadTx, runID domain.RunID, milestone domain.RunMilestone,
) (domain.WorkUnitCompletion, error) {
	declaration, err := tx.GetWorkUnitDeclarationByRun(ctx, runID)
	if store.IsRowVerdict(err) {
		return domain.WorkUnitCompletion{}, fmt.Errorf("milestone %s run %q has no supported work unit: %w (%w)",
			milestone.Kind, runID, domain.ErrParentKeyMismatch, err)
	}
	if err != nil {
		return domain.WorkUnitCompletion{}, fmt.Errorf("milestone %s: %w", milestone.Kind, err)
	}
	completion, err := tx.GetWorkUnitCompletion(ctx, declaration.ID)
	if store.IsRowVerdict(err) {
		// No done bit, a done bit the store's own evidence does not derive,
		// or a row it cannot reconstruct: either way there is no authority
		// for this milestone, and the contradiction belongs to this run, so
		// the listing reads exclude it instead of failing the whole request.
		// Any other error is infrastructure and propagates unwrapped, so a
		// transient failure never reads as an integrity contradiction.
		return domain.WorkUnitCompletion{}, fmt.Errorf("milestone %s has no supported completion record: %w (%w)",
			milestone.Kind, domain.ErrParentKeyMismatch, err)
	}
	if err != nil {
		return domain.WorkUnitCompletion{}, fmt.Errorf("milestone %s: %w", milestone.Kind, err)
	}
	if !completion.RecordedAt.Equal(milestone.RecordedAt) {
		return domain.WorkUnitCompletion{}, fmt.Errorf("milestone %s instant disagrees with the completion record: %w",
			milestone.Kind, domain.ErrParentKeyMismatch)
	}
	if err := authenticatedCompletionPRBinding(ctx, tx, runID, declaration.ID); err != nil {
		return domain.WorkUnitCompletion{}, fmt.Errorf("milestone %s: %w", milestone.Kind, err)
	}
	return completion, nil
}

// authenticatedCompletionPRBinding requires the work-unit binding the
// completion was derived from to restate the run's independently
// reconstructed ready resource. The store re-derives a completion from the
// declaration, the binding, and the merge facts, which binds those rows to
// each other but not to the pull request this run actually published: a
// consistently forged binding and merge fact would otherwise report a run
// published as one PR as completed by another PR's merge. The capture pass
// makes the same comparison before it observes anything through the binding
// (scheduler.go, "work-unit binding disagrees with ready resource"), but a
// write-time check is not read-time evidence, which is the whole reason the
// store re-gates on read.
func authenticatedCompletionPRBinding(
	ctx context.Context, tx *store.ReadTx, runID domain.RunID, unitID domain.WorkUnitID,
) error {
	binding, err := tx.GetWorkUnitPRBinding(ctx, unitID)
	if store.IsRowVerdict(err) {
		return fmt.Errorf("work unit %s has no supported pr binding: %w (%w)",
			unitID, domain.ErrParentKeyMismatch, err)
	}
	if err != nil {
		return err
	}
	ready, err := tx.GetReadyItemPRBinding(ctx, domain.ProductionReadyItemID(runID))
	if store.IsRowVerdict(err) {
		return fmt.Errorf("run %q has no supported ready resource binding: %w (%w)",
			runID, domain.ErrParentKeyMismatch, err)
	}
	if err != nil {
		return err
	}
	if ready.RunID != runID || binding.Repo != ready.Repo || binding.RepositoryID != ready.RepositoryID ||
		binding.PRNumber != ready.PRNumber || binding.BaseRef != ready.BaseRef ||
		binding.HeadSHA != ready.HeadSHA {
		return fmt.Errorf("work-unit binding %s disagrees with ready resource %s: %w",
			binding.UnitID, ready.ItemID, domain.ErrParentKeyMismatch)
	}
	return nil
}

// projectRunSnapshot re-derives one run's authenticated observation projection
// for the listing reads (Bootstrap, ListRuns). A returned error wrapping
// ErrRunObservationIntegrity marks a per-run semantic contradiction the caller
// isolates by excluding that run; any other error is an infrastructure failure
// the caller propagates to fail the whole read closed.
func projectRunSnapshot(
	ctx context.Context,
	tx *store.ReadTx,
	state store.ServerState,
	run domain.Run,
	snapshot store.Snapshot,
	items []store.Snapshotted[domain.AttentionItem],
) (RunSnapshot, error) {
	observation, err := tx.ObserveRun(ctx, run.ID)
	if err != nil {
		return RunSnapshot{}, fmt.Errorf("run %q timeline: %w", run.ID, err)
	}
	if err := authenticateRunObservation(ctx, tx, state, run, observation, items); err != nil {
		return RunSnapshot{}, fmt.Errorf("run %q timeline: %w", run.ID, asRunObservationIntegrityError(err))
	}
	observation = withAuthoritativeInvocationStatuses(observation)
	conclusion, err := AuthenticatedRunConclusion(
		ctx, tx, run, observation, publicationReadyMilestone(observation),
	)
	if err != nil {
		return RunSnapshot{}, fmt.Errorf("run %q conclusion: %w", run.ID, asRunObservationIntegrityError(err))
	}
	facts, err := runProjectionFactsFor(ctx, tx, run.ID, observation)
	if err != nil {
		return RunSnapshot{}, fmt.Errorf("run %q facts: %w", run.ID, asRunObservationIntegrityError(err))
	}
	displayNames, err := tx.DisplayNamesFor(ctx, run.ProjectID, domain.Subject{
		Type: domain.SubjectRun, ID: domain.SubjectID(run.ID), RunID: &run.ID,
	})
	if err != nil {
		return RunSnapshot{}, fmt.Errorf("run %q display names: %w", run.ID, err)
	}
	return runSnapshot(run, snapshot, observation, conclusion, state.Revision, displayNames, facts), nil
}

func publicationReadyMilestone(observation domain.RunObservation) bool {
	for _, milestone := range observation.Milestones {
		if milestone.Kind == domain.MilestonePublicationReady {
			return true
		}
	}
	return false
}

// asRunObservationIntegrityError tags a semantic projection contradiction with
// the ErrRunObservationIntegrity sentinel the listing reads isolate on, so the
// skip keys on an intentional signal rather than the broadly-reused
// domain.ErrParentKeyMismatch. An infrastructure error (a store or db read
// failure) carries no ErrParentKeyMismatch and is returned unwrapped, so it
// still fails the whole read closed.
func asRunObservationIntegrityError(err error) error {
	if errors.Is(err, domain.ErrParentKeyMismatch) {
		return fmt.Errorf("%w: %w", ErrRunObservationIntegrity, err)
	}
	return err
}

// excludedRun pairs a run the listing reads dropped with the integrity
// contradiction that dropped it, so logExcludedRuns can name both after the
// read transaction closes. projectID carries the run's project so the durable
// AttentionSystemHealth item the converge mints binds to the same project the
// run does (#770); a mis-bound item would itself trip authenticateRunObservation.
type excludedRun struct {
	id        domain.RunID
	projectID domain.ProjectID
	err       error
}

// logExcludedRuns records one durable Warn per run a listing read excluded for
// a projection integrity contradiction (#767), so the cause is diagnosable
// server-side instead of vanishing into an undifferentiated 500. Called after
// the read transaction closes, never inside it.
func (s *Service) logExcludedRuns(excluded []excludedRun) {
	for _, run := range excluded {
		s.logger.Warn("run observation projection integrity failure; excluding run from listing",
			"run", run.id, "error", run.err)
	}
}

type runAttemptBinding struct {
	stageID   domain.StageID
	attemptID domain.AttemptID
}

// authenticateRunObservation re-binds every outcome-bearing projection row
// to the durable authority it summarizes. Observation rows are deliberately
// powerless workflow mirrors; serving one as an operator observation therefore
// requires more than structural decoding. A forged milestone must fail the
// read, not turn a failed run into a ready one.
func authenticateRunObservation(
	ctx context.Context,
	tx *store.ReadTx,
	state store.ServerState,
	run domain.Run,
	observation domain.RunObservation,
	items []store.Snapshotted[domain.AttentionItem],
) error {
	attempts := runAttemptBindings(run)
	var readyBinding *domain.ReadyItemPRBinding
	blockedReasons := make(map[domain.RunHoldReason]bool)
	reevaluationCommands := make(map[string]bool)
	allDefinitiveBlocksRerun := true
	openDefinitiveBlocks := 0
	readyMilestone := false
	blockedMilestone := false
	lastReadyMilestone := -1
	lastBlockedMilestone := -1
	for _, snapshot := range items {
		if err := validateSnapshot(state, snapshot.Snapshot); err != nil {
			return fmt.Errorf("attention item %q authority: %w", snapshot.Value.ID, err)
		}
		item := snapshot.Value
		if item.Subject.RunID == nil || *item.Subject.RunID != run.ID {
			continue
		}
		if item.ProjectID != run.ProjectID || item.Subject.Type != domain.SubjectRun ||
			item.Subject.ID != domain.SubjectID(run.ID) {
			return fmt.Errorf("attention item %q does not bind to run %q: %w",
				item.ID, run.ID, domain.ErrParentKeyMismatch)
		}
		switch item.Type {
		case domain.AttentionReadyForFinalReview:
			if item.ID != domain.ProductionReadyItemID(run.ID) {
				continue
			}
			if binding, err := tx.GetReadyItemPRBinding(ctx, item.ID); err == nil {
				readyBinding = &binding
			} else if !errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("ready item %q authority: %w", item.ID, err)
			}
		case domain.AttentionPublishBlocked:
			reason, definitive := domain.DefinitivePublicationBlockReason(item.Reason)
			if !definitive {
				continue
			}
			identityValid := item.ID == domain.ProductionBlockedItemID(run.ID)
			if !identityValid {
				var err error
				identityValid, err = AuthenticateReevaluatedBlockedItemIdentity(
					ctx, tx, item.ID, run.ID, run.ProjectID,
				)
				if err != nil {
					return err
				}
			}
			legacyActions := []domain.Action{domain.ActionInspectTrustFailure, domain.ActionStop}
			rerunnableActions := []domain.Action{
				domain.ActionRerunTrustEvaluation, domain.ActionInspectTrustFailure, domain.ActionStop,
			}
			if identityValid && (slices.Equal(item.RequestedDecision, legacyActions) ||
				slices.Equal(item.RequestedDecision, rerunnableActions)) {
				blockedReasons[reason] = true
				if _, commandID, ok := ReevaluatedBlockedItemCoordinates(item.ID); ok {
					reevaluationCommands[commandID] = true
				}
				if item.Status == domain.StatusOpen {
					openDefinitiveBlocks++
					allDefinitiveBlocksRerun = false
				} else {
					rerun, err := definitiveBlockResolvedByRerun(ctx, tx, item)
					if err != nil {
						return err
					}
					allDefinitiveBlocksRerun = allDefinitiveBlocksRerun && rerun
				}
			}
		case domain.AttentionSpecApproval, domain.AttentionExecutionFailure,
			domain.AttentionAgentQuestion, domain.AttentionReviewDiminishing,
			domain.AttentionReviewDispute, domain.AttentionReviewContradiction,
			domain.AttentionReviewConfiguration, domain.AttentionFindingAdjudication,
			domain.AttentionRunProposal,
			domain.AttentionSystemHealth, domain.AttentionBlocked:
		}
	}
	for _, invocation := range observation.Invocations {
		if _, ok := attempts[invocation.InvocationID]; !ok {
			return fmt.Errorf("invocation observation %q is not an attempt of run %q: %w",
				invocation.InvocationID, run.ID, domain.ErrParentKeyMismatch)
		}
	}
	if observation.Hold != nil {
		if observation.Hold.InvocationID == nil {
			return fmt.Errorf("hold invocation is not bound to run %q: %w",
				run.ID, domain.ErrParentKeyMismatch)
		}
		holdInvocation := *observation.Hold.InvocationID
		if !runObservationInvocation(run.ID, holdInvocation, attempts) {
			// A hold can precede any attempt: a run submitted but refused
			// admission (a backend below the floor, an identity-parallelism
			// limit) holds its reserved invocation before that invocation
			// becomes an attempt. Bind it under the same reserved-intent
			// authority the run_submitted milestone uses, so a hold on an
			// invocation the run never reserved still fails closed.
			if err := authenticateRunSubmission(ctx, tx, run, holdInvocation); err != nil {
				return fmt.Errorf("hold invocation is not bound to run %q: %w",
					run.ID, domain.ErrParentKeyMismatch)
			}
		}
	}
	for index, milestone := range observation.Milestones {
		invocation := *milestone.InvocationID
		publicationInvocation := domain.ProductionPublicationInvocationID(run.ID)
		switch milestone.Kind {
		case domain.MilestoneRunSubmitted:
			if err := authenticateRunSubmission(ctx, tx, run, invocation); err != nil {
				return fmt.Errorf("milestone %s: %w", milestone.Kind, err)
			}
		case domain.MilestoneInvocationAdmitted:
			if err := authenticateAdmissionRun(ctx, tx, invocation, run.ID, attempts[invocation]); err != nil {
				return fmt.Errorf("milestone %s: %w", milestone.Kind, err)
			}
		case domain.MilestoneInvocationStarted:
			if _, ok := attempts[invocation]; !ok {
				return fmt.Errorf("milestone %s invocation %q is not an attempt of run %q: %w",
					milestone.Kind, invocation, run.ID, domain.ErrParentKeyMismatch)
			}
			entry, err := tx.GetOutbox(ctx, string(invocation))
			if err != nil {
				return fmt.Errorf("milestone %s: %w", milestone.Kind, err)
			}
			if !entry.Dispatched() {
				return fmt.Errorf("milestone %s invocation %q was not dispatched: %w",
					milestone.Kind, invocation, domain.ErrParentKeyMismatch)
			}
			if err := domain.AuthenticateInvocationDispatchIntent(domain.InvocationDispatchIntent{
				Kind: entry.Kind, IdempotencyKey: entry.IdempotencyKey, Payload: entry.Payload,
			}, invocation, run.ID, attempts[invocation].stageID); err != nil {
				return fmt.Errorf("milestone %s: %w", milestone.Kind, err)
			}
			if err := authenticateConversationInvocationIntent(ctx, tx, entry, invocation, run.ID); err != nil {
				return fmt.Errorf("milestone %s: %w", milestone.Kind, err)
			}
			// A conversation intent omits its run and stage (see
			// AuthenticateInvocationDispatchIntent) and, unlike a production or
			// specification attempt, has no execution admission to bind against:
			// the engine dispatches a conversation invocation unbound, so no
			// admission record exists and demanding one would fail a legitimate
			// start closed. Its attempt identity is still deterministic, so bind
			// the started attempt to that identity: a run graph that retargets
			// the conversation invocation to a different attempt fails this
			// equality while a legitimately unbound start passes. Stage
			// retargeting has no first-order authority for an unbound invocation
			// and is left to run-record integrity (the note's boundary line).
			if entry.Kind == string(domain.AgentInvocationRequestedKind) &&
				attempts[invocation].attemptID != attemptIDForInvocation(invocation) {
				return fmt.Errorf("milestone %s conversation invocation %q is not its own attempt: %w",
					milestone.Kind, invocation, domain.ErrParentKeyMismatch)
			}
		case domain.MilestoneExecutionExportRecorded:
			if _, err := tx.GetExecutionExportRecord(ctx, invocation); err != nil {
				return fmt.Errorf("milestone %s: %w", milestone.Kind, err)
			}
			if err := authenticateAdmissionRun(ctx, tx, invocation, run.ID, attempts[invocation]); err != nil {
				return fmt.Errorf("milestone %s: %w", milestone.Kind, err)
			}
		case domain.MilestoneExecutionOutcomeRecorded:
			outcome, err := tx.GetExecutionOutcomeRecord(ctx, invocation)
			if err != nil {
				return fmt.Errorf("milestone %s: %w", milestone.Kind, err)
			}
			if outcome.Status != *milestone.Outcome {
				return fmt.Errorf("milestone %s outcome disagrees with authority: %w",
					milestone.Kind, domain.ErrParentKeyMismatch)
			}
			if err := authenticateAdmissionRun(ctx, tx, invocation, run.ID, attempts[invocation]); err != nil {
				return fmt.Errorf("milestone %s: %w", milestone.Kind, err)
			}
		case domain.MilestoneTerminalRecorded:
			if err := authenticateTerminal(ctx, tx, invocation, run.ID, attempts[invocation], *milestone.Terminal); err != nil {
				return fmt.Errorf("milestone %s: %w", milestone.Kind, err)
			}
		case domain.MilestonePublicationReady:
			readyMilestone = true
			lastReadyMilestone = index
			if err := authenticatePublicationInvocation(
				run.ID, invocation, publicationInvocation, attempts,
			); err != nil {
				return fmt.Errorf("milestone %s: %w", milestone.Kind, err)
			}
			if readyBinding == nil || readyBinding.PublicationInvocationID != invocation {
				return fmt.Errorf("milestone %s has no durable ready item: %w",
					milestone.Kind, domain.ErrParentKeyMismatch)
			}
		case domain.MilestonePublicationBlocked:
			blockedMilestone = true
			lastBlockedMilestone = index
			if err := authenticateBlockedMilestoneInvocation(
				run.ID, invocation, publicationInvocation, attempts, reevaluationCommands,
			); err != nil {
				return fmt.Errorf("milestone %s: %w", milestone.Kind, err)
			}
			if !blockedReasons[*milestone.Reason] {
				return fmt.Errorf("milestone %s reason has no matching definitive blocked item: %w",
					milestone.Kind, domain.ErrParentKeyMismatch)
			}
		case domain.MilestoneWorkUnitCompleted:
			// The completion ends a published timeline: it carries the
			// publication invocation, follows an authenticated ready that
			// itself follows the last definitive block, and mirrors the
			// store's re-gated completion record.
			if !readyMilestone || lastReadyMilestone < lastBlockedMilestone {
				return fmt.Errorf("milestone %s has no authenticated publication_ready after the last block: %w",
					milestone.Kind, domain.ErrParentKeyMismatch)
			}
			if _, err := authenticatedCompletionMilestone(ctx, tx, run, milestone, attempts); err != nil {
				return err
			}
		}
	}
	if openDefinitiveBlocks > 1 {
		return fmt.Errorf("run %q has %d open definitive publication blocks: %w",
			run.ID, openDefinitiveBlocks, domain.ErrParentKeyMismatch)
	}
	if err := validatePublicationAuthorityExclusivity(
		run.ID, readyMilestone, blockedMilestone, allDefinitiveBlocksRerun,
		lastReadyMilestone > lastBlockedMilestone,
	); err != nil {
		return err
	}
	return nil
}

// withAuthoritativeInvocationStatuses overlays each authenticated terminal
// fact on the paced driver observation returned to clients. An invocation
// observation is a last-look cache and can legitimately lag an export,
// outcome, or terminal record; the durable fact is the status authority while
// the observation still owns its timestamp. A terminal invocation cannot be
// live, so the projection also clears a stale live bit. The store-returned
// slice is cloned before rewriting it.
func withAuthoritativeInvocationStatuses(observation domain.RunObservation) domain.RunObservation {
	terminalByInvocation := make(map[domain.InvocationID]domain.ObservedInvocationStatus)
	for _, milestone := range observation.Milestones {
		invocation := *milestone.InvocationID
		switch milestone.Kind {
		case domain.MilestoneExecutionExportRecorded:
			terminalByInvocation[invocation] = domain.ObservedStatusCompleted
		case domain.MilestoneExecutionOutcomeRecorded:
			terminalByInvocation[invocation] = observedStatusForExecutionOutcome(*milestone.Outcome)
		case domain.MilestoneTerminalRecorded:
			terminalByInvocation[invocation] = *milestone.Terminal
		case domain.MilestoneRunSubmitted, domain.MilestoneInvocationAdmitted,
			domain.MilestoneInvocationStarted, domain.MilestonePublicationReady,
			domain.MilestonePublicationBlocked, domain.MilestoneWorkUnitCompleted:
		}
	}
	if len(terminalByInvocation) == 0 || len(observation.Invocations) == 0 {
		return observation
	}
	observation.Invocations = slices.Clone(observation.Invocations)
	for index := range observation.Invocations {
		status, ok := terminalByInvocation[observation.Invocations[index].InvocationID]
		if !ok {
			continue
		}
		observation.Invocations[index].Status = status
		observation.Invocations[index].Live = false
	}
	return observation
}

func observedStatusForExecutionOutcome(status domain.ExecutionOutcomeStatus) domain.ObservedInvocationStatus {
	switch status {
	case domain.ExecutionOutcomeFailed:
		return domain.ObservedStatusFailed
	case domain.ExecutionOutcomeCanceled:
		return domain.ObservedStatusCanceled
	case domain.ExecutionOutcomeLost:
		return domain.ObservedStatusGone
	case domain.ExecutionOutcomeBlocked:
		return domain.ObservedStatusBlocked
	}
	return ""
}

func authenticateRunSubmission(
	ctx context.Context, tx *store.ReadTx, run domain.Run, invocation domain.InvocationID,
) error {
	entry, err := tx.GetOutbox(ctx, string(invocation))
	if err != nil {
		return err
	}
	for _, stage := range run.Stages {
		if err := domain.AuthenticateInvocationDispatchIntent(domain.InvocationDispatchIntent{
			Kind: entry.Kind, IdempotencyKey: entry.IdempotencyKey, Payload: entry.Payload,
		}, invocation, run.ID, stage.ID); err == nil {
			return authenticateConversationInvocationIntent(ctx, tx, entry, invocation, run.ID)
		}
	}
	return fmt.Errorf("submitted invocation %q does not bind to a stage of run %q: %w",
		invocation, run.ID, domain.ErrParentKeyMismatch)
}

func validatePublicationAuthorityExclusivity(
	runID domain.RunID, ready, blocked, everyBlockResolvedByRerun, readyAfterLastBlock bool,
) error {
	if ready && blocked && (!everyBlockResolvedByRerun || !readyAfterLastBlock) {
		return fmt.Errorf("run %q has both ready and blocked publication authority: %w",
			runID, domain.ErrParentKeyMismatch)
	}
	return nil
}

// AuthenticateReevaluatedBlockedItemIdentity proves that itemID names the
// successor of an accepted production reevaluation command. Each prior
// reevaluation carrier is authenticated recursively back to the original
// production blocked-item identity.
func AuthenticateReevaluatedBlockedItemIdentity(
	ctx context.Context, tx *store.ReadTx, itemID domain.ItemID,
	runID domain.RunID, projectID domain.ProjectID,
) (bool, error) {
	return authenticateReevaluatedBlockedItemIdentity(
		ctx, tx, itemID, runID, projectID, make(map[string]bool),
	)
}

func authenticateReevaluatedBlockedItemIdentity(
	ctx context.Context, tx *store.ReadTx, itemID domain.ItemID,
	runID domain.RunID, projectID domain.ProjectID, visited map[string]bool,
) (bool, error) {
	itemRunID, commandID, ok := ReevaluatedBlockedItemCoordinates(itemID)
	if !ok || itemRunID != runID {
		return false, nil
	}
	if visited[commandID] {
		return false, domain.ErrParentKeyMismatch
	}
	visited[commandID] = true
	command, err := tx.GetCommand(ctx, commandID)
	if err != nil {
		return false, err
	}
	carrier, err := tx.GetAttentionItem(ctx, command.ItemID)
	if err != nil {
		return false, err
	}
	if command.Action != domain.ActionRerunTrustEvaluation || carrier.Subject.RunID == nil ||
		*carrier.Subject.RunID != runID || carrier.Subject.Type != domain.SubjectRun ||
		carrier.Subject.ID != domain.SubjectID(runID) || carrier.ProjectID != projectID ||
		carrier.Type != domain.AttentionPublishBlocked ||
		carrier.Status != domain.StatusResolved || carrier.DecidedAt == nil ||
		command.ItemVersion+1 != carrier.ItemVersion || command.PRHeadSHA != carrier.PRHeadSHA ||
		!slices.Equal(command.ArtifactDigests, carrier.ArtifactDigests) ||
		!slices.Equal(carrier.RequestedDecision, []domain.Action{
			domain.ActionRerunTrustEvaluation, domain.ActionInspectTrustFailure, domain.ActionStop,
		}) {
		return false, domain.ErrParentKeyMismatch
	}
	if _, definitive := domain.DefinitivePublicationBlockReason(carrier.Reason); !definitive {
		return false, domain.ErrParentKeyMismatch
	}
	if carrier.ID == domain.ProductionBlockedItemID(runID) {
		return true, nil
	}
	return authenticateReevaluatedBlockedItemIdentity(
		ctx, tx, carrier.ID, runID, projectID, visited,
	)
}

func definitiveBlockResolvedByRerun(
	ctx context.Context, tx *store.ReadTx, item domain.AttentionItem,
) (bool, error) {
	_, found, err := definitiveBlockRerunCommand(ctx, tx, item)
	return found, err
}

func definitiveBlockRerunCommand(
	ctx context.Context, tx *store.ReadTx, item domain.AttentionItem,
) (domain.Command, bool, error) {
	if item.Status != domain.StatusResolved || item.DecidedAt == nil || item.ItemVersion < 2 {
		return domain.Command{}, false, nil
	}
	commands, err := tx.ListCommandsForItem(ctx, item.ID)
	if err != nil {
		return domain.Command{}, false, err
	}
	var found *domain.Command
	for _, command := range commands {
		if command.Action != domain.ActionRerunTrustEvaluation ||
			command.ItemVersion+1 != item.ItemVersion || command.PRHeadSHA != item.PRHeadSHA ||
			!slices.Equal(command.ArtifactDigests, item.ArtifactDigests) {
			continue
		}
		if found != nil {
			return domain.Command{}, false, domain.ErrParentKeyMismatch
		}
		command := command
		found = &command
	}
	if found == nil {
		return domain.Command{}, false, nil
	}
	return *found, true, nil
}

func authenticateConversationInvocationIntent(
	ctx context.Context, tx *store.ReadTx, entry store.QueueEntry,
	invocation domain.InvocationID, runID domain.RunID,
) error {
	if entry.Kind == string(domain.SpecificationDiscussionRequestedKind) {
		return authenticateSpecificationDiscussionInvocationIntent(ctx, tx, entry, invocation, runID)
	}
	if entry.Kind != string(domain.AgentInvocationRequestedKind) {
		return nil
	}
	request, err := domain.DecodeConversationInvocationIntent(entry.Payload)
	if err != nil {
		return err
	}
	durable, err := tx.GetAgentInvocation(ctx, invocation)
	if err != nil {
		return err
	}
	item, err := tx.GetAttentionItem(ctx, request.ItemID)
	if err != nil {
		return err
	}
	if durable.ConversationID == nil || *durable.ConversationID != request.ConversationID {
		return fmt.Errorf("conversation invocation %q does not bind conversation %q: %w",
			invocation, request.ConversationID, domain.ErrParentKeyMismatch)
	}
	// Later agent and operator actions supersede the item version after the
	// request commits. It may advance but can never regress below the version
	// named by the immutable dispatch intent.
	if item.ConversationID == nil || *item.ConversationID != request.ConversationID ||
		item.ItemVersion < request.ItemVersion || item.Subject.RunID == nil || *item.Subject.RunID != runID {
		return fmt.Errorf("conversation invocation intent does not bind run %q: %w", runID, domain.ErrParentKeyMismatch)
	}
	return nil
}

func authenticateSpecificationDiscussionInvocationIntent(
	ctx context.Context, tx *store.ReadTx, entry store.QueueEntry,
	invocation domain.InvocationID, runID domain.RunID,
) error {
	request, err := domain.DecodeSpecificationDiscussionInvocationIntent(entry.Payload)
	if err != nil {
		return err
	}
	baseInvocationID := domain.SpecificationInvocationID(request.SpecificationRunID, request.Iteration)
	baseEntry, err := tx.GetOutbox(ctx, string(baseInvocationID))
	if err != nil {
		return err
	}
	var base struct {
		SpecificationRunID  domain.RunID        `json:"specification_run_id"`
		ImplementationRunID domain.RunID        `json:"implementation_run_id"`
		ProjectID           domain.ProjectID    `json:"project_id"`
		InvocationID        domain.InvocationID `json:"invocation_id"`
		Iteration           int                 `json:"iteration"`
		InputArtifactIDs    []domain.ArtifactID `json:"input_artifact_ids"`
		PriorSpecArtifactID *domain.ArtifactID  `json:"prior_spec_artifact_id"`
		FeedbackArtifactIDs []domain.ArtifactID `json:"feedback_artifact_ids"`
		PolicyArtifactID    domain.ArtifactID   `json:"policy_artifact_id"`
	}
	if err := strictjson.DecodeAllowingUnknownFields(
		baseEntry.Payload, &base, strictjson.RejectInvalidUTF8, strictjson.NoLimit,
	); err != nil {
		return err
	}
	terminalEntry, err := tx.GetInbox(ctx, string(baseInvocationID))
	if err != nil {
		return err
	}
	var terminal struct {
		InvocationID   domain.InvocationID `json:"invocation_id"`
		Iteration      int                 `json:"iteration"`
		Status         string              `json:"status"`
		SpecArtifactID *domain.ArtifactID  `json:"spec_artifact_id"`
		ApprovalItemID *domain.ItemID      `json:"approval_item_id"`
	}
	if err := strictjson.DecodeAllowingUnknownFields(
		terminalEntry.Payload, &terminal, strictjson.RejectInvalidUTF8, strictjson.NoLimit,
	); err != nil {
		return err
	}
	providerInvocation, err := tx.GetAgentInvocation(ctx, invocation)
	if err != nil {
		return err
	}
	discussEntry, err := tx.GetOutbox(ctx, string(request.DiscussInvocationID))
	if err != nil {
		return err
	}
	discussRequest, err := domain.DecodeConversationInvocationIntent(discussEntry.Payload)
	if err != nil {
		return err
	}
	discussInvocation, err := tx.GetAgentInvocation(ctx, request.DiscussInvocationID)
	if err != nil {
		return err
	}
	conversation, err := tx.GetConversation(ctx, request.ConversationID)
	if err != nil {
		return err
	}
	prefixDigest, _, err := conversation.PrefixContent(request.ThroughSequence)
	if err != nil {
		return err
	}
	item, err := tx.GetAttentionItem(ctx, request.ItemID)
	if err != nil {
		return err
	}
	expectedInputs := slices.DeleteFunc(slices.Clone(base.InputArtifactIDs), func(id domain.ArtifactID) bool {
		return base.PriorSpecArtifactID != nil && id == *base.PriorSpecArtifactID ||
			slices.Contains(base.FeedbackArtifactIDs, id)
	})
	if !slices.Contains(expectedInputs, request.SpecArtifactID) {
		expectedInputs = append(expectedInputs, request.SpecArtifactID)
	}
	expectedInputs = append(expectedInputs, base.FeedbackArtifactIDs...)
	discussionArtifactID := domain.ArtifactID(
		"spec-discussion-" + strings.TrimPrefix(string(request.DiscussInvocationID), "inv-"),
	)
	discussionArtifact, err := tx.GetArtifact(ctx, discussionArtifactID)
	if err != nil {
		return err
	}
	expectedInputs = append(expectedInputs, discussionArtifactID)
	if request.InvocationID != invocation || request.SpecificationRunID != runID ||
		baseEntry.Kind != string(domain.SpecificationInvocationRequestedKind) ||
		baseEntry.IdempotencyKey != string(baseInvocationID) || !baseEntry.Dispatched() ||
		base.SpecificationRunID != request.SpecificationRunID ||
		base.ImplementationRunID != request.ImplementationRunID || base.ProjectID != request.ProjectID ||
		base.InvocationID != baseInvocationID || base.Iteration != request.Iteration ||
		base.PolicyArtifactID != request.PolicyArtifactID ||
		terminalEntry.Kind != "specification_stage_terminal" ||
		terminalEntry.IdempotencyKey != string(baseInvocationID) ||
		terminal.InvocationID != baseInvocationID || terminal.Iteration != request.Iteration ||
		terminal.Status != "completed" || terminal.SpecArtifactID == nil ||
		*terminal.SpecArtifactID != request.SpecArtifactID || terminal.ApprovalItemID == nil ||
		*terminal.ApprovalItemID != request.ItemID ||
		!slices.Equal(request.InputArtifactIDs, expectedInputs) ||
		providerInvocation.ConversationID == nil || *providerInvocation.ConversationID != request.ConversationID ||
		providerInvocation.ThroughSequence != request.ThroughSequence ||
		!slices.Equal(providerInvocation.InputIDs, expectedInputs) ||
		discussEntry.Kind != string(domain.AgentInvocationRequestedKind) || discussEntry.Quarantined() ||
		discussRequest.InvocationID != request.DiscussInvocationID ||
		discussRequest.ConversationID != request.ConversationID || discussRequest.ItemID != request.ItemID ||
		discussRequest.ItemVersion != request.ItemVersion || discussInvocation.ConversationID == nil ||
		*discussInvocation.ConversationID != request.ConversationID ||
		discussInvocation.ThroughSequence != request.ThroughSequence || prefixDigest != request.PrefixDigest ||
		!authenticatesSpecificationDiscussionArtifact(
			discussionArtifact, discussionArtifactID, request.PrefixDigest, baseInvocationID,
		) ||
		item.ConversationID == nil || *item.ConversationID != request.ConversationID ||
		item.ItemVersion < request.ItemVersion || item.ProjectID != request.ProjectID ||
		item.Subject.RunID == nil || *item.Subject.RunID != request.SpecificationRunID {
		return fmt.Errorf("specification discussion invocation intent does not bind durable discussion state: %w",
			domain.ErrParentKeyMismatch)
	}
	return nil
}

func authenticatesSpecificationDiscussionArtifact(
	artifact domain.Artifact,
	id domain.ArtifactID,
	digest domain.Digest,
	producer domain.InvocationID,
) bool {
	provenance := artifact.Provenance
	return artifact.ID == id && artifact.Type == domain.ArtifactKindResearch && artifact.Digest == digest &&
		provenance.ProducerClass == domain.ProducerDaemon && provenance.ProducerInvocationID == producer &&
		provenance.HeadBinding == domain.HeadIndependent && provenance.SourceHeadSHA == "" &&
		provenance.VerificationRecipeDigest == nil && provenance.SensitivityClass == domain.SensitivityNormal
}

// attemptIDForInvocation mirrors the engine's deterministic attempt identity
// (engine.attemptIDFor: "attempt-<invocation>"), which the engine enforces when
// it records an attempt. The read boundary re-derives it to bind a conversation
// start, which carries no run/stage-bearing dispatch intent and no execution
// admission, to its own attempt. This is the same recompute-an-engine-id pattern
// as productionPublicationInvocationID; both stay in step with their engine
// convention by construction.
func attemptIDForInvocation(invocation domain.InvocationID) domain.AttemptID {
	return domain.AttemptID("attempt-" + string(invocation))
}

func authenticatePublicationInvocation(
	runID domain.RunID,
	invocation, publicationInvocation domain.InvocationID,
	attempts map[domain.InvocationID]runAttemptBinding,
) error {
	if invocation != publicationInvocation {
		return fmt.Errorf("invocation %q is not the publication invocation of run %q: %w",
			invocation, runID, domain.ErrParentKeyMismatch)
	}
	if _, isAttempt := attempts[invocation]; isAttempt {
		return fmt.Errorf("publication invocation %q is also an attempt of run %q: %w",
			invocation, runID, domain.ErrParentKeyMismatch)
	}
	return nil
}

// authenticateBlockedMilestoneInvocation accepts the run's publication
// invocation, or the reevaluation identity of a command whose reevaluated
// blocked item this pass already authenticated. A reevaluation identity
// naming any other command fails closed before the reason check.
func authenticateBlockedMilestoneInvocation(
	runID domain.RunID,
	invocation, publicationInvocation domain.InvocationID,
	attempts map[domain.InvocationID]runAttemptBinding,
	reevaluationCommands map[string]bool,
) error {
	milestoneRunID, commandID, ok := publicationReevaluationBlockedMilestoneCoordinates(invocation)
	if !ok {
		return authenticatePublicationInvocation(runID, invocation, publicationInvocation, attempts)
	}
	if milestoneRunID != runID || !reevaluationCommands[commandID] {
		return fmt.Errorf("invocation %q names no authenticated reevaluation of run %q: %w",
			invocation, runID, domain.ErrParentKeyMismatch)
	}
	if _, isAttempt := attempts[invocation]; isAttempt {
		return fmt.Errorf("publication invocation %q is also an attempt of run %q: %w",
			invocation, runID, domain.ErrParentKeyMismatch)
	}
	return nil
}

func runObservationInvocation[T any](
	runID domain.RunID,
	invocation domain.InvocationID,
	attempts map[domain.InvocationID]T,
) bool {
	if _, ok := attempts[invocation]; ok {
		return true
	}
	return invocation == domain.ProductionPublicationInvocationID(runID)
}

func authenticateAdmissionRun(
	ctx context.Context, tx *store.ReadTx, invocation domain.InvocationID, runID domain.RunID,
	attempt runAttemptBinding,
) error {
	admission, err := tx.GetExecutionAdmissionRecord(ctx, invocation)
	if err != nil {
		return err
	}
	if admission.RunID != runID || admission.StageID != attempt.stageID ||
		admission.AttemptID != attempt.attemptID {
		return fmt.Errorf("invocation %q admission does not match run %q attempt: %w",
			invocation, runID, domain.ErrParentKeyMismatch)
	}
	return nil
}

func authenticateTerminal(
	ctx context.Context,
	tx *store.ReadTx,
	invocation domain.InvocationID,
	runID domain.RunID,
	attempt runAttemptBinding,
	terminal domain.ObservedInvocationStatus,
) error {
	if err := authenticateAdmissionRun(ctx, tx, invocation, runID, attempt); err != nil {
		return err
	}
	if terminal == domain.ObservedStatusCompleted {
		_, err := tx.GetExecutionExportRecord(ctx, invocation)
		return err
	}
	outcome, err := tx.GetExecutionOutcomeRecord(ctx, invocation)
	if err != nil {
		return err
	}
	want := domain.ExecutionOutcomeFailed
	switch terminal {
	case domain.ObservedStatusFailed:
		want = domain.ExecutionOutcomeFailed
	case domain.ObservedStatusCanceled:
		want = domain.ExecutionOutcomeCanceled
	case domain.ObservedStatusGone:
		want = domain.ExecutionOutcomeLost
	case domain.ObservedStatusBlocked:
		want = domain.ExecutionOutcomeBlocked
	case domain.ObservedStatusPending, domain.ObservedStatusRunning,
		domain.ObservedStatusCompleted:
	}
	if outcome.Status != want {
		return fmt.Errorf("terminal %q disagrees with outcome %q: %w",
			terminal, outcome.Status, domain.ErrParentKeyMismatch)
	}
	return nil
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
	if item.YieldHistory != nil {
		history := *item.YieldHistory
		history.Rounds = nonNilSlice(slices.Clone(history.Rounds))
		item.YieldHistory = &history
	}
	if item.FindingAdjudication != nil {
		binding := *item.FindingAdjudication
		binding.Proposals = nonNilSlice(binding.Proposals)
		if len(binding.Proposals) > 0 {
			binding.Proposals = slices.Clone(binding.Proposals)
			for idx := range binding.Proposals {
				proposal := &binding.Proposals[idx]
				proposal.Evidence = nonNilSlice(proposal.Evidence)
				proposal.CitedRules = nonNilSlice(proposal.CitedRules)
				proposal.Assumptions = nonNilSlice(proposal.Assumptions)
				proposal.OpenQuestions = nonNilSlice(proposal.OpenQuestions)
				proposal.OfferedAlternatives = nonNilSlice(proposal.OfferedAlternatives)
			}
		}
		item.FindingAdjudication = &binding
	}
	if item.ReviewDispute != nil {
		binding := *item.ReviewDispute
		binding.FindingIDs = nonNilSlice(slices.Clone(binding.FindingIDs))
		item.ReviewDispute = &binding
	}
	if item.SpecRevision != nil {
		facts := *item.SpecRevision
		facts.PriorComments = nonNilSlice(slices.Clone(facts.PriorComments))
		facts.ClaimedAddressals = nonNilSlice(slices.Clone(facts.ClaimedAddressals))
		item.SpecRevision = &facts
	}
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
