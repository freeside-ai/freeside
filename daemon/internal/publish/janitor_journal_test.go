package publish_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

const (
	journalFileName     = "installation-janitor-journal.json"
	journalLockFileName = "installation-janitor-journal.lock"
)

// operatorDocument is the authority for the registration the janitor tests
// share: App 501, owned by operator/101.
func operatorDocument(bindings ...publish.TrustedInstallationRecord) publish.InstallationAuthorityDocument {
	// Never nil: an omitted binding list is a rejected document, since absent
	// and "trust nothing" must not be the same authored file.
	if bindings == nil {
		bindings = []publish.TrustedInstallationRecord{}
	}
	return publish.InstallationAuthorityDocument{
		Version: 1,
		Registrations: []publish.InstallationAuthorityEntry{{
			RegistrationID:        501,
			ActiveEpoch:           1,
			DurableIntentRevision: 1,
			TrustedOwners:         []publish.TrustedOwnerRecord{{Login: "operator", ID: 101}},
			TrustedInstallations:  bindings,
		}},
	}
}

func operatorBinding(installationID int64, repositoryIDs ...int64) publish.TrustedInstallationRecord {
	return publish.TrustedInstallationRecord{
		InstallationID: installationID,
		Account:        "operator",
		AccountID:      101,
		RepositoryIDs:  repositoryIDs,
	}
}

// newDocumentStore writes the document into a fresh state directory and returns
// both, so a test can reopen the directory to model a restart.
func newDocumentStore(t *testing.T, document publish.InstallationAuthorityDocument) (string, *publish.InstallationAuthorityStore) {
	t.Helper()
	payload, err := document.Encode()
	if err != nil {
		t.Fatalf("encode document: %v", err)
	}
	return newAuthorityStore(t, string(payload))
}

func reopenAuthorityStore(t *testing.T, dir string) *publish.InstallationAuthorityStore {
	t.Helper()
	store, err := publish.NewInstallationAuthorityStore(dir)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	return store
}

// journalOnDisk decodes the durable journal the way a restarted daemon would,
// rather than trusting the writer's in-memory view of it.
type journalOnDisk struct {
	Version     int `json:"version"`
	Quarantined []struct {
		RegistrationID int64     `json:"registration_id"`
		InstallationID int64     `json:"installation_id"`
		RecordedAt     time.Time `json:"recorded_at"`
	} `json:"quarantined"`
	Entries []struct {
		Action                string    `json:"action"`
		RequestedAt           time.Time `json:"requested_at"`
		RegistrationID        int64     `json:"registration_id"`
		InstallationID        int64     `json:"installation_id"`
		AccountID             int64     `json:"account_id"`
		Reason                string    `json:"reason"`
		ObservedRepositoryIDs []int64   `json:"observed_repository_ids"`
	} `json:"entries"`
}

func readJournal(t *testing.T, dir string) journalOnDisk {
	t.Helper()
	path := filepath.Join(dir, journalFileName)
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat journal: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("journal is mode %04o, want 0600", info.Mode().Perm())
	}
	payload, err := os.ReadFile(path) //nolint:gosec // test-owned path under t.TempDir
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	var journal journalOnDisk
	if err := json.Unmarshal(payload, &journal); err != nil {
		t.Fatalf("decode journal: %v", err)
	}
	return journal
}

// assertNoResidue proves the durable write left nothing behind: a surviving
// temporary file would mean the rename, not the write, is what a reader races.
func assertNoResidue(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read state dir: %v", err)
	}
	for _, entry := range entries {
		switch entry.Name() {
		case "installation-authority.json", journalFileName, journalLockFileName:
		default:
			t.Fatalf("state directory holds %q after a durable write", entry.Name())
		}
	}
}

func removalRecord(installationID int64, reason publish.InstallationRemovalReason) publish.InstallationRemovalRecord {
	return publish.InstallationRemovalRecord{
		RequestedAt:    fixtureTime,
		RegistrationID: 501,
		InstallationID: installationID,
		AccountID:      101,
		Reason:         reason,
	}
}

