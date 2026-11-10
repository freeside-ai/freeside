package engine

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/exec"
	"github.com/freeside-ai/freeside/daemon/internal/exec/fake"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/store/storetest"
	"github.com/freeside-ai/freeside/daemon/internal/verify"
)

var hostVerificationAdmittedAt = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func hostVerificationBackend(caps ...exec.Capability) fake.RunnerBackend {
	return fake.RunnerBackend{
		BackendName: string(domain.BackendCredentialFreeHostVerification),
		Caps:        exec.NewCapabilitySet(caps...),
	}
}

func hostVerificationEngine(
	t *testing.T, mode domain.OperatingMode, backend exec.ConfigurationBoundBackend,
) *Engine {
	t.Helper()
	st := storetest.Open(t, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	e := &Engine{
		store: st,
		admission: &admitter{
			environment: AdmissionEnvironment{OperatingMode: mode},
			now:         func() time.Time { return hostVerificationAdmittedAt },
		},
	}
	if err := WithHostVerificationAdmission(backend, newHostRoomForTest)(e); err != nil {
		t.Fatalf("WithHostVerificationAdmission: %v", err)
	}
	return e
}

func recordHostConformance(
	t *testing.T, e *Engine, outcome domain.ConformanceOutcome, digest domain.Digest, caps domain.CapabilitySnapshot,
) {
	t.Helper()
	record, err := domain.NewBackendConformance(domain.BackendConformanceInput{
		Backend:             domain.BackendCredentialFreeHostVerification,
		Outcome:             outcome,
		ConfigurationDigest: digest,
		Capabilities:        caps,
		ProvedAt:            hostVerificationAdmittedAt,
	})
	if err != nil {
		t.Fatalf("NewBackendConformance: %v", err)
	}
	if err := e.store.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		_, err := tx.RecordBackendConformance(context.Background(), record)
		return err
	}); err != nil {
		t.Fatalf("RecordBackendConformance: %v", err)
	}
}

// hostRoomForTest is the room an admitted host job runs in; the gate tests
// never run a command in it.
type hostRoomForTest struct{}

func (hostRoomForTest) Run(context.Context, string, []string) (verify.StepResult, error) {
	return verify.StepResult{}, nil
}

func newHostRoomForTest() (verify.Room, error) { return hostRoomForTest{}, nil }

// verificationTask is a run's verification job for the production writer's
// export: its verification id is the one the task's producer derives.
func verificationTask(runID domain.RunID, producer domain.InvocationID) productionPublicationTask {
	return productionPublicationTask{
		RunID:                 runID,
		ProducingInvocationID: producer,
		VerificationID:        productionVerificationInvocationIDForProducer(runID, producer),
	}
}

