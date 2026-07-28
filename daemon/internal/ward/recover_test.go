package ward

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
)

const testRecoveryToken = "00112233445566778899aabbccddeeff"

// recoveryFixture is a handoff fixture plus the journal and leaser recovery
// needs, and helpers to construct the post-crash world directly: a real kill
// cannot be simulated in-process (Handoff's deferred teardown always runs),
// so tests place the dead run's objects in the fake runtime themselves,
// labeled with the persisted token, exactly as the crashed process left
// them.
type recoveryFixture struct {
	*handoffFixture
	j *fakeJournal
	l *fakeLeaser
}

func newRecoveryFixture(t *testing.T) *recoveryFixture {
	t.Helper()
	fx := newHandoffFixture(t)
	j := fx.journalled()
	base := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	fx.cfg.Now = func() time.Time { return base }
	l := &fakeLeaser{rt: fx.rt}
	fx.cfg.AuthStoreLeaser = l
	return &recoveryFixture{handoffFixture: fx, j: j, l: l}
}

// openRecord journals an open record for hs bound to the fixture token.
func (fx *recoveryFixture) openRecord(t *testing.T, hs HandoffSpec) HandoffJournalRecord {
	t.Helper()
	digest, err := specDigest(hs)
	if err != nil {
		t.Fatal(err)
	}
	rec := HandoffJournalRecord{
		RunID:          hs.RunID,
		OwnershipToken: testRecoveryToken,
		SpecDigest:     digest,
		OpenedAt:       time.Date(2026, 7, 1, 11, 0, 0, 0, time.UTC),
	}
	if hs.AuthStoreLease != nil {
		// The recorded window matches the live rows the matching-row tests
		// seed: the re-gate binds by exact window equality.
		rec.Lease = &HandoffJournalLease{
			AuthIdentityID: hs.AuthStoreLease.AuthIdentityID,
			Holder:         hs.AuthStoreLease.Holder,
			Fence:          1,
			AcquiredAt:     rec.OpenedAt.Add(-time.Hour),
			ExpiresAt:      rec.OpenedAt.Add(100 * time.Hour),
		}
	}
	if err := fx.j.Begin(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func (fx *recoveryFixture) runLabels(runID string) []Label {
	return []Label{{Key: labelKey, Value: runID}, {Key: ownershipLabelKey, Value: testRecoveryToken}}
}

func (fx *recoveryFixture) worldVolume(t *testing.T, name string, labels []Label) {
	t.Helper()
	if err := fx.rt.CreateVolume(context.Background(), name, 64, labels); err != nil {
		t.Fatal(err)
	}
}

func (fx *recoveryFixture) worldContainer(t *testing.T, spec ContainerSpec, running bool) {
	t.Helper()
	ctx := context.Background()
	if err := fx.rt.CreateContainer(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if running {
		if err := fx.rt.StartContainer(ctx, spec.Name); err != nil {
			t.Fatal(err)
		}
	}
}

func (fx *recoveryFixture) recover(t *testing.T, runID string, hs HandoffSpec) (*RecoveryResult, error) {
	t.Helper()
	res, err := fx.backend(t).Recover(context.Background(), runID, hs)
	if res != nil && res.ExportDir != "" {
		t.Cleanup(func() { _ = os.RemoveAll(res.ExportDir) })
	}
	return res, err
}

// wantClosed asserts the journal record's terminal outcome.
func (fx *recoveryFixture) wantClosed(t *testing.T, runID string, want HandoffJournalOutcome) {
	t.Helper()
	rec := fx.j.snapshot(runID)
	if rec == nil || rec.Outcome == nil || *rec.Outcome != want {
		t.Fatalf("journal record = %+v, want closed as %q", rec, want)
	}
}

// wantOpen asserts the journal record was not closed.
func (fx *recoveryFixture) wantOpen(t *testing.T, runID string) {
	t.Helper()
	rec := fx.j.snapshot(runID)
	if rec == nil || rec.Outcome != nil {
		t.Fatalf("journal record = %+v, want open", rec)
	}
}

// TestRecoveryOutcomeEnum pins the enum shape: every registered member is
// valid, and the zero value is invalid by design.
func TestRecoveryOutcomeEnum(t *testing.T) {
	for _, o := range AllRecoveryOutcomes {
		if !o.valid() {
			t.Errorf("registered outcome %q reports invalid", o)
		}
	}
	if RecoveryOutcome("").valid() {
		t.Error("zero RecoveryOutcome reports valid")
	}
}

// TestRecoverRefusals enumerates the reconstruction re-gate: every
// malformed, closed, diverged, or unsupported durable record refuses typed,
// with no destructive action, no runtime call, and no loss commit. The
// records are tampered in the journal itself — Recover reads the row, so the
// row is the trust boundary.
func TestRecoverRefusals(t *testing.T) {
	completed := HandoffCompleted
	cases := []struct {
		name    string
		prepare func(t *testing.T, fx *recoveryFixture) (string, HandoffSpec)
		wantErr error
	}{
		{"malformed token", func(t *testing.T, fx *recoveryFixture) (string, HandoffSpec) {
			hs := testHandoffSpec()
			rec := fx.openRecord(t, hs)
			rec.OwnershipToken = "not-hex"
			fx.j.put(rec)
			return hs.RunID, hs
		}, ErrInvalidJournalRecord},
		{"closed record", func(t *testing.T, fx *recoveryFixture) (string, HandoffSpec) {
			hs := testHandoffSpec()
			rec := fx.openRecord(t, hs)
			rec.Outcome = &completed
			fx.j.put(rec)
			return hs.RunID, hs
		}, ErrInvalidJournalRecord},
		{"spec mismatch", func(t *testing.T, fx *recoveryFixture) (string, HandoffSpec) {
			hs := testHandoffSpec()
			fx.openRecord(t, hs)
			diverged := hs
			diverged.Agent.Command = []string{"sh", "-c", "false"}
			return hs.RunID, diverged
		}, ErrInvalidJournalRecord},
		{"spec names another run", func(t *testing.T, fx *recoveryFixture) (string, HandoffSpec) {
			hs := testHandoffSpec()
			fx.openRecord(t, hs)
			other := hs
			other.RunID = "other-run"
			return hs.RunID, other
		}, ErrInvalidJournalRecord},
		{"lease disagreement", func(t *testing.T, fx *recoveryFixture) (string, HandoffSpec) {
			hs := testHandoffSpec()
			rec := fx.openRecord(t, hs)
			rec.Lease = &HandoffJournalLease{AuthIdentityID: "id", Holder: "h", Fence: 1}
			fx.j.put(rec)
			return hs.RunID, hs
		}, ErrInvalidJournalRecord},
		{"writer-complete leased record without pre-digest", func(t *testing.T, fx *recoveryFixture) (string, HandoffSpec) {
			hs := testLeasedHandoffSpec()
			rec := fx.openRecord(t, hs)
			rec.WriterComplete = true
			fx.j.put(rec)
			return hs.RunID, hs
		}, ErrInvalidJournalRecord},
		{"record lease names another identity", func(t *testing.T, fx *recoveryFixture) (string, HandoffSpec) {
			hs := testLeasedHandoffSpec()
			rec := fx.openRecord(t, hs)
			rec.Lease.AuthIdentityID = "other-identity"
			fx.j.put(rec)
			return hs.RunID, hs
		}, ErrInvalidJournalRecord},
		{"record lease names another holder", func(t *testing.T, fx *recoveryFixture) (string, HandoffSpec) {
			hs := testLeasedHandoffSpec()
			rec := fx.openRecord(t, hs)
			rec.Lease.Holder = "usurper"
			fx.j.put(rec)
			return hs.RunID, hs
		}, ErrInvalidJournalRecord},
		{"writer-complete seeded record without observed base", func(t *testing.T, fx *recoveryFixture) (string, HandoffSpec) {
			hs := testHandoffSpec()
			hs.Seed = WorkspaceSeed{Mode: SeedBaseCheckout, SourceDir: "/tmp/seed-fixture", Base: testBaseRevision()}
			rec := fx.openRecord(t, hs)
			rec.WriterComplete = true
			fx.j.put(rec)
			return hs.RunID, hs
		}, ErrInvalidJournalRecord},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newRecoveryFixture(t)
			runID, hs := tc.prepare(t, fx)
			_, err := fx.recover(t, runID, hs)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Recover = %v, want %v", err, tc.wantErr)
			}
			for _, c := range fx.rt.calls {
				if !strings.HasPrefix(c, "journal-") {
					t.Errorf("runtime call %q happened during a refusal", c)
				}
			}
		})
	}
}

