package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/publish"
)

func TestOnboardRecoveryReturnsFreshNativeInstallationRequest(t *testing.T) {
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
	if err := authority.UpdateDocument(t.Context(), func(doc *publish.InstallationAuthorityDocument) error {
		doc.Registrations[0].TrustedInstallations = []publish.TrustedInstallationRecord{{InstallationID: 777, Account: "example", AccountID: 42, RepositoryIDs: []int64{44}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := authority.RecordInstallationQuarantine(publish.InstallationRemovalRecord{
		RegistrationID: 91, InstallationID: 777, AccountID: 42, Reason: publish.InstallationRemovalGrantDrift,
		RequestedAt: time.Now().UTC(), ObservedRepositoryIDs: []int64{44, 55},
	}); err != nil {
		t.Fatal(err)
	}
	recipe := filepath.Join(root, "recipe.json")
	if err := os.WriteFile(recipe, []byte(`{"commands":[["go","test","./..."]],"capture":"none"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{
		"example/repo", "-db", filepath.Join(root, "freeside.db"), "-state-dir", stateDir, "-credentials-dir", credentialsDir,
		"-registration-id", "91", "-repository-id", "44", "-account", "example", "-account-id", "42",
		"-commit", "0123456789012345678901234567890123456789", "-base-ref", "main",
		"-base-image", "example.invalid/agent@sha256:test", "-base-build-ref", "local/agent:test",
		"-review-config-digest", "sha256:review", "-recipe", recipe, "-recover-installation", "777",
	}
	var output bytes.Buffer
	if err := runOnboardCommand(t.Context(), args, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Status          string                        `json:"status"`
		Pending         publish.PendingEnvelopeRecord `json:"pending"`
		InstallationURL string                        `json:"installation_url"`
	}
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "installation_pending" || result.InstallationURL != "https://github.com/apps/freeside-example/installations/new" ||
		result.Pending.InstallationID == nil || *result.Pending.InstallationID != 0 || len(result.Pending.CurrentRepositoryIDs) != 0 {
		t.Fatalf("fresh recovery result = %+v", result)
	}
	snapshot, err := authority.InstallationAuthority(t.Context(), 91)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.TrustedInstallations) != 0 || len(snapshot.QuarantinedInstallationIDs) != 1 || snapshot.QuarantinedInstallationIDs[0] != 777 {
		t.Fatalf("recovery changed terminal authority: %+v", snapshot)
	}
}
