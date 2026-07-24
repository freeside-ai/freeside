package publish_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func trustedPublicBinding(repositoryIDs ...int64) installationAuthoritySource {
	return publicAuthority(publish.TrustedInstallation{
		RegistrationID: 501,
		InstallationID: 701,
		Account:        "operator",
		AccountID:      101,
		RepositoryIDs:  repositoryIDs,
	})
}

// TestInstallationJanitorQuarantinesGrantDrift pins the credential and
// destructive-effect sequence: the janitor mints a metadata-only unfiltered
// token, completes enumeration, revokes it, commits local quarantine, suspends
// the installation, and deletes it without ever calling an unsuspend endpoint.
func TestInstallationJanitorQuarantinesGrantDrift(t *testing.T) {
	ks := publicJanitorKeystore(t)
	recorder := &removalRecorder{}
	var events []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_, _ = io.WriteString(w,
				`[{"id":701,"app_id":501,"target_id":101,"repository_selection":"selected","account":{"login":"operator","id":101}}]`)
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/701/access_tokens":
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"permissions":{"metadata":"read"}}` {
				t.Errorf("grant-read mint body = %s", body)
			}
			events = append(events, "mint")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w,
				`{"token":"`+fixtureTokenValue+`","permissions":{"metadata":"read"},"repository_selection":"selected"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
			events = append(events, "list")
			_, _ = io.WriteString(w,
				`{"total_count":2,"repositories":[{"id":990012},{"id":990011}]}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
			events = append(events, "revoke")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPut && r.URL.Path == "/app/installations/701/suspended":
			if len(recorder.quarantineSnapshot()) != 1 {
				t.Error("suspension reached GitHub before durable local quarantine")
			}
			events = append(events, "suspend")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/app/installations/701":
			events = append(events, "delete")
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	janitor := newJanitor(
		t,
		ks,
		srv,
		trustedPublicBinding(fixtureRepositoryID),
		recorder,
		1,
	)
	cycle, err := janitor.RunCycle(context.Background())
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if cycle.Examined != 1 || cycle.Removed != 1 || cycle.RemovalLimitReached {
		t.Errorf("cycle = %+v", cycle)
	}
	wantEvents := []string{"mint", "list", "revoke", "suspend", "delete"}
	if !reflect.DeepEqual(events, wantEvents) {
		t.Errorf("events = %v, want %v", events, wantEvents)
	}
	records := recorder.quarantineSnapshot()
	if len(records) != 1 {
		t.Fatalf("quarantine records = %d, want 1", len(records))
	}
	if records[0].Reason != publish.InstallationRemovalGrantDrift ||
		!reflect.DeepEqual(records[0].ObservedRepositoryIDs, []int64{990011, 990012}) {
		t.Errorf("quarantine record = %+v", records[0])
	}
}

func TestInstallationJanitorQuarantinesRepositorySelectionDrift(t *testing.T) {
	for _, mode := range []string{"", "all", "future_mode"} {
		t.Run(fmt.Sprintf("mode_%q", mode), func(t *testing.T) {
			ks := publicJanitorKeystore(t)
			recorder := &removalRecorder{}
			tokenMints := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
					_, _ = fmt.Fprintf(w,
						`[{"id":701,"app_id":501,"target_id":101,"repository_selection":%q,"account":{"login":"operator","id":101}}]`,
						mode,
					)
				case r.Method == http.MethodPost:
					tokenMints++
					t.Error("selection drift reached the token endpoint")
				case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/suspended"):
					w.WriteHeader(http.StatusNoContent)
				case r.Method == http.MethodDelete && r.URL.Path == "/app/installations/701":
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			}))
			defer srv.Close()

			janitor := newJanitor(
				t,
				ks,
				srv,
				trustedPublicBinding(fixtureRepositoryID),
				recorder,
				1,
			)
			cycle, err := janitor.RunCycle(context.Background())
			if err != nil {
				t.Fatalf("RunCycle: %v", err)
			}
			if tokenMints != 0 || cycle.Removed != 1 {
				t.Errorf("token mints = %d, cycle = %+v", tokenMints, cycle)
			}
			records := recorder.quarantineSnapshot()
			if len(records) != 1 ||
				records[0].Reason != publish.InstallationRemovalSelectionDrift ||
				records[0].ObservedRepositoryIDs != nil {
				t.Errorf("quarantine records = %+v", records)
			}
		})
	}
}

func TestInstallationJanitorRejectsUntrustedRepositoryPages(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "incomplete pagination",
			body: `{"total_count":2,"repositories":[{"id":990011}]}`,
		},
		{
			name: "duplicate ID",
			body: `{"total_count":2,"repositories":[{"id":990011},{"id":990011}]}`,
		},
		{
			name: "malformed ID",
			body: `{"total_count":1,"repositories":[{"id":0}]}`,
		},
		{
			name: "null page",
			body: `{"total_count":0,"repositories":null}`,
		},
		{
			name: "over safety bound",
			body: `{"total_count":10001,"repositories":[]}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ks := publicJanitorKeystore(t)
			recorder := &removalRecorder{}
			revokes := 0
			destructive := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
					_, _ = io.WriteString(w,
						`[{"id":701,"app_id":501,"target_id":101,"repository_selection":"selected","account":{"login":"operator","id":101}}]`)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
					w.WriteHeader(http.StatusCreated)
					_, _ = io.WriteString(w,
						`{"token":"`+fixtureTokenValue+`","permissions":{"metadata":"read"},"repository_selection":"selected"}`)
				case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
					_, _ = io.WriteString(w, tc.body)
				case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
					revokes++
					w.WriteHeader(http.StatusNoContent)
				case r.Method == http.MethodPut || r.Method == http.MethodDelete:
					destructive++
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			}))
			defer srv.Close()

			janitor := newJanitor(
				t,
				ks,
				srv,
				trustedPublicBinding(fixtureRepositoryID),
				recorder,
				1,
			)
			if _, err := janitor.RunCycle(context.Background()); err == nil {
				t.Fatal("RunCycle accepted an untrusted repository page")
			}
			if revokes != 1 || destructive != 0 || len(recorder.snapshot()) != 0 {
				t.Errorf(
					"revokes = %d, destructive = %d, records = %d",
					revokes,
					destructive,
					len(recorder.snapshot()),
				)
			}
		})
	}
}

