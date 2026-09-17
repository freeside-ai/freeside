package domain

import (
	"fmt"
	"time"
)

// Task is the durable identity of requested work across specification runs,
// campaigns, and implementation attempts. Its name is stored, never inferred
// from the newest run. Source is absent only for legacy or demo work without
// a recoverable intake reference. Lifecycle is a read projection, not authority.
type Task struct {
	ID          TaskID               `json:"id"`
	ProjectID   ProjectID            `json:"project_id"`
	Source      *SpecificationSource `json:"source"`
	Name        DisplayName          `json:"name"`
	CreatedAt   time.Time            `json:"created_at"`
	CampaignIDs []CampaignID         `json:"campaign_ids"`
	// LifecycleFacts is the task's append-only, ordinal-ordered lifecycle log
	// (issue #1318). It is the authority for WIP admission counting; the store
	// persists it in a side table and loads it with the task, so a decoded body
	// carries none. Order is recorded order, preserved across idempotent replay.
	LifecycleFacts []TaskLifecycleFact `json:"lifecycle_facts"`
	Cancellation   *TaskCancellation   `json:"cancellation"`
}

// TaskLifecycleFact is one recorded event in a task's lifecycle log. Facts
// rest WIP counting on recorded start/completion/abandonment, never on the
// newest run (issue #1318). RunID is the run that produced the fact.
// CampaignID is that run's campaign and decides a completion's currency (D4):
// a completion whose campaign is not the task's current campaign is history.
// BindingUnitID is the completed work unit's binding, set only on a completion.
// SourceID makes replay idempotent; the store dedupes on it.
type TaskLifecycleFact struct {
	// EpisodeOrdinal is reconstructed from ordered run membership and start
	// facts. It prevents a late result from completing a newer same-campaign
	// episode; it is never trusted from serialized input.
	EpisodeOrdinal int                   `json:"-"`
	Ordinal        int                   `json:"ordinal"`
	Kind           TaskLifecycleFactKind `json:"kind"`
	RunID          RunID                 `json:"run_id"`
	CampaignID     *CampaignID           `json:"campaign_id"`
	BindingUnitID  *WorkUnitID           `json:"binding_unit_id"`
	SourceID       string                `json:"source_id"`
	RecordedAt     time.Time             `json:"recorded_at"`
}

func (f TaskLifecycleFact) Validate() error {
	if !f.Kind.valid() {
		return fmt.Errorf("task lifecycle fact kind %q: %w", f.Kind, ErrInvalidTaskLifecycleFact)
	}
	if f.Ordinal < 1 {
		return fmt.Errorf("task lifecycle fact ordinal %d: %w", f.Ordinal, ErrNonContiguous)
	}
	if f.RunID == "" {
		return fmt.Errorf("task lifecycle fact run_id: %w", ErrEmptyID)
	}
	if f.SourceID == "" {
		return fmt.Errorf("task lifecycle fact source_id: %w", ErrEmptyField)
	}
	if f.CampaignID != nil && *f.CampaignID == "" {
		return fmt.Errorf("task lifecycle fact campaign_id: %w", ErrEmptyField)
	}
	// A completion carries the completed unit's binding; no other kind may.
	switch f.Kind {
	case TaskLifecycleCompleted:
		if f.BindingUnitID == nil || *f.BindingUnitID == "" {
			return fmt.Errorf("task lifecycle completed fact binding_unit_id: %w", ErrEmptyField)
		}
	case TaskLifecycleStarted, TaskLifecycleAbandoned:
		if f.BindingUnitID != nil {
			return fmt.Errorf("task lifecycle %s fact binding_unit_id: %w", f.Kind, ErrInvalidTaskLifecycleFact)
		}
	}
	if f.RecordedAt.IsZero() {
		return fmt.Errorf("task lifecycle fact recorded_at: %w", ErrMissingTimestamp)
	}
	if f.RecordedAt.Location() != time.UTC {
		return fmt.Errorf("task lifecycle fact recorded_at: %w", ErrTimestampNotUTC)
	}
	return nil
}

// TaskWIP reports whether the task occupies a work-in-progress admission slot,
// derived from its lifecycle facts (never stored; issue #1318 D2, D4). The
// task is WIP when its newest recorded start has neither a later abandonment
// nor a later completion bound to that start's episode and campaign (the current work
// episode). Currency is decided against the newest start's campaign, not the
// last of CampaignIDs: a reserved-but-unstarted proposal appends its campaign
// to CampaignIDs but records no start, and it must not shift currency so that a
// completion of the actually-running campaign stops releasing the slot
// (docs/plan.md §5.11). A completion bound to a superseded campaign stays in
// the log but does not release the slot. Facts before the newest start are
// prior history: they cannot clear a restarted task's slot. Bound confirmed
// cancellation also releases the slot, including confirmations recorded before
// atomic cancellation-to-abandonment projection existed.
func TaskWIP(t Task) bool {
	newestStart := -1
	for i, f := range t.LifecycleFacts {
		if f.Kind == TaskLifecycleStarted {
			newestStart = i
		}
	}
	if newestStart < 0 {
		return false
	}
	if t.confirmedCancellationForCurrentEpisode() {
		return false
	}
	current := t.LifecycleFacts[newestStart].CampaignID
	for i := newestStart + 1; i < len(t.LifecycleFacts); i++ {
		f := t.LifecycleFacts[i]
		switch f.Kind {
		case TaskLifecycleAbandoned:
			return false
		case TaskLifecycleCompleted:
			if sameCampaign(f.CampaignID, current) && (f.EpisodeOrdinal == 0 || f.EpisodeOrdinal == t.LifecycleFacts[newestStart].Ordinal) {
				return false
			}
		case TaskLifecycleStarted:
			// Unreachable: newestStart is the index of the last start.
		}
	}
	return true
}

