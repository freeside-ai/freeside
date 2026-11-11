package daemoncompose

import (
	"path/filepath"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func TestExecutionConfigurationNoticeConverges(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	for _, enabled := range []bool{false, false, true} {
		if err := convergeExecutionConfiguration(ctx, st, enabled, nil); err != nil {
			t.Fatal(err)
		}
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			items, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
			if err != nil {
				return err
			}
			want := 1
			if enabled {
				want = 0
			}
			if len(items) != want {
				t.Errorf("enabled=%v: %d open notices, want %d", enabled, len(items), want)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
