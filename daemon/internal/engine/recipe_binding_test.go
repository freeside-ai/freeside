package engine

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/importer"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

func noImageBinding() productionBinding {
	var binding productionBinding
	binding.run.ID = "run-no-image"
	binding.recipeBinding = &domain.RecipeBinding{
		Repository: "acme/widgets", RepositoryID: 42,
		CommitSHA: strings.Repeat("c", 40), RecipeDigest: domain.Digest("sha256:" + strings.Repeat("b", 64)),
	}
	binding.admission.Base = domain.BaseRevision{Repo: "acme/widgets", RepositoryID: 42, BaseSHA: strings.Repeat("a", 40)}
	return binding
}

// TestBoundRecipeIsThePinnedBindingsDigest: the resolver every proof reads
// answers the pinned binding's digest for a no-image run, even beside an
// image record, and the image's for an image run.
func TestBoundRecipeIsThePinnedBindingsDigest(t *testing.T) {
	t.Parallel()
	binding := noImageBinding()
	binding.image.RecipeDigest = "sha256:image-recipe"
	if got := binding.boundRecipe(); got != binding.recipeBinding.RecipeDigest {
		t.Fatalf("boundRecipe = %s, want the binding's %s", got, binding.recipeBinding.RecipeDigest)
	}
	binding.recipeBinding = nil
	if got := binding.boundRecipe(); got != "sha256:image-recipe" {
		t.Fatalf("boundRecipe = %s, want the image's", got)
	}
}

// TestBindAdmittedRecipeLooksUpNoImageForAPinnedBinding: a store with no
// project-image record binds an admission that pins a binding, and refuses
// one that names an image; the binding's repository is then checked against
// the admitted base.
func TestBindAdmittedRecipeLooksUpNoImageForAPinnedBinding(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
	pinned := noImageBinding()
	var binding productionBinding
	binding.admission.Base = pinned.admission.Base
	binding.admission.RecipeBinding = pinned.recipeBinding
	var imageAdmission productionBinding
	imageAdmission.admission.Base = pinned.admission.Base
	imageAdmission.admission.ImageRef = domain.ImageRef("registry.example/agent@sha256:" + strings.Repeat("d", 64))
	var pinnedErr, imageErr error
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		pinnedErr = bindAdmittedRecipe(ctx, tx, &binding)
		imageErr = bindAdmittedRecipe(ctx, tx, &imageAdmission)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if pinnedErr != nil {
		t.Fatalf("bind a pinned binding: %v", pinnedErr)
	}
	if !errors.Is(imageErr, domain.ErrParentKeyMismatch) {
		t.Fatalf("bind an image admission with no image record = %v, want ErrParentKeyMismatch", imageErr)
	}
	if binding.recipeBinding == nil || *binding.recipeBinding != *pinned.recipeBinding || binding.image.ID != "" {
		t.Fatalf("bound %+v with image %q, want the pinned binding and no image", binding.recipeBinding, binding.image.ID)
	}
	if binding.recipeBinding == binding.admission.RecipeBinding {
		t.Fatal("the bound recipe shares the admission's pointer")
	}
	if !binding.recipeSourceIn(binding.admission.Base) {
		t.Fatal("the pinned binding does not name its own base's repository")
	}
	other := binding.admission.Base
	other.RepositoryID = 43
	if binding.recipeSourceIn(other) {
		t.Fatal("the pinned binding names another repository id's base")
	}
}

