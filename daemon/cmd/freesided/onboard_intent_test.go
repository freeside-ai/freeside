package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

// TestOnboardRefusesSilentIntentReplacement covers the gate between a
// recorded installation intent and the next onboard call: only -resume
// continues a live intent, and only -replace-intent moves its revision.
func TestOnboardRefusesSilentIntentReplacement(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// intent is the recorded intent's state: "", "live", or "expired".
		intent string
		extra  []string
		// wantErr lists what a refusal names besides the intent's revision.
		wantErr   []string
		rejectErr []string
	}{
		{
			name: "no -resume with a live intent", intent: "live",
			wantErr: []string{"repositories [55]", "expires", "-resume", "-replace-intent", "approval digest"},
		},
		{
			name: "no -resume with an expired intent", intent: "expired",
			wantErr:   []string{"repositories [55]", "expired at", "-replace-intent", "approval digest"},
			rejectErr: []string{"-resume"},
		},
		{
			name: "-resume with an expired intent", intent: "expired", extra: []string{"-resume"},
			wantErr:   []string{"expired at", "-replace-intent"},
			rejectErr: []string{"-resume"},
		},
		{
			name: "-replace-intent with no intent", extra: []string{"-replace-intent"},
			wantErr: []string{"no pending installation intent to replace"},
		},
		{name: "-replace-intent over an expired intent", intent: "expired", extra: []string{"-replace-intent"}},
		{name: "-replace-intent over a live intent", intent: "live", extra: []string{"-replace-intent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newOnboardIntentFixture(t)
			wantRevision := int64(0)
			if tc.intent != "" {
				wantRevision = fixture.recordIntent(t, tc.intent == "expired")
			}
			var output bytes.Buffer
			err := runOnboardCommand(t.Context(), append(fixture.args, tc.extra...), &output, io.Discard)
			if tc.wantErr != nil {
				if err == nil {
					t.Fatalf("onboard accepted the call; output %s", output.String())
				}
				if wantRevision > 0 {
					tc.wantErr = append(tc.wantErr, fmt.Sprintf("revision %d", wantRevision))
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not name %q", err, want)
					}
				}
				for _, reject := range tc.rejectErr {
					if strings.Contains(err.Error(), reject) {
						t.Errorf("error %q names %q", err, reject)
					}
				}
				if got := fixture.revision(t); got != wantRevision {
					t.Fatalf("refused call moved the intent revision to %d, want %d", got, wantRevision)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Status  string                        `json:"status"`
				Pending publish.PendingEnvelopeRecord `json:"pending"`
				Next    string                        `json:"next"`
			}
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.Status != "installation_pending" || result.Pending.DurableIntentRevision != wantRevision+1 {
				t.Fatalf("replacement result = %+v, want installation_pending at revision %d", result, wantRevision+1)
			}
			for _, want := range []string{
				"-resume", "-approve", "complete", "-replace-intent", "-recover-installation", "pending.current_repository_ids",
			} {
				if !strings.Contains(result.Next, want) {
					t.Errorf("next %q does not name %q", result.Next, want)
				}
			}
			if got := fixture.revision(t); got != wantRevision+1 {
				t.Fatalf("recorded revision = %d, want %d", got, wantRevision+1)
			}
		})
	}
}

// TestOnboardReportsStoredRepositoriesAfterGrant replays the exit run's order
// (#1613): the operator grants repository 55 on GitHub after the first call,
// but the intent path makes no GitHub call, so the replacement intent still
// shows the stored binding's list. next must say so instead of leaving the
// operator to read [44] as the grant not having landed.
func TestOnboardReportsStoredRepositoriesAfterGrant(t *testing.T) {
	t.Parallel()
	fixture := newOnboardIntentFixture(t)
	fixture.trustInstallation(t, 777, 44)
	run := func(extra ...string) (publish.PendingEnvelopeRecord, string) {
		t.Helper()
		var output bytes.Buffer
		if err := runOnboardCommand(t.Context(), append(fixture.args, extra...), &output, io.Discard); err != nil {
			t.Fatal(err)
		}
		var result struct {
			Status  string                        `json:"status"`
			Pending publish.PendingEnvelopeRecord `json:"pending"`
			Next    string                        `json:"next"`
		}
		if err := json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.Status != "installation_pending" {
			t.Fatalf("status = %q, want installation_pending", result.Status)
		}
		return result.Pending, result.Next
	}

	first, firstNext := run()
	if !slices.Equal(first.CurrentRepositoryIDs, []int64{44}) || !slices.Equal(first.ExpectedRepositoryIDs, []int64{44, 55}) {
		t.Fatalf("first intent = %+v, want current [44] and expected [44 55]", first)
	}
	for _, want := range []string{
		"pending.current_repository_ids", "recorded when this intent opened", "pending.expected_repository_ids", "repository 55", "-resume",
	} {
		if !strings.Contains(firstNext, want) {
			t.Errorf("next %q does not name %q", firstNext, want)
		}
	}

	// The grant lands on GitHub while the intent expires; nothing here can
	// see it.
	fixture.expireIntent(t)
	second, secondNext := run("-replace-intent")
	if second.DurableIntentRevision != first.DurableIntentRevision+1 || !slices.Equal(second.CurrentRepositoryIDs, []int64{44}) {
		t.Fatalf("replacement intent = %+v, want revision %d with the stored current [44]", second, first.DurableIntentRevision+1)
	}
	if secondNext != firstNext {
		t.Fatalf("replacement next = %q, want the first call's %q", secondNext, firstNext)
	}
}

