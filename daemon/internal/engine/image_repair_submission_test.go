package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

const (
	repairTestProject      domain.ProjectID = "project-1"
	repairTestRepository                    = "owner/repo"
	repairTestRepositoryID int64            = 7
)

// repairTestRefusal builds a recorded-shape refusal at tip for the test
// repository unless repositoryID says otherwise.
func repairTestRefusal(
	t *testing.T, class domain.ImageUpkeepRefusalClass, repositoryID int64, tip string,
) domain.ImageUpkeepRefusal {
	t.Helper()
	return repairTestRefusalFrom(t, class, repositoryID, tip, "pointed image", "lockfile root disagrees")
}

// repairTestRefusalFrom builds a refusal measured from the image image names,
// whose finding carries detail.
func repairTestRefusalFrom(
	t *testing.T, class domain.ImageUpkeepRefusalClass, repositoryID int64, tip, image, detail string,
) domain.ImageUpkeepRefusal {
	t.Helper()
	var findings []domain.ImageUpkeepClauseFinding
	if class != domain.ImageUpkeepRefusalHostFault {
		findings = []domain.ImageUpkeepClauseFinding{{
			Clause: domain.RebuildRefusalLockfileInconsistent, Detail: detail,
		}}
	}
	refusal, err := domain.NewImageUpkeepRefusal(domain.ImageUpkeepRefusalInput{
		RepositoryID: repositoryID, Repository: repairTestRepository, Harness: domain.HarnessClientClaudeCode,
		TipSHA: tip, MeasuredFromImageID: domain.Digest(contentaddr.Sum([]byte(image))),
		Class: class, Findings: findings, RecordedAt: time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	return refusal
}

func repairTestInitiator() ManualInitiator {
	prov := domain.KeyProvenance{
		Source: domain.ProvenancePreset, Digest: domain.Digest(contentaddr.Sum([]byte("image-repair-policy"))),
	}
	return ManualInitiator{
		PolicyKeys: []domain.PolicyKey{
			{Key: specify.PolicySpecApproval, Value: "true", Provenance: prov},
			{Key: specify.PolicyMaxIterations, Value: "4", Provenance: prov},
			{Key: specify.PolicyStageActiveTime, Value: "1m", Provenance: prov},
			{Key: specify.PolicyResearchAllowlist, Value: "https://docs.example", Provenance: prov},
			{Key: specify.PolicyResearchMaxBytes, Value: "1024", Provenance: prov},
			{Key: "paths", Value: "package.json,package-lock.json", Provenance: prov},
		},
		CommitAuthor: ProductionCommitAuthor{AppSlug: "freeside-test", BotUserID: 12345},
	}
}

// newImageRepairHarness opens a store with the test project registered on
// its repository, the given refusals recorded, and a signet service whose
// submitter is configured for configured projects only.
func newImageRepairHarness(
	t *testing.T, configured []domain.ProjectID, refusals ...domain.ImageUpkeepRefusal,
) (*signet.Service, *store.Store) {
	t.Helper()
	ctx := context.Background()
	st := storetest.Open(t, t.TempDir()+"/engine.db", store.Options{})
	t.Cleanup(func() { _ = st.Close() })
	blobs, err := signet.NewBlobStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.RegisterProject(ctx, domain.Project{
			ID: repairTestProject, Repo: repairTestRepository, RepositoryID: repairTestRepositoryID,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		for _, refusal := range refusals {
			if _, err := tx.RecordImageUpkeepRefusal(ctx, refusal); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	submitter := NewTaskSubmitter(blobs, func(project domain.ProjectID) (ManualInitiator, bool) {
		for _, id := range configured {
			if id == project {
				return repairTestInitiator(), true
			}
		}
		return ManualInitiator{}, false
	})
	return signet.NewService(st, signet.WithBlobStore(blobs), signet.WithTaskSubmitter(submitter)), st
}

// TestSubmitImageRepairTaskOpensOneTask: a repair refusal opens one task with
// a daemon origin, no device, the refusal as its source, no bound issue, and
// bound_pr_merged completion, and submitting it again converges on it.
func TestSubmitImageRepairTaskOpensOneTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	refusal := repairTestRefusal(t, domain.ImageUpkeepRefusalRepair, repairTestRepositoryID, strings.Repeat("a", 40))
	service, st := newImageRepairHarness(t, []domain.ProjectID{repairTestProject}, refusal)

	submission, err := service.SubmitImageRepairTask(ctx, repairTestProject, refusal.ID)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	identity := domain.ImageUpkeepSubmissionIdentity(refusal.ID)
	if submission.CommandID != identity || submission.Origin != domain.TaskSubmissionOriginDaemon ||
		submission.DeviceID != nil || submission.ProjectID != repairTestProject ||
		submission.SourceDigest != refusal.ID || submission.TaskID == "" || submission.SpecificationRunID == "" {
		t.Fatalf("submission = %+v", submission)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		recorded, err := tx.GetTaskSubmission(ctx, identity)
		if err != nil {
			return err
		}
		if recorded != submission {
			t.Fatalf("recorded = %+v, want %+v", recorded, submission)
		}
		task, err := tx.GetTask(ctx, submission.TaskID)
		if err != nil {
			return err
		}
		if want := "Repair the project image at " + refusal.TipSHA[:12]; task.Name.Text != want {
			t.Fatalf("task name = %+v, want %q", task.Name, want)
		}
		if byKey, err := tx.GetTaskByIntakeKey(ctx, repairTestProject, "submission:"+identity); err != nil ||
			byKey.ID != task.ID {
			t.Fatalf("intake key task = %+v err=%v, want %s", byKey, err, task.ID)
		}
		source, err := tx.GetArtifact(ctx, task.Source.WorkItemArtifactID)
		if err != nil {
			return err
		}
		if source.Digest != refusal.ID {
			t.Fatalf("source artifact digest = %s, want the refusal ID %s", source.Digest, refusal.ID)
		}
		entry, err := tx.GetOutbox(ctx, string(specificationInvocationID(submission.SpecificationRunID, 1)))
		if err != nil {
			return err
		}
		request, err := decodeSpecificationRequest(entry)
		if err != nil {
			return err
		}
		if request.Publication.SourceIssue != "" {
			t.Fatalf("publication binds issue %q, want none", request.Publication.SourceIssue)
		}
		if request.WorkUnit == nil || request.WorkUnit.CompletionCriterion != domain.CompletionBoundPRMerged ||
			len(request.WorkUnit.DeclaredPaths) == 0 {
			t.Fatalf("work unit = %+v, want bound_pr_merged with the policy's declared paths", request.WorkUnit)
		}
		policy, err := tx.GetResolvedPolicy(ctx, submission.SpecificationRunID)
		if err != nil {
			return err
		}
		if !slices.Contains(policy.Keys, repairTestInitiator().PolicyKeys[0]) ||
			repairTestInitiator().PolicyKeys[0].Key != specify.PolicySpecApproval {
			t.Fatalf("policy keys = %+v, want the specification to await approval", policy.Keys)
		}
		ids, err := tx.TaskRunIDs(ctx, task.ID)
		if err != nil || len(ids) != 1 {
			t.Fatalf("task runs = %v err=%v, want one", ids, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	again, err := service.SubmitImageRepairTask(ctx, repairTestProject, refusal.ID)
	if err != nil || again != submission {
		t.Fatalf("resubmit = %+v err=%v, want the recorded %+v", again, err, submission)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		ids, err := tx.TaskRunIDs(ctx, submission.TaskID)
		if err != nil || len(ids) != 1 {
			t.Fatalf("task runs after resubmit = %v err=%v, want one", ids, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestSubmitImageRepairTaskRefusesOtherRefusals: only a recorded repair
// refusal on the project's repository opens a task, and a project with no
// submission policy returns the error and writes nothing.
func TestSubmitImageRepairTaskRefusesOtherRefusals(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tip := strings.Repeat("b", 40)
	policy := repairTestRefusal(t, domain.ImageUpkeepRefusalPolicy, repairTestRepositoryID, tip)
	hostFault := repairTestRefusal(t, domain.ImageUpkeepRefusalHostFault, repairTestRepositoryID, tip)
	foreign := repairTestRefusal(t, domain.ImageUpkeepRefusalRepair, 42, tip)
	repair := repairTestRefusal(t, domain.ImageUpkeepRefusalRepair, repairTestRepositoryID, tip)
	service, st := newImageRepairHarness(t, []domain.ProjectID{repairTestProject}, policy, hostFault, foreign, repair)

	for name, id := range map[string]domain.Digest{
		"policy class":     policy.ID,
		"host fault":       hostFault.ID,
		"other repository": foreign.ID,
	} {
		if _, err := service.SubmitImageRepairTask(ctx, repairTestProject, id); !errors.Is(err, store.ErrDaemonSubmissionUnsourced) {
			t.Fatalf("%s: submit = %v, want ErrDaemonSubmissionUnsourced", name, err)
		}
	}
	if _, err := service.SubmitImageRepairTask(ctx, repairTestProject, domain.Digest(contentaddr.Sum([]byte("none")))); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unrecorded refusal: submit = %v, want ErrNotFound", err)
	}

	unconfigured, unconfiguredStore := newImageRepairHarness(t, nil, repair)
	if _, err := unconfigured.SubmitImageRepairTask(ctx, repairTestProject, repair.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unconfigured project: submit = %v, want ErrNotFound", err)
	}
	for _, s := range []*store.Store{st, unconfiguredStore} {
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			for _, id := range []domain.Digest{policy.ID, hostFault.ID, foreign.ID, repair.ID} {
				if _, err := tx.GetTaskSubmission(ctx, domain.ImageUpkeepSubmissionIdentity(id)); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("submission for %s = %v, want ErrNotFound", id, err)
				}
			}
			if _, err := tx.GetArtifact(ctx, domain.ArtifactID(
				"artifact-specification-"+contentaddr.Hex(string(repair.ID)))); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("source artifact = %v, want ErrNotFound", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestDeviceSubmissionCannotTakeTheDaemonIdentity: a device submitting a
// refusal's preimage keys its task under client:, never the daemon identity,
// so it is a different task from the repair task.
func TestDeviceSubmissionCannotTakeTheDaemonIdentity(t *testing.T) {
	t.Parallel()
	if _, err := submissionIdentity(domain.TaskSubmissionOriginDaemon, "cmd-1"); !errors.Is(err, domain.ErrTaskSubmissionOriginMismatch) {
		t.Fatalf("daemon origin outside its namespace = %v, want ErrTaskSubmissionOriginMismatch", err)
	}
	if _, err := submissionIdentity("", "daemon:image-upkeep:x"); !errors.Is(err, domain.ErrInvalidTaskSubmissionOrigin) {
		t.Fatalf("zero origin = %v, want ErrInvalidTaskSubmissionOrigin", err)
	}
	if got, err := submissionIdentity(domain.TaskSubmissionOriginDevice, "daemon:image-upkeep:x"); err != nil ||
		got != "client:daemon:image-upkeep:x" {
		t.Fatalf("device identity = %q err=%v", got, err)
	}
}

// requireNoRepairSubmission fails unless no submission and no task are
// recorded under refusal's identity.
func requireNoRepairSubmission(t *testing.T, st *store.Store, refusal domain.ImageUpkeepRefusal) {
	t.Helper()
	ctx := context.Background()
	identity := domain.ImageUpkeepSubmissionIdentity(refusal.ID)
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetTaskSubmission(ctx, identity); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("submission for refusal %s = %v, want ErrNotFound", refusal.ID, err)
		}
		if _, err := tx.GetTaskByIntakeKey(ctx, repairTestProject, "submission:"+identity); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("task for refusal %s = %v, want ErrNotFound", refusal.ID, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestSubmitImageRepairTaskOpensOneTaskPerImage: while a repair task is open,
// a later refusal measured from the same image returns that task's
// submission and records nothing, whether the tip moved (an unrelated merge)
// or only the findings' detail changed (a restart that measured again). A
// refusal measured from another image opens its own task.
func TestSubmitImageRepairTaskOpensOneTaskPerImage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tip := strings.Repeat("a", 40)
	first := repairTestRefusalFrom(t, domain.ImageUpkeepRefusalRepair, repairTestRepositoryID, tip,
		"broken image", "proof failed after 1.2s")
	advanced := repairTestRefusalFrom(t, domain.ImageUpkeepRefusalRepair, repairTestRepositoryID,
		strings.Repeat("b", 40), "broken image", "proof failed after 1.2s")
	restarted := repairTestRefusalFrom(t, domain.ImageUpkeepRefusalRepair, repairTestRepositoryID, tip,
		"broken image", "proof failed after 3.4s")
	other := repairTestRefusalFrom(t, domain.ImageUpkeepRefusalRepair, repairTestRepositoryID, tip,
		"other image", "proof failed after 1.2s")
	service, st := newImageRepairHarness(t, []domain.ProjectID{repairTestProject}, first, advanced, restarted, other)

	submission, err := service.SubmitImageRepairTask(ctx, repairTestProject, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	for name, refusal := range map[string]domain.ImageUpkeepRefusal{"tip advance": advanced, "restart": restarted} {
		got, err := service.SubmitImageRepairTask(ctx, repairTestProject, refusal.ID)
		if err != nil || got != submission {
			t.Fatalf("%s: submit = %+v err=%v, want the open %+v", name, got, err, submission)
		}
		requireNoRepairSubmission(t, st, refusal)
	}
	own, err := service.SubmitImageRepairTask(ctx, repairTestProject, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if own.CommandID != domain.ImageUpkeepSubmissionIdentity(other.ID) || own.TaskID == submission.TaskID {
		t.Fatalf("another image's submission = %+v, want its own task", own)
	}
}

// TestSubmitImageRepairTaskReopensAfterTheTaskCloses: a started task still
// holds its image, but once it is stopped, completed, or abandoned, a later
// refusal measured from the same image opens a new task, and submitting the
// closed task's own refusal still replays it.
func TestSubmitImageRepairTaskReopensAfterTheTaskCloses(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	for name, closeTask := range map[string]func(*testing.T, *store.Store, domain.TaskSubmission) error{
		"stopped": func(t *testing.T, st *store.Store, submission domain.TaskSubmission) error {
			ctx := t.Context()
			state, err := st.ServerState(ctx)
			if err != nil {
				return err
			}
			return st.Write(ctx, func(tx *store.WriteTx) error {
				if err := tx.PutDevice(ctx, domain.Device{
					ID: "device-1", DisplayName: "Mac", Status: domain.DeviceActive, PairedAt: at,
				}); err != nil {
					return err
				}
				_, _, err := tx.StopTask(ctx, domain.StopTaskRequest{
					CommandID: "stop-1", DeviceID: "device-1", TaskID: submission.TaskID,
					ProjectID: repairTestProject, ExpectedSyncEpoch: state.SyncEpoch,
					ExpectedEntityVersion: state.Revision,
				}, at)
				return err
			})
		},
		// A completion needs a recorded work-unit completion behind it, so
		// this reuses the completion tests' bound-PR-merged chain on the
		// task's specification run.
		"completed": func(t *testing.T, st *store.Store, submission domain.TaskSubmission) error {
			ctx := t.Context()
			var (
				run    domain.Run
				policy domain.ResolvedPolicy
			)
			if err := st.Read(ctx, func(tx *store.ReadTx) error {
				var err error
				if run, err = tx.GetRun(ctx, submission.SpecificationRunID); err != nil {
					return err
				}
				policy, err = tx.GetResolvedPolicy(ctx, run.ID)
				return err
			}); err != nil {
				return err
			}
			seedRunCompletion(t, st, run, policy, completionFixturePR, at)
			return nil
		},
		"abandoned": func(t *testing.T, st *store.Store, submission domain.TaskSubmission) error {
			ctx := t.Context()
			return st.Write(ctx, func(tx *store.WriteTx) error {
				_, err := tx.AbandonTask(ctx, submission.TaskID, at)
				return err
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			first := repairTestRefusal(t, domain.ImageUpkeepRefusalRepair, repairTestRepositoryID, strings.Repeat("a", 40))
			later := repairTestRefusal(t, domain.ImageUpkeepRefusalRepair, repairTestRepositoryID, strings.Repeat("b", 40))
			service, st := newImageRepairHarness(t, []domain.ProjectID{repairTestProject}, first, later)
			submission, err := service.SubmitImageRepairTask(ctx, repairTestProject, first.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.Write(ctx, func(tx *store.WriteTx) error {
				return tx.RecordTaskStart(ctx, submission.SpecificationRunID, at.Add(-time.Hour))
			}); err != nil {
				t.Fatal(err)
			}
			if got, err := service.SubmitImageRepairTask(ctx, repairTestProject, later.ID); err != nil || got != submission {
				t.Fatalf("started task: submit = %+v err=%v, want the open %+v", got, err, submission)
			}
			requireNoRepairSubmission(t, st, later)

			if err := closeTask(t, st, submission); err != nil {
				t.Fatal(err)
			}
			reopened, err := service.SubmitImageRepairTask(ctx, repairTestProject, later.ID)
			if err != nil {
				t.Fatal(err)
			}
			if reopened.CommandID != domain.ImageUpkeepSubmissionIdentity(later.ID) ||
				reopened.TaskID == submission.TaskID {
				t.Fatalf("after the task closed: submission = %+v, want a new task", reopened)
			}
			if again, err := service.SubmitImageRepairTask(ctx, repairTestProject, first.ID); err != nil ||
				again != submission {
				t.Fatalf("closed task's own refusal: submit = %+v err=%v, want the recorded %+v", again, err, submission)
			}
		})
	}
}

// TestSubmitImageRepairTaskConcurrentSubmitsOpenOneTask: two upkeep passes
// submitting refusals measured from the same image at once open one task.
// It pins the outcome; the race window is small, so it would not reliably
// catch a check moved out of the write transaction, which is what store
// write serialization makes atomic.
func TestSubmitImageRepairTaskConcurrentSubmitsOpenOneTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	refusals := []domain.ImageUpkeepRefusal{
		repairTestRefusal(t, domain.ImageUpkeepRefusalRepair, repairTestRepositoryID, strings.Repeat("a", 40)),
		repairTestRefusal(t, domain.ImageUpkeepRefusalRepair, repairTestRepositoryID, strings.Repeat("b", 40)),
	}
	service, st := newImageRepairHarness(t, []domain.ProjectID{repairTestProject}, refusals...)
	submissions := make([]domain.TaskSubmission, len(refusals))
	errs := make([]error, len(refusals))
	var wg sync.WaitGroup
	for i, refusal := range refusals {
		wg.Go(func() {
			submissions[i], errs[i] = service.SubmitImageRepairTask(ctx, repairTestProject, refusal.ID)
		})
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}
	if submissions[0] != submissions[1] {
		t.Fatalf("concurrent submissions = %+v and %+v, want one task", submissions[0], submissions[1])
	}
	for _, refusal := range refusals {
		if domain.ImageUpkeepSubmissionIdentity(refusal.ID) != submissions[0].CommandID {
			requireNoRepairSubmission(t, st, refusal)
		}
	}
}
