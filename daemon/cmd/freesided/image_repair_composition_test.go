package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

func TestImageRepairProjectIsTheFirstConfiguredProject(t *testing.T) {
	t.Parallel()
	if _, ok := imageRepairProject(nil); ok {
		t.Fatal("no configured project chose one")
	}
	got, ok := imageRepairProject(map[domain.ProjectID]engine.ManualInitiator{"project-b": {}, "project-a": {}})
	if !ok || got != "project-a" {
		t.Fatalf("project = %q %v, want project-a", got, ok)
	}
}

// The upkeep hold names the maintenance task while it exists and is not
// cancelled.
func TestPendingImageRepairTaskFollowsTheTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := storetest.Open(t, t.TempDir()+"/freesided.db", store.Options{})
	t.Cleanup(func() { _ = st.Close() })
	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	refusal, err := domain.NewImageUpkeepRefusal(domain.ImageUpkeepRefusalInput{
		RepositoryID: 7, Repository: "owner/repo", Harness: domain.HarnessClientClaudeCode,
		TipSHA: strings.Repeat("a", 40), MeasuredFromImageID: domain.Digest(contentaddr.Sum([]byte("image"))),
		Class: domain.ImageUpkeepRefusalRepair,
		Findings: []domain.ImageUpkeepClauseFinding{{
			Clause: domain.RebuildRefusalLockfileInconsistent, Detail: "lockfile root disagrees",
		}},
		RecordedAt: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutDevice(ctx, domain.Device{
			ID: "device-1", DisplayName: "Mac", Status: domain.DeviceActive,
			PairedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		}); err != nil {
			return err
		}
		return tx.RegisterProject(ctx, domain.Project{ID: "project-1", Repo: "owner/repo", RepositoryID: 7})
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, err := tx.RecordImageUpkeepRefusal(ctx, refusal)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	prov := domain.KeyProvenance{Source: domain.ProvenancePreset, Digest: domain.Digest(contentaddr.Sum([]byte("policy")))}
	projects := map[domain.ProjectID]engine.ManualInitiator{"project-1": {
		PolicyKeys: []domain.PolicyKey{
			{Key: specify.PolicySpecApproval, Value: "true", Provenance: prov},
			{Key: specify.PolicyMaxIterations, Value: "4", Provenance: prov},
			{Key: specify.PolicyStageActiveTime, Value: "1m", Provenance: prov},
			{Key: specify.PolicyResearchAllowlist, Value: "https://docs.example", Provenance: prov},
			{Key: specify.PolicyResearchMaxBytes, Value: "1024", Provenance: prov},
			{Key: "paths", Value: "package.json", Provenance: prov},
		},
		CommitAuthor: engine.ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 12345},
	}}
	service := signet.NewService(st, signet.WithBlobStore(blobs),
		signet.WithTaskSubmitter(engine.NewTaskSubmitter(blobs, manualInitiatorLookup(projects))))

	if _, pending, err := pendingImageRepairTask(ctx, st, refusal.ID); err != nil || pending {
		t.Fatalf("before any submit: pending = %v err=%v, want none", pending, err)
	}
	project, _ := imageRepairProject(projects)
	submission, err := service.SubmitImageRepairTask(ctx, project, refusal.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task, pending, err := pendingImageRepairTask(ctx, st, refusal.ID); err != nil || !pending ||
		task != submission.TaskID {
		t.Fatalf("open task: pending = %q %v err=%v, want %s", task, pending, err, submission.TaskID)
	}
	state, err := st.ServerState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		_, _, err := tx.StopTask(ctx, domain.StopTaskRequest{
			CommandID: "stop-1", DeviceID: "device-1", TaskID: submission.TaskID, ProjectID: project,
			ExpectedSyncEpoch: state.SyncEpoch, ExpectedEntityVersion: state.Revision,
		}, time.Now().UTC())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, pending, err := pendingImageRepairTask(ctx, st, refusal.ID); err != nil || pending {
		t.Fatalf("cancelled task: pending = %v err=%v, want none", pending, err)
	}
}

// Only a composition that selects each attempt's image submits maintenance
// tasks: without selection a task could not run at its refusal's tip.
func TestImageUpkeepSubmitsMaintenanceOnlyWithImageSelection(t *testing.T) {
	t.Parallel()
	st := storetest.Open(t, t.TempDir()+"/freesided.db", store.Options{})
	t.Cleanup(func() { _ = st.Close() })
	cfg := config{Claude: &claudeDriverConfig{SeedRoot: t.TempDir(), Repo: "owner/repo", RepositoryID: 7}}
	for name, selection := range map[string]engine.ImageSelection{
		"attended": nil,
		"unattended": func(context.Context, domain.InvocationID) (domain.ImageRef, error) {
			return "", nil
		},
	} {
		upkeep, err := composeImageUpkeep(st, signet.NewService(st), cfg,
			&claudeComposition{selectImage: selection}, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		submits := upkeep.SubmitMaintenance != nil && upkeep.MaintenancePending != nil
		if submits != (selection != nil) {
			t.Fatalf("%s composition submits maintenance = %t", name, submits)
		}
	}
}
