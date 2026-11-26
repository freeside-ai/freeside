package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/freeside-ai/freeside/daemon/internal/contentaddr"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// reconfigurableBackend is a ward-like backend whose configuration, and so
// the digest each image yields, the test changes the way a daemon upgrade
// does. It proves the images in proved.
type reconfigurableBackend struct {
	exec.RunnerBackend
	configuration *string
	proved        *[]string
}

func (b reconfigurableBackend) Name() string {
	return string(domain.BackendFreshVMReadOnlyVolumeHandoff)
}

func (b reconfigurableBackend) ConfigurationDigest() domain.Digest { return b.digestFor("") }

func (b reconfigurableBackend) digestFor(image string) domain.Digest {
	return domain.Digest(contentaddr.Sum([]byte(*b.configuration + " configuration of " + image)))
}

func (b reconfigurableBackend) ConformanceFor(image string) (domain.Digest, exec.CapabilitySet, bool) {
	caps := b.Capabilities()
	proved := slices.Contains(*b.proved, image)
	if proved {
		caps[exec.CapNetworklessExport] = struct{}{}
		caps[exec.CapEnforcedProviderEgress] = struct{}{}
	}
	return b.digestFor(image), caps, proved
}

// startAuthorityDriver refuses to start an invocation whose stored admission
// binds another configuration than the backend now yields for its image, as
// the production start authority does before the driver commits an intent
// (admissionauth.Authority.requireRunnableConfiguration).
type startAuthorityDriver struct {
	exec.StageDriver
	store   *store.Store
	backend reconfigurableBackend
}

func (d startAuthorityDriver) Start(ctx context.Context, id domain.InvocationID, spec exec.StartSpec) error {
	var admission domain.ExecutionAdmission
	if err := d.store.Read(ctx, func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(ctx, id)
		return err
	}); err != nil {
		return err
	}
	if admission.ID != spec.AdmissionID {
		return fmt.Errorf("start of %s cites admission %s, the store holds %s", id, spec.AdmissionID, admission.ID)
	}
	if running := d.backend.digestFor(string(admission.ImageRef)); running != admission.BackendConfigurationDigest {
		return fmt.Errorf("admission %s binds %s, the backend yields %s: %w",
			admission.ID, admission.BackendConfigurationDigest, running, domain.ErrAdmissionConfigurationMismatch)
	}
	return d.StageDriver.Start(ctx, id, spec)
}

// startedThenFailed starts its first invocation and then reports a failure,
// as a daemon that crashed after the driver committed its intent and before
// the dispatch was recorded would.
type startedThenFailed struct {
	exec.StageDriver
	failed bool
}

func (d *startedThenFailed) Start(ctx context.Context, id domain.InvocationID, spec exec.StartSpec) error {
	if err := d.StageDriver.Start(ctx, id, spec); err != nil || d.failed {
		return err
	}
	d.failed = true
	return errors.New("injected failure after the driver started")
}

// readmissionFixture is an unattended specification run whose attempt is
// recorded under configuration "A" and left unstarted or started as the
// driver wrapper decides.
type readmissionFixture struct {
	*selectionHoldFixture
	backend       reconfigurableBackend
	configuration string
	proved        []string
}

func newReadmissionFixture(t *testing.T, wrap func(exec.StageDriver) exec.StageDriver) *readmissionFixture {
	t.Helper()
	h := &readmissionFixture{selectionHoldFixture: newSelectionHoldFixture(t), configuration: "A"}
	h.backend = reconfigurableBackend{
		RunnerBackend: h.engine.admission.backend, configuration: &h.configuration, proved: &h.proved,
	}
	h.engine.admission.backend = h.backend
	h.engine.admission.environment.OperatingMode = domain.ModeUnattended
	h.engine.admission.environment.ReviewConfigurationDigest = "sha256:review-config"
	h.engine.driver = startAuthorityDriver{
		StageDriver: wrap(h.engine.driver), store: h.fixture.store, backend: h.backend,
	}
	h.selected = selectionTestImage
	seedUnattendedAdmissionTrust(t, h.fixture.store, h.engine.admission.environment.Base)
	h.prove(t)
	if _, err := h.engine.dispatchPendingInvocations(t.Context()); err == nil {
		t.Fatal("the injected start failure did not surface")
	}
	if got := h.storedAdmission(t).BackendConfigurationDigest; got != h.digest() {
		t.Fatalf("recorded admission binds %s, want configuration A's %s", got, h.digest())
	}
	return h
}

// digest is the configuration digest the backend now yields for the
// attempt's image.
func (h *readmissionFixture) digest() domain.Digest {
	return h.backend.digestFor(string(selectionTestImage))
}

