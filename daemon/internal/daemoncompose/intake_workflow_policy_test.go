package daemoncompose

import (
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func (f *intakeFixture) setWorkflowPolicy(
	t *testing.T, project *domain.ProjectID, key domain.WorkflowPolicyKey, value string,
) {
	t.Helper()
	scope := domain.WorkflowPolicyScopeGeneral
	if project != nil {
		scope = domain.WorkflowPolicyScopeProject
	}
	if err := f.store.Write(t.Context(), func(tx *store.WriteTx) error {
		_, err := engine.SetWorkflowPolicyTx(t.Context(), tx, engine.SetWorkflowPolicyRequest{
			Scope: scope, ProjectID: project, Key: key, Value: &value, SetBy: "device:device-1",
		}, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func policyKeyNamed(keys []domain.PolicyKey, key domain.WorkflowPolicyKey) (domain.PolicyKey, bool) {
	for _, k := range keys {
		if k.Key == string(key) {
			return k, true
		}
	}
	return domain.PolicyKey{}, false
}

// TestIntakeResolvesTheWorkflowPolicyLayers: a label-intake task's policy
// takes each exposed key from the layers, with the winning layer as its
// provenance, and admission re-run after a crash keeps the workflow keys the
// reserved run was resolved with even when a setting changed in between.
func TestIntakeResolvesTheWorkflowPolicyLayers(t *testing.T) {
	t.Parallel()
	f := newIntakeFixture(t)
	project := intakeTestProj
	f.setWorkflowPolicy(t, nil, domain.WorkflowGateSourceIssueClosure, "true")
	f.setWorkflowPolicy(t, &project, domain.WorkflowFollowUpMaxPerRun, "1")
	init := intakeInitiatorFor(t, domain.InitiatorModePropose, domain.ProvenanceOverride, 5)
	r := f.reconciler([]IntakeInitiator{init}, labeledOpen(7), nil)
	r.reconcile(t.Context(), nil)
	o := f.latestOccurrence(t, 7)
	if o.Admission == nil {
		t.Fatal("occurrence was not admitted")
	}
	runID := o.Admission.Subject.SpecificationRunID
	var resolved domain.ResolvedPolicy
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		resolved, err = tx.GetResolvedPolicy(t.Context(), runID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[domain.WorkflowPolicyKey]struct {
		value  string
		source domain.ProvenanceSource
	}{
		domain.WorkflowGateSourceIssueClosure: {"true", domain.ProvenanceGeneral},
		domain.WorkflowFollowUpMaxPerRun:      {"1", domain.ProvenanceProject},
		domain.WorkflowGateIssueComment:       {"false", domain.ProvenancePreset},
	} {
		if got, ok := policyKeyNamed(resolved.Keys, key); !ok || got.Value != want.value || got.Provenance.Source != want.source {
			t.Errorf("%s = %+v, %t; want %s from %s", key, got, ok, want.value, want.source)
		}
	}

	f.setWorkflowPolicy(t, nil, domain.WorkflowGateSourceIssueClosure, "false")
	keys, err := r.intakePolicyKeys(t.Context(), init, runID)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := domain.NewResolvedPolicy(runID, keys)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Digest != resolved.Digest {
		t.Fatalf("recovered policy %+v, want the reserved run's %+v", recovered.Keys, resolved.Keys)
	}
}
