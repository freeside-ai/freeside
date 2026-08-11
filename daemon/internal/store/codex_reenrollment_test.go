package store_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func testCodexReenrollmentMarkerID(id domain.AuthIdentityID) domain.ItemID {
	itemID, err := store.CodexReenrollmentMarkerID(id, 1)
	if err != nil {
		panic(err)
	}
	return itemID
}

func seedCodexReenrollmentIdentity(
	t *testing.T, st *store.Store, at time.Time,
) domain.AuthIdentity {
	t.Helper()
	identity := domain.AuthIdentity{
		ID: "codex-primary", Provider: "codex", AuthStoreMutationLease: true,
		AuthStoreVolume: "codex-auth", MaxParallelExecutions: 1,
		RefreshStrategy: domain.RefreshOnDemand,
	}
	if err := st.WriteInternal(context.Background(), func(tx *store.InternalTx) error {
		return tx.RecordAuthIdentity(context.Background(), identity, at)
	}); err != nil {
		t.Fatalf("RecordAuthIdentity: %v", err)
	}
	marker, err := store.NewCodexReenrollmentMarker(
		identity.ID, 1,
		"project-1", 1, domain.StatusOpen, nil,
	)
	if err != nil {
		t.Fatalf("NewAttentionItem: %v", err)
	}
	if err := st.Write(context.Background(), func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(context.Background(), marker)
	}); err != nil {
		t.Fatalf("PutAttentionItem: %v", err)
	}
	return identity
}

