package publish_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/operations"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

func recoveryStore(t *testing.T) (string, *publish.InstallationAuthorityStore, operations.InstallationIntentRequest) {
	t.Helper()
	doc := operatorDocument(operatorBinding(701, 44), publish.TrustedInstallationRecord{
		InstallationID: 702, Account: "other", AccountID: 102, RepositoryIDs: []int64{66},
	})
	doc.Registrations[0].TrustedOwners = append(doc.Registrations[0].TrustedOwners, publish.TrustedOwnerRecord{Login: "other", ID: 102})
	id := int64(701)
	doc.Registrations[0].Pending = &publish.PendingEnvelopeRecord{
		ActiveEpoch: 1, DurableIntentRevision: 1, ExpectedAccount: "operator", ExpectedAccountID: 101,
		InstallationID: &id, CurrentRepositoryIDs: []int64{44}, ExpectedRepositoryIDs: []int64{44, 55},
		RequiredRepositoryMode: "selected", ExpiresAt: fixedNow().Add(time.Hour),
	}
	dir, st := newDocumentStore(t, doc)
	if err := st.RecordInstallationQuarantine(publish.InstallationRemovalRecord{
		RegistrationID: 501, InstallationID: 701, AccountID: 101,
		Reason: publish.InstallationRemovalGrantDrift, RequestedAt: fixedNow(), ObservedRepositoryIDs: []int64{44, 55},
	}); err != nil {
		t.Fatal(err)
	}
	return dir, st, operations.InstallationIntentRequest{
		RegistrationID: 501, Account: "operator", AccountID: 101, RepositoryID: 44, ExpiresAt: fixedNow().Add(time.Hour),
	}
}

func recoveryBytes(t *testing.T, dir, name string) []byte {
	t.Helper()
	f, err := os.OpenInRoot(dir, name)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	}()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestInstallationRecoveryRetainsAuthorityAndRequiresFreshApproval(t *testing.T) {
	t.Parallel()
	dir, st, req := recoveryStore(t)
	before := recoveryBytes(t, dir, "installation-authority.json")
	journal := recoveryBytes(t, dir, journalFileName)
	pending, err := operations.RecoverInstallation(t.Context(), st, 701, req, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if *pending.InstallationID != 0 || len(pending.CurrentRepositoryIDs) != 0 ||
		!reflect.DeepEqual(pending.ExpectedRepositoryIDs, []int64{44}) || pending.DurableIntentRevision != 2 {
		t.Fatalf("fresh intent = %+v", pending)
	}
	archive := "installation-recovery-501-701-1.json"
	if !bytes.Equal(recoveryBytes(t, dir, archive), before) || !bytes.Equal(recoveryBytes(t, dir, journalFileName), journal) {
		t.Fatal("recovery lost prior authority or changed terminal quarantine")
	}
	info, err := os.Stat(filepath.Join(dir, archive))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("archive permission: %v, %v", info, err)
	}
	reopened := reopenAuthorityStore(t, dir)
	snapshot, err := reopened.InstallationAuthority(t.Context(), 501)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snapshot.QuarantinedInstallationIDs, []int64{701}) ||
		len(snapshot.TrustedInstallations) != 1 || snapshot.TrustedInstallations[0].InstallationID != 702 || snapshot.Pending == nil {
		t.Fatalf("reopened authority = %+v", snapshot)
	}
	if _, err := operations.RecoverInstallation(t.Context(), reopened, 701, req, fixedNow); err == nil {
		t.Fatal("recovery replay replaced the fresh request")
	}
	approved := *snapshot.Pending
	approved.InstallationID = 703
	approved.DurableIntentRevision--
	if err := operations.PromoteInstallation(t.Context(), reopened, approved, 44, fixedNow); err == nil {
		t.Fatal("prior intent approval promoted the replacement")
	}
	approved.DurableIntentRevision++
	if err := operations.PromoteInstallation(t.Context(), reopened, approved, 44, fixedNow); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(recoveryBytes(t, dir, archive), before) || !bytes.Equal(recoveryBytes(t, dir, journalFileName), journal) {
		t.Fatal("promotion changed retained recovery evidence")
	}
}

