package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/freeside-ai/freeside/daemon/internal/domain"
	"github.com/freeside-ai/freeside/daemon/internal/publish"
	"github.com/freeside-ai/freeside/daemon/internal/store"
)

func TestParseOnboardConfigBuildEgress(t *testing.T) {
	baseArgs := []string{
		"example/repo",
		"-db", "/tmp/freeside.db",
		"-state-dir", "/tmp/freeside-state",
		"-registration-id", "11",
		"-repository-id", "44",
		"-commit", "0123456789012345678901234567890123456789",
		"-base-ref", "main",
		"-base-image", "example.invalid/agent@sha256:test",
		"-base-build-ref", "local/agent:test",
		"-review-config-digest", "sha256:review",
		"-recipe", "/tmp/verify.json",
	}
	t.Run("provided values preserve order", func(t *testing.T) {
		cfg, err := parseOnboardConfig(append(baseArgs,
			"-dns", "1.1.1.1",
			"-build-proxy", "http://192.168.64.1:53536",
			"-dns", "8.8.8.8",
		), io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if want := []string{"1.1.1.1", "8.8.8.8"}; !slices.Equal(cfg.DNS, want) {
			t.Fatalf("DNS = %v, want %v", cfg.DNS, want)
		}
		if want := "http://192.168.64.1:53536"; cfg.BuildProxy != want {
			t.Fatalf("BuildProxy = %q, want %q", cfg.BuildProxy, want)
		}
	})
	t.Run("absent values stay zero", func(t *testing.T) {
		cfg, err := parseOnboardConfig(baseArgs, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.DNS != nil {
			t.Fatalf("DNS = %v, want nil", cfg.DNS)
		}
		if cfg.BuildProxy != "" {
			t.Fatalf("BuildProxy = %q, want empty", cfg.BuildProxy)
		}
	})
}

func TestOnboardStoreCanPassSubmitTopicKeyGate(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "freeside.db")
	recipePath := filepath.Join(root, "verify.json")
	if err := os.WriteFile(
		recipePath, []byte(`{"commands":[["go","test","./..."]],"capture":"none"}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	err := runOnboardCommand(t.Context(), []string{
		"example/repo",
		"-db", dbPath,
		"-state-dir", filepath.Join(root, "state"),
		"-credentials-dir", filepath.Join(root, "credentials"),
		"-registration-id", "11",
		"-repository-id", "44",
		"-commit", "0123456789012345678901234567890123456789",
		"-base-ref", "main",
		"-base-image", "example.invalid/agent@sha256:test",
		"-base-build-ref", "local/agent:test",
		"-review-config-digest", "sha256:review",
		"-recipe", recipePath,
	}, io.Discard, io.Discard)
	const want = "-account, positive -account-id, and non-negative -installation-id are required until the repository is trusted"
	if err == nil || err.Error() != want {
		t.Fatalf("runOnboardCommand error = %v, want %q", err, want)
	}
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("stat onboarded store: %v", err)
	}
	info, err := os.Stat(dbPath + topicKeySuffix)
	if err != nil {
		t.Fatalf("stat onboarded topic key: %v", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != sha256.Size {
		t.Fatalf("onboarded topic key mode/size = %v/%d, want regular 0600/%d", info.Mode(), info.Size(), sha256.Size)
	}
	key, err := loadOrCreateTopicKey(dbPath, true)
	if err != nil {
		t.Fatalf("submit topic-key gate: %v", err)
	}
	if len(key) != sha256.Size {
		t.Fatalf("topic key length = %d, want %d", len(key), sha256.Size)
	}
}

func TestOnboardRejectsInvalidCommitPlanBeforeStoreWork(t *testing.T) {
	recipePath := filepath.Join(t.TempDir(), "verify.json")
	if err := os.WriteFile(
		recipePath, []byte(`{"commands":[["go","test","./..."]],"capture":"none"}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	err := runOnboardCommand(t.Context(), []string{
		"example/repo",
		"-db", filepath.Join(t.TempDir(), "missing", "freeside.db"),
		"-state-dir", t.TempDir(),
		"-registration-id", "11",
		"-repository-id", "44",
		"-commit", "0123456789012345678901234567890123456789",
		"-base-ref", "main",
		"-base-image", "example.invalid/agent@sha256:test",
		"-base-build-ref", "local/agent:test",
		"-review-config-digest", "sha256:review",
		"-recipe", recipePath,
		"-commit-plan", "bogus",
	}, io.Discard, io.Discard)
	const want = "-commit-plan \"bogus\" is invalid; valid values: [single_commit plan_preferred]"
	if err == nil || err.Error() != want {
		t.Fatalf("runOnboardCommand error = %v, want %q", err, want)
	}
}

type installationAuthorityFixtures map[int64]publish.InstallationAuthority

func (f installationAuthorityFixtures) InstallationAuthority(
	_ context.Context, registrationID int64,
) (publish.InstallationAuthority, error) {
	authority, ok := f[registrationID]
	if !ok {
		return publish.InstallationAuthority{}, errors.New("missing registration fixture")
	}
	return authority, nil
}

func TestTrustedInstallationChecksEveryLocalRegistration(t *testing.T) {
	const repositoryID int64 = 44
	trusted := func(registrationID, installationID int64) publish.InstallationAuthority {
		return publish.InstallationAuthority{
			TrustedInstallations: []publish.TrustedInstallation{{
				RegistrationID: registrationID, InstallationID: installationID,
				Account: "example", AccountID: 33,
				RepositoryIDs: []int64{repositoryID},
			}},
		}
	}
	for _, tc := range []struct {
		name        string
		authorities installationAuthorityFixtures
		selected    int64
		want        *publish.TrustedInstallation
		wantErr     error
	}{
		{
			name: "selected registration owns repository",
			authorities: installationAuthorityFixtures{
				11: trusted(11, 21),
				12: {TrustedInstallations: []publish.TrustedInstallation{}},
			},
			selected: 11,
			want: &publish.TrustedInstallation{
				RegistrationID: 11, InstallationID: 21,
				Account: "example", AccountID: 33,
				RepositoryIDs: []int64{repositoryID},
			},
		},
		{
			name: "different registration already owns repository",
			authorities: installationAuthorityFixtures{
				11: {TrustedInstallations: []publish.TrustedInstallation{}},
				12: trusted(12, 22),
			},
			selected: 11, wantErr: publish.ErrAmbiguousInstallation,
		},
		{
			name: "multiple registrations already own repository",
			authorities: installationAuthorityFixtures{
				11: trusted(11, 21),
				12: trusted(12, 22),
			},
			selected: 11, wantErr: publish.ErrAmbiguousInstallation,
		},
		{
			name: "repository is not yet trusted",
			authorities: installationAuthorityFixtures{
				11: {TrustedInstallations: []publish.TrustedInstallation{}},
				12: {TrustedInstallations: []publish.TrustedInstallation{}},
			},
			selected: 11,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := trustedInstallation(
				t.Context(), tc.authorities, []int64{11, 12},
				tc.selected, repositoryID,
			)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("trustedInstallation error = %v, want %v", err, tc.wantErr)
			}
			if tc.want == nil {
				if got != nil {
					t.Fatalf("trustedInstallation = %+v, want nil", got)
				}
				return
			}
			if got == nil || got.RegistrationID != tc.want.RegistrationID ||
				got.InstallationID != tc.want.InstallationID {
				t.Fatalf("trustedInstallation = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestAttendedImportUsesCurrentActiveTrustProfile(t *testing.T) {
	ctx := t.Context()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "freeside.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	profile, err := domain.NewAutomationTrustProfile(domain.AutomationTrustProfileInput{
		Repo: "example/repo", RepositoryID: 44,
		PRExecution:                domain.PRExecutionAuditedSameRepo,
		CandidateAutomationChanges: domain.AutomationChangesBlocked,
		PRGitHubTokenPermissions:   domain.TokenPermissionsReadOnly,
		CommitPlan:                 domain.CommitPlanSingleCommit,
		MessageRuleset:             domain.MessageRulesetGitHub1,
		WorkflowAuditDigest:        "sha256:workflow-audit",
		Review: domain.ReviewSettings{
			Mode: domain.ReviewFreesideInvoked, ConfigDigest: "sha256:review",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 31, 6, 15, 0, 0, time.UTC)
	if err := st.WriteInternal(ctx, func(tx *store.InternalTx) error {
		if err := tx.RecordInactiveTrustProfile(ctx, profile, now); err != nil {
			return err
		}
		return tx.ActivateTrustProfile(ctx, profile.Repo, profile.ProfileDigest, now)
	}); err != nil {
		t.Fatal(err)
	}
	authority := storeAdmissionAuthority{store: st}
	admission := domain.ExecutionAdmission{
		ID: "sha256:attended-admission", OperatingMode: domain.ModeAttendedDev,
		Base: domain.BaseRevision{Repo: profile.Repo, RepositoryID: profile.RepositoryID},
	}
	got, err := authority.importTrustProfile(ctx, admission)
	if err != nil {
		t.Fatalf("attended import profile: %v", err)
	}
	if got.ProfileDigest != profile.ProfileDigest {
		t.Fatalf("attended import profile = %s, want %s", got.ProfileDigest, profile.ProfileDigest)
	}
	admission.OperatingMode = domain.ModeUnattended
	digest := profile.ProfileDigest
	admission.TrustProfileDigest = &digest
	got, err = authority.importTrustProfile(ctx, admission)
	if err != nil {
		t.Fatalf("bound unattended import profile: %v", err)
	}
	if got.ProfileDigest != profile.ProfileDigest {
		t.Fatalf("unattended import profile = %s, want %s", got.ProfileDigest, profile.ProfileDigest)
	}
	admission.TrustProfileDigest = nil
	if _, err := authority.importTrustProfile(ctx, admission); !errors.Is(err, domain.ErrEmptyField) {
		t.Fatalf("unbound unattended import profile = %v, want ErrEmptyField", err)
	}
}