// TestPendingIntentGateLetsResumeContinueALiveIntent pins the calls the gate
// must keep passing, which the command test can't reach offline: -resume
// against a live intent goes on to poll GitHub.
func TestPendingIntentGateLetsResumeContinueALiveIntent(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	live := &publish.PendingInstallationEnvelope{
		DurableIntentRevision: 2, ExpectedRepositoryIDs: []int64{55}, ExpiresAt: now.Add(time.Minute),
	}
	expired := &publish.PendingInstallationEnvelope{
		DurableIntentRevision: 2, ExpectedRepositoryIDs: []int64{55}, ExpiresAt: now,
	}
	for _, tc := range []struct {
		name            string
		pending         *publish.PendingInstallationEnvelope
		resume, replace bool
		wantErr         bool
	}{
		{name: "first call", pending: nil},
		{name: "-resume with no intent reaches the poll", pending: nil, resume: true},
		{name: "-resume continues a live intent", pending: live, resume: true},
		{name: "-replace-intent over a live intent", pending: live, replace: true},
		{name: "-replace-intent over an expired intent", pending: expired, replace: true},
		{name: "an intent expiring now is expired", pending: expired, resume: true, wantErr: true},
		{name: "no -resume with a live intent", pending: live, wantErr: true},
		{name: "-replace-intent with no intent", pending: nil, replace: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := pendingIntentGate(tc.pending, tc.resume, tc.replace, now)
			if (err != nil) != tc.wantErr {
				t.Fatalf("pendingIntentGate() = %v, want error %t", err, tc.wantErr)
			}
		})
	}
}