func TestBeginCodexReenrollmentAuthenticatesCurrentMarkerBeforeLease(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	identity := seedCodexReenrollmentIdentity(t, st, at)

	posture := domain.HealthPostureAdvisory
	unrelated, err := domain.NewAttentionItem(domain.AttentionItemInput{
		ID: "system-health-unrelated", ProjectID: "project-1",
		Subject: domain.Subject{Type: domain.SubjectSystem, ID: "daemon"},
		Type:    domain.AttentionSystemHealth, Priority: domain.PriorityHigh,
		Reason:            "unrelated system health item",
		RequestedDecision: []domain.Action{domain.ActionAcknowledge},
		ItemVersion:       1, InterruptionClass: domain.InterruptionExceptional,
		Posture: &posture, Status: domain.StatusOpen,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		return tx.PutAttentionItem(ctx, unrelated)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.BeginCodexReenrollmentJournal(
			ctx, identity.ID, unrelated.ID, "enroll-unrelated", at, at.Add(time.Minute))
		return err
	}); !errors.Is(err, domain.ErrCodexReenrollmentMarkerMismatch) {
		t.Fatalf("unrelated marker begin = %v, want marker mismatch", err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		if _, err := tx.GetAuthStoreMutationLease(ctx, identity.ID); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("rejected marker lease = %v, want not found", err)
		}
		if _, found, err := tx.LatestCodexReenrollmentJournal(ctx, identity.ID); err != nil || found {
			t.Fatalf("rejected marker journal = %t, %v", found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	firstID := testCodexReenrollmentMarkerID(identity.ID)
	secondID := domain.ItemID(store.CodexReenrollmentMarkerPrefix(identity.ID) + "2")
	if err := st.Write(ctx, func(tx *store.WriteTx) error {
		first, err := tx.GetAttentionItem(ctx, firstID)
		if err != nil {
			return err
		}
		first.Status = domain.StatusSuperseded
		first.ItemVersion++
		if err := tx.PutAttentionItem(ctx, first); err != nil {
			return err
		}
		second, err := store.NewCodexReenrollmentMarker(
			identity.ID, 2, "project-1", 1, domain.StatusOpen, nil,
		)
		if err != nil {
			return err
		}
		return tx.PutAttentionItem(ctx, second)
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.BeginCodexReenrollmentJournal(
			ctx, identity.ID, firstID, "enroll-stale", at, at.Add(time.Minute))
		return err
	}); !errors.Is(err, domain.ErrCodexReenrollmentMarkerMismatch) {
		t.Fatalf("stale marker begin = %v, want marker mismatch", err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.BeginCodexReenrollmentJournal(
			ctx, identity.ID, secondID, "enroll-current", at, at.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatalf("current marker begin: %v", err)
	}
}

func TestCodexReenrollmentJournalRecoversAndLatestFenceWins(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "freeside.db")
	at := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	st, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	identity := seedCodexReenrollmentIdentity(t, st, at)
	var first store.CodexReenrollmentJournal
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		first, _, err = tx.BeginCodexReenrollmentJournal(
			ctx, identity.ID, testCodexReenrollmentMarkerID(identity.ID), "enroll-1", at, at.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatalf("begin: %v", err)
	}
	if first.Terminal != nil {
		t.Fatal("new journal is not pending")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	var recovered store.CodexReenrollmentJournal
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		var found bool
		var err error
		recovered, found, err = tx.LatestCodexReenrollmentJournal(ctx, identity.ID)
		if err == nil && !found {
			t.Fatal("pending row disappeared across reopen")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if recovered != first {
		t.Fatalf("recovered = %+v, want %+v", recovered, first)
	}

	verifiedAt := at.Add(10 * time.Second)
	expiresAt := at.Add(24 * time.Hour)
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.VerifyCodexReenrollment(
			ctx, identity.ID, first.Holder, first.LeaseFence,
			"sha256:replacement", verifiedAt, verifiedAt)
	}); err == nil {
		t.Fatal("verify accepted evidence that expired at the verification instant")
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.VerifyCodexReenrollment(
			ctx, identity.ID, "wrong-holder", first.LeaseFence,
			"sha256:replacement", expiresAt, verifiedAt)
	}); !errors.Is(err, store.ErrCodexReenrollmentLeaseMismatch) {
		t.Fatalf("wrong holder = %v, want lease mismatch", err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.VerifyCodexReenrollment(
			ctx, identity.ID, first.Holder, first.LeaseFence,
			"sha256:replacement", expiresAt, verifiedAt)
	}); err != nil {
		t.Fatalf("verify: %v", err)
	}
	// An exact terminal retry converges; a divergent terminal outcome refuses.
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.VerifyCodexReenrollment(
			ctx, identity.ID, first.Holder, first.LeaseFence,
			"sha256:replacement", expiresAt, verifiedAt)
	}); err != nil {
		t.Fatalf("verify replay: %v", err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.VerifyCodexReenrollment(
			ctx, identity.ID, "wrong-holder", first.LeaseFence,
			"sha256:replacement", expiresAt, verifiedAt)
	}); !errors.Is(err, store.ErrCodexReenrollmentLeaseMismatch) {
		t.Fatalf("wrong-holder replay = %v, want lease mismatch", err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.FailCodexReenrollment(
			ctx, identity.ID, first.Holder, first.LeaseFence,
			store.CodexReenrollmentVerificationFailed, verifiedAt)
	}); !errors.Is(err, store.ErrCodexReenrollmentOutcomeConflict) {
		t.Fatalf("divergent outcome = %v, want conflict", err)
	}

	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.ReleaseAuthStoreMutationLease(
			ctx, identity.ID, first.Holder, first.LeaseFence, at.Add(20*time.Second))
	}); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		return tx.VerifyCodexReenrollment(
			ctx, identity.ID, first.Holder, first.LeaseFence,
			"sha256:replacement", expiresAt, verifiedAt)
	}); err != nil {
		t.Fatalf("original-holder replay after release: %v", err)
	}
	var second store.CodexReenrollmentJournal
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		second, _, err = tx.BeginCodexReenrollmentJournal(
			ctx, identity.ID, testCodexReenrollmentMarkerID(identity.ID), "enroll-2", at.Add(21*time.Second), at.Add(2*time.Minute))
		return err
	}); err != nil {
		t.Fatalf("second begin: %v", err)
	}
	if second.LeaseFence <= first.LeaseFence {
		t.Fatalf("second fence = %d, first %d", second.LeaseFence, first.LeaseFence)
	}
	if _, err := second.RecoveryBinding(); !errors.Is(err, store.ErrCodexReenrollmentNotVerified) {
		t.Fatalf("new pending binding = %v", err)
	}
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		latest, found, err := tx.LatestCodexReenrollmentJournal(ctx, identity.ID)
		if err == nil && (!found || latest.LeaseFence != second.LeaseFence || latest.Terminal != nil) {
			t.Fatalf("latest = %+v found %v, want second pending", latest, found)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseLostCodexReenrollmentRequiresProvenLoss(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	start := func(
		t *testing.T, holder domain.InvocationID, expiresAt time.Time,
	) (*store.Store, domain.AuthIdentity, store.CodexReenrollmentJournal) {
		t.Helper()
		st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		identity := seedCodexReenrollmentIdentity(t, st, at)
		var rec store.CodexReenrollmentJournal
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			var err error
			rec, _, err = tx.BeginCodexReenrollmentJournal(
				ctx, identity.ID, testCodexReenrollmentMarkerID(identity.ID), holder, at, expiresAt)
			return err
		}); err != nil {
			t.Fatalf("begin: %v", err)
		}
		return st, identity, rec
	}

	t.Run("expiry", func(t *testing.T) {
		expiresAt := at.Add(time.Minute)
		st, identity, rec := start(t, "enroll-expired", expiresAt)
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.FailCodexReenrollment(
				ctx, identity.ID, rec.Holder, rec.LeaseFence,
				store.CodexReenrollmentLeaseLost, expiresAt.Add(-time.Nanosecond))
		}); !errors.Is(err, store.ErrCodexReenrollmentLeaseMismatch) {
			t.Fatalf("lease_lost while live = %v, want lease mismatch", err)
		}
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.VerifyCodexReenrollment(
				ctx, identity.ID, rec.Holder, rec.LeaseFence,
				"sha256:replacement", at.Add(24*time.Hour), expiresAt)
		}); !errors.Is(err, store.ErrCodexReenrollmentLeaseMismatch) {
			t.Fatalf("verify at expiry = %v, want lease mismatch", err)
		}
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.FailCodexReenrollment(
				ctx, identity.ID, rec.Holder, rec.LeaseFence,
				store.CodexReenrollmentVerificationFailed, expiresAt)
		}); !errors.Is(err, store.ErrCodexReenrollmentLeaseMismatch) {
			t.Fatalf("ordinary failure at expiry = %v, want lease mismatch", err)
		}
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.FailCodexReenrollment(
				ctx, identity.ID, rec.Holder, rec.LeaseFence,
				store.CodexReenrollmentLeaseLost, expiresAt)
		}); err != nil {
			t.Fatalf("lease_lost at expiry: %v", err)
		}
	})

	t.Run("release", func(t *testing.T) {
		st, identity, rec := start(t, "enroll-released", at.Add(time.Minute))
		releasedAt := at.Add(20 * time.Second)
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.ReleaseAuthStoreMutationLease(
				ctx, identity.ID, rec.Holder, rec.LeaseFence, releasedAt)
		}); err != nil {
			t.Fatalf("release: %v", err)
		}
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.FailCodexReenrollment(
				ctx, identity.ID, rec.Holder, rec.LeaseFence,
				store.CodexReenrollmentLeaseLost, releasedAt.Add(-time.Nanosecond))
		}); !errors.Is(err, store.ErrCodexReenrollmentLeaseMismatch) {
			t.Fatalf("lease_lost before release = %v, want lease mismatch", err)
		}
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.FailCodexReenrollment(
				ctx, identity.ID, rec.Holder, rec.LeaseFence,
				store.CodexReenrollmentLeaseLost, releasedAt)
		}); err != nil {
			t.Fatalf("lease_lost at release: %v", err)
		}
	})

	t.Run("takeover", func(t *testing.T) {
		st, identity, first := start(t, "enroll-stale", at.Add(time.Minute))
		takeoverAt := at.Add(2 * time.Minute)
		var second store.CodexReenrollmentJournal
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			var err error
			second, _, err = tx.BeginCodexReenrollmentJournal(
				ctx, identity.ID, testCodexReenrollmentMarkerID(identity.ID), "enroll-current",
				takeoverAt, takeoverAt.Add(time.Minute))
			return err
		}); err != nil {
			t.Fatalf("takeover begin: %v", err)
		}
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.FailCodexReenrollment(
				ctx, identity.ID, first.Holder, first.LeaseFence,
				store.CodexReenrollmentLeaseLost, takeoverAt.Add(-time.Nanosecond))
		}); !errors.Is(err, store.ErrCodexReenrollmentLeaseMismatch) {
			t.Fatalf("lease_lost before takeover = %v, want lease mismatch", err)
		}
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.FailCodexReenrollment(
				ctx, identity.ID, "wrong-holder", first.LeaseFence,
				store.CodexReenrollmentLeaseLost, takeoverAt)
		}); !errors.Is(err, store.ErrCodexReenrollmentLeaseMismatch) {
			t.Fatalf("wrong-holder lease_lost = %v, want lease mismatch", err)
		}
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.VerifyCodexReenrollment(
				ctx, identity.ID, first.Holder, first.LeaseFence,
				"sha256:replacement", at.Add(24*time.Hour), takeoverAt)
		}); !errors.Is(err, store.ErrCodexReenrollmentLeaseMismatch) {
			t.Fatalf("stale verify after takeover = %v, want lease mismatch", err)
		}
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.FailCodexReenrollment(
				ctx, identity.ID, first.Holder, first.LeaseFence,
				store.CodexReenrollmentVerificationFailed, takeoverAt)
		}); !errors.Is(err, store.ErrCodexReenrollmentLeaseMismatch) {
			t.Fatalf("ordinary failure after takeover = %v, want lease mismatch", err)
		}
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.FailCodexReenrollment(
				ctx, identity.ID, first.Holder, first.LeaseFence,
				store.CodexReenrollmentLeaseLost, takeoverAt)
		}); err != nil {
			t.Fatalf("lease_lost after takeover: %v", err)
		}
		// Exact terminal replay converges even though a newer holder owns the
		// current lease; replay grants no authority and cannot change the row.
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.FailCodexReenrollment(
				ctx, identity.ID, first.Holder, first.LeaseFence,
				store.CodexReenrollmentLeaseLost, takeoverAt)
		}); err != nil {
			t.Fatalf("lease_lost replay after takeover: %v", err)
		}
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.FailCodexReenrollment(
				ctx, identity.ID, "wrong-holder", first.LeaseFence,
				store.CodexReenrollmentLeaseLost, takeoverAt)
		}); !errors.Is(err, store.ErrCodexReenrollmentLeaseMismatch) {
			t.Fatalf("wrong-holder lease_lost replay = %v, want lease mismatch", err)
		}
		if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
			return tx.FailCodexReenrollment(
				ctx, identity.ID, first.Holder, first.LeaseFence,
				store.CodexReenrollmentVerificationFailed, takeoverAt)
		}); !errors.Is(err, store.ErrCodexReenrollmentOutcomeConflict) {
			t.Fatalf("divergent failure replay = %v, want outcome conflict", err)
		}
		if err := st.Read(ctx, func(tx *store.ReadTx) error {
			latest, found, err := tx.LatestCodexReenrollmentJournal(ctx, identity.ID)
			if err == nil && (!found || latest.LeaseFence != second.LeaseFence || latest.Terminal != nil) {
				t.Fatalf("latest = %+v found %v, want newer pending operation", latest, found)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPendingCodexReenrollmentSurvivesProcessKill(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "freeside.db")
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	cmd := exec.Command(binary, "-test.run=^TestCodexReenrollmentKillWriter$") //nolint:gosec // reexecutes this test binary with fixed arguments.
	cmd.Env = append(os.Environ(), "FREESIDE_CODEX_REENROLLMENT_KILL_DB="+path)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err == nil && line != "ready\n" {
			err = fmt.Errorf("unexpected readiness marker %q", line)
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("kill writer readiness: %v: %s", err, output.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("kill writer did not become ready: %s", output.String())
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("kill writer exited cleanly, want forced termination")
	}

	ctx := context.Background()
	reopened, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatalf("open after kill: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	if err := reopened.Read(ctx, func(tx *store.ReadTx) error {
		latest, found, err := tx.LatestCodexReenrollmentJournal(ctx, "codex-primary")
		if err != nil {
			return err
		}
		if !found || latest.Terminal != nil {
			t.Fatalf("recovered journal = %+v found %v, want pending", latest, found)
		}
		if _, err := latest.RecoveryBinding(); !errors.Is(err, store.ErrCodexReenrollmentNotVerified) {
			t.Fatalf("pending recovery binding = %v, want not verified", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCodexReenrollmentKillWriter(t *testing.T) {
	path := os.Getenv("FREESIDE_CODEX_REENROLLMENT_KILL_DB")
	if path == "" {
		t.Skip("helper process")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	identity := seedCodexReenrollmentIdentity(t, st, at)
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.BeginCodexReenrollmentJournal(
			ctx, identity.ID, testCodexReenrollmentMarkerID(identity.ID), "enroll-kill", at, at.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
		t.Fatal(err)
	}
	select {}
}

func TestBeginCodexReenrollmentRefusesSameHolderConvergence(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	at := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	identity := seedCodexReenrollmentIdentity(t, st, at)
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.BeginCodexReenrollmentJournal(
			ctx, identity.ID, testCodexReenrollmentMarkerID(identity.ID), "enroll-1", at, at.Add(time.Minute))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		_, _, err := tx.BeginCodexReenrollmentJournal(
			ctx, identity.ID, testCodexReenrollmentMarkerID(identity.ID), "enroll-1", at.Add(time.Second), at.Add(2*time.Minute))
		return err
	}); err == nil {
		t.Fatal("same-holder convergence opened a second operation")
	}
}

func TestFailedCodexReenrollmentIsCredentialFreeAndDurable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "freeside.db")
	st, err := store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	identity := seedCodexReenrollmentIdentity(t, st, at)
	var rec store.CodexReenrollmentJournal
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		var err error
		rec, _, err = tx.BeginCodexReenrollmentJournal(
			ctx, identity.ID, testCodexReenrollmentMarkerID(identity.ID), "enroll-failed", at, at.Add(time.Minute))
		if err != nil {
			return err
		}
		return tx.FailCodexReenrollment(
			ctx, identity.ID, rec.Holder, rec.LeaseFence,
			store.CodexReenrollmentVerificationFailed, at.Add(time.Second))
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Read(ctx, func(tx *store.ReadTx) error {
		latest, found, err := tx.LatestCodexReenrollmentJournal(ctx, identity.ID)
		if err != nil {
			return err
		}
		if !found || latest.Terminal == nil ||
			latest.Terminal.Outcome != store.CodexReenrollmentFailed ||
			latest.Terminal.FailureClass == nil ||
			latest.Terminal.AuthStoreDigest != nil ||
			latest.Terminal.AccessTokenExpiresAt != nil {
			t.Fatalf("failed reconstruction = %+v", latest)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
