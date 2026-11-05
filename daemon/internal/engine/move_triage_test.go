package engine

import (
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

// A move keeps an escalated specifier on a counterpart where the new pool has
// one, and falls back to the pool's standard agent where it has none.
func TestMoveKeepsAnEscalatedSpecifierEscalated(t *testing.T) {
	for _, tc := range []struct {
		name        string
		counterpart bool
		want        string
	}{
		{name: "pool with a counterpart", counterpart: true, want: agentTestSecondEscalatedAgent},
		{name: "pool with none", want: agentTestSecondAgent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgentAdmissionFixture(t)
			_, second := f.addSecondAgent(t)
			f.addEscalatedSpecifier(t, true)
			if tc.counterpart {
				f.addSecondEscalated(t)
			}
			policy := f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour),
				order(domain.RoleSpecifier, agentTestSpecifierAgent, agentTestSecondAgent),
				order(domain.RoleImplementer, agentTestWriterAgent, agentTestSecondAgent))
			triage := f.escalate(t)
			f.moveTask(t, policy, nil, false)
			specifier := f.heads(t)[domain.RoleSpecifier]
			if specifier.Source != domain.TaskLineSourceAlternateAgentMove || specifier.Agent != tc.want ||
				specifier.PredecessorID == nil || *specifier.PredecessorID != triage.ID {
				t.Fatalf("specifier head = %+v, want a move to %s over the triage line", specifier, tc.want)
			}
			admission, err := f.admitRole(t, domain.RoleSpecifier)
			if err != nil || admission.binding.SelectionRecordID != specifier.ID ||
				admission.binding.EnrollmentID != second.ID {
				t.Fatalf("admission = %+v, %v; want the moved line on the second pool", admission.binding, err)
			}
		})
	}
}