func TestRecordInstallationRemovalCommitsDurably(t *testing.T) {
	t.Parallel()
	dir, store := newDocumentStore(t, operatorDocument(operatorBinding(701, fixtureRepositoryID)))

	if err := store.RecordInstallationRemoval(removalRecord(702, publish.InstallationRemovalUnbound)); err != nil {
		t.Fatalf("record removal: %v", err)
	}

	journal := readJournal(t, dir)
	if journal.Version != 1 || len(journal.Entries) != 1 {
		t.Fatalf("journal is %+v, want one entry at version 1", journal)
	}
	entry := journal.Entries[0]
	if entry.Action != "removal" || entry.InstallationID != 702 ||
		entry.Reason != string(publish.InstallationRemovalUnbound) || !entry.RequestedAt.Equal(fixtureTime) {
		t.Fatalf("entry is %+v", entry)
	}
	// A removal names an installation that was never trusted, so it withdraws
	// nothing: only quarantine may narrow the authority.
	if len(journal.Quarantined) != 0 {
		t.Fatalf("removal quarantined %+v", journal.Quarantined)
	}
	assertNoResidue(t, dir)
}

func TestRemovalLeavesTrustIntact(t *testing.T) {
	t.Parallel()
	dir, store := newDocumentStore(t, operatorDocument(operatorBinding(701, fixtureRepositoryID)))

	if err := store.RecordInstallationRemoval(removalRecord(701, publish.InstallationRemovalUnbound)); err != nil {
		t.Fatalf("record removal: %v", err)
	}

	authority, err := reopenAuthorityStore(t, dir).InstallationAuthority(t.Context(), 501)
	if err != nil {
		t.Fatalf("installation authority: %v", err)
	}
	if len(authority.TrustedInstallations) != 1 {
		t.Fatalf("trusted installations are %+v, want the binding kept", authority.TrustedInstallations)
	}
}

func TestQuarantineWithdrawsTrustAcrossRestart(t *testing.T) {
	t.Parallel()
	dir, store := newDocumentStore(t, operatorDocument(operatorBinding(701, fixtureRepositoryID)))

	record := removalRecord(701, publish.InstallationRemovalGrantDrift)
	record.ObservedRepositoryIDs = []int64{fixtureRepositoryID, fixtureRepositoryID + 1}
	if err := store.RecordInstallationQuarantine(record); err != nil {
		t.Fatalf("record quarantine: %v", err)
	}

	// The operator's file still names installation 701. A restarted daemon must
	// not read it back as trust: the withdrawal is what survives, not the stale
	// authored binding.
	authority, err := reopenAuthorityStore(t, dir).InstallationAuthority(t.Context(), 501)
	if err != nil {
		t.Fatalf("installation authority after restart: %v", err)
	}
	if len(authority.TrustedInstallations) != 0 {
		t.Fatalf("restart re-trusted %+v", authority.TrustedInstallations)
	}

	journal := readJournal(t, dir)
	if len(journal.Quarantined) != 1 || journal.Quarantined[0].InstallationID != 701 {
		t.Fatalf("quarantine set is %+v", journal.Quarantined)
	}
	if len(journal.Entries) != 1 || journal.Entries[0].Action != "quarantine" ||
		!slices.Equal(journal.Entries[0].ObservedRepositoryIDs, record.ObservedRepositoryIDs) {
		t.Fatalf("audit entry is %+v", journal.Entries)
	}
}

func TestRepeatedQuarantineKeepsOneWithdrawalAndEveryRequest(t *testing.T) {
	t.Parallel()
	dir, store := newDocumentStore(t, operatorDocument(operatorBinding(701, fixtureRepositoryID)))

	for range 2 {
		if err := store.RecordInstallationQuarantine(removalRecord(701, publish.InstallationRemovalSelectionDrift)); err != nil {
			t.Fatalf("record quarantine: %v", err)
		}
	}

	journal := readJournal(t, dir)
	if len(journal.Quarantined) != 1 {
		t.Fatalf("quarantine set is %+v, want one member", journal.Quarantined)
	}
	// Each destructive request is its own event even when the withdrawal it
	// implies is already recorded.
	if len(journal.Entries) != 2 {
		t.Fatalf("audit entries are %+v, want both requests", journal.Entries)
	}
}

