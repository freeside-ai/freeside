package engine

import (
	"context"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// A move keeps an escalated specifier on the escalated line the moved-to
// enrollment has, and falls back to the specifier's standard lineup model and
// effort on that enrollment where it has none (#2735).
func TestMoveKeepsAnEscalatedSpecifierEscalated(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name        string
		counterpart bool
		model       string
	}{
		{name: "pool with a counterpart", counterpart: true, model: agentTestEscalatedModel},
		{name: "pool with none", model: "claude-specifier-test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAgentAdmissionFixture(t)
			_, second := f.addSecondAgent(t)
			f.addEscalatedSpecifier(t, true)
			if tc.counterpart {
				f.addSecondEscalated(t)
			}
			policy := f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour),
				order(domain.RoleSpecifier, agentTestEnrollment, agentTestSecondEnrollment),
				order(domain.RoleImplementer, agentTestEnrollment, agentTestSecondEnrollment))
			triage := f.escalate(t)
			f.moveTask(t, policy, nil, false)
			specifier := f.heads(t)[domain.RoleSpecifier]
			want := domain.TaskLineSelection{EnrollmentID: second.ID, ModelID: tc.model, Effort: string(domain.EffortHigh)}
			if specifier.Source != domain.TaskLineSourceAlternateAgentMove || specifier.Selection == nil ||
				*specifier.Selection != want || specifier.PredecessorID == nil || *specifier.PredecessorID != triage.ID {
				t.Fatalf("specifier head = %+v, want a move to %+v over the triage line", specifier, want)
			}
			admission, err := f.admitRole(t, domain.RoleSpecifier)
			if err != nil || admission.binding.SelectionRecordID != specifier.ID ||
				admission.binding.EnrollmentID != second.ID {
				t.Fatalf("admission = %+v, %v; want the moved line on the second pool", admission.binding, err)
			}
		})
	}
}

// TestOnlyAV2MoveSkipsTheCounterpartCheck: a v2 record's escalated
// specifier line on an enrollment the policy orders needs no counterpart
// check, since its anchor holds it to that enrollment's lineup tuple. A v1
// record's line, which names a tree agent the policy does not order, keeps
// the check, as before #2735.
func TestOnlyAV2MoveSkipsTheCounterpartCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAgentAdmissionFixture(t)
	f.addSecondAgent(t)
	f.addEscalatedSpecifier(t, true)
	f.addSecondEscalated(t)
	policy := f.setRoutingPolicy(t, agentTestAt.Add(-time.Hour),
		order(domain.RoleSpecifier, agentTestEnrollment, agentTestSecondEnrollment),
		order(domain.RoleImplementer, agentTestEnrollment, agentTestSecondEnrollment))
	f.escalate(t)
	f.moveTask(t, policy, nil, false)
	v2Line := f.heads(t)[domain.RoleSpecifier]
	var v1 domain.AlternateAgentRetry
	if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		v1, err = tx.GetAlternateAgentRetry(ctx, domain.Digest(v2Line.SetBy))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	v1.Selection, v1.Agent, v1.CommandID = nil, agentTestSecondAgent, "cmd-move-v1"
	var err error
	if v1.ID, err = v1.ComputeID(); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RecordAlternateAgentRetry(ctx, v1)
	}); err != nil {
		t.Fatal(err)
	}
	v1Line := v2Line
	v1Line.SetBy, v1Line.Selection, v1Line.Agent = string(v1.ID), nil, agentTestSecondEscalatedAgent
	bases := func(line domain.TaskLine) []string {
		var got []string
		if err := f.store.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			got, _, err = counterpartBases(ctx, tx, f.selection.Tree, domain.LineupSelection{}, line)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := bases(v2Line); got != nil {
		t.Fatalf("v2 line counterpart bases = %v, want none", got)
	}
	if got := bases(v1Line); len(got) == 0 {
		t.Fatal("v1 line counterpart bases are empty, want the policy's enrollments")
	}
}
