package main

import (
	"bytes"
	"encoding/json"
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
	ks, err := publish.NewKeystore(credentialsDir, stateDir)
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
	authority, err := publish.NewInstallationAuthorityStore(stateDir)
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