func TestQuarantineDropsThePendingEnvelopeItNames(t *testing.T) {
	t.Parallel()
	document := operatorDocument()
	document.Registrations[0].Pending = &publish.PendingEnvelopeRecord{
		ActiveEpoch:            1,
		DurableIntentRevision:  1,
		ExpectedAccount:        "operator",
		ExpectedAccountID:      101,
		InstallationID:         ptrInt64(705),
		ExpectedRepositoryIDs:  []int64{fixtureRepositoryID},
		RequiredRepositoryMode: "selected",
		ExpiresAt:              fixtureTime.Add(time.Hour),
	}
	dir, store := newDocumentStore(t, document)

	if err := store.RecordInstallationQuarantine(removalRecord(705, publish.InstallationRemovalIdentityDrift)); err != nil {
		t.Fatalf("record quarantine: %v", err)
	}

	authority, err := reopenAuthorityStore(t, dir).InstallationAuthority(t.Context(), 501)
	if err != nil {
		t.Fatalf("installation authority after restart: %v", err)
	}
	if authority.Pending != nil {
		t.Fatalf("restart served the quarantined envelope %+v", authority.Pending)
	}
}

func TestQuarantineRefusesAStaleBindingBesideAnUnrelatedEnvelope(t *testing.T) {
	t.Parallel()
	// The regression this pins: dropping binding 701 would leave a document the
	// janitor previously rejected in a shape it accepts, because several of its
	// cross-binding rules are satisfiable by removal. Rather than decide what a
	// stale binding plus a live exception meant, the pass fails.
	document := operatorDocument(operatorBinding(701, fixtureRepositoryID))
	document.Registrations[0].TrustedOwners = append(
		document.Registrations[0].TrustedOwners,
		publish.TrustedOwnerRecord{Login: "other-org", ID: 202},
	)
	document.Registrations[0].Pending = &publish.PendingEnvelopeRecord{
		ActiveEpoch:            1,
		DurableIntentRevision:  1,
		ExpectedAccount:        "other-org",
		ExpectedAccountID:      202,
		InstallationID:         new(int64),
		ExpectedRepositoryIDs:  []int64{fixtureRepositoryID + 5},
		RequiredRepositoryMode: "selected",
		ExpiresAt:              fixtureTime.Add(time.Hour),
	}
	dir, store := newDocumentStore(t, document)

	if err := store.RecordInstallationQuarantine(removalRecord(701, publish.InstallationRemovalGrantDrift)); err != nil {
		t.Fatalf("record quarantine: %v", err)
	}

	authority, err := reopenAuthorityStore(t, dir).InstallationAuthority(t.Context(), 501)
	if !errors.Is(err, publish.ErrInstallationAuthoritySnapshot) {
		t.Fatalf("served %+v with error %v, want a snapshot error", authority, err)
	}
}

func TestUnusableJournalDeniesTheAuthority(t *testing.T) {
	t.Parallel()
	valid := `{"version":1,"quarantined":[{"registration_id":501,"installation_id":701,` +
		`"recorded_at":"2026-01-02T03:04:05Z"}],"entries":[{"action":"quarantine",` +
		`"requested_at":"2026-01-02T03:04:05Z","registration_id":501,"installation_id":701,` +
		`"account_id":101,"reason":"repository_grant_drift","observed_repository_ids":null}]}`

	cases := map[string]string{
		"withdrawal deleted from the set": `{"version":1,"quarantined":[],"entries":[{"action":"quarantine",` +
			`"requested_at":"2026-01-02T03:04:05Z","registration_id":501,"installation_id":701,` +
			`"account_id":101,"reason":"repository_grant_drift","observed_repository_ids":null}]}`,
		"unknown version": `{"version":2,"quarantined":[],"entries":[]}`,
		// The quarantine set is the key an operator rotating the audit log could
		// drop by hand, and absent would read as "nothing was ever withdrawn".
		"omitted quarantine set": `{"version":1,"entries":[]}`,
		"omitted entries":        `{"version":1,"quarantined":[]}`,
		"quarantine record omits its timestamp": `{"version":1,"quarantined":` +
			`[{"registration_id":501,"installation_id":701}],"entries":[]}`,
		"unknown field": `{"version":1,"quarantined":[],"entries":[],"rotated":true}`,
		"unknown action": `{"version":1,"quarantined":[],"entries":[{"action":"suspend",` +
			`"requested_at":"2026-01-02T03:04:05Z","registration_id":501,"installation_id":701,` +
			`"account_id":101,"reason":"repository_grant_drift","observed_repository_ids":null}]}`,
		"unknown reason": `{"version":1,"quarantined":[],"entries":[{"action":"removal",` +
			`"requested_at":"2026-01-02T03:04:05Z","registration_id":501,"installation_id":701,` +
			`"account_id":101,"reason":"because","observed_repository_ids":null}]}`,
		"trailing data": valid + "\n{}",
		"truncated":     valid[:len(valid)/2],
	}

	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			dir, store := newDocumentStore(t, operatorDocument(operatorBinding(701, fixtureRepositoryID)))
			if err := os.WriteFile(filepath.Join(dir, journalFileName), []byte(payload), 0o600); err != nil {
				t.Fatalf("write journal: %v", err)
			}
			if _, err := store.InstallationAuthority(t.Context(), 501); err == nil {
				t.Fatal("an unusable journal served an authority")
			}
		})
	}

	t.Run("valid journal still serves", func(t *testing.T) {
		dir, store := newDocumentStore(t, operatorDocument(operatorBinding(701, fixtureRepositoryID)))
		if err := os.WriteFile(filepath.Join(dir, journalFileName), []byte(valid), 0o600); err != nil {
			t.Fatalf("write journal: %v", err)
		}
		authority, err := store.InstallationAuthority(t.Context(), 501)
		if err != nil {
			t.Fatalf("installation authority: %v", err)
		}
		if len(authority.TrustedInstallations) != 0 {
			t.Fatalf("quarantined binding survived: %+v", authority.TrustedInstallations)
		}
	})
}

