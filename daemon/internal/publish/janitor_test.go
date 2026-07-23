package publish_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

type trustedOwnerSource map[int64][]publish.TrustedOwner

func (s trustedOwnerSource) TrustedOwners(_ context.Context, registrationID int64) ([]publish.TrustedOwner, error) {
	return append([]publish.TrustedOwner(nil), s[registrationID]...), nil
}

type removalRecorder struct {
	mu      sync.Mutex
	records []publish.InstallationRemovalRecord
	err     error
}

func (r *removalRecorder) RecordInstallationRemoval(record publish.InstallationRemovalRecord) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.records = append(r.records, record)
	return nil
}

func (r *removalRecorder) snapshot() []publish.InstallationRemovalRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]publish.InstallationRemovalRecord(nil), r.records...)
}

func publicJanitorKeystore(t *testing.T) *publish.Keystore {
	t.Helper()
	ks := newTestKeystore(t)
	saveResolverApp(t, ks, "operator", 101, 501, publish.AppVisibilityPublic)
	return ks
}

func newJanitor(
	t *testing.T,
	ks *publish.Keystore,
	server *httptest.Server,
	owners trustedOwnerSource,
	recorder *removalRecorder,
	maxRemovals int,
) *publish.InstallationJanitor {
	t.Helper()
	janitor, err := publish.NewInstallationJanitor(
		ks,
		server.Client(),
		server.URL,
		owners,
		recorder,
		fixedNow,
		maxRemovals,
	)
	if err != nil {
		t.Fatalf("NewInstallationJanitor: %v", err)
	}
	return janitor
}

// TestInstallationJanitorRemovesUnknownOwner proves the public-default
// safety path: the trusted owner is untouched, the unsolicited installation
// is removed, and its safe numeric coordinates cross the audit barrier before
// the destructive request.
func TestInstallationJanitorRemovesUnknownOwner(t *testing.T) {
	ks := publicJanitorKeystore(t)
	var deletes []string
	recorder := &removalRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_, _ = io.WriteString(w, `[
				{"id":701,"app_id":501,"target_id":101,"account":{"login":"operator","id":101}},
				{"id":702,"app_id":501,"target_id":202,"account":{"login":"unsolicited-owner","id":202}}
			]`)
		case r.Method == http.MethodDelete:
			if len(recorder.snapshot()) != 1 {
				t.Error("delete reached GitHub before its audit record")
			}
			deletes = append(deletes, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	janitor := newJanitor(t, ks, srv, trustedOwnerSource{
		501: {{Login: "operator", ID: 101}},
	}, recorder, 10)
	cycle, err := janitor.RunCycle(context.Background())
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if cycle.Examined != 2 || cycle.Removed != 1 || cycle.RemovalLimitReached {
		t.Errorf("cycle = %+v", cycle)
	}
	if len(deletes) != 1 || deletes[0] != "/app/installations/702" {
		t.Errorf("deletes = %v, want only installation 702", deletes)
	}
	records := recorder.snapshot()
	if len(records) != 1 {
		t.Fatalf("audit records = %d, want 1", len(records))
	}
	want := publish.InstallationRemovalRecord{
		RequestedAt:    fixtureTime,
		RegistrationID: 501,
		InstallationID: 702,
		AccountID:      202,
		Reason:         publish.InstallationRemovalUntrustedOwner,
	}
	if records[0] != want {
		t.Errorf("audit record = %+v, want %+v", records[0], want)
	}
}

// TestInstallationJanitorBoundsRemovalWork proves a cycle never exceeds its
// destructive-work budget even when GitHub returns more unknown installations.
func TestInstallationJanitorBoundsRemovalWork(t *testing.T) {
	ks := publicJanitorKeystore(t)
	deletes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `[
				{"id":701,"app_id":501,"target_id":201,"account":{"login":"unknown-one","id":201}},
				{"id":702,"app_id":501,"target_id":202,"account":{"login":"unknown-two","id":202}},
				{"id":703,"app_id":501,"target_id":203,"account":{"login":"unknown-three","id":203}}
			]`)
			return
		}
		deletes++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	janitor := newJanitor(t, ks, srv, trustedOwnerSource{
		501: {{Login: "operator", ID: 101}},
	}, &removalRecorder{}, 2)
	cycle, err := janitor.RunCycle(context.Background())
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if deletes != 2 || cycle.Removed != 2 || !cycle.RemovalLimitReached {
		t.Errorf("deletes = %d, cycle = %+v", deletes, cycle)
	}
}