func TestInstallationRecoveryRefusesUnsafeReplacement(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"not quarantined", "wrong account", "expired", "known new ID", "unrelated pending", "archive conflict", "archive unreadable"} {
		t.Run(name, func(t *testing.T) {
			dir, st, req := recoveryStore(t)
			old := int64(701)
			switch name {
			case "not quarantined":
				old = 702
			case "wrong account":
				req.AccountID = 102
			case "expired":
				req.ExpiresAt = fixedNow()
			case "known new ID":
				req.InstallationID = 703
			case "unrelated pending":
				if err := st.UpdateDocument(t.Context(), func(doc *publish.InstallationAuthorityDocument) error {
					p := doc.Registrations[0].Pending
					*p.InstallationID = 702
					p.ExpectedAccount, p.ExpectedAccountID = "other", 102
					p.CurrentRepositoryIDs, p.ExpectedRepositoryIDs = []int64{66}, []int64{66, 77}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			case "archive conflict":
				if err := os.WriteFile(filepath.Join(dir, "installation-recovery-501-701-1.json"), []byte("retained evidence"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "archive unreadable":
				if err := os.Mkdir(filepath.Join(dir, "installation-recovery-501-701-1.json"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			before := recoveryBytes(t, dir, "installation-authority.json")
			journal := recoveryBytes(t, dir, journalFileName)
			if _, err := operations.RecoverInstallation(t.Context(), st, old, req, fixedNow); err == nil {
				t.Fatal("unsafe replacement accepted")
			}
			if !bytes.Equal(before, recoveryBytes(t, dir, "installation-authority.json")) || !bytes.Equal(journal, recoveryBytes(t, dir, journalFileName)) {
				t.Fatal("rejected recovery changed authority or quarantine")
			}
		})
	}
}

func TestInstallationRecoveryResumesAfterAuthorityWasArchived(t *testing.T) {
	t.Parallel()
	dir, st, req := recoveryStore(t)
	before := recoveryBytes(t, dir, "installation-authority.json")
	// Model a crash after durable archival but before active authority replacement.
	if err := os.WriteFile(filepath.Join(dir, "installation-recovery-501-701-1.json"), before, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := operations.RecoverInstallation(t.Context(), st, 701, req, fixedNow); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, recoveryBytes(t, dir, "installation-recovery-501-701-1.json")) {
		t.Fatal("retry replaced preserved authority")
	}
}

func TestInstallationRecoveryJanitorRejectsOldAndAdmitsNewAfterRestart(t *testing.T) {
	t.Parallel()
	dir, st, req := recoveryStore(t)
	if _, err := operations.RecoverInstallation(t.Context(), st, 701, req, fixedNow); err != nil {
		t.Fatal(err)
	}
	// The old installation can still exist if deletion failed after quarantine.
	// It must be removed without token minting; the new ID gets the exception.
	oldDeleted, newMinted := false, false
	listedRepository := int64(44)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/app/installations":
			items := `{"id":702,"app_id":501,"target_id":102,"repository_selection":"selected","account":{"login":"other","id":102}},{"id":703,"app_id":501,"target_id":101,"repository_selection":"selected","account":{"login":"operator","id":101}}`
			if !oldDeleted {
				items = `{"id":701,"app_id":501,"target_id":101,"repository_selection":"selected","account":{"login":"operator","id":101}},` + items
			}
			_, _ = io.WriteString(w, `[`+items+`]`)
		case r.Method == http.MethodDelete && r.URL.Path == "/app/installations/701":
			oldDeleted = true
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/app/installations/701/access_tokens":
			t.Error("quarantined installation received a token")
			w.WriteHeader(http.StatusForbidden)
		default:
			if r.Method == http.MethodPost && r.URL.Path == "/app/installations/702/access_tokens" {
				listedRepository = 66
			}
			if r.Method == http.MethodPost && r.URL.Path == "/app/installations/703/access_tokens" {
				newMinted = true
				listedRepository = 44
			}
			if !handleExactGrant(w, r, listedRepository) {
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		}
	}))
	t.Cleanup(server.Close)
	reopened := reopenAuthorityStore(t, dir)
	janitor := newJanitor(t, publicJanitorKeystore(t), server, reopened, reopened, 2)
	if err := janitor.RunScheduledPass(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !oldDeleted || !newMinted {
		t.Fatalf("old deleted=%v, new minted=%v", oldDeleted, newMinted)
	}
	// A pass that removed an installation cannot publish readiness. The next
	// complete pass observes the fresh native selection after deletion.
	if err := janitor.RunScheduledPass(t.Context()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := reopened.InstallationAuthority(t.Context(), 501)
	if err != nil {
		t.Fatal(err)
	}
	if id, ready := janitor.PendingReady(*snapshot.Pending); !ready || id != 703 {
		t.Fatalf("pending ready = %d, %v", id, ready)
	}
	if janitor.AllowsRepository(501, 703, 44) {
		t.Fatal("fresh pending installation gained repository authority before approval")
	}
}