// TestRemovedInstallationErrorNamesTheRecovery pins what the operator reads
// when the janitor removed the installation an intent waits for (#3434), and
// which journal entries count as that installation.
func TestRemovedInstallationErrorNamesTheRecovery(t *testing.T) {
	t.Parallel()
	since := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	removal := func(installationID, accountID int64, at time.Time) publish.InstallationRemovalRecord {
		return publish.InstallationRemovalRecord{
			RequestedAt: at, RegistrationID: 91, InstallationID: installationID, AccountID: accountID,
			Reason: publish.InstallationRemovalUntrustedOwner,
		}
	}
	before, after := since.Add(-time.Minute), since.Add(time.Minute)
	for _, tc := range []struct {
		name           string
		removals       []publish.InstallationRemovalRecord
		installationID int64
		wantRemoved    int64
	}{
		{
			name:           "named installation removed before the wait",
			removals:       []publish.InstallationRemovalRecord{removal(777, 42, before)},
			installationID: 777,
			wantRemoved:    777,
		},
		{
			name:           "another installation removed",
			removals:       []publish.InstallationRemovalRecord{removal(778, 42, after)},
			installationID: 777,
		},
		{
			name:     "zero-ID intent ignores a removal before the wait",
			removals: []publish.InstallationRemovalRecord{removal(777, 42, before)},
		},
		{
			name:     "zero-ID intent ignores another account",
			removals: []publish.InstallationRemovalRecord{removal(777, 43, after)},
		},
		{
			name:     "zero-ID intent ignores a retried removal of an older installation",
			removals: []publish.InstallationRemovalRecord{removal(776, 42, before), removal(776, 42, after)},
		},
		{
			name:        "zero-ID intent names a removal on its account during the wait",
			removals:    []publish.InstallationRemovalRecord{removal(776, 42, before), removal(777, 42, since)},
			wantRemoved: 777,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := removedInstallationError(tc.removals, tc.installationID, 42, since, "/state/installation-janitor-journal.json")
			if tc.wantRemoved == 0 {
				if err != nil {
					t.Fatalf("removedInstallationError() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("removedInstallationError() = nil, want the removal")
			}
			for _, want := range []string{
				fmt.Sprintf("installation %d", tc.wantRemoved), "untrusted_owner", "/state/installation-janitor-journal.json",
				"-installation-id 0", "-replace-intent", "installation_url", "-resume",
			} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}
	err := removedInstallationError(
		[]publish.InstallationRemovalRecord{removal(777, 42, before)}, 777, 42, since, "journal")
	if err == nil || !strings.Contains(err.Error(), before.Format(time.RFC3339)) {
		t.Errorf("error %v does not name the removal time %s", err, before.Format(time.RFC3339))
	}
}

// TestOnboardRefusesAnIntentForARemovedInstallation covers the exit run's
// hang (#3434): an intent that names an installation the janitor removed is
// refused before it is recorded, and a -resume of one fails before it arms the
// poll or starts the janitor, instead of waiting out the intent.
func TestOnboardRefusesAnIntentForARemovedInstallation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// intent is the recorded intent's state: "", "live", or "expired".
		intent string
		extra  []string
	}{
		{name: "first call"},
		{name: "-replace-intent over an expired intent", intent: "expired", extra: []string{"-replace-intent"}},
		{name: "-resume of a live intent", intent: "live", extra: []string{"-resume"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newOnboardIntentFixture(t)
			flag := slices.Index(fixture.args, "-installation-id")
			fixture.args[flag+1] = "777"
			wantRevision := int64(0)
			if tc.intent != "" {
				pending, err := operations.BeginInstallation(t.Context(), fixture.authority, operations.InstallationIntentRequest{
					RegistrationID: 91, Account: "example", AccountID: 42, InstallationID: 777, RepositoryID: 55,
					ExpiresAt: time.Now().UTC().Add(time.Hour),
				}, time.Now)
				if err != nil {
					t.Fatal(err)
				}
				wantRevision = pending.DurableIntentRevision
				if tc.intent == "expired" {
					fixture.expireIntent(t)
				}
			}
			if err := fixture.authority.RecordInstallationRemoval(publish.InstallationRemovalRecord{
				RequestedAt: time.Now().UTC(), RegistrationID: 91, InstallationID: 777, AccountID: 42,
				Reason: publish.InstallationRemovalUntrustedOwner,
			}); err != nil {
				t.Fatal(err)
			}

			err := runOnboardCommand(t.Context(), append(fixture.args, tc.extra...), io.Discard, io.Discard)
			if err == nil {
				t.Fatal("onboard accepted an intent for a removed installation")
			}
			for _, want := range []string{"installation 777", "untrusted_owner", fixture.authority.JournalPath(), "-installation-id 0"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
			if got := fixture.revision(t); got != wantRevision {
				t.Fatalf("refused call moved the intent revision to %d, want %d", got, wantRevision)
			}
			if armed := fixture.pollArmed(t); armed {
				t.Fatal("refused call armed the installation poll")
			}
		})
	}
}

// TestInstallationPollEndsWhenTheJanitorRemovesTheInstallation covers the
// other half of the hang: a removal the resume's own janitor makes while the
// poll waits ends the wait with the removal, not the intent's expiry.
func TestInstallationPollEndsWhenTheJanitorRemovesTheInstallation(t *testing.T) {
	t.Parallel()
	fixture := newOnboardIntentFixture(t)
	fixture.recordIntent(t, false)
	snapshot, err := fixture.authority.InstallationAuthority(t.Context(), 91)
	if err != nil {
		t.Fatal(err)
	}
	pending := *snapshot.Pending
	st, _, err := openStoreWithTopicKey(t.Context(), fixture.dbPath(t), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sched, err := newInstallPollScheduler(st, fixture.authority, noPendingReady{})
	if err != nil {
		t.Fatal(err)
	}
	if err := armInstallationPoll(t.Context(), sched, 91, pending.ActiveEpoch, pending.DurableIntentRevision, pending.ExpiresAt); err != nil {
		t.Fatal(err)
	}

	started := time.Now().UTC()
	if err := fixture.authority.RecordInstallationRemoval(publish.InstallationRemovalRecord{
		RequestedAt: started, RegistrationID: 91, InstallationID: 777, AccountID: 42,
		Reason: publish.InstallationRemovalSelectionDrift,
	}); err != nil {
		t.Fatal(err)
	}
	err = awaitInstallationPoll(t.Context(), st, sched, installPollScheduleID(91), func(ctx context.Context) error {
		return refuseRemovedInstallation(ctx, fixture.authority, 91, pending.InstallationID, pending.ExpectedAccountID, started)
	})
	if err == nil || !strings.Contains(err.Error(), "installation 777") ||
		!strings.Contains(err.Error(), string(publish.InstallationRemovalSelectionDrift)) {
		t.Fatalf("awaitInstallationPoll() = %v, want the removal of installation 777", err)
	}
}

type onboardIntentFixture struct {
	authority *publish.InstallationAuthorityStore
	args      []string
}

// newOnboardIntentFixture prepares an App registration with no trusted
// installation, so every onboard call for repository 55 takes the intent path.
func newOnboardIntentFixture(t *testing.T) onboardIntentFixture {
	t.Helper()
	root := t.TempDir()
	stateDir, credentialsDir := filepath.Join(root, "state"), filepath.Join(root, "credentials")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ks, err := openTestKeystore(t, stateDir, credentialsDir)
	if err != nil {
		t.Fatal(err)
	}
	app := publish.AppCredentials{
		Owner: "example", OwnerID: 42, Visibility: publish.AppVisibilityPublic,
		AppID: 91, Name: "Freeside Example", Slug: "freeside-example", ClientID: "Iv1.example", Key: setupTestKey(t),
	}
	if err := ks.SaveApp(app); err != nil {
		t.Fatal(err)
	}
	authority, err := openTestAuthorityStore(t, stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.InitializeDocument(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := authority.InitializeRegistration(t.Context(), app.Registration()); err != nil {
		t.Fatal(err)
	}
	recipe := filepath.Join(root, "recipe.json")
	if err := os.WriteFile(recipe, []byte(`{"commands":[["go","test","./..."]],"capture":"none"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	seedTemplateStore(t, filepath.Join(root, "freeside.db"))
	return onboardIntentFixture{authority: authority, args: []string{
		"example/repo", "-db", filepath.Join(root, "freeside.db"), "-state-dir", stateDir, "-credentials-dir", credentialsDir,
		"-registration-id", "91", "-repository-id", "55", "-account", "example", "-account-id", "42",
		"-installation-id", "0",
		"-commit", "0123456789012345678901234567890123456789", "-base-ref", "main",
		"-base-image", "example.invalid/agent@sha256:test", "-base-build-ref", "local/agent:test",
		"-review-config-digest", "sha256:review", "-recipe", recipe,
	}}
}

// recordIntent records an intent the way a first onboard call does, backdates
// it when the case needs an expired intent, and returns its revision.
func (f onboardIntentFixture) recordIntent(t *testing.T, expired bool) int64 {
	t.Helper()
	pending, err := operations.BeginInstallation(t.Context(), f.authority, operations.InstallationIntentRequest{
		RegistrationID: 91, Account: "example", AccountID: 42, RepositoryID: 55,
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if expired {
		f.expireIntent(t)
	}
	return pending.DurableIntentRevision
}

// expireIntent backdates the recorded intent's expiry.
func (f onboardIntentFixture) expireIntent(t *testing.T) {
	t.Helper()
	if err := f.authority.UpdateDocument(t.Context(), func(doc *publish.InstallationAuthorityDocument) error {
		doc.Registrations[0].Pending.ExpiresAt = time.Now().UTC().Add(-time.Minute)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// trustInstallation records a trusted binding of the App owner's
// installationID holding repositoryIDs, and points the fixture's onboard call
// at that installation.
func (f *onboardIntentFixture) trustInstallation(t *testing.T, installationID int64, repositoryIDs ...int64) {
	t.Helper()
	if err := f.authority.UpdateDocument(t.Context(), func(doc *publish.InstallationAuthorityDocument) error {
		doc.Registrations[0].TrustedInstallations = []publish.TrustedInstallationRecord{{
			InstallationID: installationID, Account: "example", AccountID: 42, RepositoryIDs: repositoryIDs,
		}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	flag := slices.Index(f.args, "-installation-id")
	if flag < 0 || flag+1 >= len(f.args) {
		t.Fatal("fixture args carry no -installation-id value")
	}
	f.args[flag+1] = strconv.FormatInt(installationID, 10)
}

func (f onboardIntentFixture) dbPath(t *testing.T) string {
	t.Helper()
	flag := slices.Index(f.args, "-db")
	if flag < 0 || flag+1 >= len(f.args) {
		t.Fatal("fixture args carry no -db value")
	}
	return f.args[flag+1]
}

// pollArmed reports whether any onboard call armed the registration's
// installation poll.
func (f onboardIntentFixture) pollArmed(t *testing.T) bool {
	t.Helper()
	st, _, err := openStoreWithTopicKey(t.Context(), f.dbPath(t), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	err = st.Read(t.Context(), func(tx *store.ReadTx) error {
		_, err := tx.GetSchedule(t.Context(), installPollScheduleID(91))
		return err
	})
	if errors.Is(err, store.ErrNotFound) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

func (f onboardIntentFixture) revision(t *testing.T) int64 {
	t.Helper()
	snapshot, err := f.authority.InstallationAuthority(t.Context(), 91)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Pending == nil {
		return 0
	}
	return snapshot.Pending.DurableIntentRevision
}
