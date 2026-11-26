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

// TestResolveRunWorkflowPolicyKeys covers both arms of
// ResolveRunWorkflowPolicyKeys: an unstored run resolves from the store's
// layers (project over general over default, never from base), while a run
// with a stored policy keeps its stored workflow keys unchanged regardless of
// current layers, including a pre-layer file key with override provenance.
func TestResolveRunWorkflowPolicyKeys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, s, _ := newSubmitTaskHarness(t)
	project := domain.ProjectID("project-1")
	baseProvenance := domain.KeyProvenance{
		Source: domain.ProvenanceOverride,
		Digest: "sha256:1111111111111111111111111111111111111111111111111111111111111111",
	}
	base := []domain.PolicyKey{
		{Key: "paths", Value: "daemon/**", Provenance: baseProvenance},
		{Key: string(domain.WorkflowGateSourceIssueClosure), Value: "false", Provenance: baseProvenance},
	}

	setWorkflowPolicy(t, s, domain.WorkflowPolicyScopeGeneral, nil, domain.WorkflowGateIssueComment, new("true"))
	setWorkflowPolicy(t, s, domain.WorkflowPolicyScopeProject, &project, domain.WorkflowGateSourceIssueClosure, new("true"))

	// Unstored arm: overlays project > general > default and drops base's workflow key.
	var unstored []domain.PolicyKey
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		unstored, err = engine.ResolveRunWorkflowPolicyKeys(ctx, tx, project, base, "run-unstored")
		return err
	}); err != nil {
		t.Fatalf("unstored resolve: %v", err)
	}
	byKey := make(map[string]domain.PolicyKey, len(unstored))
	for _, k := range unstored {
		byKey[k.Key] = k
	}
	if got := byKey["paths"]; got.Value != "daemon/**" || got.Provenance != baseProvenance {
		t.Fatalf("unstored paths = %+v, want base key unchanged", got)
	}
	if got := byKey[string(domain.WorkflowGateSourceIssueClosure)]; got.Value != "true" || got.Provenance.Source != domain.ProvenanceProject {
		t.Fatalf("unstored closure gate = %+v, want true from project layer", got)
	}
	if got := byKey[string(domain.WorkflowGateIssueComment)]; got.Value != "true" || got.Provenance.Source != domain.ProvenanceGeneral {
		t.Fatalf("unstored issue comment gate = %+v, want true from general layer", got)
	}
	if got := byKey[string(domain.WorkflowFollowUpMaxPerDay)]; got.Value != "10" || got.Provenance.Source != domain.ProvenancePreset {
		t.Fatalf("unstored daily cap = %+v, want default 10 from preset", got)
	}

	// Stored arm: a historical run holding gates.source_issue_closure with
	// override provenance keeps that exact key even after layers change.
	storedClosure := domain.PolicyKey{
		Key:        string(domain.WorkflowGateSourceIssueClosure),
		Value:      "true",
		Provenance: baseProvenance,
	}
	const storedRunID = domain.RunID("run-stored-historical")
	storedPolicy, err := domain.NewResolvedPolicy(storedRunID, []domain.PolicyKey{
		{Key: "paths", Value: "daemon/**", Provenance: baseProvenance},
		storedClosure,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, domain.Run{
			ID:           storedRunID,
			ProjectID:    project,
			SpecDigest:   "sha256:2222222222222222222222222222222222222222222222222222222222222222",
			PolicyDigest: storedPolicy.Digest,
			Stages:       []domain.Stage{},
		}); err != nil {
			return err
		}
		return tx.PutResolvedPolicy(ctx, storedPolicy)
	}); err != nil {
		t.Fatal(err)
	}
	setWorkflowPolicy(t, s, domain.WorkflowPolicyScopeProject, &project, domain.WorkflowGateSourceIssueClosure, new("false"))

	var replayed []domain.PolicyKey
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		replayed, err = engine.ResolveRunWorkflowPolicyKeys(ctx, tx, project, base, storedRunID)
		return err
	}); err != nil {
		t.Fatalf("stored resolve: %v", err)
	}
	replayedPolicy, err := domain.NewResolvedPolicy(storedRunID, replayed)
	if err != nil {
		t.Fatal(err)
	}
	if replayedPolicy.Digest != storedPolicy.Digest {
		t.Fatalf("stored arm digest = %s, want %s (keys=%+v)", replayedPolicy.Digest, storedPolicy.Digest, replayedPolicy.Keys)
	}
	replayedByKey := make(map[string]domain.PolicyKey, len(replayed))
	for _, k := range replayed {
		replayedByKey[k.Key] = k
	}
	if got := replayedByKey[string(domain.WorkflowGateSourceIssueClosure)]; got != storedClosure {
		t.Fatalf("stored arm closure gate = %+v, want %+v", got, storedClosure)
	}
	if _, injected := replayedByKey[string(domain.WorkflowGateIssueComment)]; injected {
		t.Fatalf("stored arm injected layer key %s into historical policy", domain.WorkflowGateIssueComment)
	}
}
