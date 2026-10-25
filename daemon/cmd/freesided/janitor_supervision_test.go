package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

// supervisionJanitorPorts keeps mint holds in memory: these tests exercise
// one janitor, so durability across a restart is not under test here.
type supervisionJanitorPorts struct {
	mu    sync.Mutex
	holds map[int64]publish.InstallationMintHold
}

func (*supervisionJanitorPorts) InstallationAuthority(context.Context, int64) (publish.InstallationAuthority, error) {
	return publish.InstallationAuthority{
		TrustedOwners:        []publish.TrustedOwner{{Login: "example", ID: 42}},
		TrustedInstallations: []publish.TrustedInstallation{{RegistrationID: 91, InstallationID: 777, Account: "example", AccountID: 42, RepositoryIDs: []int64{44}}},
	}, nil
}

func (*supervisionJanitorPorts) RecordInstallationRemoval(publish.InstallationRemovalRecord) error {
	return nil
}

func (*supervisionJanitorPorts) RecordInstallationQuarantine(publish.InstallationRemovalRecord) error {
	return nil
}

func (*supervisionJanitorPorts) RecordInstallationMint(publish.InstallationMintRecord) error {
	return nil
}

func (p *supervisionJanitorPorts) RecordInstallationMintHold(hold publish.InstallationMintHold) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.holds == nil {
		p.holds = map[int64]publish.InstallationMintHold{}
	}
	p.holds[hold.InstallationID] = hold
	return nil
}

func (p *supervisionJanitorPorts) ReleaseInstallationMintHold(installationID int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.holds, installationID)
	return nil
}

func (p *supervisionJanitorPorts) InstallationMintHold(installationID int64) (publish.InstallationMintHold, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	hold, found := p.holds[installationID]
	return hold, found, nil
}

func TestScheduledJanitorCredentialFailures(t *testing.T) {
	t.Parallel()
	for _, failure := range []string{"mint", "revoke", "missing-token", "rejected-expiry"} {
		t.Run(failure, func(t *testing.T) {
			now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
			root := t.TempDir()
			state := filepath.Join(root, "state")
			if err := os.MkdirAll(state, 0o700); err != nil {
				t.Fatal(err)
			}
			ks, err := publish.NewKeystore(filepath.Join(root, "credentials"), state)
			if err != nil {
				t.Fatal(err)
			}
			if err := ks.SaveApp(publish.AppCredentials{
				Owner: "example", OwnerID: 42, Visibility: publish.AppVisibilityPublic,
				AppID: 91, Name: "Freeside Example", Slug: "freeside-example", ClientID: "Iv1.example", Key: setupTestKey(t),
			}); err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/app/installations":
					_, _ = io.WriteString(w, `[{"id":777,"app_id":91,"target_id":42,"repository_selection":"selected","account":{"login":"example","id":42}}]`)
				case "/app/installations/777/access_tokens":
					if failure == "mint" {
						w.WriteHeader(500)
						return
					}
					w.WriteHeader(201)
					if failure == "missing-token" {
						_, _ = io.WriteString(w, `{}`)
						return
					}
					expiresAt := "2026-10-13T13:00:00Z"
					if failure == "rejected-expiry" {
						expiresAt = "2099-10-13T13:00:00Z"
					}
					_, _ = fmt.Fprintf(w, `{"token":"fixture-retained-token","expires_at":%q,"permissions":{"metadata":"read"},"repository_selection":"selected"}`, expiresAt)
				case "/installation/repositories":
					_, _ = io.WriteString(w, `{"total_count":1,"repositories":[{"id":44}]}`)
				case "/installation/token":
					w.WriteHeader(500)
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			ports := &supervisionJanitorPorts{}
			j, err := publish.NewInstallationJanitor(ks, srv.Client(), srv.URL, ports, ports, ports, ports, func() time.Time { return now }, 4)
			if err != nil {
				t.Fatal(err)
			}
			tracker := newScheduledFailureTracker(func() time.Time { return now }, nil)
			for attempt := 1; attempt <= scheduledFailureThreshold; attempt++ {
				passErr := j.RunScheduledPass(t.Context())
				if passErr == nil {
					passErr = janitorRegistrationFailures(j.RegistrationFaults())
				}
				if passErr == nil {
					t.Fatal("injected failure did not reach supervision")
				}
				consumption, err := tracker.consumption(domain.ScheduleJanitor, passErr)
				if failure == "missing-token" || failure == "rejected-expiry" || attempt == scheduledFailureThreshold {
					var run *scheduledFailureRunError
					if !errors.As(err, &run) || len(run.failures) != attempt {
						t.Fatalf("attempt %d: %v", attempt, err)
					}
					break
				}
				if err != nil || consumption.Outcome != domain.OutcomeObserveFailed {
					t.Fatalf("attempt %d: %v, %v", attempt, consumption, err)
				}
				if failure == "mint" {
					for holdPass := 0; holdPass < 138; holdPass++ {
						if err := j.RunScheduledPass(t.Context()); err != nil {
							t.Fatal(err)
						}
						held := janitorRegistrationFailures(j.RegistrationFaults())
						got, err := tracker.consumption(domain.ScheduleJanitor, fmt.Errorf("janitor: %w", held))
						if err != nil || got.Outcome != domain.OutcomeObserveFailed {
							t.Fatalf("hold pass: %v, %v", got, err)
						}
					}
					now = now.Add(69 * time.Minute)
				}
			}
		})
	}
}

func TestJanitorMixedHoldsDoNotHideFailures(t *testing.T) {
	for _, status := range []int{500, 401} {
		tracker := newScheduledFailureTracker(time.Now, nil)
		failure := janitorRegistrationFailures([]publish.JanitorRegistrationFault{
			{RegistrationID: 1, Err: publish.ErrJanitorMintHeld},
			{RegistrationID: 2, Err: &publish.APIError{Status: status}},
		})
		got, err := tracker.consumption(domain.ScheduleJanitor, failure)
		if status == 500 && (err != nil || got.Outcome != domain.OutcomeObserveFailed || len(tracker.failures[domain.ScheduleJanitor]) != 1) {
			t.Fatalf("mixed transient: %v, %v", got, err)
		}
		if status == 401 && err == nil {
			t.Fatal("mixed definitive failure did not stop")
		}
	}
}