// TestHostVerificationAdmission is the gate end to end: attended_dev admits
// whatever the executor declares and records it; unattended needs the
// declaration, a bound digest, and a current passed record for that digest.
func TestHostVerificationAdmission(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-host")
	task := verificationTask(runID, productionInvocationID(runID))
	declared := domain.NewCapabilitySnapshot(domain.CapCredentialFreeHostVerification)

	t.Run("attended admits an empty declaration", func(t *testing.T) {
		t.Parallel()
		backend := hostVerificationBackend()
		e := hostVerificationEngine(t, domain.ModeAttendedDev, backend)
		got, err := e.admitHostVerification(ctx, task)
		if err != nil {
			t.Fatalf("admitHostVerification = %v, want admitted", err)
		}
		want := HostVerificationAdmission{
			InvocationID:               productionVerificationInvocationID(runID),
			RunID:                      runID,
			Backend:                    string(domain.BackendCredentialFreeHostVerification),
			BackendConfigurationDigest: backend.ConfigurationDigest(),
			OperatingMode:              domain.ModeAttendedDev,
			AdmittedAt:                 hostVerificationAdmittedAt,
		}
		if !hostVerificationAdmissionEqual(got, want) {
			t.Errorf("admission = %+v, want %+v", got, want)
		}
	})

	t.Run("unattended refuses an empty declaration by naming the capability", func(t *testing.T) {
		t.Parallel()
		e := hostVerificationEngine(t, domain.ModeUnattended, hostVerificationBackend())
		_, err := e.admitHostVerification(ctx, task)
		var refusal *HostVerificationRefusal
		if !errors.As(err, &refusal) || !errors.Is(err, ErrHostVerificationNotAdmissible) {
			t.Fatalf("admitHostVerification = %v, want a HostVerificationRefusal", err)
		}
		if !slices.Equal(refusal.Missing, []exec.Capability{exec.CapCredentialFreeHostVerification}) {
			t.Errorf("refusal missing = %v, want the executor capability", refusal.Missing)
		}
	})

	t.Run("unattended refuses without a conformance record", func(t *testing.T) {
		t.Parallel()
		e := hostVerificationEngine(t, domain.ModeUnattended,
			hostVerificationBackend(exec.CapCredentialFreeHostVerification))
		if _, err := e.admitHostVerification(ctx, task); !errors.Is(err, store.ErrBackendNotConformant) {
			t.Fatalf("admitHostVerification = %v, want ErrBackendNotConformant", err)
		}
	})

	t.Run("unattended refuses a failed record", func(t *testing.T) {
		t.Parallel()
		backend := hostVerificationBackend(exec.CapCredentialFreeHostVerification)
		e := hostVerificationEngine(t, domain.ModeUnattended, backend)
		recordHostConformance(t, e, domain.ConformanceFailed, backend.ConfigurationDigest(), nil)
		if _, err := e.admitHostVerification(ctx, task); !errors.Is(err, store.ErrBackendNotConformant) {
			t.Fatalf("admitHostVerification = %v, want ErrBackendNotConformant", err)
		}
	})

	t.Run("unattended refuses a record for another configuration", func(t *testing.T) {
		t.Parallel()
		backend := hostVerificationBackend(exec.CapCredentialFreeHostVerification)
		e := hostVerificationEngine(t, domain.ModeUnattended, backend)
		other := domain.Digest("sha256:" + strings.Repeat("2", 64))
		recordHostConformance(t, e, domain.ConformancePassed, other, declared)
		if _, err := e.admitHostVerification(ctx, task); !errors.Is(err, domain.ErrAdmissionConfigurationMismatch) {
			t.Fatalf("admitHostVerification = %v, want ErrAdmissionConfigurationMismatch", err)
		}
	})

	t.Run("unattended refuses a declaration beyond the class ceiling", func(t *testing.T) {
		t.Parallel()
		// A forged declaration carrying a ward proof: the record for the
		// fake's digest can only ever hold the executor capability.
		backend := hostVerificationBackend(exec.CapCredentialFreeHostVerification, exec.CapNetworklessExport)
		e := hostVerificationEngine(t, domain.ModeUnattended, backend)
		recordHostConformance(t, e, domain.ConformancePassed, backend.ConfigurationDigest(), declared)
		if _, err := e.admitHostVerification(ctx, task); !errors.Is(err, domain.ErrAdmissionExceedsConformance) {
			t.Fatalf("admitHostVerification = %v, want ErrAdmissionExceedsConformance", err)
		}
	})

	t.Run("unattended refuses an unbound configuration digest", func(t *testing.T) {
		t.Parallel()
		backend := unboundHostBackend{hostVerificationBackend(exec.CapCredentialFreeHostVerification)}
		e := hostVerificationEngine(t, domain.ModeUnattended, backend)
		if _, err := e.admitHostVerification(ctx, task); !errors.Is(err, domain.ErrConformanceConfigurationUnbound) {
			t.Fatalf("admitHostVerification = %v, want ErrConformanceConfigurationUnbound", err)
		}
	})

	t.Run("unattended admits under a current passed record", func(t *testing.T) {
		t.Parallel()
		backend := hostVerificationBackend(exec.CapCredentialFreeHostVerification)
		e := hostVerificationEngine(t, domain.ModeUnattended, backend)
		recordHostConformance(t, e, domain.ConformancePassed, backend.ConfigurationDigest(), declared)
		got, err := e.admitHostVerification(ctx, task)
		if err != nil {
			t.Fatalf("admitHostVerification = %v, want admitted", err)
		}
		if got.OperatingMode != domain.ModeUnattended ||
			!slices.Equal(got.Capabilities, declared) ||
			got.BackendConfigurationDigest != backend.ConfigurationDigest() {
			t.Errorf("admission = %+v", got)
		}
		// A recheck that begins supersedes the pass, and the gate holds the
		// job out until the recheck concludes.
		recordHostConformance(t, e, domain.ConformanceSuperseded, backend.ConfigurationDigest(), nil)
		if _, err := e.admitHostVerification(ctx, task); !errors.Is(err, store.ErrBackendNotConformant) {
			t.Fatalf("admitHostVerification during a recheck = %v, want ErrBackendNotConformant", err)
		}
	})
}

