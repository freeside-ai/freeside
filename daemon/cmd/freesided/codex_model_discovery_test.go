package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/daemoncompose"
	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/store"
	"github.com/freeside-ai/freeside/daemon/internal/ward"
)

const codexModelDiscoveryTestImage = "example.test/agent-codex@sha256:" +
	"2222222222222222222222222222222222222222222222222222222222222222"

func codexDiscoveredReport(ids ...string) ward.ModelReport {
	report := ward.ModelReport{CLIBuild: "codex-cli 0.162.1"}
	for _, id := range ids {
		report.Models = append(report.Models,
			domain.DiscoveredModel{ID: id, DisplayName: id, EffortLevels: []string{"low", "max"}})
	}
	return report
}

// codexAuthAddArgs writes an operator login under a private input root and
// returns auth add's Codex arguments with the live store's root and path.
func codexAuthAddArgs(t *testing.T, dbPath string) (args []string, storeRoot, storePath string) {
	t.Helper()
	root := t.TempDir()
	inputRoot, storeRoot := filepath.Join(root, "input"), filepath.Join(root, "store")
	for _, dir := range []string{inputRoot, storeRoot} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	storeRoot, err := filepath.EvalSymlinks(storeRoot)
	if err != nil {
		t.Fatal(err)
	}
	inputPath, storePath := filepath.Join(inputRoot, "auth.json"), filepath.Join(storeRoot, "auth.json")
	var input map[string]any
	if err := json.Unmarshal(commandCodexAuth("operator-refresh"), &input); err != nil {
		t.Fatal(err)
	}
	input["tokens"].(map[string]any)["account_id"] = "acct-fixture-0001"
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inputPath, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{
		"-db", dbPath, "-client", "codex_cli", "-auth-identity", "codex-primary",
		"-route", "openai-subscription", "-cost-owner", "operator", "-project", "project-1",
		"-input-root", inputRoot, "-input-file", inputPath,
		"-auth-store-root", storeRoot, "-auth-store", storePath,
	}, storeRoot, storePath
}

// fakeCodexDiscovery counts discoveries and answers with report or err. The
// credential it gets is ward.ReviewSnapshotProbeCredential's, whose
// refresh-token strip the ward's tests pin.
type fakeCodexDiscovery struct {
	calls  int
	report ward.ModelReport
	err    error
}

func (f *fakeCodexDiscovery) discoverer(ward.Runtime) daemoncompose.CodexModelDiscoverer {
	return func(_ context.Context, image string, _ ward.LineupProbeCredential) (ward.ModelReport, error) {
		f.calls++
		if image != codexModelDiscoveryTestImage {
			return ward.ModelReport{}, errors.New("discovered in another image")
		}
		return f.report, f.err
	}
}

// TestAuthAddCodexDiscoversWithAnAgentImage: given -agent-image, auth add
// discovers the new Codex enrollment, and auth list prints its catalog; a
// failed discovery only warns, and without the flag the command says when
// discovery will run.
func TestAuthAddCodexDiscoversWithAnAgentImage(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		image     bool
		err       error
		wantCalls int
		stderr    string
	}{
		{"discovers", true, nil, 1, ""},
		{"failed discovery warns", true, errors.New("probe failed"), 1, "warning: model discovery for [codex-primary/codex_cli] failed"},
		{"no image", false, nil, 0, "the next daemon start with a Codex reviewer discovers it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dbPath := filepath.Join(t.TempDir(), "freeside.db")
			seedTemplateStore(t, dbPath)
			args, _, _ := codexAuthAddArgs(t, dbPath)
			if tc.image {
				args = append(args, "-agent-image", codexModelDiscoveryTestImage)
			}
			fake := &fakeCodexDiscovery{report: codexDiscoveredReport("gpt-6.1-sol"), err: tc.err}
			deps := authAddTestDeps()
			deps.codexModelDiscovery = fake.discoverer
			var stdout, stderr bytes.Buffer
			if err := runAuthAddCommand(t.Context(), args, nil, &stdout, &stderr, deps); err != nil {
				t.Fatalf("auth add: %v; stderr = %s", err, stderr.String())
			}
			if fake.calls != tc.wantCalls || !strings.Contains(stderr.String(), tc.stderr) {
				t.Fatalf("calls = %d, stderr = %q", fake.calls, stderr.String())
			}
			catalog := runAuthListForTest(t, dbPath).Identities[0].Enrollments[0].ModelCatalog
			switch {
			case tc.image && tc.err == nil:
				if catalog == nil || catalog.HarnessBuild != "codex-cli 0.162.1" || len(catalog.Models) != 1 ||
					catalog.Models[0].ID != "gpt-6.1-sol" || !slices.Equal(catalog.Models[0].EffortLevels, []string{"low", "max"}) {
					t.Fatalf("listed Codex catalog = %+v", catalog)
				}
			case catalog != nil:
				t.Fatalf("an undiscovered Codex enrollment lists a catalog: %+v", catalog)
			}
		})
	}
}