func TestRecoverRefusesMismatchedBoundAuthStoreVolume(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testLeasedHandoffSpec()
	fx.openRecord(t, hs)
	fx.l.volume = "other-provider-cred"

	_, err := fx.recover(t, hs.RunID, hs)
	if !errors.Is(err, ErrInvalidJournalRecord) {
		t.Fatalf("Recover = %v, want ErrInvalidJournalRecord", err)
	}
	for _, call := range fx.rt.calls {
		if strings.HasPrefix(call, "journal-") || strings.HasPrefix(call, "identity-volume ") {
			continue
		}
		t.Errorf("runtime call %q happened after the binding refusal", call)
	}
}

// deadlineJournal asserts recovery's budget is in force before the first
// durable read: a stalling adapter must be bounded by the recovery deadline.
type deadlineJournal struct {
	*fakeJournal
	sawDeadline bool
}

func (j *deadlineJournal) Get(ctx context.Context, runID string) (HandoffJournalRecord, error) {
	_, j.sawDeadline = ctx.Deadline()
	return j.fakeJournal.Get(ctx, runID)
}

// TestAuditRunAbsentDetachesFromExpiredContext: the audit runs after the
// detached teardown, so it must survive the run deadline the same way — a
// spurious audit failure after teardown voids delivered work.
func TestAuditRunAbsentDetachesFromExpiredContext(t *testing.T) {
	fx := newHandoffFixture(t)
	b := fx.backend(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := b.auditRunAbsent(ctx, testOwnershipLabel()); err != nil {
		t.Fatalf("audit under an expired context = %v, want a clean detached pass", err)
	}
}

// TestRecoverBoundsDurableReads: the HandoffTimeout context wraps the
// journal and lease-store reads too, so an adapter that honors its context
// cannot block restart recovery indefinitely.
func TestRecoverBoundsDurableReads(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testHandoffSpec()
	fx.openRecord(t, hs)
	dj := &deadlineJournal{fakeJournal: fx.j}
	fx.cfg.Journal = dj

	if _, err := fx.recover(t, hs.RunID, hs); err != nil {
		t.Fatalf("Recover = %v, want committed loss", err)
	}
	if !dj.sawDeadline {
		t.Fatal("the journal read ran outside the recovery deadline")
	}
}

// TestRecoverMissingRecordRefuses: a run the journal has never seen has
// nothing durable to recover; the refusal touches nothing.
func TestRecoverMissingRecordRefuses(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testHandoffSpec()
	if _, err := fx.recover(t, hs.RunID, hs); err == nil {
		t.Fatal("Recover of an unjournalled run succeeded")
	}
	for _, c := range fx.rt.calls {
		if !strings.HasPrefix(c, "journal-") {
			t.Errorf("runtime call %q happened during a refusal", c)
		}
	}
}

// TestRecoverNilJournalRefuses: recovery without a journal has nothing
// durable to commit to.
func TestRecoverNilJournalRefuses(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testHandoffSpec()
	fx.openRecord(t, hs)
	fx.cfg.Journal = nil

	_, err := fx.recover(t, hs.RunID, hs)
	wantCheckFailure(t, err, CheckRecovery)
}

// TestRecoverLeasedNilLeaserRefuses: a leased record without a leaser cannot
// end its window and refuses up front.
func TestRecoverLeasedNilLeaserRefuses(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testLeasedHandoffSpec()
	fx.openRecord(t, hs)
	fx.cfg.AuthStoreLeaser = nil

	_, err := fx.recover(t, hs.RunID, hs)
	wantCheckFailure(t, err, CheckRecovery)
	fx.wantOpen(t, hs.RunID)
}

// TestRecoverPreWriterCompleteTearsDownAndCommitsLoss: without the
// writer-complete mark nothing is adoptable — the egress proxy died with the
// daemon — so the stranded objects, including a still-running writer, are
// reaped and the loss durably committed.
func TestRecoverPreWriterCompleteTearsDownAndCommitsLoss(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testHandoffSpec()
	fx.openRecord(t, hs)
	names := namesFor(hs.RunID)
	labels := fx.runLabels(hs.RunID)
	fx.worldVolume(t, names.Workspace, labels)
	fx.worldContainer(t, ContainerSpec{
		Name: names.Agent, Image: hs.Agent.Image, Command: hs.Agent.Command, Labels: labels,
	}, true)
	if err := fx.rt.CreateNetwork(context.Background(), names.Network, labels); err != nil {
		t.Fatal(err)
	}

	res, err := fx.recover(t, hs.RunID, hs)
	if err != nil {
		t.Fatalf("Recover = %v, want committed loss", err)
	}
	if res.Outcome != RecoveryLoss {
		t.Fatalf("Outcome = %q, want loss", res.Outcome)
	}
	fx.wantClosed(t, hs.RunID, HandoffLoss)
	fx.assertReaped(t)
}

// TestRecoverWriterCompleteAdoptsToVerifiedExport: with the writer-complete
// mark and a provably-owned workspace, recovery runs a fresh exporter and
// releases a freshly verified export; the record closes completed.
func TestRecoverWriterCompleteAdoptsToVerifiedExport(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testHandoffSpec()
	rec := fx.openRecord(t, hs)
	names := namesFor(hs.RunID)
	if err := fx.j.MarkWriterComplete(context.Background(), hs.RunID); err != nil {
		t.Fatal(err)
	}
	rec.WriterComplete = true
	fx.worldVolume(t, names.Workspace, fx.runLabels(hs.RunID))

	res, err := fx.recover(t, hs.RunID, hs)
	if err != nil {
		t.Fatalf("Recover = %v, want adoption", err)
	}
	if res.Outcome != RecoveryExported {
		t.Fatalf("Outcome = %q (loss cause %q), want exported", res.Outcome, res.LossCause)
	}
	if len(res.Manifest.Entries) != 1 {
		t.Errorf("Manifest entries = %d, want the fixture archive's 1", len(res.Manifest.Entries))
	}
	if _, err := os.Stat(res.ExportDir); err != nil {
		t.Errorf("released output dir: %v", err)
	}
	fx.wantClosed(t, hs.RunID, HandoffCompleted)
	fx.assertReaped(t)
}

// TestRecoverWriterCompleteWorkspaceGoneCommitsLoss: writer complete but the
// workspace no longer exists — nothing of this run's to export, and the loss
// says why.
func TestRecoverWriterCompleteWorkspaceGoneCommitsLoss(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testHandoffSpec()
	fx.openRecord(t, hs)
	if err := fx.j.MarkWriterComplete(context.Background(), hs.RunID); err != nil {
		t.Fatal(err)
	}

	res, err := fx.recover(t, hs.RunID, hs)
	if err != nil {
		t.Fatalf("Recover = %v, want committed loss", err)
	}
	if res.Outcome != RecoveryLoss || res.LossCause == "" {
		t.Fatalf("result = %+v, want loss with a cause", res)
	}
	fx.wantClosed(t, hs.RunID, HandoffLoss)
}

// TestRecoverForeignWorkspaceCommitsLossUntouched: a same-name workspace
// carrying someone else's token is not this run's; recovery commits loss
// without touching it.
func TestRecoverForeignWorkspaceCommitsLossUntouched(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testHandoffSpec()
	fx.openRecord(t, hs)
	if err := fx.j.MarkWriterComplete(context.Background(), hs.RunID); err != nil {
		t.Fatal(err)
	}
	names := namesFor(hs.RunID)
	foreign := []Label{
		{Key: labelKey, Value: hs.RunID},
		{Key: ownershipLabelKey, Value: "ffffffffffffffffffffffffffffffff"},
	}
	fx.worldVolume(t, names.Workspace, foreign)

	res, err := fx.recover(t, hs.RunID, hs)
	if err != nil {
		t.Fatalf("Recover = %v, want committed loss", err)
	}
	if res.Outcome != RecoveryLoss {
		t.Fatalf("Outcome = %q, want loss", res.Outcome)
	}
	fx.wantClosed(t, hs.RunID, HandoffLoss)
	fx.rt.mu.Lock()
	_, survives := fx.rt.vols[names.Workspace]
	fx.rt.mu.Unlock()
	if !survives {
		t.Fatal("foreign workspace was deleted by another run's recovery")
	}
}

// TestRecoverUnprovableWorkspaceErrsWithoutLoss: labels that cannot be
// observed prove neither ownership nor absence; recovery errors, commits
// nothing, and leaves the record open for a retry.
func TestRecoverUnprovableWorkspaceErrsWithoutLoss(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testHandoffSpec()
	fx.openRecord(t, hs)
	if err := fx.j.MarkWriterComplete(context.Background(), hs.RunID); err != nil {
		t.Fatal(err)
	}
	names := namesFor(hs.RunID)
	fx.worldVolume(t, names.Workspace, fx.runLabels(hs.RunID))
	strip := func(v VolumeSummary) VolumeSummary {
		if v.Name == names.Workspace {
			v.Labels = nil
			v.LabelsObserved = false
		}
		return v
	}
	fx.rt.onListVolumes = func(list []VolumeSummary) ([]VolumeSummary, error) {
		for i := range list {
			list[i] = strip(list[i])
		}
		return list, nil
	}
	fx.rt.onInspectVolume = func(_ string, v VolumeSummary) (VolumeSummary, error) {
		return strip(v), nil
	}

	_, err := fx.recover(t, hs.RunID, hs)
	wantCheckFailure(t, err, CheckRecovery)
	fx.wantOpen(t, hs.RunID)
}

// TestRecoverTokenOrphanFailsAudit: an object under an unexpected name still
// carrying this run's token fails the final audit — the loss cannot be
// committed while anything of the run's provably survives.
func TestRecoverTokenOrphanFailsAudit(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testHandoffSpec()
	fx.openRecord(t, hs)
	fx.worldVolume(t, "stray-detached-volume", fx.runLabels(hs.RunID))

	_, err := fx.recover(t, hs.RunID, hs)
	wantCheckFailure(t, err, CheckRecovery)
	fx.wantOpen(t, hs.RunID)
	fx.rt.mu.Lock()
	_, survives := fx.rt.vols["stray-detached-volume"]
	fx.rt.mu.Unlock()
	if !survives {
		t.Fatal("audit deleted the orphan; the audit only observes")
	}
}

// TestRecoverDoubleRecoveryRefused: the first recovery closes the record;
// replaying it against the closed record refuses without touching anything.
func TestRecoverDoubleRecoveryRefused(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testHandoffSpec()
	fx.openRecord(t, hs)

	if _, err := fx.recover(t, hs.RunID, hs); err != nil {
		t.Fatalf("first Recover = %v, want committed loss", err)
	}
	callsBefore := len(fx.rt.calls)
	_, err := fx.recover(t, hs.RunID, hs)
	if !errors.Is(err, ErrInvalidJournalRecord) {
		t.Fatalf("second Recover = %v, want closed-record refusal", err)
	}
	for _, c := range fx.rt.calls[callsBefore:] {
		if !strings.HasPrefix(c, "journal-") {
			t.Errorf("second recovery touched the runtime: %q", c)
		}
	}
}

// TestRecoverLeasedLapsedWindowTolerated: a recovery arriving after the
// window expired (the common crash case) treats the ended window as nothing
// left to release and still commits its outcome.
func TestRecoverLeasedLapsedWindowTolerated(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testLeasedHandoffSpec()
	fx.openRecord(t, hs)
	// The store still holds the crashed run's row, lapsed before the
	// fixture's Now; a zero row would be an incoherent store, which fails
	// the re-gate closed instead of modeling the common crash case.
	fx.l.lease = domain.AuthStoreMutationLease{
		AuthIdentityID: hs.AuthStoreLease.AuthIdentityID, Holder: hs.AuthStoreLease.Holder, Fence: 1,
		AcquiredAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
		ExpiresAt:  time.Date(2026, 7, 1, 11, 30, 0, 0, time.UTC),
	}
	fx.l.onRelease = func(domain.AuthIdentityID, domain.InvocationID, int64, time.Time) error {
		return fmt.Errorf("release auth store mutation lease: %w", ErrLeaseWindowEnded)
	}

	res, err := fx.recover(t, hs.RunID, hs)
	if err != nil {
		t.Fatalf("Recover = %v, want committed loss despite the lapsed window", err)
	}
	if res.Outcome != RecoveryLoss {
		t.Fatalf("Outcome = %q, want loss", res.Outcome)
	}
	fx.wantClosed(t, hs.RunID, HandoffLoss)
}

// TestRecoverLeasedReleaseFailureKeepsRecordOpen: a release refusal that is
// not an ended window is a real teardown problem; recovery errors and the
// record stays open.
func TestRecoverLeasedReleaseFailureKeepsRecordOpen(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testLeasedHandoffSpec()
	rec := fx.openRecord(t, hs)
	// The live row matches the record's window (same fence, acquired before
	// the record opened, still live), so the release is really attempted.
	fx.l.lease = domain.AuthStoreMutationLease{
		AuthIdentityID: rec.Lease.AuthIdentityID, Holder: rec.Lease.Holder, Fence: rec.Lease.Fence,
		AcquiredAt: rec.OpenedAt.Add(-time.Hour), ExpiresAt: rec.OpenedAt.Add(100 * time.Hour),
	}
	fx.l.onRelease = func(domain.AuthIdentityID, domain.InvocationID, int64, time.Time) error {
		return errors.New("fixture: store unavailable")
	}

	_, err := fx.recover(t, hs.RunID, hs)
	if err == nil {
		t.Fatal("Recover succeeded despite the failed release")
	}
	fx.wantOpen(t, hs.RunID)
}

// TestRecoverAdoptionConformanceFailureCommitsLoss: an adoption failure that
// is evidence about the run — here the scanner refusing the export — commits
// loss: a retry would refuse the same way, and rerun is the honest signal.
func TestRecoverAdoptionConformanceFailureCommitsLoss(t *testing.T) {
	fx := newRecoveryFixture(t)
	fx.cfg.Scanner = scannerFunc(func(context.Context, string) error {
		return errors.New("fixture: refuse every export")
	})
	hs := testHandoffSpec()
	fx.openRecord(t, hs)
	if err := fx.j.MarkWriterComplete(context.Background(), hs.RunID); err != nil {
		t.Fatal(err)
	}
	fx.worldVolume(t, namesFor(hs.RunID).Workspace, fx.runLabels(hs.RunID))

	res, err := fx.recover(t, hs.RunID, hs)
	if err != nil {
		t.Fatalf("Recover = %v, want committed loss", err)
	}
	if res.Outcome != RecoveryLoss || !strings.Contains(res.LossCause, "export") {
		t.Fatalf("result = %+v, want loss caused at the export", res)
	}
	if res.ExportDir != "" {
		t.Error("a refused export was still released")
	}
	fx.wantClosed(t, hs.RunID, HandoffLoss)
	fx.assertReaped(t)
}

// TestRecoverAdoptionOperationalFailureRetryable: an operational failure
// says nothing about the world; the workspace survives, the record stays
// open, and the recovery can be retried.
func TestRecoverAdoptionOperationalFailureRetryable(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testHandoffSpec()
	names := namesFor(hs.RunID)
	fx.openRecord(t, hs)
	if err := fx.j.MarkWriterComplete(context.Background(), hs.RunID); err != nil {
		t.Fatal(err)
	}
	fx.worldVolume(t, names.Workspace, fx.runLabels(hs.RunID))
	fail := true
	fx.rt.onCreateContainer = func(spec ContainerSpec) error {
		if fail && spec.Name == names.Exporter {
			return errors.New("fixture: runtime hiccup")
		}
		return nil
	}

	if _, err := fx.recover(t, hs.RunID, hs); err == nil {
		t.Fatal("Recover succeeded despite the runtime failure")
	}
	fx.wantOpen(t, hs.RunID)
	fx.rt.mu.Lock()
	_, workspaceSurvives := fx.rt.vols[names.Workspace]
	fx.rt.mu.Unlock()
	if !workspaceSurvives {
		t.Fatal("an operational failure destroyed the still-adoptable workspace")
	}
	// The retry adopts the preserved workspace to a released export.
	fail = false
	res, err := fx.recover(t, hs.RunID, hs)
	if err != nil {
		t.Fatalf("retried Recover = %v, want adoption", err)
	}
	if res.Outcome != RecoveryExported {
		t.Fatalf("retried Outcome = %q (loss cause %q), want exported", res.Outcome, res.LossCause)
	}
	fx.wantClosed(t, hs.RunID, HandoffCompleted)
	fx.assertReaped(t)
}

// TestRecoverLeasedObserverFailureRetryable: the post-write credential
// observer digests the leased volume, not the workspace, so its failure —
// even one the observer path wraps in the conformance class, like a create
// collision — is never evidence against the completed workspace; recovery
// errors retryably, the workspace survives, and the retry adopts.
func TestRecoverLeasedObserverFailureRetryable(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testLeasedHandoffSpec()
	rec := fx.openRecord(t, hs)
	names := namesFor(hs.RunID)
	if err := fx.j.MarkCredentialObserved(context.Background(), hs.RunID, strings.Repeat("ab", 32)); err != nil {
		t.Fatal(err)
	}
	if err := fx.j.MarkWriterComplete(context.Background(), hs.RunID); err != nil {
		t.Fatal(err)
	}
	fx.l.lease = domain.AuthStoreMutationLease{
		AuthIdentityID: rec.Lease.AuthIdentityID, Holder: rec.Lease.Holder, Fence: rec.Lease.Fence,
		AcquiredAt: rec.OpenedAt.Add(-time.Hour), ExpiresAt: rec.OpenedAt.Add(100 * time.Hour),
	}
	fx.worldVolume(t, names.Workspace, fx.runLabels(hs.RunID))
	fail := true
	fx.rt.onCreateContainer = func(spec ContainerSpec) error {
		if fail && spec.Name == names.CredObsPost {
			return errors.New("fixture: attach collision")
		}
		return nil
	}

	if _, err := fx.recover(t, hs.RunID, hs); err == nil {
		t.Fatal("Recover succeeded despite the observer failure")
	}
	fx.wantOpen(t, hs.RunID)
	fx.rt.mu.Lock()
	_, workspaceSurvives := fx.rt.vols[names.Workspace]
	fx.rt.mu.Unlock()
	if !workspaceSurvives {
		t.Fatal("an observer failure destroyed the completed workspace")
	}
	// The retry adopts the preserved workspace to a released export.
	fail = false
	res, err := fx.recover(t, hs.RunID, hs)
	if err != nil {
		t.Fatalf("retried Recover = %v, want adoption", err)
	}
	if res.Outcome != RecoveryExported || !res.AuthStore.Leased {
		t.Fatalf("retried result = %+v, want a leased export", res)
	}
	fx.wantClosed(t, hs.RunID, HandoffCompleted)
}

// TestRecoverLapsedWindowReportsAttestationLost: with the recorded window
// lapsed, later holders may have mutated the store since the writer, and no
// fresh serialization can recreate the state the writer left; recovery
// adopts, but reports the post-write attestation as lost (PostAttested
// false, no observer run, no window taken) instead of attributing an
// intervening holder's mutation to this run.
func TestRecoverLapsedWindowReportsAttestationLost(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testLeasedHandoffSpec()
	rec := fx.openRecord(t, hs)
	names := namesFor(hs.RunID)
	if err := fx.j.MarkCredentialObserved(context.Background(), hs.RunID, strings.Repeat("ab", 32)); err != nil {
		t.Fatal(err)
	}
	if err := fx.j.MarkWriterComplete(context.Background(), hs.RunID); err != nil {
		t.Fatal(err)
	}
	// The store holds the crashed run's row, lapsed before the fixture's Now.
	fx.l.lease = domain.AuthStoreMutationLease{
		AuthIdentityID: rec.Lease.AuthIdentityID, Holder: rec.Lease.Holder, Fence: rec.Lease.Fence,
		AcquiredAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
		ExpiresAt:  time.Date(2026, 7, 1, 11, 30, 0, 0, time.UTC),
	}
	fx.worldVolume(t, names.Workspace, fx.runLabels(hs.RunID))

	res, err := fx.recover(t, hs.RunID, hs)
	if err != nil {
		t.Fatalf("Recover = %v, want adoption", err)
	}
	if res.Outcome != RecoveryExported || !res.AuthStore.Leased {
		t.Fatalf("result = %+v, want a leased export", res)
	}
	if res.AuthStore.PostAttested || res.AuthStore.PostDigest != "" || res.AuthStore.Mutated {
		t.Fatalf("AuthStore = %+v, want the post-write attestation reported as lost", res.AuthStore)
	}
	if res.AuthStore.PreDigest != strings.Repeat("ab", 32) {
		t.Errorf("PreDigest = %q, want the persisted pre-writer digest", res.AuthStore.PreDigest)
	}
	if fx.rt.callIndex("create-container "+names.CredObsPost) != -1 {
		t.Fatal("an observer ran outside the run's window")
	}
	if fx.rt.callIndex("lease-acquire "+string(rec.Lease.AuthIdentityID)) != -1 {
		t.Fatal("recovery took a window it cannot attribute an observation to")
	}
	fx.wantClosed(t, hs.RunID, HandoffCompleted)
}

// TestRecoverShortWindowSkipsObservation: an adopted window that would
// expire before a full observation budget cannot guarantee the observer
// runs unaccompanied — a takeover mid-read would write beside it — so the
// observation is skipped and reported unattested; the still-live window is
// still released after the clean audit.
func TestRecoverShortWindowSkipsObservation(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testLeasedHandoffSpec()
	rec := fx.openRecord(t, hs)
	names := namesFor(hs.RunID)
	// Live and matching, but with only a minute left: held for release,
	// insufficient for an attributable observation.
	rec.Lease.ExpiresAt = time.Date(2026, 7, 1, 12, 1, 0, 0, time.UTC)
	fx.j.put(rec)
	if err := fx.j.MarkCredentialObserved(context.Background(), hs.RunID, strings.Repeat("ab", 32)); err != nil {
		t.Fatal(err)
	}
	if err := fx.j.MarkWriterComplete(context.Background(), hs.RunID); err != nil {
		t.Fatal(err)
	}
	fx.l.lease = domain.AuthStoreMutationLease{
		AuthIdentityID: rec.Lease.AuthIdentityID, Holder: rec.Lease.Holder, Fence: rec.Lease.Fence,
		AcquiredAt: rec.Lease.AcquiredAt, ExpiresAt: rec.Lease.ExpiresAt,
	}
	fx.worldVolume(t, names.Workspace, fx.runLabels(hs.RunID))

	res, err := fx.recover(t, hs.RunID, hs)
	if err != nil {
		t.Fatalf("Recover = %v, want adoption", err)
	}
	if res.AuthStore.PostAttested || fx.rt.callIndex("create-container "+names.CredObsPost) != -1 {
		t.Fatal("an observer ran inside a window that cannot cover it")
	}
	if !fx.l.released {
		t.Fatal("the still-live short window was not released after the audit")
	}
	fx.wantClosed(t, hs.RunID, HandoffCompleted)
}

// TestRecoverExporterLifecycleFailureRetryable: the exporter's
// container-lifecycle stage says nothing about the workspace content, so a
// failure there — even one the shared path wraps in the conformance class,
// like an inspect error — must preserve the completed workspace and leave
// the record open; only verifyExport's refusals commit loss.
func TestRecoverExporterLifecycleFailureRetryable(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testHandoffSpec()
	names := namesFor(hs.RunID)
	fx.openRecord(t, hs)
	if err := fx.j.MarkWriterComplete(context.Background(), hs.RunID); err != nil {
		t.Fatal(err)
	}
	fx.worldVolume(t, names.Workspace, fx.runLabels(hs.RunID))
	fail := true
	fx.rt.onInspect = func(id string, rep InspectReport) (InspectReport, error) {
		if fail && id == names.Exporter {
			return InspectReport{}, errors.New("fixture: transient inspect failure")
		}
		return rep, nil
	}

	if _, err := fx.recover(t, hs.RunID, hs); err == nil {
		t.Fatal("Recover succeeded despite the exporter inspect failure")
	}
	fx.wantOpen(t, hs.RunID)
	fx.rt.mu.Lock()
	_, workspaceSurvives := fx.rt.vols[names.Workspace]
	fx.rt.mu.Unlock()
	if !workspaceSurvives {
		t.Fatal("an exporter lifecycle failure destroyed the completed workspace")
	}
	fail = false
	res, err := fx.recover(t, hs.RunID, hs)
	if err != nil {
		t.Fatalf("retried Recover = %v, want adoption", err)
	}
	if res.Outcome != RecoveryExported {
		t.Fatalf("retried Outcome = %q (loss cause %q), want exported", res.Outcome, res.LossCause)
	}
	fx.wantClosed(t, hs.RunID, HandoffCompleted)
}

// TestRecoverTokenStrayContainerRefusesAdoption: a container under an
// unexpected name still carrying this run's token could be a
// credential-bearing writer mutating outside every lease window; adoption
// refuses retryably before observing, acquiring a replacement window, or
// exporting — and reaps nothing it cannot name. A foreign-token container
// under an unexpected name is none of this run's business.
func TestRecoverTokenStrayContainerRefusesAdoption(t *testing.T) {
	for _, stray := range []string{"ours", "foreign"} {
		t.Run(stray, func(t *testing.T) {
			fx := newRecoveryFixture(t)
			hs := testLeasedHandoffSpec()
			rec := fx.openRecord(t, hs)
			names := namesFor(hs.RunID)
			if err := fx.j.MarkCredentialObserved(context.Background(), hs.RunID, strings.Repeat("ab", 32)); err != nil {
				t.Fatal(err)
			}
			if err := fx.j.MarkWriterComplete(context.Background(), hs.RunID); err != nil {
				t.Fatal(err)
			}
			fx.l.lease = domain.AuthStoreMutationLease{
				AuthIdentityID: rec.Lease.AuthIdentityID, Holder: rec.Lease.Holder, Fence: rec.Lease.Fence,
				AcquiredAt: rec.Lease.AcquiredAt, ExpiresAt: rec.Lease.ExpiresAt,
			}
			fx.worldVolume(t, names.Workspace, fx.runLabels(hs.RunID))
			token := testRecoveryToken
			if stray == "foreign" {
				token = "ffffffffffffffffffffffffffffffff"
			}
			fx.worldContainer(t, ContainerSpec{
				Name: "unexpected-name", Image: "example.test/img@sha256:" + strings.Repeat("9", 64),
				Command: []string{"sh"},
				Labels:  []Label{{Key: labelKey, Value: hs.RunID}, {Key: ownershipLabelKey, Value: token}},
			}, true)

			res, err := fx.recover(t, hs.RunID, hs)
			if stray == "foreign" {
				if err != nil || res.Outcome != RecoveryExported {
					t.Fatalf("Recover beside a foreign stray = (%+v, %v), want adoption", res, err)
				}
				return
			}
			if err == nil {
				t.Fatal("Recover adopted beside a token-carrying stray")
			}
			if fx.rt.callIndex("create-container "+names.CredObsPost) != -1 {
				t.Fatal("the observer ran beside a token-carrying stray")
			}
			fx.rt.mu.Lock()
			_, straySurvives := fx.rt.ctrs["unexpected-name"]
			fx.rt.mu.Unlock()
			if !straySurvives {
				t.Fatal("recovery reaped a container it cannot name")
			}
			fx.wantOpen(t, hs.RunID)
		})
	}
}

// TestRecoverOversizeExportCommitsLoss: the export byte cap is a
// deterministic fact about the immutable completed workspace — every retry
// refuses identically — so it carries the content-evidence mark and commits
// loss, delivering the caller its rerun-safe signal instead of an open
// record that can never close.
func TestRecoverOversizeExportCommitsLoss(t *testing.T) {
	fx := newRecoveryFixture(t)
	fx.cfg.MaxExportBytes = 1
	hs := testHandoffSpec()
	fx.openRecord(t, hs)
	if err := fx.j.MarkWriterComplete(context.Background(), hs.RunID); err != nil {
		t.Fatal(err)
	}
	fx.worldVolume(t, namesFor(hs.RunID).Workspace, fx.runLabels(hs.RunID))

	res, err := fx.recover(t, hs.RunID, hs)
	if err != nil {
		t.Fatalf("Recover = %v, want committed loss", err)
	}
	if res.Outcome != RecoveryLoss || !strings.Contains(res.LossCause, "export") {
		t.Fatalf("result = %+v, want loss caused at export verification", res)
	}
	fx.wantClosed(t, hs.RunID, HandoffLoss)
}

// TestRecoverVerifyExportOperationalFailureRetryable: operational I/O
// inside the verification stage (here a corrupt archive read) is not
// content evidence — only the explicitly marked deterministic refusals
// commit loss — so the workspace survives, the record stays open, and the
// retry adopts. The regression pins the fail-safe default: an unmarked
// conformance failure never destroys the workspace.
func TestRecoverVerifyExportOperationalFailureRetryable(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testHandoffSpec()
	names := namesFor(hs.RunID)
	fx.openRecord(t, hs)
	if err := fx.j.MarkWriterComplete(context.Background(), hs.RunID); err != nil {
		t.Fatal(err)
	}
	fx.worldVolume(t, names.Workspace, fx.runLabels(hs.RunID))
	fail := true
	fx.rt.onExport = func(id string, dest io.Writer) error {
		if fail && id == names.Exporter {
			// Garbage ahead of the archive: the verification stage's tar
			// read fails as host/runtime I/O, not as content evidence.
			_, _ = dest.Write([]byte("not a tar header"))
		}
		return nil
	}

	if _, err := fx.recover(t, hs.RunID, hs); err == nil {
		t.Fatal("Recover succeeded despite the corrupt archive")
	}
	fx.wantOpen(t, hs.RunID)
	fx.rt.mu.Lock()
	_, workspaceSurvives := fx.rt.vols[names.Workspace]
	fx.rt.mu.Unlock()
	if !workspaceSurvives {
		t.Fatal("an operational verification failure destroyed the completed workspace")
	}
	fail = false
	res, err := fx.recover(t, hs.RunID, hs)
	if err != nil {
		t.Fatalf("retried Recover = %v, want adoption", err)
	}
	if res.Outcome != RecoveryExported {
		t.Fatalf("retried Outcome = %q (loss cause %q), want exported", res.Outcome, res.LossCause)
	}
	fx.wantClosed(t, hs.RunID, HandoffCompleted)
}

// TestRecoverLeasedLaterWindowNeverReleased: a damaged row carrying a later
// run's fence (sequential same-identity, same-holder acquisition is legal)
// must not let this run's recovery release that later run's live window; the
// live store row is the authority, and the binding is the recorded window's
// exact equality with it — no decoded ordering claim is load-bearing, so
// damage that also inflates OpenedAt past the later window's acquisition
// (which would have satisfied an acquisition-precedes-Begin test) changes
// nothing.
func TestRecoverLeasedLaterWindowNeverReleased(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testLeasedHandoffSpec()
	rec := fx.openRecord(t, hs)
	// The later run's live window, acquired after this record really opened.
	laterAcquired := rec.OpenedAt.Add(30 * time.Minute)
	rec.Lease.Fence = 7                                // the damaged row carries the later window's fence
	rec.OpenedAt = laterAcquired.Add(30 * time.Minute) // and an inflated OpenedAt past its acquisition
	fx.j.put(rec)
	fx.l.lease = domain.AuthStoreMutationLease{
		AuthIdentityID: rec.Lease.AuthIdentityID, Holder: rec.Lease.Holder, Fence: 7,
		AcquiredAt: laterAcquired,
		ExpiresAt:  laterAcquired.Add(100 * time.Hour),
	}

	res, err := fx.recover(t, hs.RunID, hs)
	if err != nil {
		t.Fatalf("Recover = %v, want committed loss", err)
	}
	if res.Outcome != RecoveryLoss {
		t.Fatalf("Outcome = %q, want loss", res.Outcome)
	}
	if fx.rt.callIndex("lease-release "+string(rec.Lease.AuthIdentityID)) != -1 {
		t.Fatal("recovery released a window acquired after its own record opened")
	}
	fx.wantClosed(t, hs.RunID, HandoffLoss)
}

// TestRecoverLeasedIncoherentRowFailsClosed: the re-gate's returned row
// crosses the same trust boundary as every other leaser read; a malformed
// row, or one naming another identity, is an incoherent store, so recovery
// errors retryably — destroying nothing, releasing nothing, committing
// nothing — instead of proceeding as though the window were not this run's.
func TestRecoverLeasedIncoherentRowFailsClosed(t *testing.T) {
	rows := map[string]domain.AuthStoreMutationLease{
		"malformed": {
			AuthIdentityID: "identity-fixture", Holder: "holder-fixture", Fence: 1,
			// AcquiredAt missing: a row shape Validate refuses.
			ExpiresAt: time.Date(2026, 7, 1, 13, 0, 0, 0, time.UTC),
		},
		"other-identity": {
			AuthIdentityID: "other-identity", Holder: "holder-fixture", Fence: 1,
			AcquiredAt: time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC),
			ExpiresAt:  time.Date(2026, 7, 1, 13, 0, 0, 0, time.UTC),
		},
	}
	for name, row := range rows {
		t.Run(name, func(t *testing.T) {
			fx := newRecoveryFixture(t)
			hs := testLeasedHandoffSpec()
			rec := fx.openRecord(t, hs)
			fx.l.lease = row

			if _, err := fx.recover(t, hs.RunID, hs); err == nil {
				t.Fatal("Recover accepted an incoherent lease row")
			}
			if fx.rt.callIndex("lease-release "+string(rec.Lease.AuthIdentityID)) != -1 {
				t.Fatal("recovery released over an incoherent lease row")
			}
			fx.wantOpen(t, hs.RunID)
		})
	}
}

