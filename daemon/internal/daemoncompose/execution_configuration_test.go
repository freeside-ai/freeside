package daemoncompose

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// TestExecutionConfigurationNoticeConverges pins that the card follows the
// state across restarts: one open card naming the current reason, a changed
// reason or a changed detail under one reason replacing it, and a running
// driver concluding it.
func TestExecutionConfigurationNoticeConverges(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	type step struct {
		state  ExecutionState
		detail string
	}
	for _, step := range []step{
		{state: ExecutionNotConfigured},
		{state: ExecutionNotConfigured},
		{state: ExecutionAuthorityInUse},
		{state: ExecutionAuthorityInUse},
		{state: ExecutionRigLeasePresent},
		{state: ExecutionConfigured},
		{state: ExecutionConfigured},
		{state: ExecutionRigLeasePresent},
		{ExecutionSettingsRefused, "the agent image is unset"},
		{ExecutionSettingsRefused, "the agent image is unset"},
		{ExecutionSettingsRefused, "the seed root is unset"},
	} {
		state := step.state
		if err := convergeExecutionConfiguration(ctx, st, state, step.detail, nil); err != nil {
			t.Fatal(err)
		}
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			items, err := tx.ListOpenAttentionItems(ctx, domain.AttentionSystemHealth)
			if err != nil {
				return err
			}
			code, reason := executionStateDiagnostic(state, step.detail)
			if state.Runs() {
				if len(items) != 0 {
					t.Errorf("%s: %d open notices, want none", state, len(items))
				}
				return nil
			}
			if len(items) != 1 {
				t.Fatalf("%s: %d open notices, want 1", state, len(items))
			}
			item := items[0]
			if item.HealthDiagnostic == nil || item.HealthDiagnostic.Code != code || item.Reason != reason {
				t.Errorf("%s: open notice %+v, want code %s", state, item.HealthDiagnostic, code)
			}
			if item.Posture == nil || *item.Posture != domain.HealthPostureBlocking ||
				item.HealthDiagnostic.Impairs != domain.ImpairedCapabilityUnattendedAdmission {
				t.Errorf("%s: notice does not close unattended admission", state)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := convergeExecutionConfiguration(ctx, st, "", "", nil); err == nil {
		t.Fatal("zero execution state accepted")
	}
}

func TestExecutionStateReasonsNameTheirRemedy(t *testing.T) {
	t.Parallel()
	for state, wants := range map[ExecutionState][]string{
		ExecutionNotConfigured:          {"README's real-run setup", "closes on the next deputy start whose start-up checks pass"},
		ExecutionAuthorityInUse:         {"freesided execution resume"},
		ExecutionRigLeasePresent:        {"freesided execution resume", "freesided rig recover"},
		ExecutionSettingsRefused:        {"DETAIL", "freesided settings set", "freesided execution resume"},
		ExecutionNoEnabledProject:       {"-section enabled", "freesided execution resume"},
		ExecutionSeveralProjectsEnabled: {"DETAIL", "freesided settings set", "freesided execution resume"},
		ExecutionCompositionFailed:      {"DETAIL", "freesided execution resume"},
	} {
		_, reason := executionStateDiagnostic(state, "DETAIL")
		for _, want := range wants {
			if !strings.Contains(reason, want) {
				t.Errorf("%s reason %q lacks %q", state, reason, want)
			}
		}
	}
}
