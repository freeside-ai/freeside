package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func setWorkflowPolicy(
	t *testing.T, s *store.Store, scope domain.WorkflowPolicyScope, project *domain.ProjectID,
	key domain.WorkflowPolicyKey, value *string,
) engine.SetWorkflowPolicyResult {
	t.Helper()
	var result engine.SetWorkflowPolicyResult
	if err := s.Write(context.Background(), func(tx *store.WriteTx) error {
		var err error
		result, err = engine.SetWorkflowPolicyTx(context.Background(), tx, engine.SetWorkflowPolicyRequest{
			Scope: scope, ProjectID: project, Key: key, Value: value, SetBy: "device:device-1",
		}, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return result
}

func runPolicyKey(t *testing.T, s *store.Store, run domain.RunID, key domain.WorkflowPolicyKey) (domain.PolicyKey, bool) {
	t.Helper()
	var policy domain.ResolvedPolicy
	if err := s.Read(context.Background(), func(tx *store.ReadTx) error {
		var err error
		policy, err = tx.GetResolvedPolicy(context.Background(), run)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, k := range policy.Keys {
		if k.Key == string(key) {
			return k, true
		}
	}
	return domain.PolicyKey{}, false
}

// TestSetWorkflowPolicyRecordsOnlyAChange: setting the value a layer already
// holds, or clearing a layer that holds none, records nothing.
func TestSetWorkflowPolicyRecordsOnlyAChange(t *testing.T) {
	t.Parallel()
	_, s, _ := newSubmitTaskHarness(t)
	general := domain.WorkflowPolicyScopeGeneral
	if got := setWorkflowPolicy(t, s, general, nil, domain.WorkflowGateIssueComment, nil); got.Recorded || got.Revision != 0 {
		t.Fatalf("clear of an unset layer = %+v", got)
	}
	if got := setWorkflowPolicy(t, s, general, nil, domain.WorkflowGateIssueComment, new("true")); !got.Recorded || got.Revision != 1 {
		t.Fatalf("set = %+v", got)
	}
	if got := setWorkflowPolicy(t, s, general, nil, domain.WorkflowGateIssueComment, new("true")); got.Recorded || got.Revision != 1 {
		t.Fatalf("repeat = %+v", got)
	}
	if got := setWorkflowPolicy(t, s, general, nil, domain.WorkflowGateIssueComment, nil); !got.Recorded || got.Revision != 2 {
		t.Fatalf("clear = %+v", got)
	}
}

// TestSubmitTaskResolvesTheWorkflowPolicyLayers: a submitted task's policy
// carries every exposed key from the layers, with the winning layer as its
// provenance, and a submission after a settings change derives a new run
// under the new value while the first run keeps the policy it began with.
func TestSubmitTaskResolvesTheWorkflowPolicyLayers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	service, s, _ := newSubmitTaskHarness(t)
	project := domain.ProjectID("project-1")
	setWorkflowPolicy(t, s, domain.WorkflowPolicyScopeGeneral, nil, domain.WorkflowGateSourceIssueClosure, new("true"))

	const source = "Record the layers."
	first, err := service.Submit(ctx, submitCmd("cmd-1", "project-1", source, ""))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	firstRun := first.Submission.SpecificationRunID
	if got, ok := runPolicyKey(t, s, firstRun, domain.WorkflowGateSourceIssueClosure); !ok || got.Value != "true" ||
		got.Provenance.Source != domain.ProvenanceGeneral {
		t.Fatalf("closure gate = %+v, %t; want true from the general layer", got, ok)
	}
	if got, ok := runPolicyKey(t, s, firstRun, domain.WorkflowFollowUpMaxPerDay); !ok || got.Value != "10" ||
		got.Provenance.Source != domain.ProvenancePreset {
		t.Fatalf("daily cap = %+v, %t; want the default", got, ok)
	}

	setWorkflowPolicy(t, s, domain.WorkflowPolicyScopeProject, &project, domain.WorkflowGateSourceIssueClosure, new("false"))
	second, err := service.Submit(ctx, submitCmd("cmd-2", "project-1", source, ""))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Submission.SpecificationRunID == firstRun {
		t.Fatal("a submission under changed settings reused the first run")
	}
	if got, _ := runPolicyKey(t, s, second.Submission.SpecificationRunID, domain.WorkflowGateSourceIssueClosure); got.Value != "false" ||
		got.Provenance.Source != domain.ProvenanceProject {
		t.Fatalf("second closure gate = %+v, want false from the project layer", got)
	}
	if got, _ := runPolicyKey(t, s, firstRun, domain.WorkflowGateSourceIssueClosure); got.Value != "true" {
		t.Fatalf("first run's closure gate = %+v, want the value it began with", got)
	}
	replay, err := service.Submit(ctx, submitCmd("cmd-1", "project-1", source, ""))
	if err != nil || replay.Submission.SpecificationRunID != firstRun {
		t.Fatalf("replay = %+v, %v; want the first run", replay.Submission, err)
	}
}
