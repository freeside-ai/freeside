package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// unattendedAdmissionFixture is the encrypted-backup unattended admission the
// §5.7 operating-state tests start from.
func unattendedAdmissionFixture(t *testing.T) admissionFixture {
	t.Helper()
	activeProfile := testTrustProfile(t, "owner/repo", 424242).ProfileDigest
	return newAdmissionFixture(t, func(in *domain.ExecutionAdmissionInput) {
		in.OperatingMode = domain.ModeUnattended
		in.BackendConfigurationDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
		in.Capabilities = conformantCapabilities(t)
		in.TrustProfileDigest = &activeProfile
	})
}

// unattendedOptions is the operator configuration under which the fixture
// admission is valid on its own merits, so a refusal in these tests isolates
// the operating-state gate rather than missing backup evidence.
func unattendedOptions() store.Options {
	return store.Options{
		AdmissionFloors: map[domain.OperatingMode]domain.CapabilitySnapshot{
			domain.ModeAttendedDev: domain.NewCapabilitySnapshot(domain.CapPostExitExport),
			domain.ModeUnattended:  domain.NewCapabilitySnapshot(domain.CapPostExitExport),
		},
		ApprovedCredentialModes: []domain.CredentialMode{domain.CredentialSubscriptionContained},
		BackupHealthSource:      healthyBackupHealthSource(),
	}
}

// recordTransition appends one transition through the full trust binding:
// the accepted command that authorizes it is seeded first (a transition is
// never written unbacked), against an open decisions item created on demand.
func recordTransition(t *testing.T, s *store.Store, transition domain.UnattendedOperationTransition) {
	t.Helper()
	ctx := context.Background()
	action, ok := transition.State.AuthorizingAction()
	if !ok {
		t.Fatalf("state %q has no authorizing action", transition.State)
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		itemID := domain.ItemID("decisions-" + *transition.CommandID)
		if err := tx.PutAttentionItem(ctx, decisionsItem(t, itemID)); err != nil {
			return err
		}
		command, err := domain.NewCommand(domain.CommandInput{
			CommandID: *transition.CommandID, DeviceID: "device-1",
			ItemID: itemID, ItemVersion: 1, Action: action,
		})
		if err != nil {
			return err
		}
		if err := tx.PutCommand(ctx, command); err != nil {
			return err
		}
		// Conclude the carrier as signet's accepting transaction would, so
		// it does not linger as an open unconditional blocker of the very
		// admissions these tests exercise.
		concluded := decisionsItem(t, itemID)
		concluded.ItemVersion = 2
		concluded.Status = domain.StatusResolved
		if concluded, err = concluded.WithDecidedAt(transition.OccurredAt); err != nil {
			return err
		}
		if err := tx.PutAttentionItem(ctx, concluded); err != nil {
			return err
		}
		return tx.RecordUnattendedOperationTransition(ctx, transition)
	}); err != nil {
		t.Fatalf("RecordUnattendedOperationTransition: %v", err)
	}
}

// decisionsItem is an open system_health item offering both operating-state
// actions, the carrier the test commands decide against.
func decisionsItem(t *testing.T, id domain.ItemID) domain.AttentionItem {
	t.Helper()
	posture := domain.HealthPostureBlocking
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: id, ProjectID: "proj-1",
		Subject:           domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:              domain.AttentionSystemHealth,
		Priority:          domain.PriorityNormal,
		Reason:            "operating-state decision carrier",
		RequestedDecision: []domain.Action{domain.ActionStopUnattended, domain.ActionResumeUnattended},
		ItemVersion:       1,
		InterruptionClass: domain.InterruptionExceptional,
		Posture:           &posture,
		Status:            domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	return item
}

func stoppedAt(at time.Time, commandID string) domain.UnattendedOperationTransition {
	return domain.UnattendedOperationTransition{
		State: domain.UnattendedStopped, CommandID: &commandID,
		Reason:     "operator stopped unattended operation",
		OccurredAt: at,
	}
}

