package domain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

func TestAgentBaseBumpSubmissionIdentityNamesOneOccurrence(t *testing.T) {
	t.Parallel()
	nominal := time.Date(2026, 11, 26, 12, 0, 0, 5, time.UTC)
	for _, scheduleID := range []domain.ScheduleID{"schedule-agent-base-bump", "schedule:with:colons"} {
		commandID := domain.AgentBaseBumpSubmissionIdentity(scheduleID, 3, nominal)
		gotID, gotGeneration, gotNominal, ok := domain.ParseAgentBaseBumpSubmissionIdentity(commandID)
		if !ok || gotID != scheduleID || gotGeneration != 3 || !gotNominal.Equal(nominal) {
			t.Fatalf("parse %q = %q %d %v %v", commandID, gotID, gotGeneration, gotNominal, ok)
		}
		if domain.IsImageUpkeepSubmission(commandID) {
			t.Fatalf("%q reads as an image-upkeep submission", commandID)
		}
	}
	for _, commandID := range []string{
		"daemon:agent-base-bump:",
		"daemon:agent-base-bump:schedule",
		"daemon:agent-base-bump::3:5",
		"daemon:agent-base-bump:schedule:x:5",
		"daemon:agent-base-bump:schedule:3:x",
		"daemon:agent-base-bump:schedule:03:5",
		"daemon:agent-base-bump:schedule:3:+5",
		"daemon:image-upkeep:sha256:" + strings.Repeat("a", 64),
		"client:agent-base-bump:schedule:3:5",
	} {
		if _, _, _, ok := domain.ParseAgentBaseBumpSubmissionIdentity(commandID); ok {
			t.Errorf("parse %q = ok", commandID)
		}
	}
}