// TestRecoverMismatchedObservedBaseRefused: a syntactically valid but wrong
// observed base — or any base on a blank-seed record — is a damaged row, not
// an attestation; both refuse before any action.
func TestRecoverMismatchedObservedBaseRefused(t *testing.T) {
	t.Run("seeded record with a different base", func(t *testing.T) {
		fx := newRecoveryFixture(t)
		hs := testHandoffSpec()
		hs.Seed = WorkspaceSeed{Mode: SeedBaseCheckout, SourceDir: "/tmp/seed-fixture", Base: testBaseRevision()}
		rec := fx.openRecord(t, hs)
		rec.WriterComplete = true
		rec.ObservedBaseSHA = strings.Repeat("77", 20)
		fx.j.put(rec)
		if _, err := fx.recover(t, hs.RunID, hs); !errors.Is(err, ErrInvalidJournalRecord) {
			t.Fatalf("Recover = %v, want invalid-record refusal", err)
		}
		fx.wantOpen(t, hs.RunID)
	})
	t.Run("blank-seed record with an injected base", func(t *testing.T) {
		fx := newRecoveryFixture(t)
		hs := testHandoffSpec()
		rec := fx.openRecord(t, hs)
		rec.ObservedBaseSHA = strings.Repeat("77", 20)
		fx.j.put(rec)
		if _, err := fx.recover(t, hs.RunID, hs); !errors.Is(err, ErrInvalidJournalRecord) {
			t.Fatalf("Recover = %v, want invalid-record refusal", err)
		}
		fx.wantOpen(t, hs.RunID)
	})
}

