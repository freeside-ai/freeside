package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/projectimage"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// reentryImageFixture is a re-entry whose base advanced past what the run's
// admitted image baked: the base changed the lockfile, and a recorded image
// built at the base serves it.
type reentryImageFixture struct {
	repo     upkeepRepo
	st       *store.Store
	lane     *productionPublicationWorkflow
	admitted domain.ProjectImage
	served   domain.ProjectImage
	base     string
	// merge is the candidate merged into base; it changes no manifest.
	merge string
}

func newReentryImageFixture(t *testing.T) reentryImageFixture {
	t.Helper()
	repo := newUpkeepRepo(t)
	built := repo.npmBase()
	admitted := upkeepImage(t, built, func(input *domain.ProjectImageInput) {
		input.Environment.PreparationDigest = projectimage.PreparationDigest()
	})
	base := repo.commit(map[string]string{"package.json": upkeepHeadJSON, "package-lock.json": upkeepAddedLock})
	merge := repo.commit(map[string]string{"src/index.js": "export const a = 1;\n"})
	served := upkeepImage(t, base, func(input *domain.ProjectImageInput) {
		input.ImageRef = domain.ImageRef("ghcr.io/owner/project@sha256:" + strings.Repeat("e", 64))
		input.Environment = &domain.ProjectImageEnvironment{
			PackageJSONSHA256: upkeepSHA256(upkeepHeadJSON), PackageLockSHA256: upkeepSHA256(upkeepAddedLock),
			PreparationDigest: projectimage.PreparationDigest(),
		}
	})
	st := openUpkeepStore(t)
	if err := st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		if err := tx.RecordProjectImage(t.Context(), admitted); err != nil {
			return err
		}
		return tx.RecordProjectImage(t.Context(), served)
	}); err != nil {
		t.Fatal(err)
	}
	return reentryImageFixture{
		repo: repo, st: st, admitted: admitted, served: served, base: base, merge: merge,
		lane: &productionPublicationWorkflow{store: st, preparationDigest: projectimage.PreparationDigest()},
	}
}

func (f reentryImageFixture) binding() productionBinding {
	return productionBinding{image: f.admitted, run: domain.Run{ID: "reentry-run"}}
}

// A cycle whose admitted image cannot serve its base binds the first
// recorded image that serves both the base and the commit it verifies, as
// the rebuild row every later reader resolves.
func TestReentryBindsARecordedImageThatServesItsBase(t *testing.T) {
	t.Parallel()
	f := newReentryImageFixture(t)
	if err := requireProjectImageServesBase(
		t.Context(), f.admitted, f.repo.dir, f.base, projectimage.PreparationDigest(),
	); !errors.Is(err, domain.ErrProjectImageIncompatible) {
		t.Fatalf("admitted image at the advanced base = %v, want it refused", err)
	}

	bound, found, err := f.lane.bindBaseServingImage(t.Context(), f.binding(), f.repo.dir, f.base, f.merge)
	if err != nil || !found || bound.verificationImage().ID != f.served.ID {
		t.Fatalf("bind = (%v, %t, %v), want the recorded image built at the base", bound.rebuilt, found, err)
	}
	// A later pass and the checkpoint's readers resolve the same image from
	// the row, and the bound image keeps serving.
	resolved := f.binding()
	if err := f.lane.resolveVerificationImage(t.Context(), &resolved, f.merge); err != nil {
		t.Fatal(err)
	}
	if resolved.verificationImage().ID != f.served.ID {
		t.Fatalf("resolved image = %s, want %s", resolved.verificationImage().ImageRef, f.served.ImageRef)
	}
	if again, found, err := f.lane.bindBaseServingImage(
		t.Context(), resolved, f.repo.dir, f.base, f.merge,
	); err != nil || !found || again.verificationImage().ID != f.served.ID {
		t.Fatalf("rebind = (%v, %t, %v), want the recorded binding kept", again.rebuilt, found, err)
	}
}

// A candidate that changes a manifest itself is left unserved: no image that
// serves the base bakes its change, and binding one would skip the rebuild
// policy that judges it.
func TestReentryLeavesACandidateThatChangesItsManifestsUnserved(t *testing.T) {
	t.Parallel()
	f := newReentryImageFixture(t)
	changed := f.repo.commit(map[string]string{"package.json": upkeepPackageJSON, "package-lock.json": upkeepLock})
	// A record that bakes exactly the candidate's manifests serves the
	// commit verified, but not the base.
	candidateImage := upkeepImage(t, f.admitted.CommitSHA, func(input *domain.ProjectImageInput) {
		input.ImageRef = domain.ImageRef("ghcr.io/owner/project@sha256:" + strings.Repeat("f", 64))
		input.Environment.PreparationDigest = projectimage.PreparationDigest()
	})
	if err := f.st.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		return tx.RecordProjectImage(t.Context(), candidateImage)
	}); err != nil {
		t.Fatal(err)
	}
	bound, found, err := f.lane.bindBaseServingImage(t.Context(), f.binding(), f.repo.dir, f.base, changed)
	if err != nil || found {
		t.Fatalf("bind = (%v, %t, %v), want nothing bound", bound.rebuilt, found, err)
	}
	resolved := f.binding()
	if err := f.lane.resolveVerificationImage(t.Context(), &resolved, changed); err != nil || resolved.rebuilt != nil {
		t.Fatalf("resolved = (%v, %v), want no row written", resolved.rebuilt, err)
	}
}