func TestInstallationJanitorTokenFailuresNeverPublishCoverage(t *testing.T) {
	tests := []struct {
		name         string
		mintStatus   int
		mintBody     string
		revokeStatus int
		wantRevoke   int
	}{
		{
			name:         "mint failure",
			mintStatus:   http.StatusInternalServerError,
			mintBody:     `{"message":"` + fixtureTokenValue + `"}`,
			revokeStatus: http.StatusNoContent,
		},
		{
			name:       "broader returned permissions",
			mintStatus: http.StatusCreated,
			mintBody: `{"token":"` + fixtureTokenValue +
				`","permissions":{"metadata":"read","contents":"read"},"repository_selection":"selected"}`,
			revokeStatus: http.StatusNoContent,
			wantRevoke:   1,
		},
		{
			name:         "revoke failure",
			mintStatus:   http.StatusCreated,
			mintBody:     `{"token":"` + fixtureTokenValue + `","permissions":{"metadata":"read"},"repository_selection":"selected"}`,
			revokeStatus: http.StatusInternalServerError,
			wantRevoke:   1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ks := publicJanitorKeystore(t)
			recorder := &removalRecorder{}
			revokes := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
					_, _ = io.WriteString(w,
						`[{"id":701,"app_id":501,"target_id":101,"repository_selection":"selected","account":{"login":"operator","id":101}}]`)
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
					w.WriteHeader(tc.mintStatus)
					_, _ = io.WriteString(w, tc.mintBody)
				case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
					_, _ = io.WriteString(w,
						`{"total_count":1,"repositories":[{"id":990011}]}`)
				case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
					revokes++
					w.WriteHeader(tc.revokeStatus)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			}))
			defer srv.Close()

			janitor := newJanitor(
				t,
				ks,
				srv,
				trustedPublicBinding(fixtureRepositoryID),
				recorder,
				1,
			)
			err := janitor.Run(context.Background(), time.Hour)
			if err == nil {
				t.Fatal("Run accepted a token or revoke failure")
			}
			if revokes != tc.wantRevoke || janitor.ActiveFor(501) ||
				len(recorder.snapshot()) != 0 {
				t.Errorf(
					"revokes = %d, active = %t, records = %d",
					revokes,
					janitor.ActiveFor(501),
					len(recorder.snapshot()),
				)
			}
			if strings.Contains(err.Error(), fixtureTokenValue) {
				t.Errorf("error leaked the enumeration token: %v", err)
			}
		})
	}
}

