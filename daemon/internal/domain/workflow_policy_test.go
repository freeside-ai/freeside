package domain_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
)

func workflowRevision(
	t *testing.T, revision int, scope domain.WorkflowPolicyScope, project *domain.ProjectID,
	key domain.WorkflowPolicyKey, value *string,
) domain.WorkflowPolicyRevision {
	t.Helper()
	r, err := domain.NewWorkflowPolicyRevision(domain.WorkflowPolicyInput{
		Scope: scope, ProjectID: project, Key: key, Value: value, SetBy: "device:device-1",
	}, revision, receiptAt)
	if err != nil {
		t.Fatalf("NewWorkflowPolicyRevision: %v", err)
	}
	return r
}

// TestWorkflowPolicyRevisionGolden pins a revision's stored body; each
// fixture is also a validation-positive case.
func TestWorkflowPolicyRevisionGolden(t *testing.T) {
	for _, tc := range []struct {
		name     string
		revision domain.WorkflowPolicyRevision
	}{
		{"workflow_policy_revision_general", workflowRevision(t, 1, domain.WorkflowPolicyScopeGeneral, nil,
			domain.WorkflowFollowUpMaxPerDay, new("12"))},
		{"workflow_policy_revision_project_clear", workflowRevision(t, 2, domain.WorkflowPolicyScopeProject,
			new(domain.ProjectID("proj-1")), domain.WorkflowGateSourceIssueClosure, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.revision.Validate(); err != nil {
				t.Fatalf("Validate: %v", err)
			}
			body, err := json.MarshalIndent(tc.revision, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			golden.Assert(t, tc.name, append(body, '\n'))
		})
	}
}

func TestWorkflowPolicyValues(t *testing.T) {
	for _, tc := range []struct {
		key   domain.WorkflowPolicyKey
		value string
		ok    bool
	}{
		{domain.WorkflowGateFollowUpFiling, "true", true},
		{domain.WorkflowGateIssueComment, "false", true},
		{domain.WorkflowGateSourceIssueClosure, "1", false},
		{domain.WorkflowGateSourceIssueClosure, "True", false},
		{domain.WorkflowFollowUpMaxPerDay, "10", true},
		{domain.WorkflowFollowUpMaxPerRun, "0", false},
		{domain.WorkflowFollowUpMaxDepth, "-1", false},
		{domain.WorkflowFollowUpMaxDepth, "+2", false},
		{domain.WorkflowFollowUpMaxDepth, "02", false},
		{domain.WorkflowFollowUpMaxPerDay, "ten", false},
		{"follow_up_filing.settle_interval", "10m", false},
	} {
		err := tc.key.ValidateValue(tc.value)
		if (err == nil) != tc.ok {
			t.Errorf("%s=%q: err %v, want ok %v", tc.key, tc.value, err, tc.ok)
		}
	}
	for _, key := range domain.AllWorkflowPolicyKeys {
		if err := key.ValidateValue(key.Default()); err != nil {
			t.Errorf("%s default %q: %v", key, key.Default(), err)
		}
	}
}

func TestWorkflowPolicyRevisionRefusals(t *testing.T) {
	project := new(domain.ProjectID("proj-1"))
	for _, tc := range []struct {
		name string
		in   domain.WorkflowPolicyInput
		want error
	}{
		{
			"unknown scope",
			domain.WorkflowPolicyInput{Scope: "team", Key: domain.WorkflowGateIssueComment, SetBy: "x"},
			domain.ErrInvalidWorkflowPolicyScope,
		},
		{"project scope without project", domain.WorkflowPolicyInput{
			Scope: domain.WorkflowPolicyScopeProject, Key: domain.WorkflowGateIssueComment, SetBy: "x",
		}, domain.ErrWorkflowPolicyProjectMismatch},
		{"general scope with project", domain.WorkflowPolicyInput{
			Scope: domain.WorkflowPolicyScopeGeneral, ProjectID: project, Key: domain.WorkflowGateIssueComment, SetBy: "x",
		}, domain.ErrWorkflowPolicyProjectMismatch},
		{"empty project", domain.WorkflowPolicyInput{
			Scope: domain.WorkflowPolicyScopeProject, ProjectID: new(domain.ProjectID("")),
			Key: domain.WorkflowGateIssueComment, SetBy: "x",
		}, domain.ErrWorkflowPolicyProjectMismatch},
		{"unknown key", domain.WorkflowPolicyInput{
			Scope: domain.WorkflowPolicyScopeGeneral, Key: "paths", SetBy: "x",
		}, domain.ErrInvalidWorkflowPolicyKey},
		{"bad value", domain.WorkflowPolicyInput{
			Scope: domain.WorkflowPolicyScopeGeneral, Key: domain.WorkflowFollowUpMaxDepth, Value: new("0"), SetBy: "x",
		}, domain.ErrInvalidWorkflowPolicyValue},
		{"no set_by", domain.WorkflowPolicyInput{
			Scope: domain.WorkflowPolicyScopeGeneral, Key: domain.WorkflowFollowUpMaxDepth, Value: new("3"),
		}, domain.ErrEmptyField},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := domain.NewWorkflowPolicyRevision(tc.in, 1, receiptAt); !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
	if _, err := domain.NewWorkflowPolicyRevision(domain.WorkflowPolicyInput{
		Scope: domain.WorkflowPolicyScopeGeneral, Key: domain.WorkflowFollowUpMaxDepth, SetBy: "x",
	}, 0, receiptAt); !errors.Is(err, domain.ErrNonPositive) {
		t.Fatalf("revision 0: %v", err)
	}
	local := time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600))
	r, err := domain.NewWorkflowPolicyRevision(domain.WorkflowPolicyInput{
		Scope: domain.WorkflowPolicyScopeGeneral, Key: domain.WorkflowFollowUpMaxDepth, SetBy: "x",
	}, 1, local)
	if err != nil || r.SetAt.Location() != time.UTC {
		t.Fatalf("set_at not normalized: %v %v", r.SetAt, err)
	}
}