func TestRecorderRejectsAnIncompleteRecord(t *testing.T) {
	t.Parallel()
	_, store := newDocumentStore(t, operatorDocument(operatorBinding(701, fixtureRepositoryID)))

	for name, record := range map[string]publish.InstallationRemovalRecord{
		"no reason":       {RequestedAt: fixtureTime, RegistrationID: 501, InstallationID: 701, AccountID: 101},
		"no timestamp":    {RegistrationID: 501, InstallationID: 701, AccountID: 101, Reason: publish.InstallationRemovalUnbound},
		"no registration": {RequestedAt: fixtureTime, InstallationID: 701, AccountID: 101, Reason: publish.InstallationRemovalUnbound},
		"no installation": {RequestedAt: fixtureTime, RegistrationID: 501, AccountID: 101, Reason: publish.InstallationRemovalUnbound},
		"no account":      {RequestedAt: fixtureTime, RegistrationID: 501, InstallationID: 701, Reason: publish.InstallationRemovalUnbound},
		"unsorted observed": {
			RequestedAt: fixtureTime, RegistrationID: 501, InstallationID: 701, AccountID: 101,
			Reason: publish.InstallationRemovalGrantDrift, ObservedRepositoryIDs: []int64{9, 9},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := store.RecordInstallationQuarantine(record); err == nil {
				t.Fatal("an incomplete record was committed")
			}
		})
	}
}

// newStoreJanitor wires a janitor whose authority and audit both come from the
// state directory, which is the composition #236 will build in freesided.
func newStoreJanitor(
	t *testing.T,
	ks *publish.Keystore,
	srv *httptest.Server,
	store *publish.InstallationAuthorityStore,
	maxRemovals int,
) *publish.InstallationJanitor {
	t.Helper()
	janitor, err := publish.NewInstallationJanitor(ks, srv.Client(), srv.URL, store, store, &captureMintRecorder{}, fixedNow, maxRemovals)
	if err != nil {
		t.Fatalf("NewInstallationJanitor: %v", err)
	}
	return janitor
}