func TestInstallationJanitorRemovesTrustedOwnerWithoutBindingBeforeMint(t *testing.T) {
	ks := publicJanitorKeystore(t)
	recorder := &removalRecorder{}
	tokenMints := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			_, _ = io.WriteString(w,
				`[{"id":701,"app_id":501,"target_id":101,"repository_selection":"selected","account":{"login":"operator","id":101}}]`)
		case http.MethodPost:
			tokenMints++
			t.Error("unbound installation reached the token endpoint")
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	janitor := newJanitor(t, ks, srv, publicAuthority(), recorder, 1)
	if _, err := janitor.RunCycle(context.Background()); err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	records := recorder.snapshot()
	if tokenMints != 0 || len(records) != 1 ||
		records[0].Reason != publish.InstallationRemovalUnbound {
		t.Errorf("token mints = %d, records = %+v", tokenMints, records)
	}
}

func TestStalePendingEnvelopeResumesOrdinaryCleanup(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*publish.InstallationAuthority)
	}{
		{
			name: "stale epoch",
			mutate: func(authority *publish.InstallationAuthority) {
				authority.Pending.ActiveEpoch--
			},
		},
		{
			name: "superseded revision",
			mutate: func(authority *publish.InstallationAuthority) {
				authority.Pending.DurableIntentRevision--
			},
		},
		{
			name: "expired",
			mutate: func(authority *publish.InstallationAuthority) {
				authority.Pending.ExpiresAt = fixtureTime
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := publish.InstallationAuthority{
				ActiveEpoch:           7,
				DurableIntentRevision: 11,
				TrustedOwners: []publish.TrustedOwner{
					{Login: "operator", ID: 101},
				},
				Pending: &publish.PendingInstallationEnvelope{
					ActiveEpoch:            7,
					DurableIntentRevision:  11,
					RegistrationID:         501,
					ExpectedAccount:        "operator",
					ExpectedAccountID:      101,
					InstallationID:         701,
					ExpectedRepositoryIDs:  []int64{fixtureRepositoryID},
					RequiredRepositoryMode: "selected",
					ExpiresAt:              fixtureTime.Add(time.Hour),
				},
			}
			tc.mutate(&snapshot)
			tokenMints := 0
			recorder := &removalRecorder{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.Method {
				case http.MethodGet:
					_, _ = io.WriteString(w,
						`[{"id":701,"app_id":501,"target_id":101,"repository_selection":"selected","account":{"login":"operator","id":101}}]`)
				case http.MethodPost:
					tokenMints++
					t.Error("stale pending envelope reached the token endpoint")
				case http.MethodDelete:
					w.WriteHeader(http.StatusNoContent)
				}
			}))
			defer srv.Close()

			janitor := newJanitor(
				t,
				publicJanitorKeystore(t),
				srv,
				installationAuthoritySource{501: snapshot},
				recorder,
				1,
			)
			if _, err := janitor.RunCycle(context.Background()); err != nil {
				t.Fatalf("RunCycle: %v", err)
			}
			records := recorder.snapshot()
			if tokenMints != 0 || len(records) != 1 ||
				records[0].Reason != publish.InstallationRemovalUnbound {
				t.Errorf("token mints = %d, records = %+v", tokenMints, records)
			}
		})
	}
}