// TestWorkflowPolicyLayersPrecedence checks project over general over
// default, a nil value clearing a layer, and each key's provenance.
func TestWorkflowPolicyLayersPrecedence(t *testing.T) {
	proj := domain.ProjectID("proj-1")
	other := domain.ProjectID("proj-2")
	general := workflowRevision(t, 1, domain.WorkflowPolicyScopeGeneral, nil, domain.WorkflowGateFollowUpFiling, new("true"))
	project := workflowRevision(t, 2, domain.WorkflowPolicyScopeProject, &proj, domain.WorkflowGateFollowUpFiling, new("false"))
	cap := workflowRevision(t, 3, domain.WorkflowPolicyScopeProject, &proj, domain.WorkflowFollowUpMaxDepth, new("4"))
	cleared := workflowRevision(t, 4, domain.WorkflowPolicyScopeProject, &proj, domain.WorkflowFollowUpMaxDepth, nil)
	layers, err := domain.NewWorkflowPolicyLayers([]domain.WorkflowPolicyRevision{general, project, cap, cleared})
	if err != nil {
		t.Fatal(err)
	}
	if layers.Revision() != 4 {
		t.Fatalf("revision %d", layers.Revision())
	}
	check := func(got domain.WorkflowPolicySetting, value string, origin domain.WorkflowPolicyOrigin, rev int) {
		t.Helper()
		gotRev := 0
		if got.Revision != nil {
			gotRev = got.Revision.Revision
		}
		if got.Value != value || got.Origin != origin || gotRev != rev {
			t.Fatalf("%s = %q %s rev %d, want %q %s rev %d", got.Key, got.Value, got.Origin, gotRev, value, origin, rev)
		}
	}
	check(layers.GeneralSetting(domain.WorkflowGateFollowUpFiling), "true", domain.WorkflowPolicyOriginGeneral, 1)
	check(layers.ProjectSetting(proj, domain.WorkflowGateFollowUpFiling), "false", domain.WorkflowPolicyOriginProject, 2)
	check(layers.ProjectSetting(other, domain.WorkflowGateFollowUpFiling), "true", domain.WorkflowPolicyOriginGeneral, 1)
	check(layers.ProjectSetting(proj, domain.WorkflowFollowUpMaxDepth), "2", domain.WorkflowPolicyOriginDefault, 0)
	check(layers.ProjectSetting(proj, domain.WorkflowGateIssueComment), "false", domain.WorkflowPolicyOriginDefault, 0)

	keys := layers.PolicyKeys(proj)
	if len(keys) != len(domain.AllWorkflowPolicyKeys) {
		t.Fatalf("%d keys", len(keys))
	}
	want := map[string]domain.ProvenanceSource{
		string(domain.WorkflowGateFollowUpFiling): domain.ProvenanceProject,
		string(domain.WorkflowFollowUpMaxDepth):   domain.ProvenancePreset,
	}
	for _, k := range keys {
		if err := k.Validate(); err != nil {
			t.Fatalf("key %s: %v", k.Key, err)
		}
		if source, ok := want[k.Key]; ok && k.Provenance.Source != source {
			t.Fatalf("key %s provenance %s, want %s", k.Key, k.Provenance.Source, source)
		}
		if k.Key == string(domain.WorkflowGateFollowUpFiling) && k.Provenance.Digest != project.Digest() {
			t.Fatalf("project key digest %s, want the revision's", k.Provenance.Digest)
		}
	}
	otherKeys := layers.PolicyKeys(other)
	for _, k := range otherKeys {
		if k.Key == string(domain.WorkflowGateFollowUpFiling) && k.Provenance.Source != domain.ProvenanceGeneral {
			t.Fatalf("other project gate provenance %s", k.Provenance.Source)
		}
	}

	if _, err := domain.NewWorkflowPolicyLayers([]domain.WorkflowPolicyRevision{project, general}); !errors.Is(err, domain.ErrKeysNotCanonical) {
		t.Fatalf("out of order: %v", err)
	}
}