// TestInstallationJanitorEnumeratesBeforePaginatedDeletes is the regression
// for review finding P2: deleting from page one must not shift the first row
// of page two into an already-observed page and allow false clean coverage.
func TestInstallationJanitorEnumeratesBeforePaginatedDeletes(t *testing.T) {
	ks := publicJanitorKeystore(t)
	const pageSize = 100
	type wireInstallation struct {
		ID       int64 `json:"id"`
		AppID    int64 `json:"app_id"`
		TargetID int64 `json:"target_id"`
		Account  struct {
			Login string `json:"login"`
			ID    int64  `json:"id"`
		} `json:"account"`
	}
	newWireInstallation := func(id, accountID int64, login string) wireInstallation {
		installation := wireInstallation{ID: id, AppID: 501, TargetID: accountID}
		installation.Account.Login = login
		installation.Account.ID = accountID
		return installation
	}

	installations := make([]wireInstallation, 0, 101)
	installations = append(installations, newWireInstallation(701, 201, "unknown-one"))
	for id := int64(702); id < 801; id++ {
		installations = append(installations, newWireInstallation(id, 101, "operator"))
	}
	installations = append(installations, newWireInstallation(801, 202, "unknown-two"))

	var mu sync.Mutex
	var events []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			start := (page - 1) * pageSize
			end := min(start+pageSize, len(installations))
			if start > len(installations) {
				start = len(installations)
			}
			events = append(events, "GET "+strconv.Itoa(page))
			_ = json.NewEncoder(w).Encode(installations[start:end])
		case http.MethodDelete:
			id, _ := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/app/installations/"), 10, 64)
			events = append(events, "DELETE "+strconv.FormatInt(id, 10))
			for index, installation := range installations {
				if installation.ID == id {
					installations = append(installations[:index], installations[index+1:]...)
					break
				}
			}
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	janitor := newJanitor(t, ks, srv, trustedOwnerSource{
		501: {{Login: "operator", ID: 101}},
	}, &removalRecorder{}, 10)
	cycle, err := janitor.RunCycle(context.Background())
	if err != nil {
		t.Fatalf("RunCycle: %v", err)
	}
	if cycle.Examined != 101 || cycle.Removed != 2 || cycle.RemovalLimitReached {
		t.Errorf("cycle = %+v", cycle)
	}
	wantEvents := []string{"GET 1", "GET 2", "DELETE 701", "DELETE 801"}
	if fmt.Sprint(events) != fmt.Sprint(wantEvents) {
		t.Errorf("events = %v, want %v", events, wantEvents)
	}
}

// TestInstallationJanitorAuditFailurePreventsDelete is the refute-first
// destructive-path check: an unavailable audit sink cannot produce an
// unlogged uninstall.
func TestInstallationJanitorAuditFailurePreventsDelete(t *testing.T) {
	ks := publicJanitorKeystore(t)
	deletes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w,
				`[{"id":701,"app_id":501,"target_id":201,"account":{"login":"unknown","id":201}}]`)
			return
		}
		deletes++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	recorder := &removalRecorder{err: errors.New("audit unavailable")}
	janitor := newJanitor(t, ks, srv, trustedOwnerSource{
		501: {{Login: "operator", ID: 101}},
	}, recorder, 1)
	if _, err := janitor.RunCycle(context.Background()); err == nil {
		t.Fatal("RunCycle succeeded with a failing audit recorder")
	}
	if deletes != 0 {
		t.Errorf("issued %d deletes without an audit record", deletes)
	}
}

// TestInstallationJanitorRequiresRegistrationOwner is the local-policy
// refutation: an empty or misbound trusted-owner source cannot make the
// janitor interpret every installation, including the operator's own, as
// removable.
func TestInstallationJanitorRequiresRegistrationOwner(t *testing.T) {
	ks := publicJanitorKeystore(t)
	requests := 0
	client := &http.Client{Transport: cleanupTransportFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("request should not be sent")
	})}
	janitor, err := publish.NewInstallationJanitor(
		ks,
		client,
		"https://api.github.test",
		trustedOwnerSource{},
		&removalRecorder{},
		fixedNow,
		1,
	)
	if err != nil {
		t.Fatalf("NewInstallationJanitor: %v", err)
	}
	if _, err := janitor.RunCycle(context.Background()); err == nil {
		t.Fatal("RunCycle accepted a trusted-owner set missing the registration owner")
	}
	if requests != 0 {
		t.Errorf("sent %d requests under an incomplete trusted-owner policy", requests)
	}
}

// TestInstallationJanitorRejectsMalformedIdentityBeforeDelete is the returned-
// object refutation: an App-ID mismatch cannot supply a deletion coordinate,
// enter the audit log, or reflect attacker-controlled account text.
func TestInstallationJanitorRejectsMalformedIdentityBeforeDelete(t *testing.T) {
	ks := publicJanitorKeystore(t)
	const untrustedLogin = "attacker-ghs_LEAKY"
	deletes := 0
	recorder := &removalRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = fmt.Fprintf(w,
				`[{"id":701,"app_id":999,"target_id":201,"account":{"login":%q,"id":201}}]`,
				untrustedLogin,
			)
			return
		}
		deletes++
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	janitor := newJanitor(t, ks, srv, trustedOwnerSource{
		501: {{Login: "operator", ID: 101}},
	}, recorder, 1)
	_, err := janitor.RunCycle(context.Background())
	if !errors.Is(err, publish.ErrInstallationResolution) {
		t.Fatalf("err = %v, want ErrInstallationResolution", err)
	}
	if deletes != 0 || len(recorder.snapshot()) != 0 {
		t.Errorf("malformed identity reached %d deletes and %d audit records", deletes, len(recorder.snapshot()))
	}
	if strings.Contains(err.Error(), untrustedLogin) {
		t.Errorf("error reflected untrusted account text: %v", err)
	}
}