func TestPrivateRegistrationRequiresJanitorCoverage(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: cleanupTransportFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("request should not be sent")
	})}
	resolver := publish.NewInstallationResolver(
		newRegisteredKeystore(t),
		client,
		"https://api.github.test",
		fixedNow,
	)
	if _, err := resolver.Resolve(context.Background(), "freeside-ai"); !errors.Is(err, publish.ErrJanitorInactive) {
		t.Fatalf("err = %v, want ErrJanitorInactive", err)
	}
	if requests != 0 {
		t.Errorf("private registration sent %d requests before the janitor gate", requests)
	}
}

func TestPendingExpansionPreservesOnlyCurrentMintAuthority(t *testing.T) {
	const addedRepositoryID = int64(990012)
	authority := installationAuthoritySource{
		fixtureAppID: {
			ActiveEpoch:           7,
			DurableIntentRevision: 11,
			TrustedInstallations: []publish.TrustedInstallation{
				{
					RegistrationID: fixtureAppID,
					InstallationID: 777,
					Account:        "freeside-ai",
					AccountID:      testOwnerID,
					RepositoryIDs:  []int64{fixtureRepositoryID},
				},
			},
			Pending: &publish.PendingInstallationEnvelope{
				ActiveEpoch:            7,
				DurableIntentRevision:  11,
				RegistrationID:         fixtureAppID,
				ExpectedAccount:        "freeside-ai",
				ExpectedAccountID:      testOwnerID,
				InstallationID:         777,
				CurrentRepositoryIDs:   []int64{fixtureRepositoryID},
				ExpectedRepositoryIDs:  []int64{fixtureRepositoryID, addedRepositoryID},
				RequiredRepositoryMode: "selected",
				ExpiresAt:              fixtureTime.Add(time.Hour),
			},
		},
	}
	for _, tc := range []struct {
		name                string
		remoteRepositoryIDs []int64
	}{
		{
			name:                "before GitHub applies expansion",
			remoteRepositoryIDs: []int64{fixtureRepositoryID},
		},
		{
			name:                "after GitHub applies expansion",
			remoteRepositoryIDs: []int64{fixtureRepositoryID, addedRepositoryID},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ks := newRegisteredKeystore(t)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
					_, _ = fmt.Fprintf(w,
						`[{"id":777,"app_id":%d,"target_id":%d,"repository_selection":"selected","account":{"login":"freeside-ai","id":%d}}]`,
						fixtureAppID,
						testOwnerID,
						testOwnerID,
					)
				case handleExactGrant(w, r, tc.remoteRepositoryIDs...):
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				}
			}))
			defer srv.Close()

			janitor, err := publish.NewInstallationJanitor(
				ks,
				srv.Client(),
				srv.URL,
				authority,
				&removalRecorder{},
				fixedNow,
				1,
			)
			if err != nil {
				t.Fatalf("NewInstallationJanitor: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- janitor.Run(ctx, time.Hour) }()
			deadline := time.Now().Add(time.Second)
			for !janitor.ActiveFor(fixtureAppID) && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			if !janitor.ActiveFor(fixtureAppID) {
				cancel()
				t.Fatal("pending expansion did not publish registration coverage")
			}
			if !janitor.AllowsRepository(fixtureAppID, 777, fixtureRepositoryID) {
				t.Fatal("pending expansion revoked the current trusted repository")
			}
			if janitor.AllowsRepository(fixtureAppID, 777, addedRepositoryID) {
				t.Fatal("pending expansion authorized the not-yet-promoted repository")
			}
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("Run shutdown: %v", err)
			}
		})
	}
}

func TestPublicPendingEnvelopeTemporarilyExemptsExpectedOwner(t *testing.T) {
	authority := publicAuthority()
	snapshot := authority[501]
	snapshot.ActiveEpoch = 7
	snapshot.DurableIntentRevision = 11
	snapshot.Pending = &publish.PendingInstallationEnvelope{
		ActiveEpoch:            7,
		DurableIntentRevision:  11,
		RegistrationID:         501,
		ExpectedAccount:        "future-org",
		ExpectedAccountID:      202,
		InstallationID:         701,
		ExpectedRepositoryIDs:  []int64{fixtureRepositoryID},
		RequiredRepositoryMode: "selected",
		ExpiresAt:              fixtureTime.Add(time.Hour),
	}
	authority[501] = snapshot
	recorder := &removalRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_, _ = io.WriteString(w,
				`[{"id":701,"app_id":501,"target_id":202,"repository_selection":"selected","account":{"login":"future-org","id":202}}]`)
		case handleExactGrant(w, r, fixtureRepositoryID):
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	janitor := newJanitor(
		t,
		publicJanitorKeystore(t),
		srv,
		authority,
		recorder,
		1,
	)
	cycle, err := janitor.RunCycle(context.Background())
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if cycle.Examined != 1 || cycle.Removed != 0 ||
		len(recorder.snapshot()) != 0 {
		t.Errorf("cycle = %+v, records = %+v", cycle, recorder.snapshot())
	}
}