// TestWorkflowPolicyOverlay checks that an exposed key never resolves from
// the base keys and every other base key passes through.
func TestWorkflowPolicyOverlay(t *testing.T) {
	provenance := domain.KeyProvenance{Source: domain.ProvenanceOverride, Digest: "sha256:file"}
	base := []domain.PolicyKey{
		{Key: "paths", Value: "daemon/**", Provenance: provenance},
		{Key: string(domain.WorkflowGateSourceIssueClosure), Value: "true", Provenance: provenance},
		{Key: "follow_up_filing.settle_interval", Value: "5m", Provenance: provenance},
	}
	layers, err := domain.NewWorkflowPolicyLayers(nil)
	if err != nil {
		t.Fatal(err)
	}
	keys := layers.Overlay("proj-1", base)
	policy, err := domain.NewResolvedPolicy("run-1", keys)
	if err != nil {
		t.Fatalf("NewResolvedPolicy: %v", err)
	}
	values := map[string]domain.PolicyKey{}
	for _, k := range policy.Keys {
		values[k.Key] = k
	}
	if got := values[string(domain.WorkflowGateSourceIssueClosure)]; got.Value != "false" || got.Provenance.Source != domain.ProvenancePreset {
		t.Fatalf("closure gate %+v resolved from the base", got)
	}
	if values["paths"].Value != "daemon/**" || values["follow_up_filing.settle_interval"].Value != "5m" {
		t.Fatalf("base keys lost: %+v", values)
	}
	if len(base) != 3 || base[1].Value != "true" {
		t.Fatal("Overlay mutated its base")
	}
}