// TestPublicResolutionRequiresActiveJanitor proves a public registration is
// refused before GitHub is contacted when no always-on janitor covers it.
func TestPublicResolutionRequiresActiveJanitor(t *testing.T) {
	ks := publicJanitorKeystore(t)
	requests := 0
	client := &http.Client{Transport: cleanupTransportFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("request should not be sent")
	})}
	resolver := publish.NewInstallationResolver(ks, client, "https://api.github.test", fixedNow)
	if _, err := resolver.Resolve(context.Background(), "operator"); !errors.Is(err, publish.ErrJanitorInactive) {
		t.Fatalf("err = %v, want ErrJanitorInactive", err)
	}
	if requests != 0 {
		t.Errorf("sent %d requests before the janitor gate", requests)
	}
}

// TestInstallationJanitorRunActivatesOnlyAfterCleanPass checks the runtime
// status contract consumed by minting and, later, doctor: startup is closed,
// the first complete pass activates the registration, and shutdown closes it.
func TestInstallationJanitorRunActivatesOnlyAfterCleanPass(t *testing.T) {
	ks := publicJanitorKeystore(t)
	firstPassed := make(chan struct{}, 1)
	secondStarted := make(chan struct{}, 1)
	releaseSecond := make(chan struct{})
	var callsMu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		callsMu.Lock()
		calls++
		call := calls
		callsMu.Unlock()
		if call == 2 {
			secondStarted <- struct{}{}
			<-releaseSecond
		}
		_, _ = io.WriteString(w,
			`[{"id":701,"app_id":501,"target_id":101,"account":{"login":"operator","id":101}}]`)
		if call == 1 {
			firstPassed <- struct{}{}
		}
	}))
	defer srv.Close()

	janitor := newJanitor(t, ks, srv, trustedOwnerSource{
		501: {{Login: "operator", ID: 101}},
	}, &removalRecorder{}, 1)
	if janitor.ActiveFor(501) {
		t.Fatal("janitor active before its first pass")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- janitor.Run(ctx, 10*time.Millisecond) }()
	<-firstPassed
	deadline := time.Now().Add(time.Second)
	for !janitor.ActiveFor(501) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !janitor.ActiveFor(501) {
		t.Fatal("janitor did not activate after a clean pass")
	}
	<-secondStarted
	if janitor.ActiveFor(501) {
		t.Fatal("janitor left stale coverage active while the next pass was blocked")
	}
	close(releaseSecond)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run shutdown: %v", err)
	}
	if janitor.ActiveFor(501) {
		t.Fatal("janitor remained active after shutdown")
	}
}

// TestUnknownInstallationCannotMintOrCreateAttention exercises the invariant
// against an active public-registration gate. An unsolicited installation
// cannot be selected for the trusted repository, reaches no token endpoint,
// and the publish path has no AttentionItem side effect.
func TestUnknownInstallationCannotMintOrCreateAttention(t *testing.T) {
	ks := publicJanitorKeystore(t)
	s := newTestStore(t)
	seedTrust(t, s, testTrustRepo)
	trust, err := publish.NewStoreTrustSource(s)
	if err != nil {
		t.Fatalf("NewStoreTrustSource: %v", err)
	}
	tokenRequests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
			_, _ = fmt.Fprint(w,
				`[{"id":702,"app_id":501,"target_id":202,"repository_selection":"selected","account":{"login":"unsolicited-owner","id":202}}]`)
			return
		}
		tokenRequests++
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	m := publish.NewMinterWithJanitor(
		ks,
		srv.Client(),
		srv.URL,
		&captureRecorder{},
		trust,
		fixedNow,
		activeJanitorStatus{},
	)
	if _, err := m.MintInstallationToken(context.Background(), testTrustRepo); !errors.Is(err, publish.ErrNoInstallation) {
		t.Fatalf("err = %v, want ErrNoInstallation", err)
	}
	if tokenRequests != 0 {
		t.Errorf("unknown installation reached %d token requests", tokenRequests)
	}

	attentionItems := 0
	err = s.Read(context.Background(), func(tx *store.ReadTx) error {
		items, err := tx.ListAttentionItems(context.Background())
		attentionItems = len(items)
		return err
	})
	if err != nil {
		t.Fatalf("read attention state: %v", err)
	}
	if attentionItems != 0 {
		t.Errorf("unknown installation produced %d AttentionItems", attentionItems)
	}
}