// TestVerificationRefusesANoImageRunBeforeTheRoom: every fresh verification
// path refuses a no-image run with errNoVerificationRoom before it builds a
// room, and the refusal names the missing room.
func TestVerificationRefusesANoImageRunBeforeTheRoom(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	w := &productionPublicationWorkflow{
		newRoom: func(domain.ProjectImage, ward.ContainerSize) (ProductionVerificationRoom, error) {
			t.Fatal("a room was built for a no-image run")
			return nil, nil
		},
		newBoundRoom: func(domain.ProjectImage, ward.ContainerSize, domain.Run, domain.InvocationID) (ProductionVerificationRoom, error) {
			t.Fatal("a bound room was built for a no-image run")
			return nil, nil
		},
	}
	binding := noImageBinding()
	task := productionPublicationTask{RunID: binding.run.ID, VerificationID: "inv-verification"}
	_, _, roomErr := w.roomAndRecipe(ctx, task, binding, binding.verificationImage())
	_, checkpointErr := w.verifyAndCheckpoint(ctx, task, binding, importer.Result{}, t.TempDir())
	_, reentryErr := w.verifyReentry(ctx, "reentry-key", task, binding, t.TempDir())
	for name, err := range map[string]error{
		"roomAndRecipe": roomErr, "verifyAndCheckpoint": checkpointErr, "verifyReentry": reentryErr,
	} {
		if !errors.Is(err, errNoVerificationRoom) || !strings.Contains(err.Error(), "no verification room") {
			t.Errorf("%s = %v, want errNoVerificationRoom", name, err)
		}
	}
}

// TestReentryCheckpointLoaderBindsTheNoImageRecipe: a stored re-entry
// verdict for a no-image run is accepted at the pinned binding's digest and
// refused at any other.
func TestReentryCheckpointLoaderBindsTheNoImageRecipe(t *testing.T) {
	t.Parallel()
	successor := reentrySuccessorForTest(
		productionReadyItemID("run-reentry"), domain.ReadinessInvalidationBaseAdvanced)
	task := newReentryTask("project-reentry", successor)
	binding := noImageBinding()
	binding.admission.Base.BaseSHA = successor.Reentry.BaseSHA
	for name, tc := range map[string]struct {
		recipe domain.Digest
		want   error
	}{
		"the binding's digest": {binding.recipeBinding.RecipeDigest, nil},
		"another digest":       {domain.Digest("sha256:" + strings.Repeat("e", 64)), domain.ErrParentKeyMismatch},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			raw, err := json.MarshalIndent(verify.Report{
				HeadSHA: task.HeadSHA, BaseSHA: successor.Reentry.BaseSHA,
				RecipeDigest: tc.recipe, Outcome: verify.OutcomePassed,
			}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			raw = append(raw, '\n')
			artifacts := []domain.Artifact{{
				Type: domain.ArtifactKindVerificationReport, Digest: domain.Digest(contentaddr.Sum(raw)),
				Provenance: domain.Provenance{ProducerInvocationID: task.verificationInvocationID()},
			}}
			evidence, err := domain.ComputeEvidenceSnapshotDigest(artifacts)
			if err != nil {
				t.Fatal(err)
			}
			checkpoint := productionReentryCheckpoint{
				Version: productionReentryCheckpointVersion, TaskKey: task.intentKey(),
				BaseSHA: successor.Reentry.BaseSHA, HeadSHA: task.HeadSHA, TreeSHA: strings.Repeat("7", 40),
				Outcome: domain.VerificationPassed, RecipeDigest: tc.recipe,
				EvidenceSnapshotDigest: evidence, Artifacts: artifacts,
			}
			st := storetest.Open(t, filepath.Join(t.TempDir(), "store.db"), store.Options{})
			payload, err := json.Marshal(checkpoint)
			if err != nil {
				t.Fatal(err)
			}
			if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
				_, _, err := tx.RecordInbox(ctx, task.verificationCheckpointKey(), productionReentryCheckpointKind, payload)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			w := &productionPublicationWorkflow{store: st, artifacts: &findingAdjudicationArtifactStore{
				bodies: map[domain.Digest][]byte{artifacts[0].Digest: raw},
			}}
			_, found, err := w.loadReentryCheckpoint(ctx, task.reentryCheckpointKey(), task, binding)
			if !errors.Is(err, tc.want) {
				t.Fatalf("loadReentryCheckpoint = %v, want %v", err, tc.want)
			}
			if tc.want == nil && !found {
				t.Fatal("loadReentryCheckpoint did not find the stored checkpoint")
			}
		})
	}
}