// TestHostVerificationAdmissionRefusesWardInvocations proves the executor
// serves a run's verification jobs and never a ward role: a task whose
// verification id names a writer, remediator, or specifier invocation is
// refused by naming the role, and so is one whose id its producer does not
// derive. Every producer's own verification job is admitted.
func TestHostVerificationAdmissionRefusesWardInvocations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-roles")
	e := hostVerificationEngine(t, domain.ModeAttendedDev, hostVerificationBackend())

	for name, producer := range map[string]domain.InvocationID{
		"production writer":  productionInvocationID(runID),
		"remediation round":  remediationInvocationID(runID, 2),
		"operator feedback":  operatorFeedbackInvocationID("cmd-1"),
		"another producer's": productionInvocationID("run-other"),
	} {
		task := verificationTask(runID, producer)
		_, err := e.admitHostVerification(ctx, task)
		if name == "another producer's" {
			// The producer belongs to another run, so nothing derives a
			// verification id for it here.
			if !errors.Is(err, ErrHostVerificationNotAdmissible) {
				t.Errorf("%s: admitHostVerification = %v, want refused", name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s verification: admitHostVerification = %v, want admitted", name, err)
		}
	}

	for name, tc := range map[string]struct {
		id   domain.InvocationID
		role domain.RoleName
	}{
		"production writer": {productionInvocationID(runID), domain.RoleImplementer},
		"remediation round": {remediationInvocationID(runID, 1), domain.RoleRemediator},
		"operator feedback": {operatorFeedbackInvocationID("cmd-1"), domain.RoleRemediator},
		"specification":     {specificationInvocationID(runID, 1), domain.RoleSpecifier},
	} {
		task := verificationTask(runID, productionInvocationID(runID))
		task.VerificationID = tc.id
		_, err := e.admitHostVerification(ctx, task)
		var refusal *HostVerificationRefusal
		if !errors.As(err, &refusal) || !strings.Contains(refusal.Reason, string(tc.role)) {
			t.Errorf("%s: admitHostVerification = %v, want a refusal naming %s", name, err, tc.role)
		}
	}

	// A trust re-evaluation reruns the same verification under its own
	// suffixed invocation, still derived from the task.
	rerun := verificationTask(runID, productionInvocationID(runID))
	rerun.reevaluation = &productionReevaluation{CommandID: "cmd-rerun"}
	got, err := e.admitHostVerification(ctx, rerun)
	if err != nil {
		t.Fatalf("re-evaluation verification = %v, want admitted", err)
	}
	if want := rerun.verificationInvocationID(); got.InvocationID != want || !strings.HasSuffix(string(want), "/reevaluation/cmd-rerun") {
		t.Errorf("re-evaluation admission invocation = %q, want %q", got.InvocationID, want)
	}

	forged := verificationTask(runID, productionInvocationID(runID))
	forged.VerificationID = "verify-something-else"
	if _, err := e.admitHostVerification(ctx, forged); !errors.Is(err, domain.ErrParentKeyMismatch) {
		t.Errorf("underived verification id = %v, want ErrParentKeyMismatch", err)
	}
}

// TestHostVerificationAdmissionDerivesReentryAndHandoffIDs proves the gate
// admits a re-entry's and a handoff merge check's verification job when its
// id derives from the task's own sealed successor or publication, and
// refuses one derived from another run's successor or another publication.
func TestHostVerificationAdmissionDerivesReentryAndHandoffIDs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := hostVerificationEngine(t, domain.ModeAttendedDev, hostVerificationBackend())
	successor := reentrySuccessorForTest(
		productionReadyItemID("run-reentry"), domain.ReadinessInvalidationBaseAdvanced)
	reentry := newReentryTask("project-reentry", successor)

	firstCycle := verificationTask("run-reentry", productionInvocationID("run-reentry"))
	firstCycle.PublicationID = domain.ProductionPublicationInvocationID("run-reentry")
	firstHandoff := firstCycle
	firstHandoff.VerificationID = handoffVerificationInvocationID(firstCycle)
	reentryHandoff := reentry
	reentryHandoff.VerificationID = handoffVerificationInvocationID(reentry)

	for name, task := range map[string]productionPublicationTask{
		"re-entry":                 reentry,
		"handoff of a first cycle": firstHandoff,
		"handoff of a re-entry":    reentryHandoff,
	} {
		got, err := e.admitHostVerification(ctx, task)
		if err != nil {
			t.Errorf("%s: admitHostVerification = %v, want admitted", name, err)
			continue
		}
		if got.InvocationID != task.VerificationID {
			t.Errorf("%s: admission invocation = %q, want %q", name, got.InvocationID, task.VerificationID)
		}
	}

	otherSuccessor := successor
	otherSuccessor.RunID = "run-other"
	otherSuccessor.PredecessorItemID = productionReadyItemID("run-other")
	otherSuccessor.PriorReviewInvocationID = ProductionReviewInvocationID("run-other", 1)
	stolen := reentry
	stolen.VerificationID = reentryVerificationInvocationID(otherSuccessor)
	borrowed := reentry
	borrowed.Successor = &otherSuccessor
	otherPublication := firstHandoff
	otherPublication.PublicationID = domain.ProductionPublicationInvocationID("run-other")
	otherPublication.VerificationID = handoffVerificationInvocationID(otherPublication)
	mismatchedPublication := reentry
	mismatchedPublication.PublicationID = domain.ProductionPublicationInvocationID("run-reentry")
	for name, task := range map[string]productionPublicationTask{
		"another run's re-entry id":              stolen,
		"another run's successor":                borrowed,
		"another publication's handoff":          otherPublication,
		"re-entry id beside a stray publication": mismatchedPublication,
	} {
		if _, err := e.admitHostVerification(ctx, task); !errors.Is(err, domain.ErrParentKeyMismatch) {
			t.Errorf("%s: admitHostVerification = %v, want ErrParentKeyMismatch", name, err)
		}
	}

	// A ward role stays refused on a re-entry task too.
	writer := reentry
	writer.VerificationID = productionInvocationID("run-reentry")
	var refusal *HostVerificationRefusal
	if _, err := e.admitHostVerification(ctx, writer); !errors.As(err, &refusal) ||
		!strings.Contains(refusal.Reason, string(domain.RoleImplementer)) {
		t.Errorf("writer id on a re-entry task = %v, want the ward-role refusal", err)
	}
}