func TestStoreBackedJanitorPublishesCoverage(t *testing.T) {
	t.Parallel()
	ks := publicJanitorKeystore(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handleExactGrant(w, r, fixtureRepositoryID) {
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
			_, _ = io.WriteString(w, `[
				{"id":701,"app_id":501,"target_id":101,"repository_selection":"selected","account":{"login":"operator","id":101}}
			]`)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	dir, store := newDocumentStore(t, operatorDocument(operatorBinding(701, fixtureRepositoryID)))
	janitor := newStoreJanitor(t, ks, srv, store, 10)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- janitor.Run(ctx, time.Hour) }()
	deadline := time.Now().Add(2 * time.Second)
	for !janitor.ActiveFor(501) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	activated := janitor.ActiveFor(501)
	allowed := janitor.AllowsRepository(501, 701, fixtureRepositoryID)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run shutdown: %v", err)
	}

	if !activated {
		t.Fatal("a file-backed authority did not publish registration coverage")
	}
	if !allowed {
		t.Fatal("coverage did not authorize the trusted repository")
	}
	if _, err := os.Lstat(filepath.Join(dir, journalFileName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a clean pass wrote a journal (%v)", err)
	}
}

func TestStoreBackedJanitorDeniesAPassWithoutTheRegistrationOwner(t *testing.T) {
	t.Parallel()
	ks := publicJanitorKeystore(t)
	deletes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
			_, _ = io.WriteString(w, `[
				{"id":701,"app_id":501,"target_id":101,"repository_selection":"selected","account":{"login":"operator","id":101}}
			]`)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	// The document is well-formed on its own; only the registration's own
	// credentials reveal that a public App's owner is missing from the trusted
	// set. That check stays in validateInstallationAuthority, and the pass must
	// still fail closed there rather than reconciling against a partial owner set.
	document := operatorDocument(operatorBinding(701, fixtureRepositoryID))
	document.Registrations[0].TrustedOwners = []publish.TrustedOwnerRecord{{Login: "other-org", ID: 202}}
	document.Registrations[0].TrustedInstallations = []publish.TrustedInstallationRecord{}
	_, store := newDocumentStore(t, document)

	if _, err := newStoreJanitor(t, ks, srv, store, 10).RunCycle(context.Background()); err == nil {
		t.Fatal("a snapshot omitting the registration owner completed a pass")
	}
	if deletes != 0 {
		t.Fatalf("a denied pass issued %d deletes", deletes)
	}
}

func TestStoreBackedJanitorFailsThePassWhenTheJournalCannotBeWritten(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root bypasses the unwritable-directory check")
	}
	ks := publicJanitorKeystore(t)
	deletes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deletes++
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet && r.URL.Path == "/app/installations" {
			_, _ = io.WriteString(w, `[
				{"id":702,"app_id":501,"target_id":202,"account":{"login":"unsolicited-owner","id":202}}
			]`)
			return
		}
		t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()

	dir, store := newDocumentStore(t, operatorDocument())
	janitor := newStoreJanitor(t, ks, srv, store, 10)
	chmod(t, dir, 0o500)
	t.Cleanup(func() { chmod(t, dir, 0o700) })

	if _, err := janitor.RunCycle(context.Background()); err == nil {
		t.Fatal("the pass completed while its audit barrier could not commit")
	}
	if deletes != 0 {
		t.Fatalf("an uncommitted audit barrier still issued %d deletes", deletes)
	}
	if janitor.ActiveFor(501) {
		t.Fatal("a failed pass published coverage")
	}
}

