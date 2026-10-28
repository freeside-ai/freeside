package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/specify"
	specifyfake "github.com/freeside-ai/freeside/daemon/internal/specify/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// selectionHoldFixture is a submitted specification run whose writer
// attempt is admitted through an image selection the test controls.
type selectionHoldFixture struct {
	fixture      specificationFixture
	engine       *Engine
	invocationID domain.InvocationID
	// selected is what the selection returns; selectErr, when set, wins.
	selected  domain.ImageRef
	selectErr error
	// repairs records the repair refusal each selection was passed.
	repairs []*domain.ImageUpkeepRefusal
}

func newSelectionHoldFixture(t *testing.T) *selectionHoldFixture {
	t.Helper()
	return newSelectionHoldFixtureSubmitting(t, specificationFixture.submit)
}

// newSelectionHoldFixtureSubmitting submits the specification run with
// submit instead of the fixture's default submission.
func newSelectionHoldFixtureSubmitting(
	t *testing.T, submit func(specificationFixture, *testing.T),
) *selectionHoldFixture {
	t.Helper()
	f := newSpecificationFixture(t, false, 4)
	driver := f.newDriver(t)
	invocationID := specificationInvocationID("specification-run", 1)
	if err := specifyfake.Script(driver, invocationID, 0, 0, specify.Output{Specification: &specify.Specification{
		Summary: "ready", Body: "# Specification\n\nImplement.", Addressals: []specify.Addressal{},
	}}); err != nil {
		t.Fatal(err)
	}
	submit(f, t)
	h := &selectionHoldFixture{fixture: f, engine: f.newEngine(t, driver), invocationID: invocationID}
	h.engine.selectImage = func(
		_ context.Context, _ domain.InvocationID, repair *domain.ImageUpkeepRefusal,
	) (domain.ImageRef, error) {
		h.repairs = append(h.repairs, repair)
		return h.selected, h.selectErr
	}
	return h
}

func (h *selectionHoldFixture) dispatch(t *testing.T) int {
	t.Helper()
	started, err := h.engine.dispatchPendingInvocations(t.Context())
	if err != nil {
		t.Fatalf("dispatch = %v, want the pass to continue", err)
	}
	return started
}

func (h *selectionHoldFixture) hold(t *testing.T) (domain.RunHoldObservation, bool) {
	t.Helper()
	var (
		hold  domain.RunHoldObservation
		found bool
	)
	if err := h.fixture.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		hold, found, err = tx.GetRunHold(t.Context(), "specification-run")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return hold, found
}

func (h *selectionHoldFixture) admission(t *testing.T) (domain.ExecutionAdmission, error) {
	t.Helper()
	var admission domain.ExecutionAdmission
	err := h.fixture.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmission(t.Context(), h.invocationID)
		return err
	})
	return admission, err
}

var selectionTestImage = domain.ImageRef("ghcr.io/owner/project@sha256:" + strings.Repeat("e", 64))

// An attempt waiting on a rebuild holds visibly under image_upkeep_pending,
// starts once an image serves its base, and the hold clears on dispatch.
func TestImageUpkeepHoldClearsWhenTheAttemptDispatches(t *testing.T) {
	h := newSelectionHoldFixture(t)
	h.selectErr = &ImageUpkeepPendingError{Hold: domain.ImageUpkeepHold{Pending: domain.ImageUpkeepPendingRebuild}}
	if started := h.dispatch(t); started != 0 {
		t.Fatalf("started %d while upkeep was pending", started)
	}
	hold, found := h.hold(t)
	if !found || hold.Reason != domain.HoldImageUpkeepPending || hold.ImageUpkeep == nil ||
		hold.ImageUpkeep.Pending != domain.ImageUpkeepPendingRebuild {
		t.Fatalf("hold = %+v, found %t; want an image_upkeep_pending rebuild", hold, found)
	}
	if _, err := h.admission(t); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("admission = %v, want none recorded while held", err)
	}

	h.selectErr, h.selected = nil, selectionTestImage
	if started := h.dispatch(t); started != 1 {
		t.Fatalf("started %d once an image served the base, want 1", started)
	}
	if hold, found := h.hold(t); found {
		t.Fatalf("hold = %+v after dispatch, want it cleared", hold)
	}
	admission, err := h.admission(t)
	if err != nil || admission.ImageRef != selectionTestImage {
		t.Fatalf("admission image = %q, %v; want the selected image", admission.ImageRef, err)
	}
}

// A base no image serves, with nothing pending, holds visibly under
// admission_policy_refused with no check detail, and resumes once an image
// serves it.
func TestUnservedBaseHoldsAndResumes(t *testing.T) {
	h := newSelectionHoldFixture(t)
	h.selectErr = ErrProjectImageUnserved
	if started := h.dispatch(t); started != 0 {
		t.Fatalf("started %d with an unserved base", started)
	}
	hold, found := h.hold(t)
	if !found || hold.Reason != domain.HoldAdmissionPolicyRefused || hold.Detail != nil || hold.ImageUpkeep != nil {
		t.Fatalf("hold = %+v, found %t; want admission_policy_refused with no detail", hold, found)
	}

	h.selectErr, h.selected = nil, selectionTestImage
	if started := h.dispatch(t); started != 1 {
		t.Fatalf("started %d once an image served the base, want 1", started)
	}
}

// identityBackend is a runner backend that proves one image at a time.
type identityBackend struct {
	exec.RunnerBackend
	image  string
	digest domain.Digest
}

func (b identityBackend) ConformanceIdentity() (string, domain.Digest) { return b.image, b.digest }

// An unattended admission never names an image the backend does not prove:
// it is refused and held under backend_not_conformant, with no admission
// recorded.
func TestAdmissionRefusesAnImageTheBackendDoesNotProve(t *testing.T) {
	h := newSelectionHoldFixture(t)
	h.engine.admission.environment.OperatingMode = domain.ModeUnattended
	h.engine.admission.backend = identityBackend{
		RunnerBackend: h.engine.admission.backend,
		image:         "ghcr.io/owner/project@sha256:" + strings.Repeat("d", 64),
		digest:        domain.Digest("sha256:" + strings.Repeat("4", 64)),
	}
	h.selected = selectionTestImage
	if started := h.dispatch(t); started != 0 {
		t.Fatalf("started %d under an unproved image", started)
	}
	hold, found := h.hold(t)
	if !found || hold.Reason != domain.HoldBackendNotConformant {
		t.Fatalf("hold = %+v, found %t; want backend_not_conformant", hold, found)
	}
	if _, err := h.admission(t); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("admission = %v, want none recorded", err)
	}
}