// DisplayLifecycle keeps terminal task decisions separate from run history.
// A completed episode stays finished even when cancellation follows. Otherwise
// confirmed cancellation wins over administrative abandonment. A finished run
// alone retains its display meaning without certifying completion or stopping.
func (t Task) DisplayLifecycle(newest *RunLifecycle) *TaskLifecycle {
	var value TaskLifecycle
	start := t.CurrentStart()
	if start != nil {
		for _, fact := range t.LifecycleFacts {
			if fact.Ordinal > start.Ordinal && fact.Kind == TaskLifecycleCompleted &&
				sameCampaign(fact.CampaignID, start.CampaignID) &&
				(fact.EpisodeOrdinal == 0 || fact.EpisodeOrdinal == start.Ordinal) {
				value = TaskFinished
				return &value
			}
		}
	}
	if t.confirmedCancellationForCurrentEpisode() {
		value = TaskStopped
		return &value
	}
	if start != nil {
		for _, fact := range t.LifecycleFacts {
			if fact.Ordinal > start.Ordinal && fact.Kind == TaskLifecycleAbandoned {
				value = TaskAbandoned
				return &value
			}
		}
	}
	if newest == nil {
		return nil
	}
	switch *newest {
	case RunLifecycleActive:
		value = TaskActive
	case RunLifecycleFinished:
		value = TaskFinished
	}
	return &value
}

func (t Task) confirmedCancellationForCurrentEpisode() bool {
	if t.Cancellation == nil || t.Cancellation.State != TaskCancellationConfirmed {
		return false
	}
	ordinal := 0
	if start := t.CurrentStart(); start != nil {
		ordinal = start.Ordinal
	}
	return t.Cancellation.Target.EpisodeOrdinal == ordinal
}

func sameCampaign(a, b *CampaignID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// CurrentStart returns the task's newest recorded start, or nil when the task
// has never been admitted. It identifies the current work episode a WIP slot
// belongs to, so an abandonment can be tied to it and stay idempotent across
// replay while a later authorized restart begins a new episode (issue #1318).
func (t Task) CurrentStart() *TaskLifecycleFact {
	var start *TaskLifecycleFact
	for i := range t.LifecycleFacts {
		if t.LifecycleFacts[i].Kind == TaskLifecycleStarted {
			f := t.LifecycleFacts[i]
			start = &f
		}
	}
	return start
}

func (t Task) Validate() error {
	if t.ID == "" || t.ProjectID == "" {
		return fmt.Errorf("task identity: %w", ErrEmptyID)
	}
	if t.CreatedAt.IsZero() || t.CampaignIDs == nil {
		return fmt.Errorf("task creation and campaigns: %w", ErrEmptyField)
	}
	if t.Source != nil {
		if err := t.Source.Validate(); err != nil {
			return err
		}
	}
	if err := t.Name.Validate(); err != nil {
		return err
	}
	if t.Name.Source == DisplayNameSourceName {
		return fmt.Errorf("task name source: %w", ErrCardFactInconsistent)
	}
	seen := make(map[CampaignID]bool, len(t.CampaignIDs))
	for _, id := range t.CampaignIDs {
		if id == "" || seen[id] {
			return fmt.Errorf("task campaign %q: %w", id, ErrDuplicate)
		}
		seen[id] = true
	}
	// Lifecycle facts are an ordered log: ordinals run 1..n in slice order and
	// no source id repeats. This is the replay-order and no-duplicate guarantee
	// the WIP derivation relies on (issue #1318 D1).
	seenSource := make(map[string]bool, len(t.LifecycleFacts))
	for idx, f := range t.LifecycleFacts {
		if err := f.Validate(); err != nil {
			return err
		}
		if want := idx + 1; f.Ordinal != want {
			return fmt.Errorf("task lifecycle fact ordinal %d, want %d: %w", f.Ordinal, want, ErrNonContiguous)
		}
		if seenSource[f.SourceID] {
			return fmt.Errorf("task lifecycle fact source %q: %w", f.SourceID, ErrDuplicate)
		}
		seenSource[f.SourceID] = true
	}
	return nil
}
