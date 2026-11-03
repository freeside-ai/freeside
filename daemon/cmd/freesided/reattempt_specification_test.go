package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/engine"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	execfake "github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/golden"
	"github.com/freeside-ai/freeside/daemon/internal/signet"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
)

// failedSpecification is a submitted task whose first specification
// invocation failed before approval, with the engine that ran it.
type failedSpecification struct {
	dbPath    string
	submitted submitResult
	store     *store.Store
	workflow  *engine.Engine
	driver    *execfake.StageDriver
}

func newFailedSpecification(t *testing.T) failedSpecification {
	t.Helper()
	root := t.TempDir()
	taskPath, policyPath, publicationPath := writeSubmissionInputs(t, root)
	cfg := submitCommandConfig{
		DBPath: filepath.Join(root, "state.db"), TaskPath: taskPath,
		PolicyPath: policyPath, WorkUnitPath: declaredWorkUnit(t), PublicationPath: publicationPath,
		ProjectID: "project-retry-specification", SubmissionID: "retry-specification",
	}
	seedTemplateStore(t, cfg.DBPath)
	submitted, err := runSubmitCommand(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	st := storetest.Open(t, cfg.DBPath, store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
	})
	blobs, err := signet.NewBlobStore(cfg.DBPath + ".blobs")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	attention := signet.NewService(st, signet.WithBlobStore(blobs), signet.WithClock(func() time.Time { return now }))
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		return storetest.RecordIdentityOnPool(t.Context(), tx, domain.AuthIdentity{
			ID: "auth-retry", Provider: "codex", AuthStoreMutationLease: true,
			Interim: domain.InterimClientFacts{AuthStoreVolume: "provider-credentials", RefreshStrategy: domain.RefreshOnDemand},
		}, 64, now)
	}); err != nil {
		t.Fatal(err)
	}
	promptBody := []byte("Specify the submitted task.\n")
	promptDigest := domain.Digest(contentaddr.Sum(promptBody))
	if _, err := blobs.Put(promptDigest, bytes.NewReader(promptBody)); err != nil {
		t.Fatal(err)
	}
	vendorPath := filepath.Join(root, "AGENTS.md")
	if err := os.WriteFile(vendorPath, []byte("Stay within the submitted work unit.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	driver, err := execfake.NewStageDriverAt(filepath.Join(root, "driver"))
	if err != nil {
		t.Fatal(err)
	}
	fetcher, err := specify.NewFetcher(st, blobs, nil)
	if err != nil {
		t.Fatal(err)
	}
	identity := domain.AuthIdentityID("auth-retry")
	workflow, err := engine.New(st, attention, driver,
		engine.WithAdmission(submitSpecificationBackend{}, []exec.Capability{exec.CapPostExitExport}, engine.AdmissionEnvironment{
			OperatingMode: domain.ModeAttendedDev, CredentialMode: domain.CredentialSubscriptionContained,
			EgressProfile:       domain.EgressProviderOnly,
			ImageRef:            domain.ImageRef("agent@sha256:" + strings.Repeat("a", 64)),
			PromptPackageDigest: promptDigest,
			VendorInstructions: engine.VendorInstructionConfig{
				Vendor: domain.AgentVendorCodex, Delivery: domain.VendorInstructionDeliveryAppendFile,
				HostPath: vendorPath,
			},
			Base: domain.BaseRevision{
				Repo: "owner/repo", RepositoryID: 1,
				BaseRef: "refs/heads/main", BaseSHA: "deadbeef",
			},
			Workspace: "workspace-retry", AuthIdentityID: &identity,
		}, func() time.Time { return now }),
		engine.WithSpecification(engine.SpecificationConfig{
			Fetcher: fetcher, Blobs: blobs, Now: func() time.Time { return now },
			PromptPackageDigest: promptDigest,
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	driver.Script(submitted.SpecificationInvocationID, execfake.StageScript{
		Outcome: execfake.OutcomeFail,
		Result:  exec.StageResult{Artifacts: []domain.Digest{}, Summary: "Specifier fetch timed out."},
	})
	if _, err := workflow.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	failed := failedSpecification{
		dbPath: cfg.DBPath, submitted: submitted, store: st, workflow: workflow, driver: driver,
	}
	if card := failed.item(t, domain.ItemID("execution-failure-"+string(submitted.SpecificationInvocationID))); card.Status != domain.StatusOpen {
		t.Fatalf("failure card = %+v", card)
	}
	return failed
}

func (f failedSpecification) item(t *testing.T, id domain.ItemID) domain.AttentionItem {
	t.Helper()
	var item domain.AttentionItem
	if err := f.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		item, err = tx.GetAttentionItem(t.Context(), id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return item
}

func TestReattemptTaskRetriesFailedSpecification(t *testing.T) {
	t.Parallel()
	f := newFailedSpecification(t)
	failed := f.submitted.SpecificationInvocationID
	second := domain.InvocationID(strings.TrimSuffix(string(failed), "-1") + "-2")
	cfg := reattemptCommandConfig{DBPath: f.dbPath, TaskID: f.submitted.TaskID, Reason: "The fetch timeout was transient."}
	result, err := runReattemptCommand(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := submitResult{
		TaskID: f.submitted.TaskID, ProjectID: f.submitted.ProjectID,
		SpecificationRunID:            f.submitted.SpecificationRunID,
		SpecificationInvocationID:     second,
		SpecificationStageID:          f.submitted.SpecificationStageID,
		SourceDigest:                  f.submitted.SourceDigest,
		SpecificationPolicyDigest:     f.submitted.SpecificationPolicyDigest,
		SourceArtifactID:              f.submitted.SourceArtifactID,
		SpecificationPolicyArtifactID: f.submitted.SpecificationPolicyArtifactID,
		PublicationDigest:             f.submitted.PublicationDigest,
		CampaignID:                    f.submitted.CampaignID,
		AttemptReason:                 cfg.Reason,
	}
	if result != want {
		t.Fatalf("retry result = %#v, want %#v", result, want)
	}
	if replay, err := runReattemptCommand(t.Context(), cfg); err != nil || replay != want {
		t.Fatalf("replayed retry = %#v, %v", replay, err)
	}
	cfg.Reason = "Another reason."
	if _, err := runReattemptCommand(t.Context(), cfg); !errors.Is(err, engine.ErrSpecificationNotRetryable) {
		t.Fatalf("second retry while one is pending = %v", err)
	}

	if err := specifyfake.Script(f.driver, second, 0, 0, specify.Output{Specification: &specify.Specification{
		Summary: "Ready for implementation.", Body: "# Task\n\nImplement the thing.", Addressals: []specify.Addressal{},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.workflow.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	if approval := f.item(t, domain.ItemID("spec-approval-"+string(f.submitted.RunID)+"-2")); approval.Status != domain.StatusOpen {
		t.Fatalf("approval after retry = %+v", approval)
	}

	record := readRecord(t, f.store, f.submitted.TaskID)
	if len(record.Runs) != 1 || record.Runs[0].RunID != f.submitted.SpecificationRunID {
		t.Fatalf("record runs = %s, want only the specification run", quotedRunIDs(record.Runs))
	}
	run := record.Runs[0]
	invocations := make([]domain.InvocationID, 0, len(run.Invocations))
	for _, invocation := range run.Invocations {
		invocations = append(invocations, invocation.InvocationID)
	}
	slices.Sort(invocations)
	if !slices.Equal(invocations, []domain.InvocationID{failed, second}) || len(run.Errors) != 0 {
		t.Fatalf("specification run invocations = %v, errors = %+v", invocations, run.Errors)
	}
	wantRetries := []taskRecordSpecificationRetry{{
		InvocationID: second, RetriedInvocationID: failed, Reason: "The fetch timeout was transient.",
	}}
	if !slices.Equal(run.SpecificationRetries, wantRetries) {
		t.Fatalf("specification retries = %+v, want %+v", run.SpecificationRetries, wantRetries)
	}
	// The main task-record golden has no retry, so this one pins the entry's
	// shape. The submission's identities are derived, so the bytes are fixed.
	got, err := json.MarshalIndent(run.SpecificationRetries, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden.Assert(t, "task_record_specification_retries", append(got, '\n'))
}

// A label-intake task has no campaign; -task reaches its one specification
// run, which here has not started.
func TestReattemptTaskWithoutCampaignRetriesItsSpecificationRun(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "state.db")
	st, _, err := openStoreWithTopicKey(t.Context(), dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var task domain.Task
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		var err error
		task, err = tx.GetOrCreateTask(t.Context(), "project-1", domain.SpecificationSource{
			Kind:         domain.SpecificationSourceIssueSubject,
			IssueSubject: &domain.IssueSubjectRef{Repo: "owner/repo", RepositoryID: 1, IssueNumber: 42},
		})
		if err != nil {
			return err
		}
		run := engine.NewReservedSpecificationRun("run-label-intake", task.ProjectID, "sha256:spec", "sha256:policy")
		run.TaskID = task.ID
		return tx.PutRun(t.Context(), run)
	}); err != nil {
		t.Fatal(err)
	}
	cfg := reattemptCommandConfig{DBPath: dbPath, TaskID: task.ID, Reason: "Retry the intake"}
	if _, err := runReattemptCommand(t.Context(), cfg); !errors.Is(err, engine.ErrSpecificationNotRetryable) ||
		!strings.Contains(err.Error(), "run-label-intake") {
		t.Fatalf("unstarted label-intake retry = %v, want its specification run refused as not retryable", err)
	}
}