// TestRecoverWriterSurvivorContradictsCompletionClaim: an ours-classified
// writer container standing beside a writer-complete record contradicts the
// record; adoption refuses, commits nothing, and touches nothing.
func TestRecoverWriterSurvivorContradictsCompletionClaim(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testHandoffSpec()
	fx.openRecord(t, hs)
	if err := fx.j.MarkWriterComplete(context.Background(), hs.RunID); err != nil {
		t.Fatal(err)
	}
	names := namesFor(hs.RunID)
	labels := fx.runLabels(hs.RunID)
	fx.worldVolume(t, names.Workspace, labels)
	fx.worldContainer(t, ContainerSpec{
		Name: names.Agent, Image: hs.Agent.Image, Command: hs.Agent.Command, Labels: labels,
	}, false)

	_, err := fx.recover(t, hs.RunID, hs)
	wantCheckFailure(t, err, CheckRecovery)
	fx.wantOpen(t, hs.RunID)
	fx.rt.mu.Lock()
	_, agentSurvives := fx.rt.ctrs[names.Agent]
	_, workspaceSurvives := fx.rt.vols[names.Workspace]
	fx.rt.mu.Unlock()
	if !agentSurvives || !workspaceSurvives {
		t.Fatal("a contradictory-record refusal must not destroy anything")
	}
}