func TestAuthAddRefusesATagOnlyCodexAgentImage(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	args, _, _ := codexAuthAddArgs(t, dbPath)
	err := runAuthAddCommand(t.Context(), append(args, "-agent-image", "example.test/agent-codex:latest"),
		nil, &bytes.Buffer{}, &bytes.Buffer{}, authAddTestDeps())
	if err == nil || !strings.Contains(err.Error(), "digest-pinned") {
		t.Fatalf("err = %v", err)
	}
}

// TestRenewCodexDiscoversTheIdentitysEnrollment: given -agent-image,
// renew-codex rediscovers the identity's Codex enrollment from the renewed
// store; a failed discovery still exits 0 and keeps the last catalog.
func TestRenewCodexDiscoversTheIdentitysEnrollment(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "freeside.db")
	args := renewalCommandFixture(t, dbPath, nil)
	st, _, err := openStoreWithTopicKey(t.Context(), dbPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// The legacy identity gains an account and one Codex enrollment, so
	// renewal has an enrollment to rediscover and no re-enrollment hold.
	if err := st.Write(t.Context(), func(tx *store.WriteTx) error {
		identity, err := tx.GetAuthIdentity(t.Context(), "codex-primary")
		if err != nil {
			return err
		}
		identity.AccountBinding = "acct-fixture-0001"
		if err := tx.RecordAuthIdentity(t.Context(), identity, time.Now().UTC()); err != nil {
			return err
		}
		return tx.RecordClientEnrollment(t.Context(), domain.ClientEnrollment{
			ID: "codex-primary/codex_cli", AuthIdentityID: "codex-primary", HarnessClient: domain.HarnessClientCodexCLI,
			Route: "openai_chatgpt_codex", AuthMethod: domain.AuthMethodOAuth,
			CredentialMode:  domain.CredentialSubscriptionContained,
			RefreshStrategy: domain.RefreshOnDemand, SupportsReadOnlyAuthSnapshot: true,
			AccountBinding: identity.AccountBinding,
		}, time.Now().UTC())
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	renew := append(slices.Clone(args), "-agent-image", codexModelDiscoveryTestImage)
	fake := &fakeCodexDiscovery{report: codexDiscoveredReport("gpt-6-astra")}
	runtime := func(string) ward.Runtime { return unavailableRuntime{} }
	refresher := &commandCodexAuthRefresher{}
	var stderr bytes.Buffer
	if err := runRenewCodexCommandWithDeps(t.Context(), renew, &bytes.Buffer{}, &stderr,
		refresher, runtime, fake.discoverer); err != nil {
		t.Fatalf("renew-codex: %v; stderr = %s", err, stderr.String())
	}
	if fake.calls != 1 || stderr.Len() != 0 {
		t.Fatalf("calls = %d, stderr = %q", fake.calls, stderr.String())
	}
	listedCatalog := func() *authListModelCatalog {
		t.Helper()
		return runAuthListForTest(t, dbPath).Identities[0].Enrollments[0].ModelCatalog
	}
	if catalog := listedCatalog(); catalog == nil || catalog.Models[0].ID != "gpt-6-astra" {
		t.Fatalf("listed Codex catalog = %+v", catalog)
	}

	fake.report, fake.err = codexDiscoveredReport("gpt-6.1-sol"), errors.New("probe failed")
	stderr.Reset()
	if err := runRenewCodexCommandWithDeps(t.Context(), renew, &bytes.Buffer{}, &stderr,
		refresher, runtime, fake.discoverer); err != nil {
		t.Fatalf("a failed discovery failed renew-codex: %v", err)
	}
	if !strings.Contains(stderr.String(), "warning: model discovery for [codex-primary/codex_cli] failed") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if catalog := listedCatalog(); catalog == nil || catalog.Models[0].ID != "gpt-6-astra" {
		t.Fatalf("a failed discovery replaced the catalog: %+v", catalog)
	}

	stderr.Reset()
	if err := runRenewCodexCommandWithDeps(t.Context(), args, &bytes.Buffer{}, &stderr,
		refresher, runtime, fake.discoverer); err != nil {
		t.Fatal(err)
	}
	if fake.calls != 2 || !strings.Contains(stderr.String(), "skipped without -agent-image") {
		t.Fatalf("calls = %d, stderr = %q", fake.calls, stderr.String())
	}
}