// prove publishes ward's proof of the image under the current
// configuration and records its passed conformance.
func (h *readmissionFixture) prove(t *testing.T) {
	t.Helper()
	h.proved = []string{string(selectionTestImage)}
	ceiling, _ := domain.ProvableCapabilities(domain.BackendFreshVMReadOnlyVolumeHandoff)
	record, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend: domain.BackendFreshVMReadOnlyVolumeHandoff, Outcome: domain.ConformancePassed,
		ConfigurationDigest: h.digest(), Capabilities: ceiling, ProvedAt: unattendedExportEpoch,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.fixture.store.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		_, err := tx.RecordBackendConformance(t.Context(), domain.DaemonHostID, record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func (h *readmissionFixture) storedAdmission(t *testing.T) domain.ExecutionAdmission {
	t.Helper()
	var admission domain.ExecutionAdmission
	if err := h.fixture.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		admission, err = tx.GetExecutionAdmissionRecord(t.Context(), h.invocationID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return admission
}

func (h *readmissionFixture) superseded(t *testing.T) []domain.ExecutionAdmission {
	t.Helper()
	var admissions []domain.ExecutionAdmission
	if err := h.fixture.store.Read(t.Context(), func(tx *store.ReadTx) error {
		var err error
		admissions, err = tx.ListSupersededExecutionAdmissions(t.Context(), h.invocationID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return admissions
}

func (h *readmissionFixture) requireHold(t *testing.T, want domain.RunHoldReason) {
	t.Helper()
	hold, found := h.hold(t)
	if !found || hold.Reason != want {
		t.Fatalf("hold = %+v, found %t; want %s", hold, found, want)
	}
}

func (h *readmissionFixture) notice(t *testing.T, occurrence int) (domain.AttentionItem, bool) {
	t.Helper()
	item, found, err := readProductionQuarantineItem(t.Context(), h.fixture.store,
		productionQuarantineOccurrenceID(admissionConfigurationChangedPrefix, "specification-run", occurrence))
	if err != nil {
		t.Fatal(err)
	}
	return item, found
}

// A recorded attempt the driver never started is admitted again under the
// configuration the upgraded daemon yields, and starts. A fresh admission
// the gates refuse holds as a new one would and keeps the stored admission,
// and a later pass retries (#3433).
func TestReadmissionStartsAnAttemptTheDriverNeverStarted(t *testing.T) {
	t.Parallel()
	h := newReadmissionFixture(t, func(driver exec.StageDriver) exec.StageDriver {
		return &failFirstStart{StageDriver: driver}
	})
	stale := h.storedAdmission(t)

	// The daemon stopped while the credentialed attempt held its dispatch
	// reservation.
	if stale.AuthIdentityID == nil {
		t.Fatal("the fixture's admission holds no credential, so it takes no reservation")
	}
	if err := h.fixture.store.WriteInternal(t.Context(), func(tx *store.InternalTx) error {
		return tx.MarkOutboxDispatching(t.Context(), string(h.invocationID))
	}); err != nil {
		t.Fatal(err)
	}

	// The upgrade changes the digest, and ward holds no proof of the image
	// under it yet: the fresh admission is refused, nothing changes, and the
	// held attempt keeps no pool slot.
	h.configuration = "B"
	h.proved = nil
	if started := h.dispatch(t); started != 0 {
		t.Fatalf("started %d under an unproved configuration", started)
	}
	h.requireHold(t, domain.HoldBackendNotConformant)
	if err := h.fixture.store.Read(t.Context(), func(tx *store.ReadTx) error {
		entry, err := tx.GetOutbox(t.Context(), string(h.invocationID))
		if err == nil && entry.Status != "pending" {
			t.Errorf("held attempt's intent is %q, want its reservation released to pending", entry.Status)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := h.storedAdmission(t); got.ID != stale.ID {
		t.Fatalf("a refused re-admission replaced the stored admission with %s", got.ID)
	}
	if got := h.superseded(t); len(got) != 0 {
		t.Fatalf("a refused re-admission superseded %d admissions", len(got))
	}

	h.prove(t)
	if started := h.dispatch(t); started != 1 {
		t.Fatalf("started %d after the proof, want the re-admitted attempt", started)
	}
	fresh := h.storedAdmission(t)
	if fresh.BackendConfigurationDigest != h.digest() {
		t.Fatalf("re-admission binds %s, want configuration B's %s", fresh.BackendConfigurationDigest, h.digest())
	}
	if got := h.superseded(t); len(got) != 1 || got[0].ID != stale.ID {
		t.Fatalf("superseded admissions = %+v, want the configuration A admission", got)
	}
	if _, err := h.engine.driver.Inspect(t.Context(), h.invocationID); err != nil {
		t.Fatalf("the driver does not know the re-admitted attempt: %v", err)
	}
	if _, found := h.hold(t); found {
		t.Fatal("the hold outlived the start")
	}
}

// A recorded attempt the driver already started may have run its writer,
// so it is never admitted again: it keeps its backend_not_conformant hold
// and raises one notice, which later passes converge on and the start
// releases.
func TestReadmissionLeavesAStartedAttemptHeldWithANotice(t *testing.T) {
	t.Parallel()
	h := newReadmissionFixture(t, func(driver exec.StageDriver) exec.StageDriver {
		return &startedThenFailed{StageDriver: driver}
	})
	stale := h.storedAdmission(t)
	h.configuration = "B"
	h.prove(t)
	for pass := 1; pass <= 2; pass++ {
		if started := h.dispatch(t); started != 0 {
			t.Fatalf("pass %d started %d", pass, started)
		}
		h.requireHold(t, domain.HoldBackendNotConformant)
		item, found := h.notice(t, 1)
		if !found || item.Status != domain.StatusOpen || item.Type != domain.AttentionExecutionFailure ||
			item.Reason != admissionConfigurationChangedReason(h.invocationID) {
			t.Fatalf("pass %d notice = %+v, found %t; want the open execution_failure notice", pass, item, found)
		}
		if _, found := h.notice(t, 2); found {
			t.Fatalf("pass %d raised a second notice", pass)
		}
	}
	if got := h.storedAdmission(t); got.ID != stale.ID {
		t.Fatalf("a started attempt was re-admitted as %s", got.ID)
	}
	if got := h.superseded(t); len(got) != 0 {
		t.Fatalf("a started attempt superseded %d admissions", len(got))
	}

	// Back on the configuration it started under, the attempt starts and
	// the notice is released.
	h.configuration = "A"
	if started := h.dispatch(t); started != 0 {
		t.Fatalf("started %d; the driver had already started the attempt", started)
	}
	if item, _ := h.notice(t, 1); item.Status == domain.StatusOpen {
		t.Fatal("the notice outlived the start")
	}
	if _, found := h.hold(t); found {
		t.Fatal("the hold outlived the start")
	}
}
