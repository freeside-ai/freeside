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
	return nil
}