func TestPendingEnvelopeNeverAuthorizesMint(t *testing.T) {
	ks := newRegisteredKeystore(t)
	authority := installationAuthoritySource{
		fixtureAppID: {
			ActiveEpoch:           7,
			DurableIntentRevision: 11,
			Pending: &publish.PendingInstallationEnvelope{
				ActiveEpoch:            7,
				DurableIntentRevision:  11,
				RegistrationID:         fixtureAppID,
				ExpectedAccount:        "freeside-ai",
				ExpectedAccountID:      testOwnerID,
				InstallationID:         777,
				ExpectedRepositoryIDs:  []int64{fixtureRepositoryID},
				RequiredRepositoryMode: "selected",
				ExpiresAt:              fixtureTime.Add(time.Hour),
			},
		},
	}
	var janitorMints atomic.Int32
	var workerMints atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_, _ = fmt.Fprintf(w,
				`[{"id":777,"app_id":%d,"target_id":%d,"repository_selection":"selected","account":{"login":"freeside-ai","id":%d}}]`,
				fixtureAppID,
				testOwnerID,
				testOwnerID,
			)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			body, _ := io.ReadAll(r.Body)
			var request map[string]json.RawMessage
			_ = json.Unmarshal(body, &request)
			if _, worker := request["repository_ids"]; worker {
				workerMints.Add(1)
			} else {
				janitorMints.Add(1)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w,
				`{"token":"`+fixtureTokenValue+`","permissions":{"metadata":"read"},"repository_selection":"selected"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/installation/repositories":
			_, _ = io.WriteString(w,
				`{"total_count":1,"repositories":[{"id":990011}]}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/installation/token":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	janitor, err := publish.NewInstallationJanitor(
		ks,
		srv.Client(),
		srv.URL,
		authority,
		&removalRecorder{},
		fixedNow,
		1,
	)
	if err != nil {
		t.Fatalf("NewInstallationJanitor: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- janitor.Run(ctx, time.Hour) }()
	deadline := time.Now().Add(time.Second)
	for !janitor.ActiveFor(fixtureAppID) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !janitor.ActiveFor(fixtureAppID) {
		cancel()
		t.Fatal("private registration did not receive exact pending coverage")
	}
	if janitor.AllowsRepository(fixtureAppID, 777, fixtureRepositoryID) {
		t.Fatal("pending envelope entered the trusted mint allow-set")
	}

	minter := publish.NewMinterWithJanitor(
		ks,
		srv.Client(),
		srv.URL,
		&captureRecorder{},
		conformantTrust(t),
		fixedNow,
		janitor,
	)
	_, err = minter.MintInstallationToken(context.Background(), testTrustRepo)
	if !errors.Is(err, publish.ErrInstallationGrantUntrusted) {
		t.Fatalf("err = %v, want ErrInstallationGrantUntrusted", err)
	}
	if janitorMints.Load() != 1 || workerMints.Load() != 0 {
		t.Errorf(
			"janitor mints = %d, worker mints = %d",
			janitorMints.Load(),
			workerMints.Load(),
		)
	}

	s := newTestStore(t)
	var attentionItems int
	if err := s.Read(context.Background(), func(tx *store.ReadTx) error {
		items, err := tx.ListAttentionItems(context.Background())
		attentionItems = len(items)
		return err
	}); err != nil {
		t.Fatalf("read attention state: %v", err)
	}
	if attentionItems != 0 {
		t.Errorf("pending installation produced %d AttentionItems", attentionItems)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run shutdown: %v", err)
	}
}