// With no image serving the base, a re-entry holds while image upkeep has a
// resolution pending and stops as before when it has none.
func TestUnservedReentryBaseHoldsOnlyWhileUpkeepIsPending(t *testing.T) {
	t.Parallel()
	refusal := fmt.Errorf("image does not serve: %w", domain.ErrProjectImageIncompatible)
	task := productionPublicationTask{RunID: "reentry-run", HeadSHA: strings.Repeat("a", 40)}
	base := strings.Repeat("b", 40)
	stops := func(err error) bool {
		_, stop := reentryUnservedBaseReason(task, base, err)
		return stop
	}
	lane := &productionPublicationWorkflow{}
	if err := lane.unservedReentryBase(t.Context(), base, refusal); !stops(err) {
		t.Fatalf("no lookup = %v, want the stop", err)
	}

	var (
		pending   *domain.ImageUpkeepHold
		lookupErr error
	)
	lane.imageUpkeepPending = func(context.Context) (*domain.ImageUpkeepHold, error) { return pending, lookupErr }
	if err := lane.unservedReentryBase(t.Context(), base, refusal); !stops(err) {
		t.Fatalf("nothing pending = %v, want the stop", err)
	}
	pending = &domain.ImageUpkeepHold{Pending: domain.ImageUpkeepPendingRebuild}
	err := lane.unservedReentryBase(t.Context(), base, refusal)
	var held *ImageUpkeepPendingError
	if stops(err) || !productionPublicationRetryableFailure(err) || !errors.As(err, &held) ||
		held.Hold.Pending != domain.ImageUpkeepPendingRebuild {
		t.Fatalf("rebuild pending = %v, want a retryable image_upkeep_pending rebuild", err)
	}
	lookupErr = errors.New("store unavailable")
	if err := lane.unservedReentryBase(t.Context(), base, refusal); stops(err) ||
		!productionPublicationRetryableFailure(err) || !errors.Is(err, lookupErr) {
		t.Fatalf("failed lookup = %v, want it retried", err)
	}
}

// The lane records image_upkeep_pending for a task waiting on an image, a
// pending re-entry or a busy builder. A wait that recurs keeps the hold's
// span, and an attempt that ends without waiting clears it.
func TestRetryableImageWaitsHoldUnderImageUpkeep(t *testing.T) {
	t.Parallel()
	h := newSelectionHoldFixture(t)
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	lane := &productionPublicationWorkflow{store: h.fixture.store, now: func() time.Time { return now }}
	task := productionPublicationTask{RunID: "specification-run", PublicationID: h.invocationID}
	proposal := domain.ItemID("proposal-1")
	refusal := domain.Digest("sha256:" + strings.Repeat("5", 64))
	busy := productionPublicationRetryableError(errProjectImageBuilderBusy)
	pending := productionPublicationRetryableError(&ImageUpkeepPendingError{Hold: domain.ImageUpkeepHold{
		Pending: domain.ImageUpkeepPendingAdoptionProposal, RefusalRecordID: &refusal, ProposalItemID: &proposal,
	}})
	other := productionPublicationRetryableError(errors.New("network down"))
	if !waitsOnImage(busy) || !waitsOnImage(pending) || waitsOnImage(other) || waitsOnImage(nil) {
		t.Fatal("waitsOnImage misclassifies a reconcile result")
	}

	record := func(err error) domain.RunHoldObservation {
		t.Helper()
		if err := lane.recordRetryableHold(t.Context(), task, err); err != nil {
			t.Fatal(err)
		}
		hold, found := h.hold(t)
		if !found {
			t.Fatal("no hold recorded")
		}
		return hold
	}
	first := record(busy)
	if first.Reason != domain.HoldImageUpkeepPending || first.ImageUpkeep == nil ||
		first.ImageUpkeep.Pending != domain.ImageUpkeepPendingRebuild {
		t.Fatalf("busy builder hold = %+v, want an image_upkeep_pending rebuild", first)
	}
	now = now.Add(time.Minute)
	again := record(pending)
	if again.ImageUpkeep == nil || again.ImageUpkeep.Pending != domain.ImageUpkeepPendingAdoptionProposal ||
		again.ImageUpkeep.ProposalItemID == nil || !again.FirstObservedAt.Equal(first.FirstObservedAt) {
		t.Fatalf("recurring wait = %+v, want the adoption detail and the first span kept", again)
	}
	if err := lane.clearImageUpkeepHold(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if hold, found := h.hold(t); found {
		t.Fatalf("hold = %+v after an attempt that did not wait, want it cleared", hold)
	}

	if hold := record(other); hold.Reason != domain.HoldPublicationEnvironment || hold.ImageUpkeep != nil {
		t.Fatalf("other back-off = %+v, want publication_environment", hold)
	}
	now = now.Add(time.Hour)
	if err := lane.clearImageUpkeepHold(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	if _, found := h.hold(t); !found {
		t.Fatal("clearing the upkeep hold removed a publication_environment hold")
	}
}
