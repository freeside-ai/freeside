package daemoncompose

import (
	"context"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// Agent-base upkeep's composition (plan §5.7): the stored source setting
// upkeep builds from.

// agentBaseSourceSetting reads the agent_base instance setting at each call,
// nil when unset. The stored section is re-checked against the settings gate
// on every read, so a value current policy refuses never chooses what the
// daemon builds its agent base from.
func agentBaseSourceSetting(st *store.Store) func(context.Context) (*domain.AgentBaseSettings, error) {
	return func(ctx context.Context) (*domain.AgentBaseSettings, error) {
		var record domain.InstanceSettingsRecord
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			record, err = tx.InstanceSettingsRecord(ctx)
			return err
		}); err != nil {
			return nil, err
		}
		source, err := record.AgentBase()
		if err != nil || source == nil {
			return nil, err
		}
		if err := CheckInstanceSettingsSection(
			domain.InstanceSettings{AgentBase: source}, domain.InstanceSectionAgentBase,
		); err != nil {
			return nil, err
		}
		return source, nil
	}
}