// TestRecoverTokenOrphanBlocksLeaseRelease: in recovery the lease releases
// on the audit's full-token absence bar, not teardown's name-keyed writer
// check — a token-carrying orphan under an unexpected name must keep the
// window held, or a new holder could acquire the identity beside a
// credential-bearing survivor.
func TestRecoverTokenOrphanBlocksLeaseRelease(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testLeasedHandoffSpec()
	rec := fx.openRecord(t, hs)
	// The live row matches this run's window, so only the audit stands
	// between recovery and the release.
	fx.l.lease = domain.AuthStoreMutationLease{
		AuthIdentityID: rec.Lease.AuthIdentityID, Holder: rec.Lease.Holder, Fence: rec.Lease.Fence,
		AcquiredAt: rec.OpenedAt.Add(-time.Hour), ExpiresAt: rec.OpenedAt.Add(100 * time.Hour),
	}
	fx.worldVolume(t, "stray-detached-volume", fx.runLabels(hs.RunID))

	_, err := fx.recover(t, hs.RunID, hs)
	wantCheckFailure(t, err, CheckRecovery)
	fx.wantOpen(t, hs.RunID)
	if fx.rt.callIndex("lease-release "+string(rec.Lease.AuthIdentityID)) != -1 {
		t.Fatal("lease released before the full-token audit proved absence")
	}
	if fx.l.released {
		t.Fatal("lease released despite the surviving token orphan")
	}
}

// TestRecoverLeasedReleasesAfterCleanAudit: with a clean world the recovery
// still releases the window it re-gated as this run's — the audit-ordered
// release is a stronger gate, not a dropped release.
func TestRecoverLeasedReleasesAfterCleanAudit(t *testing.T) {
	fx := newRecoveryFixture(t)
	hs := testLeasedHandoffSpec()
	rec := fx.openRecord(t, hs)
	fx.l.lease = domain.AuthStoreMutationLease{
		AuthIdentityID: rec.Lease.AuthIdentityID, Holder: rec.Lease.Holder, Fence: rec.Lease.Fence,
		AcquiredAt: rec.OpenedAt.Add(-time.Hour), ExpiresAt: rec.OpenedAt.Add(100 * time.Hour),
	}

	res, err := fx.recover(t, hs.RunID, hs)
	if err != nil {
		t.Fatalf("Recover = %v, want committed loss", err)
	}
	if res.Outcome != RecoveryLoss {
		t.Fatalf("Outcome = %q, want loss", res.Outcome)
	}
	if !fx.l.released {
		t.Fatal("lease was not released after the clean audit")
	}
	fx.wantClosed(t, hs.RunID, HandoffLoss)
}