// TestHostVerificationRoomConstructionRefuses: an admitted job whose room the
// configured constructor cannot build is refused, never run elsewhere.
func TestHostVerificationRoomConstructionRefuses(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := hostVerificationEngine(t, domain.ModeAttendedDev, hostVerificationBackend())
	broken := errors.New("no executor root")
	e.newHostVerificationRoom = func() (verify.Room, error) { return nil, broken }
	task := verificationTask("run-room", productionInvocationID("run-room"))
	if _, _, err := e.admitHostVerificationRoom(ctx, task); !errors.Is(err, ErrHostVerificationNotAdmissible) ||
		!errors.Is(err, broken) {
		t.Fatalf("admitHostVerificationRoom = %v, want a refusal carrying the constructor's error", err)
	}
	e.newHostVerificationRoom = newHostRoomForTest
	room, admission, err := e.admitHostVerificationRoom(ctx, task)
	if err != nil || room == nil || admission.InvocationID != task.VerificationID {
		t.Fatalf("admitHostVerificationRoom = %v, %+v, %v; want the room", room, admission, err)
	}
}

// TestHostVerificationAdmissionNeedsConfiguration pins the gate's
// composition checks: no backend, no admitter, or a backend of another class
// is never admitted, and the ward admitter refuses the executor as its
// backend, so a writer, remediator, or specifier stage cannot land on it.
func TestHostVerificationAdmissionNeedsConfiguration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run-config")
	task := verificationTask(runID, productionInvocationID(runID))

	if _, err := (&Engine{admission: &admitter{now: time.Now}}).admitHostVerification(ctx, task); !errors.Is(err, ErrHostVerificationNotAdmissible) {
		t.Errorf("no host backend = %v, want refused", err)
	}
	if _, err := (&Engine{hostVerification: hostVerificationBackend()}).admitHostVerification(ctx, task); !errors.Is(err, ErrHostVerificationNotAdmissible) {
		t.Errorf("no admitter = %v, want refused", err)
	}
	if err := WithHostVerificationAdmission(nil, newHostRoomForTest)(&Engine{}); err == nil {
		t.Error("WithHostVerificationAdmission accepted a nil backend")
	}
	if err := WithHostVerificationAdmission(hostVerificationBackend(), nil)(&Engine{}); err == nil {
		t.Error("WithHostVerificationAdmission accepted a nil room constructor")
	}
	ward := fake.RunnerBackend{BackendName: string(domain.BackendFreshVMReadOnlyVolumeHandoff)}
	if err := WithHostVerificationAdmission(ward, newHostRoomForTest)(&Engine{}); err == nil {
		t.Error("WithHostVerificationAdmission accepted a ward-class backend")
	}

	option := WithAdmission(
		hostVerificationBackend(exec.CapCredentialFreeHostVerification), nil,
		AdmissionEnvironment{
			PromptPackageDigest: domain.Digest("sha256:" + strings.Repeat("1", 64)),
			VendorInstructions: VendorInstructionConfig{
				Vendor:   domain.AgentVendorClaude,
				Delivery: domain.VendorInstructionDeliveryAppendFile,
				HostPath: "/nonexistent/freeside-test-claude-instructions",
			},
		},
		time.Now,
	)
	if err := option(&Engine{}); err == nil || !strings.Contains(err.Error(), "cannot admit ward stages") {
		t.Errorf("WithAdmission with the executor = %v, want the ward-stage refusal", err)
	}

	// An operating mode outside the vocabulary refuses instead of skipping
	// the unattended conformance checks.
	for _, mode := range []domain.OperatingMode{"", "Unattended"} {
		e := hostVerificationEngine(t, mode, hostVerificationBackend())
		if _, err := e.admitHostVerification(ctx, task); !errors.Is(err, domain.ErrInvalidOperatingMode) {
			t.Errorf("mode %q: admitHostVerification = %v, want ErrInvalidOperatingMode", mode, err)
		}
	}
}