// TestQuarantinedInstallationIsDeletedRatherThanRetrustedAfterRestart models the
// crash between the audit barrier and the destructive request: the withdrawal is
// already durable, so the installation the operator's file still names comes back
// as unknown rather than trusted. It is removed, not suspended, which is the
// residual cost of that crash window.
func TestQuarantinedInstallationIsDeletedRatherThanRetrustedAfterRestart(t *testing.T) {
	t.Parallel()
	ks := publicJanitorKeystore(t)
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if handleExactGrant(w, r, fixtureRepositoryID, fixtureRepositoryID+1) {
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			_, _ = io.WriteString(w, `[
				{"id":701,"app_id":501,"target_id":101,"repository_selection":"selected","account":{"login":"operator","id":101}}
			]`)
		case r.Method == http.MethodPut, r.Method == http.MethodDelete:
			requests = append(requests, r.Method+" "+r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	dir, store := newDocumentStore(t, operatorDocument(operatorBinding(701, fixtureRepositoryID)))
	if _, err := newStoreJanitor(t, ks, srv, store, 10).RunCycle(context.Background()); err != nil {
		t.Fatalf("first cycle: %v", err)
	}
	if len(requests) != 2 || requests[0] != "PUT /app/installations/701/suspended" {
		t.Fatalf("first cycle issued %v, want a suspend before the delete", requests)
	}

	requests = nil
	if _, err := newStoreJanitor(t, ks, srv, reopenAuthorityStore(t, dir), 10).RunCycle(context.Background()); err != nil {
		t.Fatalf("second cycle: %v", err)
	}
	if len(requests) != 1 || requests[0] != "DELETE /app/installations/701" {
		t.Fatalf("second cycle issued %v, want a bare delete", requests)
	}
	journal := readJournal(t, dir)
	if len(journal.Entries) != 2 || journal.Entries[1].Action != "removal" {
		t.Fatalf("second cycle recorded %+v, want a removal", journal.Entries)
	}
}

func ptrInt64(value int64) *int64 { return &value }

// TestOversizedJournalIsRefusedBeforeItIsWritten pins the brick a refute-first
// pass demonstrated. The observed repository set rides the audit entry and is
// sized by the account being reconciled, which is the untrusted side of this
// boundary, so a journal could be written past the size it can be read at. That
// would deny every registration and could not be repaired by the daemon itself,
// so the write has to fail first, leaving the previous journal readable.
func TestOversizedJournalIsRefusedBeforeItIsWritten(t *testing.T) {
	t.Parallel()
	dir, store := newDocumentStore(t, operatorDocument(operatorBinding(701, fixtureRepositoryID)))

	// The janitor caps grant enumeration at 10,000 repository IDs, so a handful
	// of drifted installations reach the journal's bound on their own.
	observed := make([]int64, 10_000)
	for index := range observed {
		observed[index] = int64(index + 1)
	}
	entry := map[string]any{
		"action":                  "removal",
		"requested_at":            fixtureTime.UTC().Format(time.RFC3339Nano),
		"registration_id":         501,
		"installation_id":         702,
		"account_id":              101,
		"reason":                  string(publish.InstallationRemovalUnbound),
		"observed_repository_ids": observed,
	}
	entries := make([]map[string]any, 0, 64)
	var payload []byte
	for len(payload) < 8<<20 {
		entries = append(entries, entry)
		encoded, err := json.MarshalIndent(map[string]any{
			"version":     1,
			"quarantined": []any{},
			"entries":     entries,
		}, "", "  ")
		if err != nil {
			t.Fatalf("encode oversized journal: %v", err)
		}
		payload = append(encoded, '\n')
	}
	// One entry short of the bound, so the store can still read it and the next
	// record is what crosses the line.
	entries = entries[:len(entries)-1]
	payload, err := json.MarshalIndent(map[string]any{
		"version":     1,
		"quarantined": []any{},
		"entries":     entries,
	}, "", "  ")
	if err != nil {
		t.Fatalf("encode journal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, journalFileName), append(payload, '\n'), 0o600); err != nil {
		t.Fatalf("write journal: %v", err)
	}

	record := removalRecord(701, publish.InstallationRemovalGrantDrift)
	record.ObservedRepositoryIDs = observed
	if err := store.RecordInstallationQuarantine(record); err == nil {
		t.Fatal("a record that would grow the journal past its read limit was committed")
	}

	// The refusal must leave the store usable, not wedged.
	if _, err := reopenAuthorityStore(t, dir).InstallationAuthority(t.Context(), 501); err != nil {
		t.Fatalf("the refused record left the journal unreadable: %v", err)
	}
}

// TestConcurrentStoresKeepEveryWithdrawal pins the silent loss a refute-first
// pass demonstrated. The janitor takes its authority and its recorder as two
// separate parameters, so a composer can build one store per port; with only an
// in-process mutex per value, two read-modify-writes interleave and the later
// rename discards the earlier withdrawal, leaving an installation this daemon
// suspended and deleted trusted again after a restart.
func TestConcurrentStoresKeepEveryWithdrawal(t *testing.T) {
	t.Parallel()
	const perStore = 20
	dir, first := newDocumentStore(t, operatorDocument(operatorBinding(701, fixtureRepositoryID)))
	second := reopenAuthorityStore(t, dir)

	var wg sync.WaitGroup
	for index, store := range []*publish.InstallationAuthorityStore{first, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for offset := range perStore {
				record := removalRecord(int64(1_000+index*perStore+offset), publish.InstallationRemovalGrantDrift)
				if err := store.RecordInstallationQuarantine(record); err != nil {
					t.Errorf("record quarantine: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	journal := readJournal(t, dir)
	if len(journal.Quarantined) != 2*perStore || len(journal.Entries) != 2*perStore {
		t.Fatalf(
			"journal holds %d withdrawals and %d entries, want %d of each",
			len(journal.Quarantined), len(journal.Entries), 2*perStore,
		)
	}
}
