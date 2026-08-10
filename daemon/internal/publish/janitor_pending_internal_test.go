package publish

import (
	"errors"
	"testing"
	"time"
)

func TestPendingReadyNeverGrantsRuntimeAuthority(t *testing.T) {
	t.Parallel()
	janitor := &InstallationJanitor{
		covered: map[int64]registrationCoverage{
			11: {
				registrationID: 11,
				repositories:   map[int64]map[int64]struct{}{},
				pendingReady: &pendingCoverage{
					activeEpoch: 7, durableIntentRevision: 9,
					installationID: 22, repositories: map[int64]struct{}{33: {}},
				},
			},
		},
	}
	envelope := PendingInstallationEnvelope{
		ActiveEpoch: 7, DurableIntentRevision: 9,
		RegistrationID: 11, InstallationID: 22,
		ExpectedRepositoryIDs: []int64{33},
	}
	if installationID, ready := janitor.PendingReady(envelope); !ready || installationID != 22 {
		t.Fatal("PendingReady did not expose the exact transition signal")
	}
	unassigned := envelope
	unassigned.InstallationID = 0
	if installationID, ready := janitor.PendingReady(unassigned); !ready || installationID != 22 {
		t.Fatal("PendingReady did not resolve GitHub's assigned installation ID")
	}
	if janitor.AllowsRepository(11, 22, 33) {
		t.Fatal("pending transition signal leaked into runtime authority")
	}
	for _, changed := range []PendingInstallationEnvelope{
		func() PendingInstallationEnvelope { v := envelope; v.RegistrationID = 10; return v }(),
		func() PendingInstallationEnvelope { v := envelope; v.InstallationID = 21; return v }(),
		func() PendingInstallationEnvelope { v := envelope; v.ActiveEpoch = 8; return v }(),
		func() PendingInstallationEnvelope {
			v := envelope
			v.DurableIntentRevision = 10
			return v
		}(),
		func() PendingInstallationEnvelope {
			v := envelope
			v.ExpectedRepositoryIDs = []int64{32}
			return v
		}(),
	} {
		if _, ready := janitor.PendingReady(changed); ready {
			t.Fatalf("PendingReady accepted changed coordinates %+v", changed)
		}
	}
}

func TestStableCoverageExcludesReconciliationWithdrawal(t *testing.T) {
	t.Parallel()
	janitor := &InstallationJanitor{
		covered: map[int64]registrationCoverage{
			11: {
				registrationID: 11,
				repositories: map[int64]map[int64]struct{}{
					22: {33: {}},
				},
			},
		},
	}
	probeEntered := make(chan struct{})
	releaseProbe := make(chan struct{})
	probeDone := make(chan error, 1)
	go func() {
		probeDone <- janitor.WithStableCoverage(func() error {
			close(probeEntered)
			<-releaseProbe
			if !janitor.AllowsRepository(11, 22, 33) {
				return errors.New("coverage changed during coordinated probe")
			}
			return nil
		})
	}()
	<-probeEntered
	passStarted := make(chan struct{})
	passDone := make(chan struct{})
	go func() {
		close(passStarted)
		janitor.cycleMu.Lock()
		janitor.withdrawCoverage()
		janitor.cycleMu.Unlock()
		close(passDone)
	}()
	<-passStarted
	select {
	case <-passDone:
		t.Fatal("reconciliation withdrew coverage during coordinated probe")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseProbe)
	if err := <-probeDone; err != nil {
		t.Fatal(err)
	}
	<-passDone
	if janitor.AllowsRepository(11, 22, 33) {
		t.Fatal("reconciliation did not withdraw coverage after coordinated probe")
	}
}

func TestCoordinatedCoverageDoesNotRelockStableSection(t *testing.T) {
	t.Parallel()
	janitor := &InstallationJanitor{
		covered: map[int64]registrationCoverage{
			11: {
				registrationID: 11,
				repositories: map[int64]map[int64]struct{}{
					22: {33: {}},
				},
				pendingReady: &pendingCoverage{
					activeEpoch: 7, durableIntentRevision: 9,
					installationID: 22, repositories: map[int64]struct{}{33: {}},
				},
			},
		},
	}
	envelope := PendingInstallationEnvelope{
		ActiveEpoch: 7, DurableIntentRevision: 9,
		RegistrationID: 11, InstallationID: 22,
		ExpectedRepositoryIDs: []int64{33},
	}
	probeDone := make(chan error, 1)
	go func() {
		probeDone <- janitor.WithStableCoverage(func() error {
			if !janitor.AwaitActiveFor(11) {
				return errors.New("coordinated active probe denied stable coverage")
			}
			if !janitor.AwaitAllowsRepository(11, 22, 33) {
				return errors.New("coordinated repository probe denied stable coverage")
			}
			if installationID, ready := janitor.AwaitPendingReady(envelope); !ready || installationID != 22 {
				return errors.New("coordinated pending probe denied stable coverage")
			}
			return nil
		})
	}()
	select {
	case err := <-probeDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("coordinated coverage probe deadlocked inside stable section")
	}
}