// unboundHostBackend declares the executor capability but reports the
// unbound configuration sentinel, as a backend with no proven configuration
// identity would.
type unboundHostBackend struct{ fake.RunnerBackend }

func (unboundHostBackend) ConfigurationDigest() domain.Digest {
	return domain.UnboundBackendConfigurationDigest
}

func hostVerificationAdmissionEqual(a, b HostVerificationAdmission) bool {
	return a.InvocationID == b.InvocationID && a.RunID == b.RunID && a.Backend == b.Backend &&
		slices.Equal(a.Capabilities, b.Capabilities) &&
		a.BackendConfigurationDigest == b.BackendConfigurationDigest &&
		a.OperatingMode == b.OperatingMode && a.AdmittedAt.Equal(b.AdmittedAt)
}

// TestVerificationRoomsKeepsACanceledGateRetryable: a pass canceled while the
// gate reads the store, or a gate failure with a transient cause, is not the
// gate's verdict, so it stays retryable rather than becoming a durable failed
// verification. A plain gate error on a live pass is a refusal.
func TestVerificationRoomsKeepsACanceledGateRetryable(t *testing.T) {
	t.Parallel()
	recipe := []byte(`{"commands":[{"argv":["/usr/bin/true"],"room":"macos_host"}],"capture":"none"}`)
	gateErr := errors.New("read host conformance: interrupted")
	w := &productionPublicationWorkflow{hostVerificationRoom: func(
		context.Context, productionPublicationTask,
	) (verify.Room, HostVerificationAdmission, error) {
		return nil, HostVerificationAdmission{}, gateErr
	}}
	task := verificationTask("run-cancel", productionInvocationID("run-cancel"))
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	var unavailable *hostVerificationUnavailable
	_, err := w.verificationRooms(canceled, task, hostRoomForTest{}, recipe)
	if errors.As(err, &unavailable) || !productionPublicationRetryableFailure(err) {
		t.Fatalf("canceled gate = %v, want retryable and not a refusal", err)
	}
	_, err = w.verificationRooms(context.Background(), task, hostRoomForTest{}, recipe)
	if !errors.As(err, &unavailable) || !errors.Is(err, gateErr) {
		t.Fatalf("live gate error = %v, want a refusal carrying it", err)
	}

	// A refusal whose cause is an environmental failure, such as a store
	// read past its deadline, is not a verdict either.
	gateErr = &HostVerificationRefusal{Reason: "read conformance", cause: context.DeadlineExceeded}
	_, err = w.verificationRooms(context.Background(), task, hostRoomForTest{}, recipe)
	if errors.As(err, &unavailable) || !productionPublicationRetryableFailure(err) {
		t.Fatalf("transient gate failure = %v, want retryable and not a refusal", err)
	}
}