func resumedAt(at time.Time, commandID string) domain.UnattendedOperationTransition {
	return domain.UnattendedOperationTransition{
		State: domain.UnattendedResumed, CommandID: &commandID,
		Reason:     "operator resumed unattended operation",
		OccurredAt: at,
	}
}

// healthItem builds an open system_health item, optionally carrying the typed
// supersession condition (issue #321).
func healthItem(
	t *testing.T,
	id domain.ItemID,
	posture domain.HealthPosture,
	cond *domain.BlockingSupersession,
) domain.AttentionItem {
	t.Helper()
	item, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: id, ProjectID: "proj-1",
		Subject:              domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:                 domain.AttentionSystemHealth,
		Priority:             domain.PriorityNormal,
		Reason:               "diagnostic finding",
		RequestedDecision:    []domain.Action{domain.ActionAcknowledge},
		ItemVersion:          1,
		InterruptionClass:    domain.InterruptionExceptional,
		Posture:              &posture,
		BlockingSupersession: cond,
		Status:               domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	return item
}

func putItem(t *testing.T, s *store.Store, item domain.AttentionItem) {
	t.Helper()
	ctx := context.Background()
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, item)
	}); err != nil {
		t.Fatalf("PutAttentionItem: %v", err)
	}
}

// TestUnattendedOperationTransitionLog is the append-only latest-wins
// contract: an empty log reports absence, each append becomes the current
// state, and a repeated state is a recorded decision, not a conflict.
func TestUnattendedOperationTransitionLog(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newAdmissionFixture(t, nil)
	s := openWithFixture(t, f, store.Options{AdmissionFloors: attendedFloors()})

	latest := func() (domain.UnattendedOperationTransition, bool) {
		t.Helper()
		var (
			tr    domain.UnattendedOperationTransition
			found bool
		)
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			tr, found, err = tx.LatestUnattendedOperationTransition(ctx)
			return err
		}); err != nil {
			t.Fatalf("LatestUnattendedOperationTransition: %v", err)
		}
		return tr, found
	}

	if _, found := latest(); found {
		t.Fatal("empty log reported a transition")
	}
	recordTransition(t, s, stoppedAt(admissionEpoch, "cmd-stop-1"))
	// A second stop is a real operator decision on another item; the log
	// records it and the state is unchanged.
	recordTransition(t, s, stoppedAt(admissionEpoch.Add(time.Minute), "cmd-stop-2"))
	if tr, found := latest(); !found || tr.State != domain.UnattendedStopped {
		t.Fatalf("latest after two stops = %+v (found %v), want stopped", tr, found)
	}
	recordTransition(t, s, resumedAt(admissionEpoch.Add(2*time.Minute), "cmd-resume-1"))
	tr, found := latest()
	if !found || tr.State != domain.UnattendedResumed {
		t.Fatalf("latest after resume = %+v (found %v), want resumed", tr, found)
	}
	if !tr.OccurredAt.Equal(admissionEpoch.Add(2 * time.Minute)) {
		t.Fatalf("resumed occurred_at = %v, want %v", tr.OccurredAt, admissionEpoch.Add(2*time.Minute))
	}
}

