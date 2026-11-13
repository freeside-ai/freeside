package store_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

var workflowPolicySetAt = time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)

func readWorkflowPolicyLayers(t *testing.T, s *store.Store) domain.WorkflowPolicyLayers {
	t.Helper()
	ctx := context.Background()
	var layers domain.WorkflowPolicyLayers
	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		layers, err = tx.WorkflowPolicyLayers(ctx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return layers
}

func appendWorkflowPolicy(t *testing.T, s *store.Store, in domain.WorkflowPolicyInput) domain.WorkflowPolicyRevision {
	t.Helper()
	ctx := context.Background()
	var revision domain.WorkflowPolicyRevision
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		var err error
		revision, err = tx.AppendWorkflowPolicyRevision(ctx, in, workflowPolicySetAt)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return revision
}

// TestWorkflowPolicyLatestRevisionPerLayerWins checks that revisions number
// across layers, the latest revision of a layer is its value, a null value
// clears it, and each read decodes what was written.
func TestWorkflowPolicyLatestRevisionPerLayerWins(t *testing.T) {
	t.Parallel()
	s := storetest.Open(t, filepath.Join(t.TempDir(), "workflow.db"), store.Options{})
	if layers := readWorkflowPolicyLayers(t, s); layers.Revision() != 0 {
		t.Fatalf("empty store at revision %d", layers.Revision())
	}
	project := domain.ProjectID("proj-1")
	general := domain.WorkflowPolicyInput{
		Scope: domain.WorkflowPolicyScopeGeneral, Key: domain.WorkflowFollowUpMaxPerDay,
		Value: new("12"), SetBy: "device:device-1",
	}
	first := appendWorkflowPolicy(t, s, general)
	general.Value = new("20")
	second := appendWorkflowPolicy(t, s, general)
	override := domain.WorkflowPolicyInput{
		Scope: domain.WorkflowPolicyScopeProject, ProjectID: &project, Key: domain.WorkflowFollowUpMaxPerDay,
		Value: new("3"), SetBy: "device:device-2",
	}
	third := appendWorkflowPolicy(t, s, override)
	if first.Revision != 1 || second.Revision != 2 || third.Revision != 3 {
		t.Fatalf("revisions %d %d %d", first.Revision, second.Revision, third.Revision)
	}
	layers := readWorkflowPolicyLayers(t, s)
	if got := layers.ProjectSetting(project, domain.WorkflowFollowUpMaxPerDay); got.Value != "3" ||
		got.Origin != domain.WorkflowPolicyOriginProject || got.Revision.SetBy != "device:device-2" {
		t.Fatalf("project setting %+v", got)
	}
	if got := layers.GeneralSetting(domain.WorkflowFollowUpMaxPerDay); got.Value != "20" ||
		!reflect.DeepEqual(*got.Revision, second) {
		t.Fatalf("general setting %+v, want revision %+v", got, second)
	}

	override.Value = nil
	appendWorkflowPolicy(t, s, override)
	layers = readWorkflowPolicyLayers(t, s)
	if got := layers.ProjectSetting(project, domain.WorkflowFollowUpMaxPerDay); got.Value != "20" ||
		got.Origin != domain.WorkflowPolicyOriginGeneral {
		t.Fatalf("cleared project setting %+v, want the general value", got)
	}
	if layers.Revision() != 4 {
		t.Fatalf("revision %d after a clear", layers.Revision())
	}
}

func TestWorkflowPolicyAppendRefusesMalformed(t *testing.T) {
	t.Parallel()
	s := storetest.Open(t, filepath.Join(t.TempDir(), "workflow.db"), store.Options{})
	ctx := context.Background()
	err := s.Write(ctx, func(tx *store.WriteTx) error {
		_, err := tx.AppendWorkflowPolicyRevision(ctx, domain.WorkflowPolicyInput{
			Scope: domain.WorkflowPolicyScopeGeneral, Key: domain.WorkflowFollowUpMaxPerDay,
			Value: new("0"), SetBy: "device:device-1",
		}, workflowPolicySetAt)
		return err
	})
	if !errors.Is(err, domain.ErrInvalidWorkflowPolicyValue) {
		t.Fatalf("err = %v", err)
	}
	if layers := readWorkflowPolicyLayers(t, s); layers.Revision() != 0 {
		t.Fatalf("a refused append left revision %d", layers.Revision())
	}
}
