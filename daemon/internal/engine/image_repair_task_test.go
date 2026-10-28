package engine

import (
	"bytes"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// submitRepairRun submits the fixture's specification run with the
// refusal's preimage as its source, on a project registered to the
// refusal's repository. When daemonTask is set, the daemon submission
// recorded under the refusal's identity names that run's task, as for a
// maintenance task. Otherwise it names another task, so the run is what a
// device task submitting the same bytes would be.
func submitRepairRun(refusal domain.ImageUpkeepRefusal, daemonTask bool) func(specificationFixture, *testing.T) {
	return func(f specificationFixture, t *testing.T) {
		t.Helper()
		ctx := t.Context()
		preimage, err := refusal.Preimage()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.blobs.Put(refusal.ID, bytes.NewReader(preimage)); err != nil {
			t.Fatal(err)
		}
		source := testSpecificationArtifact(t, "repair-source", domain.ArtifactKindSpecification,
			refusal.ID, domain.ProducerDaemon, "submit-repair-source")
		if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
			if err := tx.PutArtifact(ctx, source); err != nil {
				return err
			}
			return tx.RegisterProject(ctx, domain.Project{
				ID: "project-1", Repo: refusal.Repository, RepositoryID: refusal.RepositoryID,
			})
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.store.WriteInternal(ctx, func(tx *store.InternalTx) error {
			_, err := tx.RecordImageUpkeepRefusal(ctx, refusal)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		spec := f.specWithSource(domain.SpecificationSource{
			Kind: domain.SpecificationSourceWorkItemArtifact, WorkItemArtifactID: source.ID,
		})
		spec.SourceArtifactID = source.ID
		if _, err := SubmitSpecificationRun(ctx, f.store, spec); err != nil {
			t.Fatal(err)
		}
		if err := f.store.Write(ctx, func(tx *store.WriteTx) error {
			run, err := tx.GetRun(ctx, "specification-run")
			if err != nil {
				return err
			}
			taskID, runID := run.TaskID, run.ID
			if !daemonTask {
				other, err := tx.GetOrCreateTask(ctx, "project-1", domain.SpecificationSource{
					Kind: domain.SpecificationSourceIssueSubject,
					IssueSubject: &domain.IssueSubjectRef{
						Repo: refusal.Repository, RepositoryID: refusal.RepositoryID, IssueNumber: 9,
					},
				})
				if err != nil {
					return err
				}
				taskID, runID = other.ID, "repair-run"
				if err := tx.PutRun(ctx, domain.Run{
					ID: runID, ProjectID: "project-1", TaskID: taskID,
					SpecDigest: "sha256:spec", PolicyDigest: "sha256:policy", Stages: []domain.Stage{},
				}); err != nil {
					return err
				}
			}
			return tx.PutTaskSubmission(ctx, domain.TaskSubmission{
				CommandID: domain.ImageUpkeepSubmissionIdentity(refusal.ID), Origin: domain.TaskSubmissionOriginDaemon,
				ProjectID: "project-1", SourceDigest: refusal.ID, TaskID: taskID, SpecificationRunID: runID,
				Name: domain.DisplayName{Text: "Repair the project image", Source: domain.DisplayNameSourceOperator},
			})
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// Every invocation of a maintenance task is admitted at its refusal's tip
// with no hydration, and its selection is told the refusal.
func TestRepairTaskAdmitsAtTheRefusalTipWithoutHydration(t *testing.T) {
	tip := strings.Repeat("c", 40)
	// The fixture's static base is repository 1.
	refusal := repairTestRefusal(t, domain.ImageUpkeepRefusalRepair, 1, tip)
	h := newSelectionHoldFixtureSubmitting(t, submitRepairRun(refusal, true))
	h.selected = selectionTestImage
	if started := h.dispatch(t); started != 1 {
		t.Fatalf("started %d, want the repair task's specification attempt", started)
	}
	if len(h.repairs) != 1 || h.repairs[0] == nil || h.repairs[0].ID != refusal.ID {
		t.Fatalf("selection was passed %v, want the refusal", h.repairs)
	}
	admission, err := h.admission(t)
	if err != nil {
		t.Fatal(err)
	}
	if admission.Base.BaseSHA != tip || admission.Base.RepositoryID != 1 ||
		admission.WorkspaceHydration != domain.WorkspaceHydrationNone || admission.ImageRef != selectionTestImage {
		t.Fatalf("admission base %+v hydration %q image %s, want the refusal's tip, none, and the selected image",
			admission.Base, admission.WorkspaceHydration, admission.ImageRef)
	}

	// Implementation and remediation runs share the task, so they qualify
	// the same way.
	var run domain.Run
	if err := h.fixture.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		run, err = tx.GetRun(t.Context(), "specification-run")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	implementation := domain.Run{ID: "implementation-run", ProjectID: run.ProjectID, TaskID: run.TaskID}
	got, err := readRepairTaskRefusal(t.Context(), h.fixture.store, implementation)
	if err != nil || got == nil || got.ID != refusal.ID {
		t.Fatalf("implementation run's refusal = %v err=%v, want %s", got, err, refusal.ID)
	}
}

// A task whose source bytes equal a refusal's preimage, but that is not the
// task the daemon submitted for it, is admitted like any other.
func TestDeviceTaskWithARefusalSourceGetsTheOrdinaryAdmission(t *testing.T) {
	refusal := repairTestRefusal(t, domain.ImageUpkeepRefusalRepair, 1, strings.Repeat("c", 40))
	h := newSelectionHoldFixtureSubmitting(t, submitRepairRun(refusal, false))
	h.selected = selectionTestImage
	if started := h.dispatch(t); started != 1 {
		t.Fatalf("started %d, want the specification attempt", started)
	}
	if len(h.repairs) != 1 || h.repairs[0] != nil {
		t.Fatalf("selection was passed %v, want no refusal", h.repairs)
	}
	admission, err := h.admission(t)
	if err != nil {
		t.Fatal(err)
	}
	if admission.Base.BaseSHA != "deadbeef" || admission.WorkspaceHydration != "" {
		t.Fatalf("admission base %+v hydration %q, want the configured base and no hydration fact",
			admission.Base, admission.WorkspaceHydration)
	}
}