// TestStopClosesUnattendedAdmission is #319's admission half: a recorded stop
// refuses a new unattended admission in the admitting transaction, survives a
// daemon restart structurally (nothing writes "resumed" at open), and only an
// explicit resume reopens admission. attended_dev is untouched throughout.
func TestStopClosesUnattendedAdmission(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := unattendedAdmissionFixture(t)
	path := tempDBPath(t)

	s, err := store.Open(ctx, path, unattendedOptions())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := s.Write(ctx, func(tx *store.WriteTx) error {
		if err := tx.PutRun(ctx, f.run); err != nil {
			return err
		}
		return tx.RecordAuthIdentity(ctx, f.identity, admissionEpoch)
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	seedTrustProfile(t, s, f.admission.Base.Repo, f.admission.Base.RepositoryID)
	seedBackendConformance(t, s)

	recordTransition(t, s, stoppedAt(admissionEpoch, "cmd-stop-a"))
	if err := recordAdmission(t, s, f.admission); !errors.Is(err, domain.ErrUnattendedOperationStopped) {
		t.Fatalf("unattended admission while stopped = %v, want %v", err, domain.ErrUnattendedOperationStopped)
	}

	// Restart: reopen the same database. The stop is durable state, not
	// configuration, so admission stays closed.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened, err := store.Open(ctx, path, unattendedOptions())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := recordAdmission(t, reopened, f.admission); !errors.Is(err, domain.ErrUnattendedOperationStopped) {
		t.Fatalf("unattended admission after restart = %v, want %v", err, domain.ErrUnattendedOperationStopped)
	}

	recordTransition(t, reopened, resumedAt(admissionEpoch.Add(time.Hour), "cmd-resume-a"))
	if err := recordAdmission(t, reopened, f.admission); err != nil {
		t.Fatalf("unattended admission after resume: %v", err)
	}

	// attended_dev admission is unaffected by a stop. A separate store: the
	// attended fixture reuses the same run and invocation, so it needs its
	// own database to occupy them.
	attended := newAdmissionFixture(t, nil)
	stoppedStore := openWithFixture(t, attended, unattendedOptions())
	recordTransition(t, stoppedStore, stoppedAt(admissionEpoch, "cmd-stop-b"))
	if err := recordAdmission(t, stoppedStore, attended.admission); err != nil {
		t.Fatalf("attended admission while stopped: %v", err)
	}
}

// TestStopDoesNotPoisonRecordedHistory pins the gate-placement decision: the
// operating-state checks run when an admission is recorded, never when one is
// reconstructed, so an operator stop leaves recorded history readable.
func TestStopDoesNotPoisonRecordedHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := unattendedAdmissionFixture(t)
	s := openWithFixture(t, f, unattendedOptions())
	seedTrustProfile(t, s, f.admission.Base.Repo, f.admission.Base.RepositoryID)

	if err := recordAdmission(t, s, f.admission); err != nil {
		t.Fatalf("record: %v", err)
	}
	recordTransition(t, s, stoppedAt(admissionEpoch.Add(time.Minute), "cmd-stop-c"))

	if err := s.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetExecutionAdmission(ctx, f.admission.InvocationID); err != nil {
			return err
		}
		listed, err := tx.ListRunExecutionAdmissions(ctx, f.run.ID)
		if err != nil {
			return err
		}
		if len(listed) != 1 {
			t.Fatalf("listed %d admissions, want 1", len(listed))
		}
		return nil
	}); err != nil {
		t.Fatalf("reading recorded history while stopped: %v", err)
	}
}

// TestBlockingSystemHealthRefusesUnattendedAdmission is #321's core rule in
// the admitting transaction: an open system_health item with no supersession
// condition blocks unattended admission; an old waiver-posture notice is
// superseded by healthy encrypted backup evidence. attended_dev never consults
// the rule.
func TestBlockingSystemHealthRefusesUnattendedAdmission(t *testing.T) {
	t.Parallel()
	f := unattendedAdmissionFixture(t)

	t.Run("unconditional open item blocks", func(t *testing.T) {
		s := openWithFixture(t, f, unattendedOptions())
		seedTrustProfile(t, s, f.admission.Base.Repo, f.admission.Base.RepositoryID)
		putItem(t, s, healthItem(t, "health-1", domain.HealthPostureBlocking, nil))
		if err := recordAdmission(t, s, f.admission); !errors.Is(err, domain.ErrBlockingSystemHealth) {
			t.Fatalf("admission under open health item = %v, want %v", err, domain.ErrBlockingSystemHealth)
		}

		// The same open item never gates attended_dev.
		attended := newAdmissionFixture(t, nil)
		if err := recordAdmission(t, s, attended.admission); err != nil {
			t.Fatalf("attended admission under open health item: %v", err)
		}
	})

	t.Run("encrypted backup supersedes a legacy waiver notice", func(t *testing.T) {
		s := openWithFixture(t, f, unattendedOptions())
		seedTrustProfile(t, s, f.admission.Base.Repo, f.admission.Base.RepositoryID)
		putItem(t, s, healthItem(t, "waiver-notice", domain.HealthPostureBlocking, &domain.BlockingSupersession{
			Kind: domain.SupersessionBackupEncryptionWaiver, RepositoryID: 424242,
		}))
		if err := recordAdmission(t, s, f.admission); err != nil {
			t.Fatalf("admission under superseded notice: %v", err)
		}

		// The notice is still open and visible; supersession changed only its
		// blocking effect, not its lifecycle.
		ctx := context.Background()
		var notice domain.AttentionItem
		if err := s.Read(ctx, func(tx *store.ReadTx) error {
			var err error
			notice, err = tx.GetAttentionItem(ctx, "waiver-notice")
			return err
		}); err != nil {
			t.Fatalf("GetAttentionItem: %v", err)
		}
		if notice.Status != domain.StatusOpen {
			t.Fatalf("notice status = %q, want open", notice.Status)
		}
	})

	t.Run("advisory observation never blocks unrelated admission", func(t *testing.T) {
		s := openWithFixture(t, f, unattendedOptions())
		seedTrustProfile(t, s, f.admission.Base.Repo, f.admission.Base.RepositoryID)
		item := healthItem(t, "advisory-observation", domain.HealthPostureAdvisory, nil)
		putItem(t, s, item)

		requireAdmission := func() error {
			return s.Read(context.Background(), func(tx *store.ReadTx) error {
				return tx.RequireUnattendedAdmissible(context.Background(), f.admission)
			})
		}
		if err := requireAdmission(); err != nil {
			t.Fatalf("admission with open advisory observation: %v", err)
		}

		resolved := item
		resolved.ItemVersion = 2
		resolved.Status = domain.StatusResolved
		putItem(t, s, resolved)
		if err := requireAdmission(); err != nil {
			t.Fatalf("admission after advisory observation resolved: %v", err)
		}
	})

	t.Run("resolved item stops blocking", func(t *testing.T) {
		s := openWithFixture(t, f, unattendedOptions())
		seedTrustProfile(t, s, f.admission.Base.Repo, f.admission.Base.RepositoryID)
		item := healthItem(t, "health-2", domain.HealthPostureBlocking, nil)
		putItem(t, s, item)

		resolved := item
		resolved.ItemVersion = 2
		resolved.Status = domain.StatusResolved
		var err error
		if resolved, err = resolved.WithDecidedAt(admissionEpoch); err != nil {
			t.Fatalf("WithDecidedAt: %v", err)
		}
		putItem(t, s, resolved)
		if err := recordAdmission(t, s, f.admission); err != nil {
			t.Fatalf("admission after the diagnostic cleared: %v", err)
		}
	})
}

func TestRequireUnattendedAdmissibleLegacyNoticeNeedsHealthyEncryption(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := unattendedAdmissionFixture(t)
	cleared := unattendedOptions()
	unhealthy := healthyBackupHealth()
	unhealthy.Encryption = domain.BackupHealthUnhealthy
	cleared.BackupHealthSource = store.BackupHealthSourceFunc(func(
		context.Context, store.BackupHealthContext,
	) (domain.BackupHealth, error) {
		return unhealthy, nil
	})
	s := openWithFixture(t, f, cleared)
	putItem(t, s, healthItem(t, "waiver-notice", domain.HealthPostureBlocking, &domain.BlockingSupersession{
		Kind: domain.SupersessionBackupEncryptionWaiver, RepositoryID: 424242,
	}))

	err := s.Read(ctx, func(tx *store.ReadTx) error {
		return tx.RequireUnattendedAdmissible(ctx, f.admission)
	})
	if !errors.Is(err, domain.ErrBlockingSystemHealth) || !errors.Is(err, domain.ErrCheckpointNotEncrypted) {
		t.Fatalf("unhealthy encryption = %v, want %v wrapping %v",
			err, domain.ErrBlockingSystemHealth, domain.ErrCheckpointNotEncrypted)
	}
}
